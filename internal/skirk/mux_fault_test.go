package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type firstWriteGateConn struct {
	net.Conn
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (c *firstWriteGateConn) Write(p []byte) (int, error) {
	if c.first.CompareAndSwap(false, true) {
		close(c.entered)
		<-c.release
	}
	return c.Conn.Write(p)
}

func TestMuxExitWritesInlineInitialBeforeFollowOnData(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	defer remote.Close()
	gate := &firstWriteGateConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	mux.dialExit = func(context.Context, string) (net.Conn, error) { return gate, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := muxFrame{Kind: muxFrameOpen, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 9, Payload: encodeMuxOpenPayload("example.test:80", []byte("initial"))}
	if !mux.claimExitOpenFrame(ctx, open) {
		t.Fatal("failed to claim Open frame")
	}
	opened := make(chan struct{})
	go func() { mux.openClaimedExitStream(ctx, open); close(opened) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("initial write did not begin")
	}
	mux.handleFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 9, Seq: 1, Payload: []byte("follow-on")})
	_ = remote.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var early [1]byte
	if n, err := remote.Read(early[:]); n != 0 || !isNetTimeout(err) {
		t.Fatalf("follow-on bytes overtook blocked initial write: n=%d error=%v", n, err)
	}
	close(gate.release)
	_ = remote.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("initialfollow-on"))
	if _, err := io.ReadFull(remote, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "initialfollow-on" {
		t.Fatalf("exit target received %q", got)
	}
	if stream := mux.streamByKey(open.key()); stream != nil {
		stream.close()
	}
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("Open handler did not finish")
	}
}

func TestMuxExitDialPermitReleasedBeforeBlockedInitialWrite(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	mux.exitOpenSlots = make(chan struct{}, 1)
	firstLocal, firstPeer := net.Pipe()
	secondLocal, secondPeer := net.Pipe()
	gate := &firstWriteGateConn{Conn: firstLocal, entered: make(chan struct{}), release: make(chan struct{})}
	secondDialed := make(chan struct{})
	mux.dialExit = func(_ context.Context, target string) (net.Conn, error) {
		switch target {
		case "first.test:80":
			return gate, nil
		case "second.test:80":
			close(secondDialed)
			return secondLocal, nil
		default:
			return nil, errors.New("unexpected target")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		close(gate.release)
		_ = firstPeer.Close()
		_ = secondPeer.Close()
		mux.closeAll()
		for _, done := range []<-chan struct{}{firstDone, secondDone} {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("exit Open handler did not join")
			}
		}
	})
	first := muxFrame{Kind: muxFrameOpen, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 1, Payload: encodeMuxOpenPayload("first.test:80", []byte("initial"))}
	second := muxFrame{Kind: muxFrameOpen, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 2, Payload: encodeMuxOpenPayload("second.test:80", nil)}
	if !mux.claimExitOpenFrame(ctx, first) || !mux.claimExitOpenFrame(ctx, second) {
		t.Fatal("could not claim distinct Open frames")
	}
	go func() { mux.openClaimedExitStream(ctx, first); close(firstDone) }()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first initial write did not begin")
	}
	go func() { mux.openClaimedExitStream(ctx, second); close(secondDone) }()
	select {
	case <-secondDialed:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("stalled initial write retained the only exit dial permit")
	}
}

