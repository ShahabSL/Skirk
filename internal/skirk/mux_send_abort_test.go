package skirk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func newSendAbortMux(t *testing.T, store BlobStore) *driveMux {
	t.Helper()
	m, err := newDriveMux(&Tunnel{Data: store, Secret: "send-abort-test", SessionID: [16]byte{1}, ClientID: "client", RunID: "run", SparsePriority: true}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeAll)
	return m
}

func TestMuxRemoteResetReleasesQueuedNormalDataAndBlockedProducer(t *testing.T) {
	m := newSendAbortMux(t, NewMemoryStore())
	s := m.registerStream(4, "client", "run", nil)
	s.priorityDemoted = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < muxNormalStreamQueue; i++ {
		if err := m.sendDataPayload(ctx, s, make([]byte, 8192)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- m.sendDataPayload(ctx, s, make([]byte, 8192)) }()
	select {
	case err := <-done:
		t.Fatalf("full queue did not block: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	m.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: "client", RunID: "run", StreamID: s.id})
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("aborted enqueue returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote reset left the normal producer blocked")
	}
	l := m.lanes[0]
	l.normalMu.Lock()
	defer l.normalMu.Unlock()
	if l.normalQueuedFrames != 0 || l.normalQueuedBytes != 0 || len(l.normalOrder) != 0 || s.normalPending.Load() != 0 {
		t.Fatalf("aborted stream retained queue: frames=%d bytes=%d order=%d pending=%d", l.normalQueuedFrames, l.normalQueuedBytes, len(l.normalOrder), s.normalPending.Load())
	}
}

func TestMuxRemoteResetReleasesBlockedPriorityProducer(t *testing.T) {
	m := newSendAbortMux(t, NewMemoryStore())
	s := m.registerStream(4, "client", "run", nil)
	l := m.lanes[0]
	for len(l.urgent) < cap(l.urgent) {
		l.urgent <- muxFrame{Kind: muxFrameRST, StreamID: 8}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.sendDataPayload(ctx, s, []byte("small request")) }()
	select {
	case err := <-done:
		t.Fatalf("priority queue did not block: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	m.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: "client", RunID: "run", StreamID: s.id})
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("priority abort returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote reset left priority producer blocked")
	}
	if len(l.urgent) != cap(l.urgent) {
		t.Fatal("abort changed other streams' urgent data")
	}
}

type cancelUploadStore struct {
	*MemoryStore
	started chan struct{}
	calls   atomic.Int64
}

func (s *cancelUploadStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if s.calls.Add(1) == 1 {
		close(s.started)
		<-ctx.Done()
		return ObjectInfo{}, ctx.Err()
	}
	return s.MemoryStore.PutObjectWithID(ctx, id, name, data)
}

func TestMuxRemoteResetCancelsNormalUploadAndLetsNextStreamProgress(t *testing.T) {
	store := &cancelUploadStore{MemoryStore: NewMemoryStore(), started: make(chan struct{})}
	m := newSendAbortMux(t, store)
	s := m.registerStream(4, "client", "run", nil)
	next := m.registerStream(8, "client", "run", nil)
	s.priorityDemoted, next.priorityDemoted = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := m.lanes[0]
	for _, stream := range []*muxStream{s, next} {
		if err := m.sendDataPayload(ctx, stream, make([]byte, 8192)); err != nil {
			t.Fatal(err)
		}
		frames, ok := l.takeNormalBatch(ctx)
		if !ok {
			t.Fatal("missing batch")
		}
		if stream == s {
			l.upload <- frames
		} else {
			go func() { l.upload <- frames }()
		}
	}
	workerDone := make(chan struct{})
	go func() { l.runUploadLoop(ctx, false); close(workerDone) }()
	t.Cleanup(func() { cancel(); <-workerDone })
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	m.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: "client", RunID: "run", StreamID: s.id})
	deadline := time.Now().Add(time.Second)
	for next.normalPending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.calls.Load() != 2 || next.normalPending.Load() != 0 || s.normalPending.Load() != 0 {
		t.Fatalf("reset retained upload worker: calls=%d next=%d aborted=%d", store.calls.Load(), next.normalPending.Load(), s.normalPending.Load())
	}
}

func TestMuxGracefulClosePreservesQueuedDataAndFIN(t *testing.T) {
	m := newSendAbortMux(t, NewMemoryStore())
	s := m.registerStream(4, "client", "run", nil)
	s.priorityDemoted = true
	ctx := context.Background()
	if err := m.sendDataPayload(ctx, s, []byte("final response")); err != nil {
		t.Fatal(err)
	}
	if err := m.sendFrame(ctx, muxFrame{Kind: muxFrameFIN, ClientID: "client", RunID: "run", StreamID: s.id, Seq: s.nextSendSeq()}); err != nil {
		t.Fatal(err)
	}
	s.markLocalReadDone()
	s.markRemoteReadDone()
	frames, ok := m.lanes[0].takeNormalBatch(ctx)
	if !ok || len(frames) != 2 || frames[0].Kind != muxFrameData || frames[1].Kind != muxFrameFIN {
		t.Fatalf("graceful tail lost: %+v", frames)
	}
	if err := m.lanes[0].uploadBatch(ctx, frames); err != nil {
		t.Fatal(err)
	}
	if s.normalPending.Load() != 0 {
		t.Fatal("graceful data did not complete")
	}
}

func TestAdaptiveLimiterStreamCancellationReleasesWithoutBackoff(t *testing.T) {
	l := newAdaptiveLimiter(8, 8, time.Second, "test", nil)
	l.inFlight = 2
	l.priorityBusy = 1
	l.release(false, context.Canceled, 10*time.Second, muxMaxBatch)
	if l.inFlight != 1 || l.priorityBusy != 1 || l.limit != 8 || !l.backoffUntil.IsZero() {
		t.Fatalf("cancellation changed provider health: in_flight=%d priority=%d limit=%d backoff=%v", l.inFlight, l.priorityBusy, l.limit, l.backoffUntil)
	}
	l.release(true, context.Canceled, 10*time.Second, 32)
	if l.inFlight != 0 || l.priorityBusy != 0 || l.limit != 8 {
		t.Fatal("priority cancellation retained capacity or lowered window")
	}
}
