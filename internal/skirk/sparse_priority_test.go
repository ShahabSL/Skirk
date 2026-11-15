package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func sparsePriorityFixture(enabled bool) (*driveMux, *muxStream) {
	mux := &driveMux{t: &Tunnel{SparsePriority: enabled}}
	stream := &muxStream{mux: mux}
	return mux, stream
}

func TestSparsePriorityOptInRegrantsOnlyIdleSmallFrames(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := sparsePriorityFixture(true)
	if stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start) {
		t.Fatal("large initial frame became priority")
	}
	if !stream.priorityDemoted || stream.claimPriorityFIN() {
		t.Fatal("large stream did not demote")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(muxSparsePriorityIdleGap-time.Nanosecond)) {
		t.Fatal("packet before idle gap became priority")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(2*muxSparsePriorityIdleGap)) {
		t.Fatal("sparse small packet did not regain priority")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(3*muxSparsePriorityIdleGap)) {
		t.Fatal("per-stream priority interval was bypassed")
	}
	if stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload+1, start.Add(5*time.Second)) {
		t.Fatal("oversized packet became priority")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(6*time.Second)) {
		t.Fatal("later sparse packet stayed demoted")
	}
	if stream.claimPriorityFIN() {
		t.Fatal("FIN overtook earlier normal data")
	}
}

func TestSparsePriorityRequiresNormalUploadToComplete(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := sparsePriorityFixture(true)
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	stream.normalPending.Add(1)
	if stream.claimPriorityPayloadAt(128, start.Add(time.Second)) {
		t.Fatal("priority frame overtook an outstanding normal upload")
	}
	completeMuxFrameAcks([]muxFrame{{normalOwner: stream}}, nil)
	if stream.normalPending.Load() != 0 {
		t.Fatal("normal completion did not release priority gate")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(2*time.Second)) {
		t.Fatal("completed normal upload prevented sparse regrant")
	}
}

func TestSparsePrioritySustainedSmallWritesStayNormal(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := sparsePriorityFixture(true)
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	for i := 1; i <= 1000; i++ {
		if stream.claimPriorityPayloadAt(128, start.Add(time.Duration(i)*100*time.Millisecond)) {
			t.Fatalf("continuous small write %d flooded priority", i)
		}
	}
	_, disabled := sparsePriorityFixture(false)
	disabled.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	if disabled.claimPriorityPayloadAt(128, start.Add(time.Hour)) {
		t.Fatal("default-off stream regained priority")
	}
}

func TestSparsePriorityGlobalBudgetBoundsManyStreams(t *testing.T) {
	start := time.Unix(100, 0)
	mux := &driveMux{t: &Tunnel{SparsePriority: true}}
	streams := make([]*muxStream, muxSparsePriorityGlobalBurst+2)
	for i := range streams {
		streams[i] = &muxStream{mux: mux}
		streams[i].claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	}
	granted := 0
	for _, stream := range streams {
		if stream.claimPriorityPayloadAt(128, start.Add(time.Second)) {
			granted++
		}
	}
	if granted != muxSparsePriorityGlobalBurst {
		t.Fatalf("many-stream burst grants=%d, want %d", granted, muxSparsePriorityGlobalBurst)
	}
	if streams[len(streams)-1].claimPriorityPayloadAt(128, start.Add(time.Second+muxSparsePriorityGlobalRefill-time.Nanosecond)) {
		t.Fatal("global rate limit was bypassed before refill")
	}
	if !streams[len(streams)-1].claimPriorityPayloadAt(128, start.Add(time.Second+muxSparsePriorityGlobalRefill+muxSparsePriorityIdleGap)) {
		t.Fatal("global refill did not let a waiting stream progress")
	}
}

func TestSparsePriorityCoalescingReevaluatesAfterFirstByte(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := sparsePriorityFixture(true)
	stream.sendPayloadBytes.Store(bulkStreamCoalesceAfter)
	stream.lastPayloadAt = start
	if !stream.bulkSendCoalescingAt(start.Add(muxSparsePriorityIdleGap - time.Nanosecond)) {
		t.Fatal("active bulk stream lost bulk coalescing")
	}
	if stream.bulkSendCoalescingAt(start.Add(muxSparsePriorityIdleGap)) {
		t.Fatal("idle stream kept bulk coalescing for sparse text")
	}
	stream.normalPending.Add(1)
	if !stream.bulkSendCoalescingAt(start.Add(time.Hour)) {
		t.Fatal("outstanding normal upload lost bulk coalescing")
	}
	firstRead := false
	n, err := readChunkWithPolicyFunc(bytes.NewReader([]byte("message")), make([]byte, 32), func() bool {
		firstRead = true
		return false
	})
	if err != nil || n != len("message") || !firstRead {
		t.Fatalf("dynamic read policy was not evaluated after first bytes: n=%d err=%v", n, err)
	}
}

func TestSparsePriorityConfigAndNormalFrameBookkeeping(t *testing.T) {
	var cfg TunnelConfig
	if err := json.Unmarshal([]byte(`{"sparse_priority":true}`), &cfg); err != nil || !cfg.SparsePriority {
		t.Fatalf("sparse priority config: %+v err=%v", cfg, err)
	}
	mux := &driveMux{t: &Tunnel{SparsePriority: true, ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}
	if err := mux.sendDataPayload(context.Background(), stream, make([]byte, muxPriorityStreamBytes+1)); err != nil {
		t.Fatal(err)
	}
	if stream.normalPending.Load() != 1 {
		t.Fatalf("normal pending=%d, want 1", stream.normalPending.Load())
	}
	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	batch, ok := lane.takeNormalBatch(context.Background())
	if !ok || len(batch) != 1 || batch[0].normalOwner != stream || muxPriorityFrame(batch[0]) {
		t.Fatalf("normal queue lost owner: ok=%t frames=%d", ok, len(batch))
	}
	completeMuxFrameAcks(batch, nil)
	if stream.normalPending.Load() != 0 {
		t.Fatalf("normal pending after upload=%d, want 0", stream.normalPending.Load())
	}
}
