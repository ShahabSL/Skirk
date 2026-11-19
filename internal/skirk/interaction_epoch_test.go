package skirk

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func interactionEpochFixture() (*driveMux, *muxStream) {
	mux := &driveMux{t: &Tunnel{InteractionEpoch: true}}
	return mux, &muxStream{mux: mux}
}

func TestInteractionEpochPromotesOnlyBoundedChatBurst(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := interactionEpochFixture()
	if stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start) {
		t.Fatal("large warmup frame became priority")
	}
	for i := 0; i < 24; i++ {
		at := start.Add(time.Second + time.Duration(i)*250*time.Millisecond)
		if !stream.claimPriorityPayloadAt(128, at) {
			t.Fatalf("chat frame %d did not enter bounded interaction epoch", i)
		}
	}
	if stream.claimPriorityPayloadAt(128, start.Add(7*time.Second)) {
		t.Fatal("chat frame after six-second epoch stayed priority without a fresh idle gap")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(8*time.Second)) {
		t.Fatal("fresh idle gap did not arm another interaction epoch")
	}
	if stream.claimPriorityFIN() {
		t.Fatal("FIN became priority after stream demotion")
	}
}

func TestInteractionEpochKeepsSparseMessagesAndPerStreamPacing(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := interactionEpochFixture()
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	if !stream.claimPriorityPayloadAt(128, start.Add(time.Second)) {
		t.Fatal("first quiet message stayed normal")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(time.Second+100*time.Millisecond)) {
		t.Fatal("same-stream frame bypassed 200 ms pacing")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(time.Second+250*time.Millisecond)) {
		t.Fatal("next paced message stayed normal")
	}
	for i := 1; i <= 3; i++ {
		if !stream.claimPriorityPayloadAt(128, start.Add(time.Second+250*time.Millisecond+time.Duration(i)*3*time.Second)) {
			t.Fatalf("sparse message %d stayed normal", i)
		}
	}
}

func TestInteractionEpochCapsPayloadAndTotalBytes(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := interactionEpochFixture()
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	for i := 0; i < 4; i++ {
		if !stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload, start.Add(time.Second+time.Duration(i)*250*time.Millisecond)) {
			t.Fatalf("priority chunk %d did not fit epoch byte budget", i)
		}
	}
	if stream.claimPriorityPayloadAt(128, start.Add(2*time.Second)) {
		t.Fatal("epoch exceeded 16 KiB byte budget")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(2250*time.Millisecond)) {
		t.Fatal("byte budget reset without another idle gap")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(4*time.Second)) {
		t.Fatal("idle gap reset the byte budget inside its six-second window")
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(7*time.Second)) {
		t.Fatal("byte budget did not restart after its six-second window")
	}
	if stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload+1, start.Add(7250*time.Millisecond)) {
		t.Fatal("oversized payload became priority")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(7500*time.Millisecond)) {
		t.Fatal("oversized payload failed to terminate interaction epoch")
	}
}

func TestInteractionEpochOversizedFrameCannotResetPartialBudget(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := interactionEpochFixture()
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	for i := 0; i < 2; i++ {
		if !stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload, start.Add(time.Second+time.Duration(i)*250*time.Millisecond)) {
			t.Fatalf("initial priority chunk %d was denied", i)
		}
	}
	if stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload+1, start.Add(1500*time.Millisecond)) {
		t.Fatal("oversized frame became priority")
	}
	if !stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload, start.Add(2500*time.Millisecond)) ||
		!stream.claimPriorityPayloadAt(muxSparsePriorityMaxPayload, start.Add(2750*time.Millisecond)) {
		t.Fatal("remaining byte budget was unavailable after a new idle gap")
	}
	if stream.claimPriorityPayloadAt(128, start.Add(4*time.Second)) {
		t.Fatal("oversized frame reset the six-second byte budget")
	}
}

