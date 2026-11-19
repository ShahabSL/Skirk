package skirk

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestRelayBrokerPriorityReservationValidatesConfigurationAndClass(t *testing.T) {
	for _, cfg := range []relayResearchConfig{
		{PriorityReserveBytes: -1},
		{MaxBytes: 128 << 10, FrameBytes: 128 << 10, PriorityReserveBytes: 128 << 10},
		{PriorityReserveObjects: -1},
		{MaxObjects: 1, PriorityReserveObjects: 1},
	} {
		if _, err := cfg.normalized(); !errors.Is(err, errRelayResearchBounds) {
			t.Fatal("invalid reserve accepted", cfg, err)
		}
	}
	b := rrFixture(t, relayResearchConfig{})
	p := rrPiece("id", "up/p0/control", []byte{1})
	p.Priority = false
	if _, err := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{p}}); !errors.Is(err, errRelayResearchBounds) {
		t.Fatal("priority name and class mismatch accepted", err)
	}
	if _, err := b.acceptUp(p); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("direct admission bypassed priority class validation", err)
	}
}

func TestRelayBrokerPriorityBytesRemainAvailableInBothDirections(t *testing.T) {
	const frame = 128 << 10
	b := rrFixture(t, relayResearchConfig{FrameBytes: frame, MaxBytes: 2 * frame, PriorityReserveBytes: frame, PollWait: time.Millisecond})
	normal := rrPiece("up-normal", "up/p1/normal", bytes.Repeat([]byte{1}, frame))
	if r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{normal}}); r.Status != rrStatusOK {
		t.Fatal("normal UP admission failed", r.Status)
	}
	busy := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{rrPiece("up-over", "up/p1/over", []byte{2})}})
	if busy.Status != rrStatusBusy {
		t.Fatal("normal UP consumed priority reserve", busy.Status)
	}
	priority := rrPiece("up-priority", "up/p0/control", []byte{3})
	if r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 3, Receipt: 2, Pieces: []relayResearchPiece{priority}}); r.Status != rrStatusOK || len(r.Committed) != 1 {
		t.Fatal("priority UP blocked by full normal allowance", r)
	}
	if _, err := b.putDown(context.Background(), "down-normal", "down/p1/normal", normal.Data); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := b.putDown(waitCtx, "down-over", "down/p1/over", []byte{4}); waiting <- err }()
	if _, err := b.putDown(context.Background(), "down-priority", "down/p0/control", []byte{5}); err != nil {
		cancel()
		t.Fatal("priority DOWN blocked by full normal allowance", err)
	}
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal("normal DOWN used reserve or ignored cancellation", err)
	}
	s := b.snapshot()
	if s.UpBytes != frame+1 || s.DownBytes != frame+1 || s.NormalUpBytes != frame || s.NormalDownBytes != frame || s.NormalUpObjects != 1 || s.NormalDownObjects != 1 {
		t.Fatal("priority accounting mismatch", s)
	}
}

func TestRelayBrokerPriorityClaimsSurviveDeletedNormalPayloads(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 << 10, MaxObjects: 2, PriorityReserveObjects: 1, PollWait: time.Millisecond})
	up := rrPiece("up-normal", "up/p1/normal", []byte{1})
	_ = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{up}})
	if err := b.deleteUp(up.ID); err != nil {
		t.Fatal(err)
	}
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{rrPiece("up-over", "up/p1/over", []byte{2})}})
	if r.Status != rrStatusBounds {
		t.Fatal("deleted normal UP claim was reused", r.Status)
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 3, Receipt: 2, Pieces: []relayResearchPiece{rrPiece("up-priority", "up/p0/control", []byte{3})}})
	if r.Status != rrStatusOK {
		t.Fatal("normal tombstone consumed reserved UP claim", r.Status)
	}
	if _, err := b.putDown(context.Background(), "down-normal", "down/p1/normal", []byte{4}); err != nil {
		t.Fatal(err)
	}
	_ = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 1})
	if err := b.deleteDown("down-normal"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.putDown(context.Background(), "down-over", "down/p1/over", []byte{5}); !errors.Is(err, errRelayResearchBounds) {
		t.Fatal("deleted normal DOWN claim was reused", err)
	}
	if _, err := b.putDown(context.Background(), "down-priority", "down/p0/control", []byte{6}); err != nil {
		t.Fatal("normal tombstone consumed reserved DOWN claim", err)
	}
	s := b.snapshot()
	if s.UpObjects != 2 || s.DownObjects != 2 || s.NormalUpObjects != 1 || s.NormalDownObjects != 1 || s.NormalUpBytes != 0 || s.NormalDownBytes != 0 {
		t.Fatal("normal proof counts were not retained", s)
	}
}
