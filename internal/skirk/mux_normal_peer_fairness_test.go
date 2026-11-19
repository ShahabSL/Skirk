package skirk

import (
	"context"
	"slices"
	"testing"
)

func newNormalPeerFairnessMux(t *testing.T, role string) *driveMux {
	t.Helper()
	m, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), Secret: "peer-fairness-test", SessionID: [16]byte{1}, ClientID: "client", RunID: "run", SparsePriority: true}, role, DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeAll)
	return m
}

func queuePeerFairnessFrames(t *testing.T, l *muxLane, keys []muxStreamKey, count uint64) {
	t.Helper()
	for _, key := range keys {
		for seq := uint64(1); seq <= count; seq++ {
			frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: seq, Payload: make([]byte, muxMinBatch)}
			if err := l.enqueueNormalFrame(context.Background(), frame); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestMuxExitNormalPeerThenStreamFairnessAndAbort(t *testing.T) {
	m := newNormalPeerFairnessMux(t, "exit")
	l := m.lanes[0]
	keys := []muxStreamKey{
		{ClientID: "a", RunID: "one", StreamID: 4},
		{ClientID: "a", RunID: "one", StreamID: 8},
		{ClientID: "b", RunID: "one", StreamID: 12},
		{ClientID: "b", RunID: "one", StreamID: 16},
		{ClientID: "b", RunID: "one", StreamID: 20},
		{ClientID: "a", RunID: "two", StreamID: 24},
	}
	queuePeerFairnessFrames(t, l, keys, 4)
	// Unequal stream populations get one turn per ready peer, including
	// independent runs of the same client. Each peer still rotates its streams.
	want := []int{0, 2, 5, 1, 3, 5, 0, 4, 5, 1, 2, 5}
	next := map[muxStreamKey]uint64{}
	for turn, i := range want {
		frames, ok := l.takeNormalBatch(context.Background())
		if !ok || len(frames) != 1 || frames[0].key() != keys[i] {
			t.Fatalf("turn %d: unexpected batch %+v", turn, frames)
		}
		next[keys[i]]++
		if frames[0].Seq != next[keys[i]] {
			t.Fatal("per-stream sequence changed")
		}
	}
	// A reset after rotation must not leave a second scheduling index that
	// can resurrect the aborted stream or affect another run's queued data.
	m.dropOutboundNormalFrames(keys[3])
	seen := 0
	for l.normalQueuedFrames > 0 {
		frames, ok := l.takeNormalBatch(context.Background())
		if !ok || len(frames) != 1 || frames[0].key() == keys[3] {
			t.Fatal("aborted stream returned")
		}
		key := frames[0].key()
		next[key]++
		if frames[0].Seq != next[key] {
			t.Fatal("remaining stream reordered")
		}
		seen++
	}
	if seen != 9 || len(l.normalOrder) != 0 || l.normalQueuedBytes != 0 {
		t.Fatalf("queue accounting: frames=%d order=%d bytes=%d", seen, len(l.normalOrder), l.normalQueuedBytes)
	}
}

func TestMuxClientNormalStreamOrderUnchanged(t *testing.T) {
	m := newNormalPeerFairnessMux(t, "client")
	l := m.lanes[0]
	keys := []muxStreamKey{
		{ClientID: "a", RunID: "one", StreamID: 4},
		{ClientID: "a", RunID: "one", StreamID: 8},
		{ClientID: "b", RunID: "one", StreamID: 12},
	}
	queuePeerFairnessFrames(t, l, keys, 2)
	for i := range 6 {
		frames, ok := l.takeNormalBatch(context.Background())
		if !ok || len(frames) != 1 || frames[0].key() != keys[i%3] || frames[0].Seq != uint64(i/3+1) {
			t.Fatalf("client stream order changed at turn %d: %+v", i, frames)
		}
	}
}

func TestMuxNormalPeerRotationAlreadyOrderedNoAllocation(t *testing.T) {
	a := muxStreamKey{ClientID: "a", RunID: "one", StreamID: 4}
	a2 := muxStreamKey{ClientID: "a", RunID: "one", StreamID: 8}
	b := muxStreamKey{ClientID: "b", RunID: "one", StreamID: 12}
	for name, order := range map[string][]muxStreamKey{
		"single peer":     {a, a2},
		"ordered suffix":  {b, a, a2},
		"peer now idle":   {b},
		"all queues idle": {},
	} {
		t.Run(name, func(t *testing.T) {
			l := &muxLane{normalOrder: slices.Clone(order)}
			allocs := testing.AllocsPerRun(100, func() { l.rotateNormalPeerLocked(a) })
			if allocs != 0 || !slices.Equal(l.normalOrder, order) {
				t.Fatalf("unchanged queue cost/order: allocations=%v order=%+v", allocs, l.normalOrder)
			}
		})
	}
}
