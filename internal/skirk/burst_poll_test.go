package skirk

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type pollCountingStore struct {
	BlobStore
	lists atomic.Int64
}

func TestMuxPollSharesPendingTickAndWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticks := make(chan time.Time, 1)
	wakes := make(chan struct{}, 1)
	ticks <- time.Now()
	wakes <- struct{}{}
	if !waitForMuxPoll(ctx, ticks, wakes) || len(ticks) != 0 || len(wakes) != 0 {
		t.Fatal("one poll must service both pending signals")
	}
	wakes <- struct{}{}
	if !waitForMuxPoll(ctx, ticks, wakes) {
		t.Fatal("an upload arriving during the previous poll must still wake the receiver")
	}
	ticks <- time.Now()
	if !waitForMuxPoll(ctx, ticks, wakes) {
		t.Fatal("an independent scheduled poll must still run")
	}
	cancel()
	wakes <- struct{}{}
	if waitForMuxPoll(ctx, ticks, wakes) {
		t.Fatal("a stopped receiver must not dispatch another poll")
	}
}

type classPollCountingStore struct {
	*MemoryStore
	priority atomic.Int64
	normal   atomic.Int64
}

func (s *classPollCountingStore) ListFreshContainsPageStatus(_ context.Context, contains []string, _ time.Time, _ string, _ int) (ObjectListInfo, error) {
	if containsString(contains, "/p0/") {
		s.priority.Add(1)
	}
	if containsString(contains, "/p1/") {
		s.normal.Add(1)
	}
	return ObjectListInfo{}, nil
}

func (s *pollCountingStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	s.lists.Add(1)
	return s.BlobStore.List(ctx, prefix)
}

func TestBurstPollEndsWhenLastStreamCloses(t *testing.T) {
	now := time.Now()
	tunnel := &Tunnel{
		BurstPoll:         true,
		BurstPollInterval: 75 * time.Millisecond,
		BurstPollWindow:   5 * time.Second,
	}
	tunnel.lastUploadNS.Store(now.UnixNano())
	tunnel.lastActivityNS.Store(now.UnixNano())
	tunnel.activeStreams.Store(1)
	if !tunnel.burstPollActive(now) {
		t.Fatal("active stream should use burst polling after upload")
	}
	tunnel.activeStreams.Store(0)
	if tunnel.burstPollActive(now) {
		t.Fatal("recent activity should not keep burst polling after the final stream closes")
	}
	tunnel.activeStreams.Store(1)
	if tunnel.burstPollActive(now.Add(6 * time.Second)) {
		t.Fatal("burst polling should stop after its window")
	}
}

func TestReceiveTickerReturnsToBaseAfterBurstWake(t *testing.T) {
	store := &pollCountingStore{BlobStore: NewMemoryStore()}
	tunnel := &Tunnel{
		Data:              store,
		ClientID:          "client-test",
		RunID:             "run-test",
		PollInterval:      500 * time.Millisecond,
		BurstPoll:         true,
		BurstPollInterval: 25 * time.Millisecond,
		BurstPollWindow:   5 * time.Second,
	}
	tunnel.lastUploadNS.Store(time.Now().UnixNano())
	tunnel.activeStreams.Store(1)
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.active.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mux.runReceiveLoop(ctx); close(done) }()
	deadline := time.Now().Add(time.Second)
	for store.lists.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if store.lists.Load() < 2 {
		t.Fatal("receive loop did not start")
	}
	tunnel.activeStreams.Store(0)
	mux.wakeReceiver()
	time.Sleep(50 * time.Millisecond)
	baseline := store.lists.Load()
	time.Sleep(200 * time.Millisecond)
	if extra := store.lists.Load() - baseline; extra > 2 {
		t.Fatalf("received %d list calls after burst ended, want at most one base poll", extra)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not stop")
	}
}

func TestExitIdlePollNeverBacksOff(t *testing.T) {
	mux := &driveMux{role: "exit", t: &Tunnel{PollInterval: time.Second}}
	if got := mux.pollDelay(); got != time.Second {
		t.Fatalf("exit idle delay = %s, want base interval for new client opens", got)
	}
}