func TestInteractionEpochGlobalBudgetAndPendingNormalGuard(t *testing.T) {
	start := time.Unix(100, 0)
	mux := &driveMux{t: &Tunnel{InteractionEpoch: true}}
	streams := make([]*muxStream, muxSparsePriorityGlobalBurst+2)
	for i := range streams {
		streams[i] = &muxStream{mux: mux}
		streams[i].claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	}
	grants := 0
	for _, stream := range streams {
		if stream.claimPriorityPayloadAt(128, start.Add(time.Second)) {
			grants++
		}
	}
	if grants != muxSparsePriorityGlobalBurst {
		t.Fatalf("simultaneous grants = %d, want global burst %d", grants, muxSparsePriorityGlobalBurst)
	}
	blocked := streams[len(streams)-1]
	blocked.normalPending.Add(1)
	if blocked.claimPriorityPayloadAt(128, start.Add(2*time.Second)) {
		t.Fatal("priority overtook an outstanding normal upload")
	}
	completeMuxFrameAcks([]muxFrame{{normalOwner: blocked}}, nil)
	if blocked.normalPending.Load() != 0 {
		t.Fatal("normal completion did not release pending gate")
	}
	if !blocked.claimPriorityPayloadAt(128, start.Add(3*time.Second)) {
		t.Fatal("quiet stream did not regain priority after normal upload completed")
	}
}

func TestInteractionEpochSharesTokensAcrossActiveStreams(t *testing.T) {
	start := time.Unix(100, 0)
	mux := &driveMux{t: &Tunnel{InteractionEpoch: true}}
	const streamCount = 10
	streams := make([]*muxStream, streamCount)
	grants := make([]int, streamCount)
	for i := range streams {
		streams[i] = &muxStream{mux: mux}
		streams[i].claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	}
	for round := 0; round <= 20; round++ {
		at := start.Add(time.Second + time.Duration(round)*250*time.Millisecond)
		for i, stream := range streams {
			if stream.claimPriorityPayloadAt(128, at) {
				grants[i]++
			}
		}
	}
	total, minGrants, maxGrants := 0, 100, 0
	for _, count := range grants {
		total += count
		if count < minGrants {
			minGrants = count
		}
		if count > maxGrants {
			maxGrants = count
		}
	}
	if total > muxSparsePriorityGlobalBurst+20 {
		t.Fatalf("global grants = %d, exceeded burst plus 20 refills", total)
	}
	if minGrants == 0 || maxGrants-minGrants > 2 {
		t.Fatalf("unfair grants across ten active streams: %v", grants)
	}
}

func TestInteractionEpochSustainedBulkWritesStayNormal(t *testing.T) {
	start := time.Unix(100, 0)
	_, stream := interactionEpochFixture()
	stream.claimPriorityPayloadAt(muxPriorityStreamBytes+1, start)
	for i := 1; i <= 100; i++ {
		if stream.claimPriorityPayloadAt(4*1024, start.Add(time.Duration(i)*100*time.Millisecond)) {
			t.Fatalf("continuous bulk-like write %d entered priority", i)
		}
	}
	if !stream.claimPriorityPayloadAt(128, start.Add(12*time.Second)) {
		t.Fatal("quiet stream could not begin a new interaction epoch")
	}
}

func TestInteractionEpochConfigAndNormalFrameBookkeeping(t *testing.T) {
	var cfg TunnelConfig
	if err := json.Unmarshal([]byte(`{"interaction_epoch":true}`), &cfg); err != nil || !cfg.InteractionEpoch {
		t.Fatalf("interaction epoch config: %+v err=%v", cfg, err)
	}
	mux := &driveMux{t: &Tunnel{InteractionEpoch: true, ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}
	if err := mux.sendDataPayload(context.Background(), stream, make([]byte, muxPriorityStreamBytes+1)); err != nil {
		t.Fatal(err)
	}
	if stream.normalPending.Load() != 1 {
		t.Fatalf("normal pending = %d, want one", stream.normalPending.Load())
	}
	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	batch, ok := lane.takeNormalBatch(context.Background())
	if !ok || len(batch) != 1 || batch[0].normalOwner != stream || muxPriorityFrame(batch[0]) {
		t.Fatalf("normal queue lost owner: ok=%t frames=%d", ok, len(batch))
	}
	completeMuxFrameAcks(batch, nil)
	if stream.normalPending.Load() != 0 {
		t.Fatalf("normal pending after upload = %d, want zero", stream.normalPending.Load())
	}
}
