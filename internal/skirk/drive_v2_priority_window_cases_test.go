package skirk

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func driveV2WindowClaim(t *testing.T, s *fsHybridState, objectSeq uint64, streamID uint64, payload []byte) *fsHybridOutbound {
	t.Helper()
	record := fsHybridTestRecord(t, s.outgoing, objectSeq, 0, muxFrame{Kind: muxFrameData, StreamID: streamID, Seq: objectSeq, Payload: payload})
	r, fresh, err := s.admission(record.Name, record.Sealed, time.Now().Add(time.Second))
	if err != nil || !fresh {
		t.Fatal("valid priority claim rejected before queue policy", err, fresh)
	}
	return r
}

func TestDriveV2PriorityStreamWindowFourVersusEightAndFairSecondStream(t *testing.T) {
	for _, window := range []int{4, 8} {
		t.Run(string(rune('0'+window)), func(t *testing.T) {
			client, _ := fsHybridStatePair(t)
			client.outgoing.wireMax = driveV2PriorityCipherMax
			client.outPerStreamMax = window
			for i := 1; i <= 8; i++ {
				r := driveV2WindowClaim(t, client, uint64(i), 77, []byte{byte(i)})
				admitted := client.enqueue(r)
				if admitted != (i <= window) {
					t.Fatal("per-stream admission changed", window, i, admitted)
				}
			}
			if client.outByStream[77] != window || len(client.outQueue) != window || len(client.outQueue) >= fsHybridQueueMax {
				t.Fatal("bounded queue changed", window, client.outByStream[77], len(client.outQueue))
			}
			other := driveV2WindowClaim(t, client, 9, 88, []byte("another stream still moves"))
			if !client.enqueue(other) || client.outByStream[88] != 1 || len(client.outQueue) != window+1 {
				t.Fatal("busy stream starved a separate priority stream", window)
			}
		})
	}
}

func TestDriveV2PriorityStreamWindowConfigBounds(t *testing.T) {
	for _, selected := range []int{0, 4, 8, 5, 9, -1} {
		cfg, err := (fsHybridStoreConfig{InlinePerStream: selected}).normalized()
		valid := selected == 0 || selected == 4 || selected == 8
		if (err == nil) != valid || valid && cfg.InlinePerStream != max(4, selected) {
			t.Fatal("per-stream setting escaped fixed research bounds", selected, cfg.InlinePerStream, err)
		}
	}
}

func TestDriveV2PriorityWindowEightStillRespectsEncoded16KiB(t *testing.T) {
	client, _ := fsHybridStatePair(t)
	client.outgoing.wireMax = driveV2PriorityCipherMax
	client.outPerStreamMax = 8
	admitted := 0
	for i := 1; i <= 8; i++ {
		r := driveV2WindowClaim(t, client, uint64(i), 77, bytes.Repeat([]byte{byte(i)}, 2400))
		if client.enqueue(r) {
			admitted++
			continue
		}
		if r.record.Sequence != 0 || client.outByStream[77] != admitted {
			t.Fatal("oversized snapshot partially admitted a priority claim", i)
		}
	}
	if admitted < 4 || admitted >= 8 || len(client.outQueue) != admitted || client.outgoing.wireLimit() != driveV2PriorityCipherMax {
		t.Fatal("eight-window bypassed encoded 16 KiB snapshot bound", admitted, len(client.outQueue))
	}
	if wire, _, err := client.prepare(); err != nil || len(wire) > driveV2PriorityCipherMax {
		t.Fatal("bounded prefix could not be sealed", len(wire), err)
	}
}

func TestDriveV2PriorityWindowEightEncodedCapUsesImmutableFallback(t *testing.T) {
	client, _, backend, up, _, base := driveV2PriorityStoresWindow(t, 8)
	backend.mu.Lock()
	backend.frozenReadID = up.cfg.OwnID // Withhold peer ACKs so the encoded snapshot fills.
	backend.mu.Unlock()
	for i := uint64(1); i <= 8; i++ {
		record := fsHybridTestRecord(t, client.state.outgoing, i, 0, muxFrame{
			Kind: muxFrameData, StreamID: 77, Seq: i,
			Payload: bytes.Repeat([]byte{byte(i)}, 2400),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client.PutObject(ctx, record.Name, record.Sealed)
		cancel()
		if err != nil {
			t.Fatal("priority publication failed", i, err)
		}
	}
	stats := client.Snapshot()
	if stats["priority_enqueued"] < 4 || stats["priority_immutable_only"] < 1 || stats["fallback_published"] < 1 {
		t.Fatal("encoded cap did not use immutable fallback", stats)
	}
	client.mu.Lock()
	queueLen := len(client.state.outQueue)
	queueLimit := client.state.outPerStreamMax
	client.mu.Unlock()
	if queueLimit != 8 || queueLen >= 8 {
		t.Fatal("window or encoded snapshot bound changed", queueLimit, queueLen)
	}
	backend.mu.Lock()
	length := len(backend.docs[up.cfg.OwnID].wire)
	backend.frozenReadID = ""
	backend.mu.Unlock()
	if length > driveV2PriorityCipherMax {
		t.Fatal("oversized metadata advertisement", length)
	}
	if objects, err := base.List(context.Background(), ""); err != nil || len(objects) == 0 {
		t.Fatal("immutable fallback did not reach base store", len(objects), err)
	}
}
