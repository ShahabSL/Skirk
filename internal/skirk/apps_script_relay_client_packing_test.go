package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayClientPriorityPackingCountsObjectsBeforeNormalTurn(t *testing.T) {
	s := &relayResearchClient{cfg: relayResearchConfig{FrameBytes: 768 << 10, PackPriority: true}}
	normal := &relayResearchUpload{info: ObjectInfo{ID: "normal"}, data: make([]byte, 4<<20)}
	s.upQueue = append(s.upQueue, normal)
	for i := 0; i < 10; i++ {
		s.upQueue = append(s.upQueue, &relayResearchUpload{info: ObjectInfo{ID: fmt.Sprint(i)}, data: []byte{byte(i)}, priority: true})
	}
	burst := 0
	first := s.selectUpLocked(&burst, true)
	if len(first) != 4 || burst != 4 {
		t.Fatal("priority group exceeded object burst", len(first), burst)
	}
	for _, v := range first {
		v.finished = true
		s.rotateUpLocked(v, false)
	}
	next := s.selectUpLocked(&burst, true)
	if len(next) != 1 || next[0] != normal || burst != 0 {
		t.Fatal("packed priority starved normal fragment", len(next), burst)
	}
	if got := s.selectUpLocked(&burst, false); len(got) != 0 || burst != 0 {
		t.Fatal("delete-only changed packed fairness", len(got), burst)
	}
}

func TestRelayClientPackedCommitSetIsAtomic(t *testing.T) {
	s := &relayResearchClient{}
	selected := []*relayResearchUpload{{info: ObjectInfo{ID: "one"}, data: []byte{1}}, {info: ObjectInfo{ID: "two"}, data: []byte{2}}}
	p := relayResearchPacket{Pieces: []relayResearchPiece{{ID: "one", Total: 1, Data: []byte{1}}, {ID: "two", Total: 1, Data: []byte{2}}}}
	for _, ids := range [][]string{{"one"}, {"one", "foreign"}, {"one", "one"}} {
		if err := s.acceptUpReply(p, relayResearchPacket{Committed: ids}, selected); !errors.Is(err, errRelayResearchConflict) {
			t.Fatal("incomplete or foreign grouped commit accepted", ids, err)
		}
		if selected[0].finished || selected[1].finished || len(selected[0].data) != 1 || len(selected[1].data) != 1 {
			t.Fatal("invalid grouped reply partially freed callers")
		}
	}
}