func TestClientIdleReceiverWaitsForLocalUploadWake(t *testing.T) {
	store := &pollCountingStore{BlobStore: NewMemoryStore()}
	tunnel := &Tunnel{Data: store, ClientID: "client-test", RunID: "run-test", PollInterval: 50 * time.Millisecond}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mux.runReceiveLoop(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	if got := store.lists.Load(); got != 0 {
		t.Fatalf("idle client made %d list calls, want none", got)
	}
	mux.active.Store(1)
	mux.wakeReceiver()
	deadline := time.Now().Add(500 * time.Millisecond)
	for store.lists.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if store.lists.Load() < 1 {
		t.Fatal("receiver did not poll immediately after wake")
	}
	mux.active.Store(0)
	time.Sleep(150 * time.Millisecond)
	baseline := store.lists.Load()
	time.Sleep(150 * time.Millisecond)
	if got := store.lists.Load(); got != baseline {
		t.Fatalf("inactive client made %d list calls after pausing, want none", got-baseline)
	}
	mux.active.Store(1)
	mux.wakeReceiver()
	deadline = time.Now().Add(500 * time.Millisecond)
	for store.lists.Load() <= baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if store.lists.Load() <= baseline {
		t.Fatal("receiver did not resume after a second wake")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not stop")
	}
}

func TestClientLastStreamCloseSchedulesFinalDrain(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test", PollInterval: time.Second}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	stream := mux.registerStream(1, tunnel.ClientID, tunnel.RunID, local)
	mux.unregisterStream(stream)
	if until := mux.clientFinalDrainAt.Load(); until <= time.Now().UnixNano() {
		t.Fatalf("final drain deadline = %d, want future time", until)
	}
}

func TestClientFinalDrainListsOnceThenWaits(t *testing.T) {
	store := &pollCountingStore{BlobStore: NewMemoryStore()}
	tunnel := &Tunnel{Data: store, ClientID: "client-test", RunID: "run-test", PollInterval: 20 * time.Millisecond}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.clientFinalDrainAt.Store(time.Now().Add(50 * time.Millisecond).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mux.runReceiveLoop(ctx); close(done) }()
	deadline := time.Now().Add(time.Second)
	for store.lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.lists.Load(); got != 1 {
		t.Fatalf("final drain lists = %d, want one", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := store.lists.Load(); got != 1 {
		t.Fatalf("idle lists after final drain = %d, want one", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not stop")
	}
}

func TestClientFinalDrainIncludesNormalClassWhenIdle(t *testing.T) {
	store := &classPollCountingStore{MemoryStore: NewMemoryStore()}
	tunnel := &Tunnel{Data: store, ClientID: "client-test", RunID: "run-test", PollInterval: 20 * time.Millisecond}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.clientFinalDrainAt.Store(time.Now().Add(50 * time.Millisecond).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mux.runReceiveLoop(ctx); close(done) }()
	deadline := time.Now().Add(time.Second)
	for store.normal.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.priority.Load(); got != 1 {
		t.Fatalf("final drain priority lists = %d, want one", got)
	}
	if got := store.normal.Load(); got != 1 {
		t.Fatalf("final drain normal lists = %d, want one", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not stop")
	}
}

func TestClosingStreamLeavesNoPendingFrames(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 500; id++ {
		stream := mux.registerStream(id, tunnel.ClientID, tunnel.RunID, nil)
		frame := muxFrame{Kind: muxFrameData, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: id, Seq: 1, Payload: []byte("late")}
		start := make(chan struct{})
		queued, closed := make(chan struct{}), make(chan struct{})
		go func() { <-start; mux.queuePendingFrame(context.Background(), frame); close(queued) }()
		go func() { <-start; stream.close(); close(closed) }()
		close(start)
		<-queued
		<-closed
		mux.pendingMu.Lock()
		count, bytes := len(mux.pending[stream.key()]), mux.pendingBytes[stream.key()]
		mux.pendingMu.Unlock()
		if count != 0 || bytes != 0 {
			t.Fatalf("closed stream %d left pending frames=%d bytes=%d", id, count, bytes)
		}
	}
}
