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

// The larger per-stream window does not change the protocol's global FIFO or
// cumulative processing receipt. A single stalled head can hold receipts for
// unrelated streams. That is a latency/fairness limit, not a delivery loss:
// the next priority object must take the independent immutable path.
func TestDriveV2PriorityWindowEightSaturatedStreamsAndCumulativeACK(t *testing.T) {
	control, _ := fsHybridStatePair(t)
	control.outgoing.wireMax = driveV2PriorityCipherMax
	for i := uint64(1); i <= 16; i++ {
		stream := uint64(100 + (i-1)/4)
		if !control.enqueue(driveV2WindowClaim(t, control, i, stream, []byte{byte(i)})) {
			t.Fatal("four-window control rejected a claim before four streams filled their slots", i)
		}
	}
	if !control.enqueue(driveV2WindowClaim(t, control, 17, 104, []byte("fifth stream"))) {
		t.Fatal("four-window control lost the fifth stream's spare global capacity")
	}

	client, exit := fsHybridStatePair(t)
	client.outgoing.wireMax = driveV2PriorityCipherMax
	client.outPerStreamMax = 8
	for i := uint64(1); i <= fsHybridQueueMax; i++ {
		stream := uint64(100 + (i-1)/8)
		r := driveV2WindowClaim(t, client, i, stream, []byte{byte(i)})
		if !client.enqueue(r) {
			t.Fatalf("claim %d rejected before four streams filled the global queue", i)
		}
	}
	if len(client.outQueue) != fsHybridQueueMax || len(client.outByStream) != 4 {
		t.Fatal("four streams did not saturate the bounded queue", len(client.outQueue), client.outByStream)
	}
	for stream := uint64(100); stream < 104; stream++ {
		if client.outByStream[stream] != 8 {
			t.Fatal("per-stream occupancy changed", stream, client.outByStream[stream])
		}
	}
	newStream := driveV2WindowClaim(t, client, fsHybridQueueMax+1, 104, []byte("fifth stream"))
	if client.enqueue(newStream) || newStream.record.Sequence != 0 {
		t.Fatal("global cap did not divert the fifth stream to immutable fallback")
	}
	wire, high, err := client.prepare()
	if err != nil || high != fsHybridQueueMax || len(wire) > driveV2PriorityCipherMax {
		t.Fatal("saturated snapshot broke the encoded bound", high, len(wire), err)
	}
	client.published(high)
	ready, advanced, err := exit.receive(wire, time.Now())
	if err != nil || advanced || len(ready) != fsHybridQueueMax {
		t.Fatal("saturated snapshot lost claims", len(ready), advanced, err)
	}
	for _, entry := range ready[1:] {
		if _, _, advanced, err := exit.markProcessed(entry.info.ID); err != nil || advanced {
			t.Fatal("out-of-order processing advanced cumulative receipt", entry.sequence, err)
		}
	}
	ack0, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, advanced, err = client.receive(ack0, time.Now()); err != nil || advanced || client.acked != 0 || len(client.outQueue) != fsHybridQueueMax {
		t.Fatal("stalled head released an unrelated stream's queue slot", client.acked, len(client.outQueue), err)
	}
	if _, _, advanced, err = exit.markProcessed(ready[0].info.ID); err != nil || !advanced || exit.rxProcessed != fsHybridQueueMax {
		t.Fatal("head completion failed to release contiguous receipt", exit.rxProcessed, err)
	}
	ackAll, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, advanced, err = client.receive(ackAll, time.Now()); err != nil || advanced || client.acked != fsHybridQueueMax || len(client.outQueue) != 0 || client.outRetained != len(newStream.sealed) {
		t.Fatal("drained queue lost receipt or retained payload", client.acked, len(client.outQueue), client.outRetained, err)
	}
	for stream, outstanding := range client.outByStream {
		if outstanding != 0 {
			t.Fatal("per-stream slot leaked after cumulative receipt", stream, outstanding)
		}
	}
}

func TestDriveV2PriorityWindowEightSaturatedQueuePreservesPhysicalAndBulkPaths(t *testing.T) {
	client, _, backend, up, _, base := driveV2PriorityStoresWindow(t, 8)
	backend.mu.Lock()
	backend.frozenReadID = up.cfg.OwnID // Do not let peer processing free slots.
	backend.mu.Unlock()
	client.mu.Lock()
	for i := uint64(1); i <= fsHybridQueueMax; i++ {
		record := fsHybridTestRecord(t, client.state.outgoing, i, 0, muxFrame{
			Kind: muxFrameData, StreamID: 100 + (i-1)/8, Seq: i, Payload: []byte{byte(i)},
		})
		r, fresh, err := client.state.admission(record.Name, record.Sealed, time.Now().Add(time.Second))
		if err != nil || !fresh || !client.state.enqueue(r) {
			client.mu.Unlock()
			t.Fatal("could not saturate independent priority streams", i, fresh, err)
		}
	}
	client.dirty = true
	client.ackDue = time.Time{}
	client.mu.Unlock()
	client.signal(client.wake)

	physical := fsHybridTestRecord(t, client.state.outgoing, fsHybridQueueMax+1, 0, muxFrame{
		Kind: muxFrameData, StreamID: 104, Seq: fsHybridQueueMax + 1, Payload: []byte("fifth stream"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.PutObject(ctx, physical.Name, physical.Sealed); err != nil {
		t.Fatal("fifth priority stream did not use physical fallback", err)
	}
	if got := client.Snapshot(); got["priority_immutable_only"] != 1 || got["fallback_published"] < 1 || got["pending_records"] != fsHybridQueueMax {
		t.Fatal("saturation bypassed queue bound or lost fallback", got)
	}
	bulkName := "public-fixture/bulk/p1/independent"
	bulkBody := []byte("bulk traffic continues while priority receipts are stalled")
	if info, err := client.PutObject(ctx, bulkName, bulkBody); err != nil || info.Name != bulkName {
		t.Fatal("bulk write blocked behind priority FIFO", info, err)
	}
	if got, err := base.Get(ctx, bulkName); err != nil || !bytes.Equal(got, bulkBody) {
		t.Fatal("bulk payload was lost", err)
	}
	if got, err := base.Get(ctx, physical.Name); err != nil || !bytes.Equal(got, physical.Sealed) {
		t.Fatal("physical priority fallback was lost", err)
	}
	if err := client.Close(); err != nil || client.Snapshot()["joined"] != 1 {
		t.Fatal("saturated owner did not cancel and join", err)
	}
	targets, err := client.CleanupTargets()
	if err != nil {
		t.Fatal("joined owner did not retain exact cleanup targets", err)
	}
	found := false
	for _, target := range targets {
		found = found || target.Name == physical.Name && target.ID != ""
	}
	if !found {
		t.Fatal("cancellation lost the physical fallback's exact cleanup identity", targets)
	}
}