func TestMuxExitInitialWriteHasFiniteDeadlineAndClearsIt(t *testing.T) {
	t.Run("stalled target", func(t *testing.T) {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()
		started := time.Now()
		err := writeExitOpenInitial(context.Background(), local, []byte("initial"), 50*time.Millisecond)
		if !isNetTimeout(err) || time.Since(started) > time.Second {
			t.Fatalf("stalled initial write was not bounded: %v", err)
		}
	})
	t.Run("successful write", func(t *testing.T) {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()
		read := make(chan error, 1)
		go func() {
			buf := make([]byte, len("initiallater"))
			_, err := io.ReadFull(peer, buf[:len("initial")])
			if err == nil {
				_, err = io.ReadFull(peer, buf[len("initial"):])
			}
			if err == nil && string(buf) != "initiallater" {
				err = errors.New("wrong data after initial write")
			}
			read <- err
		}()
		if err := writeExitOpenInitial(context.Background(), local, []byte("initial"), 50*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(75 * time.Millisecond)
		if err := writeAll(local, []byte("later")); err != nil {
			t.Fatalf("initial write left deadline on connection: %v", err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func TestMuxOpeningFramesWaitForInitialDataAndFlushInOrder(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(seq uint64, payload string) muxFrame {
		return muxFrame{Kind: muxFrameData, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 7, Seq: seq, Payload: []byte(payload)}
	}
	ctx := context.Background()
	mux.queuePendingFrame(ctx, frame(1, "first"))
	if mux.claimExitOpen(frame(1, "").key()) != muxExitOpenAdmitted {
		t.Fatal("failed to claim opening stream")
	}
	stream, ok := mux.registerStreamIfOpen(7, tunnel.ClientID, tunnel.RunID, nil)
	if !ok {
		t.Fatal("failed to register opening stream")
	}
	defer stream.close()
	mux.handleFrame(ctx, frame(2, "second"))
	mux.handleFrame(ctx, muxFrame{Kind: muxFrameFIN, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 7, Seq: 4})
	select {
	case payload := <-stream.inbound:
		t.Fatalf("delivered %q before initial bytes were written", payload)
	default:
	}

	// The exit calls this only after writing the Open frame's inline bytes.
	mux.flushPendingFrames(ctx, stream)
	for _, want := range []string{"first", "second"} {
		select {
		case payload := <-stream.inbound:
			if string(payload) != want {
				t.Fatalf("delivered %q, want %q", payload, want)
			}
		default:
			t.Fatalf("missing %q after opening flush", want)
		}
	}
	// A worker can retain a stale missing-stream lookup across registration
	// and the one-time flush. Such a frame must go to the live stream now.
	mux.queuePendingFrame(ctx, frame(3, "third"))
	select {
	case payload := <-stream.inbound:
		if string(payload) != "third" {
			t.Fatalf("late payload = %q", payload)
		}
	default:
		t.Fatal("frame arriving after opening flush was stranded")
	}
	select {
	case payload := <-stream.inbound:
		if payload != nil {
			t.Fatalf("terminal FIN payload = %q", payload)
		}
	default:
		t.Fatal("FIN crossing registration was stranded")
	}
	mux.pendingMu.Lock()
	pending := len(mux.pending[stream.key()])
	mux.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("live stream left %d pending frames", pending)
	}
}

func TestMuxOpeningRSTClosesRegisteredStream(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	key := muxStreamKey{ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 8}
	if mux.claimExitOpen(key) != muxExitOpenAdmitted {
		t.Fatal("failed to claim opening stream")
	}
	stream, ok := mux.registerStreamIfOpen(8, tunnel.ClientID, tunnel.RunID, nil)
	if !ok {
		t.Fatal("failed to register opening stream")
	}
	mux.queuePendingFrame(context.Background(), muxFrame{Kind: muxFrameRST, ClientID: tunnel.ClientID, RunID: tunnel.RunID, StreamID: 8})
	select {
	case <-stream.done:
	default:
		t.Fatal("RST arriving across registration did not close stream")
	}
}

func TestMuxDuplicateDriveIDsAreCleanedWithoutReplayingFrames(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test", CleanupProcessed: true}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	name := muxObjectNameWithStreamIDs(tunnel.SessionID, DirectionDown, tunnel.ClientID, tunnel.RunID, "epoch-test", 7, []uint64{7}, 0, 1, 1, 1, 1, 128, true)
	infos := []ObjectInfo{{Name: name, ID: "drive-id-a"}, {Name: name, ID: "drive-id-b"}}
	list := func(context.Context, string) ([]ObjectInfo, error) { return infos, nil }
	noMore := func() bool { return false }
	if !mux.pollMuxObjectsFromPrefix(context.Background(), mux.recvPrefix(), "test", list, parseMuxObjectInfo, noMore, nil) {
		t.Fatal("first copy was not queued")
	}
	if got := len(mux.recvUrgent); got != 1 {
		t.Fatalf("queued mux frames = %d, want one", got)
	}
	primary := <-mux.recvUrgent
	if got := len(mux.cleanupQueue); got != 0 {
		t.Fatalf("queued primary has %d premature duplicate deletes", got)
	}
	mux.markSeenProcessed(primary)
	select {
	case duplicate := <-mux.cleanupQueue:
		if duplicate.name != name || duplicate.id == primary.ID || duplicate.id == "" {
			t.Fatalf("duplicate cleanup = %+v, primary id = %q", duplicate, primary.ID)
		}
	default:
		t.Fatal("second Drive ID was not scheduled for cleanup")
	}
	mux.pollMuxObjectsFromPrefix(context.Background(), mux.recvPrefix(), "test", list, parseMuxObjectInfo, noMore, nil)
	if got := len(mux.recvUrgent); got != 0 {
		t.Fatalf("replayed mux frames = %d", got)
	}
	if got := len(mux.cleanupQueue); got != 0 {
		t.Fatalf("redundant duplicate cleanup tasks = %d", got)
	}

	missingMux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	missingMux.pollMuxObjectsFromPrefix(context.Background(), missingMux.recvPrefix(), "test", list, parseMuxObjectInfo, noMore, nil)
	missingPrimary := <-missingMux.recvUrgent
	missingMux.markSeen(missingPrimary.Name) // Primary Get returned 404.
	if got := len(missingMux.cleanupQueue); got != 0 {
		t.Fatalf("missing primary deleted %d valid retry copies", got)
	}
}

func TestMuxClientCancellationQueuesOneRSTOnlyForOpenStream(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	stream := mux.registerStream(11, tunnel.ClientID, tunnel.RunID, nil)
	mux.cancelClientStream(stream)
	lane := mux.lanes[mux.frameLane(muxFrame{StreamID: stream.id})]
	select {
	case frame := <-lane.urgent:
		if frame.Kind != muxFrameRST || frame.StreamID != stream.id {
			t.Fatalf("cancellation frame = %+v, want RST", frame)
		}
	default:
		t.Fatal("cancellation did not queue RST")
	}
	mux.cancelClientStream(stream)
	if got := len(lane.urgent); got != 0 {
		t.Fatalf("already-closed stream queued %d duplicate RSTs", got)
	}

	alreadyClosed := mux.registerStream(12, tunnel.ClientID, tunnel.RunID, nil)
	alreadyClosed.close()
	mux.cancelClientStream(alreadyClosed)
	if got := len(mux.lanes[mux.frameLane(muxFrame{StreamID: alreadyClosed.id})].urgent); got != 0 {
		t.Fatalf("naturally closed stream queued %d cancellation RSTs", got)
	}
}

func TestMuxClientCancellationAfterOpenAckQueuesOneRST(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ClientID: "client-test", RunID: "run-test"}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	local, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- mux.openClientStream(ctx, "example.test:80", local) }()
	var open muxFrame
	deadline := time.Now().Add(time.Second)
	for open.Kind == 0 && time.Now().Before(deadline) {
		for _, lane := range mux.lanes {
			select {
			case open = <-lane.urgent:
			default:
			}
		}
		if open.Kind == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if open.Kind != muxFrameOpen || open.Ack == nil {
		t.Fatalf("opening frame = %+v", open)
	}
	open.Ack.complete(nil)
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("client stream did not stop after cancellation")
	}
	lane := mux.lanes[mux.frameLane(open)]
	deadline = time.Now().Add(time.Second)
	for len(lane.urgent) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	rstCount := 0
	for len(lane.urgent) > 0 {
		frame := <-lane.urgent
		if frame.Kind == muxFrameRST {
			rstCount++
		}
	}
	if rstCount != 1 {
		t.Fatalf("canceled client queued %d RST frames, want one", rstCount)
	}
}

type flakyCleanupStore struct {
	*MemoryStore
	deleteCalls atomic.Int64
}

func (s *flakyCleanupStore) DeleteID(ctx context.Context, id string) error {
	if s.deleteCalls.Add(1) == 1 {
		return errors.New("temporary delete failure")
	}
	return s.MemoryStore.DeleteID(ctx, id)
}

func TestMuxCleanupRetriesTransientDeleteFailure(t *testing.T) {
	store := &flakyCleanupStore{MemoryStore: NewMemoryStore()}
	if _, err := store.PutObjectWithID(context.Background(), "object-id", "object-name", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Data: store, CleanupProcessed: true}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { mux.runCleanupLoop(ctx); close(done) }()
	mux.enqueueCleanup(cleanupTask{name: "object-name", id: "object-id"})
	mux.requestCleanupKick()
	deadline := time.Now().Add(5 * time.Second)
	for mux.cleanupDeleted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := store.deleteCalls.Load(); got != 2 || mux.cleanupDeleted.Load() != 1 {
		t.Fatalf("delete calls=%d successes=%d, want one retry and one success", got, mux.cleanupDeleted.Load())
	}
	if _, err := store.GetByID(context.Background(), "object-id"); err == nil {
		t.Fatal("transient cleanup retry left Drive object")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup loop did not stop")
	}
}

func TestMuxPriorityPaginationDoesNotStarveNormalDiscovery(t *testing.T) {
	store := &classFreshStatusStore{
		MemoryStore: NewMemoryStore(),
		priority:    ObjectListInfo{Truncated: true, NextPageToken: "next-priority-page"},
	}
	tunnel := &Tunnel{Data: store, ClientID: "client-test", RunID: "run-test", PollInterval: 500 * time.Millisecond}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.active.Store(1)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != 2 || !containsString(store.calls[0].contains, "/p0/") || !containsString(store.calls[1].contains, "/p1/") {
		t.Fatalf("class-list calls = %#v, want p0 then p1 despite priority continuation", store.calls)
	}
}