func TestRelayClientPackedBothDirectionsExactRetryAndProcessingDelete(t *testing.T) {
	upRelease, pullRelease := make(chan struct{}), make(chan struct{})
	var upOnce, pullOnce sync.Once
	defer upOnce.Do(func() { close(upRelease) })
	defer pullOnce.Do(func() { close(pullRelease) })
	var upFirst, pullFirst, upLost, downLost atomic.Bool
	var downValidations atomic.Int64
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 768 << 10, PollWait: 5 * time.Millisecond, PackPriority: true}, func(b *relayResearchBroker) relayResearchExchange {
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			p, e := b.codec.open(wire)
			if e != nil {
				return nil, e
			}
			if p.Kind == rrUp && upFirst.CompareAndSwap(false, true) {
				select {
				case <-upRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if p.Kind == rrPull && pullFirst.CompareAndSwap(false, true) {
				select {
				case <-pullRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			raw, e := b.exchange(ctx, wire)
			if e != nil {
				return nil, e
			}
			r, e := b.codec.open(raw)
			if e != nil {
				return nil, e
			}
			if len(p.Pieces) > 1 && upLost.CompareAndSwap(false, true) {
				return nil, errors.New("injected committed-group response loss")
			}
			if len(r.Pieces) > 1 && downLost.CompareAndSwap(false, true) {
				return nil, errors.New("injected packed-PULL response loss")
			}
			return raw, nil
		}
	})
	c.setDownHooks(func(ObjectInfo, []byte) error { downValidations.Add(1); return nil }, nil)
	dummy := make(chan error, 1)
	go func() { dummy <- c.Put(context.Background(), "up/p1/initial", []byte{0}) }()
	relayClientWait(t, func() bool { return upFirst.Load() }, "initial UP did not reach controlled exchange")
	var publishers sync.WaitGroup
	errorsOut := make(chan error, 8)
	for i := 0; i < 8; i++ {
		publishers.Add(1)
		go func(i int) {
			defer publishers.Done()
			errorsOut <- c.Put(context.Background(), fmt.Sprintf("up/p0/%d", i), []byte{byte(i)})
		}(i)
	}
	relayClientWait(t, func() bool { return c.snapshot()["up_objects"] == 9 }, "priority upload cohort was not admitted")
	down := make([]ObjectInfo, 0, 8)
	for i := 0; i < 8; i++ {
		info, err := b.exitStore().PutObject(context.Background(), fmt.Sprintf("down/p0/%d", i), []byte{byte(10 + i)})
		if err != nil {
			t.Fatal(err)
		}
		down = append(down, info)
	}
	upOnce.Do(func() { close(upRelease) })
	pullOnce.Do(func() { close(pullRelease) })
	if err := <-dummy; err != nil {
		t.Fatal(err)
	}
	publishers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	relayClientWait(t, func() bool { return c.snapshot()["received"] == 8 }, "packed DOWN was not fully admitted")
	if !upLost.Load() || !downLost.Load() || c.snapshot()["packed_up_rpcs"] != 1 || c.snapshot()["packed_down_rpcs"] != 1 || b.snapshot().AcceptedUp != 9 || b.snapshot().AcceptedDown != 8 || b.snapshot().Replayed < 2 || downValidations.Load() != 8 {
		t.Fatal("packed exact-retry/cohort counts", c.snapshot(), b.snapshot())
	}
	if b.snapshot().DownBytes != 8 {
		t.Fatal("PULL receipt prematurely deleted group")
	}
	for i, info := range down {
		got, err := c.GetByID(context.Background(), info.ID)
		if err != nil || !bytes.Equal(got, []byte{byte(10 + i)}) {
			t.Fatal("packed DOWN mismatch", i, err)
		}
		if err = c.DeleteID(context.Background(), info.ID); err != nil {
			t.Fatal(err)
		}
	}
	if b.snapshot().DownBytes != 0 || c.snapshot()["down_bytes"] != 0 {
		t.Fatal("processing deletes did not free grouped ciphertext")
	}
}

func TestRelayClientPackedDownPreflightRejectsLaterInvalidBeforeExposure(t *testing.T) {
	_, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 768 << 10, PackPriority: true, PollWait: 5 * time.Millisecond}, nil)
	c.setDownHooks(func(_ ObjectInfo, data []byte) error {
		if bytes.Equal(data, []byte("bad")) {
			return errors.New("fictional inner authentication failure")
		}
		return nil
	}, nil)
	piece := func(id string, data []byte) relayResearchPiece {
		return relayResearchPiece{ID: id, Name: "down/p0/" + id, Total: len(data), Data: data, Hash: sha256.Sum256(data), Priority: true}
	}
	pieces := []relayResearchPiece{piece("first", []byte("good")), piece("second", []byte("bad"))}
	if err := c.preflightDownGroup(pieces); err == nil {
		t.Fatal("later invalid group member accepted")
	}
	infos, err := c.List(context.Background(), "down/")
	if err != nil || len(infos) != 0 || c.snapshot()["down_bytes"] != 0 || c.snapshot()["down_objects"] != 0 {
		t.Fatal("invalid group exposed its earlier member", infos, err, c.snapshot())
	}
	scalar := &relayResearchClient{cfg: relayResearchConfig{FrameBytes: 768 << 10}}
	if err = scalar.preflightDownGroup([]relayResearchPiece{piece("one", []byte{1}), piece("two", []byte{2})}); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("scalar peer accepted packed response", err)
	}
}

func TestRelayClientPackedDownWaitsForWholeCapacityWithoutPartialExposure(t *testing.T) {
	_, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, MaxBytes: 128 << 10, PackPriority: true, PollWait: 5 * time.Millisecond}, nil)
	data := bytes.Repeat([]byte{1}, 128<<10)
	first := relayResearchPiece{ID: "retained", Name: "down/p1/retained", Total: len(data), Data: data, Hash: sha256.Sum256(data)}
	if err := c.acceptDownPiece(first); err != nil {
		t.Fatal(err)
	}
	pieces := []relayResearchPiece{{ID: "one", Name: "down/p0/one", Total: 1, Data: []byte{2}, Hash: sha256.Sum256([]byte{2}), Priority: true}, {ID: "two", Name: "down/p0/two", Total: 1, Data: []byte{3}, Hash: sha256.Sum256([]byte{3}), Priority: true}}
	done := make(chan error, 1)
	go func() { done <- c.preflightDownGroup(pieces) }()
	relayClientWait(t, func() bool { return c.snapshot()["down_waits"] > 0 }, "whole group did not wait on capacity")
	if c.snapshot()["down_objects"] != 1 {
		t.Fatal("waiting group partially allocated claims")
	}
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed capacity wait succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("group wait leaked after Close")
	}
}
