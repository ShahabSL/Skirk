package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayBrokerPriorityPackPrevalidatesAllObjectsBeforeSideEffects(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 << 10, PollWait: time.Millisecond, PackPriority: true})
	var validations, publications atomic.Int64
	b.setUpHooks(func(_ ObjectInfo, data []byte) error {
		validations.Add(1)
		if bytes.Equal(data, []byte("bad-inner-aead")) {
			return errors.New("synthetic authentication failure")
		}
		return nil
	}, func(ObjectInfo) { publications.Add(1) })
	if _, err := b.putDown(context.Background(), "down-retained", "down/p1/retained", []byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 1})
	pieces := []relayResearchPiece{rrPiece("up-a", "up/p0/a", []byte("good")), rrPiece("up-b", "up/p0/b", []byte("bad-inner-aead"))}
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: pieces, Deletes: []string{"down-retained"}})
	if r.Status != rrStatusConflict || len(r.Committed) != 0 || len(r.Deleted) != 0 || b.snapshot().UpObjects != 0 || b.snapshot().DownBytes != 1 || publications.Load() != 0 || validations.Load() != 2 {
		t.Fatal("invalid later member caused earlier object/deletion side effects", r, b.snapshot(), publications.Load(), validations.Load())
	}
	pieces[1] = rrPiece("up-b", "up/p0/b", []byte("also-good"))
	wire, err := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: pieces, Deletes: []string{"down-retained"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.exchange(context.Background(), wire)
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatal("packed ambiguous retry changed exact result", err)
	}
	r, err = b.codec.open(first)
	if err != nil || len(r.Committed) != 2 || len(r.Deleted) != 1 || b.snapshot().AcceptedUp != 2 || publications.Load() != 2 || validations.Load() != 4 {
		t.Fatal("packed publication was repeated or validated twice", err, r, b.snapshot(), publications.Load(), validations.Load())
	}
	for _, p := range pieces {
		if err := b.deleteUp(p.ID); err != nil {
			t.Fatal(err)
		}
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 3, Receipt: 2, Pieces: pieces})
	if r.Status != rrStatusOK || len(r.Committed) != 2 || b.snapshot().UpBytes != 0 || publications.Load() != 2 || validations.Load() != 4 {
		t.Fatal("packed tombstone replay reexecuted side effects", r, b.snapshot())
	}
}

func TestRelayBrokerPriorityPackIsAtomicOnAdmissionAndKeepsNormalFairness(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 << 10, MaxObjects: 1, PackPriority: true})
	pieces := []relayResearchPiece{rrPiece("up-a", "up/p0/a", []byte{1}), rrPiece("up-b", "up/p0/b", []byte{2})}
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: pieces})
	if r.Status != rrStatusBounds || b.snapshot().UpObjects != 0 {
		t.Fatal("group partially consumed immutable claim capacity", r, b.snapshot())
	}
	b = rrFixture(t, relayResearchConfig{FrameBytes: 128 << 10, PollWait: time.Millisecond, PackPriority: true})
	if _, err := b.putDown(context.Background(), "normal", "down/p1/normal", []byte{9}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := b.putDown(context.Background(), fmt.Sprint("p0-", i), fmt.Sprintf("down/p0/%d", i), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	wire, _ := b.codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	first, err := b.exchange(context.Background(), wire)
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatal("packed PULL replay changed cursor or ciphertext", err)
	}
	r, _ = b.codec.open(first)
	if len(r.Pieces) != 8 {
		t.Fatal("priority pack count was not bounded by fairness burst", len(r.Pieces))
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 2, Receipt: 1})
	if len(r.Pieces) != 1 || r.Pieces[0].ID != "normal" {
		t.Fatal("priority packing starved waiting normal fragment", r)
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 3, Receipt: 2})
	if len(r.Pieces) != 4 {
		t.Fatal("remaining priority objects not packed after normal progress", len(r.Pieces))
	}
}
