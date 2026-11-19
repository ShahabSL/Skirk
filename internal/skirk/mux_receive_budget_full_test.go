package skirk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// budgetGateStore delays real GetByID calls after the normal mux scheduler
// admits objects. This models slow in-flight Drive reads without altering mux
// counters or creating objects larger than the production 4 MiB limit.
type budgetGateStore struct {
	*MemoryStore
	mu      sync.Mutex
	gates   map[string]<-chan struct{}
	entered chan string
}

func (s *budgetGateStore) gate(id string, release <-chan struct{}) {
	s.mu.Lock()
	s.gates[id] = release
	s.mu.Unlock()
}

func (s *budgetGateStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	release := s.gates[id]
	s.mu.Unlock()
	if release != nil {
		select {
		case s.entered <- id:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.MemoryStore.GetByID(ctx, id)
}

func waitMuxBudget(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMuxFullReceiveHealthyStreamDrainsWhenAnotherFillsPrimaryBudget(t *testing.T) {
	const payloadBytes = 1 << 20
	store := &budgetGateStore{
		MemoryStore: NewMemoryStore(),
		gates:       make(map[string]<-chan struct{}),
		entered:     make(chan string, 12),
	}
	sid := [16]byte{1, 2, 3}
	tunnel := &Tunnel{Data: store, Secret: "local-receive-budget-fixture", SessionID: sid, ClientID: "client", RunID: "run", Transport: "muxv5", DownloadConcurrency: 12}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() { defer workers.Done(); mux.runReceiveWorker(ctx, false) }()
	}
	t.Cleanup(func() {
		cancel()
		workers.Wait()
		mux.closeAll()
	})

	firstLocal, firstPeer := net.Pipe()
	secondLocal, secondPeer := net.Pipe()
	t.Cleanup(func() { firstPeer.Close(); secondPeer.Close() })
	first := mux.registerStream(1, "client", "run", firstLocal)
	second := mux.registerStream(2, "client", "run", secondLocal)
	mux.startWriter(first)
	mux.startWriter(second)
	key, err := DeriveMuxLaneKeyV5(tunnel.Secret, sid, DirectionDown, "client", "run", 0, mux.epoch)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, payloadBytes)
	for i := range data {
		data[i] = byte(i)
	}
	objectSeq := uint64(0)
	addObject := func(streamID, firstFrame uint64, count int, priority bool, gate <-chan struct{}) {
		t.Helper()
		objectSeq++
		frames := make([]muxFrame, count)
		for i := range frames {
			seq := firstFrame + uint64(i)
			payload := bytes.Clone(data)
			payload[0] = byte(seq)
			frames[i] = muxFrame{Kind: muxFrameData, StreamID: streamID, Seq: seq, Payload: payload}
		}
		raw, err := encodeMuxBatch(frames)
		if err != nil || len(raw) > muxMaxBatch {
			t.Fatalf("encode %d frame object: %d bytes, %v", count, len(raw), err)
		}
		sealed, err := Seal(key, sid, DirectionDown, objectSeq, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		name := muxObjectNameWithStreamIDsForTransport("muxv5", sid, DirectionDown, "client", "run", mux.epoch, streamID, []uint64{streamID}, 0, objectSeq, count, firstFrame, firstFrame+uint64(count)-1, len(raw), priority)
		id := fmt.Sprintf("budget-object-%03d", objectSeq)
		info, err := store.PutObjectWithID(ctx, id, name, sealed)
		if err != nil {
			t.Fatal(err)
		}
		if gate != nil {
			store.gate(id, gate)
		}
		meta, ok := parseMuxObjectInfo(info)
		if !ok || meta.PlainBytes != len(raw) {
			t.Fatalf("invalid parsed budget object %s", id)
		}
		if !mux.enqueueMuxObject(ctx, meta) {
			t.Fatalf("normal mux admission rejected object %s", id)
		}
	}

	// Sixteen genuine 3 MiB Drive objects put 48 one-MiB frames through
	// decryption and ordered delivery. The blocked net.Pipe writer holds one
	// frame, leaving 47 frames in the primary channel: just below its
	// scheduling pause watermark of 48.
	for object := 0; object < 16; object++ {
		addObject(1, uint64(object*3+1), 3, false, nil)
		want := uint64(object*3 + 4)
		waitMuxBudget(t, "first-stream prefill", func() bool { return first.expectedRecvSeq() == want })
	}
	waitMuxBudget(t, "blocked first socket writer", func() bool { return len(first.inbound) == 47 })
	if frames, bytes, _ := inboundBudgetSnapshot(mux); frames != 47 || bytes != 47*payloadBytes {
		t.Fatalf("pre-admission primary budget %d frames/%d bytes, want 47 MiB", frames, bytes)
	}

	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	for object := 0; object < muxNormalStreamInflight; object++ {
		addObject(1, uint64(49+object*3), 3, false, firstRelease)
		addObject(2, uint64(1+object*3), 3, false, secondRelease)
	}
	waitMuxBudget(t, "both six-object scheduling windows", func() bool {
		mux.recvNormalMu.Lock()
		defer mux.recvNormalMu.Unlock()
		return mux.recvNormalActive[first.key()] == muxNormalStreamInflight && mux.recvNormalActive[second.key()] == muxNormalStreamInflight
	})
	// The auto profile starts with eight physical download permits and may
	// adapt upward, while twelve objects are admitted by the scheduler.
	waitMuxBudget(t, "at least eight real in-flight Drive reads", func() bool { return len(store.entered) >= 8 })
	mux.recvNormalMu.Lock()
	firstActive := mux.recvNormalActive[first.key()]
	secondActive := mux.recvNormalActive[second.key()]
	mux.recvNormalMu.Unlock()
	if firstActive != muxNormalStreamInflight || secondActive != muxNormalStreamInflight {
		t.Fatalf("in-flight count = %d/%d, want %d each", firstActive, secondActive, muxNormalStreamInflight)
	}

	close(firstRelease)
	waitMuxBudget(t, "first stream filling primary handoff", func() bool {
		_, bytes, _ := inboundBudgetSnapshot(mux)
		return bytes == muxGlobalInboundBytes && first.deferredInboundCount.Load() == 1
	})
	select {
	case <-first.done:
		t.Fatal("first slow stream closed before its one-frame deferred tail")
	default:
	}
	close(secondRelease)
	waitMuxBudget(t, "second stream retaining its ready ordered tail", func() bool {
		if second.isDone() {
			t.Fatal("blocked stream A caused stream B to reset")
		}
		mux.recvNormalMu.Lock()
		active := mux.recvNormalActive[second.key()]
		mux.recvNormalMu.Unlock()
		return active == 0 && second.recvWriterBacklog.Load()
	})
	mux.pendingMu.Lock()
	waitingFrames, waitingBytes := mux.reassemblyFrames, mux.reassemblyBytes
	mux.pendingMu.Unlock()
	if waitingFrames == 0 || waitingBytes < payloadBytes || waitingBytes > 18*(payloadBytes+muxFrameHeaderSize) {
		t.Fatalf("unexpected reassembly credit while first stream fills primary: %d frames/%d bytes", waitingFrames, waitingBytes)
	}
	// New normal work stays queued until the writer starts draining. The
	// priority FIN is already known but cannot overtake the last DATA.
	const secondFrames = 3*muxNormalStreamInflight + 1
	addObject(2, secondFrames, 1, false, nil)
	second.acceptFrame(ctx, muxFrame{Kind: muxFrameFIN, StreamID: 2, Seq: secondFrames + 1})
	mux.recvNormalMu.Lock()
	gap := mux.recvGaps[second.key()]
	queued := len(mux.recvNormalFlows[second.key()])
	mux.recvNormalMu.Unlock()
	if !gap.firstSeen.IsZero() || queued != 1 {
		t.Fatalf("buffering detected as a wire gap or lost queued data: gap=%v queued=%d", gap.firstSeen, queued)
	}
	// An unrelated small priority object still enters the shared receiver.
	// Its DATA and FIN must remain ordered even though the primary pool is
	// full and its handoff uses the deferred path.
	interactive := mux.registerStream(3, "client", "run", nil)
	objectSeq++
	frames := []muxFrame{
		{Kind: muxFrameData, StreamID: 3, Seq: 1, Payload: []byte("interactive")},
		{Kind: muxFrameFIN, StreamID: 3, Seq: 2},
	}
	raw, err := encodeMuxBatch(frames)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, sid, DirectionDown, objectSeq, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	name := muxObjectNameWithStreamIDsForTransport("muxv5", sid, DirectionDown, "client", "run", mux.epoch, 3, []uint64{3}, 0, objectSeq, len(frames), 1, 2, len(raw), true)
	info, err := store.PutObjectWithID(ctx, "budget-interactive", name, sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(info)
	if !ok || !mux.enqueueMuxObject(ctx, meta) {
		t.Fatal("interactive priority object rejected")
	}
	waitMuxBudget(t, "interactive DATA/FIN processing", func() bool { return interactive.expectedRecvSeq() == 3 })
	got, ok := interactive.nextInbound()
	if !ok || string(got) != "interactive" {
		t.Fatalf("interactive DATA = %q, ok=%t", got, ok)
	}
	fin, ok := interactive.nextInbound()
	if !ok || fin != nil {
		t.Fatalf("interactive FIN = %v, ok=%t", fin, ok)
	}

	if err := secondPeer.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, payloadBytes)
	for seq := byte(1); seq <= secondFrames; seq++ {
		if _, err := io.ReadFull(secondPeer, out); err != nil {
			t.Fatalf("stream B DATA %d: %v", seq, err)
		}
		if out[0] != seq || !bytes.Equal(out[1:], data[1:]) {
			t.Fatalf("stream B DATA %d arrived out of order or corrupted", seq)
		}
	}
	waitMuxBudget(t, "second DATA/FIN completion", func() bool {
		second.mu.Lock()
		defer second.mu.Unlock()
		return second.remoteReadDone && second.recvExpected == secondFrames+2
	})
	if _, occupiedBytes, _ := inboundBudgetSnapshot(mux); occupiedBytes != muxGlobalInboundBytes {
		t.Fatalf("stream B needed stream A to release its blocked socket: %d primary bytes", occupiedBytes)
	}
	mux.pendingMu.Lock()
	waitingFrames, waitingBytes = mux.reassemblyFrames, mux.reassemblyBytes
	mux.pendingMu.Unlock()
	if waitingFrames != 0 || waitingBytes != 0 {
		t.Fatalf("reassembly credit leaked after DATA/FIN: %d frames/%d bytes", waitingFrames, waitingBytes)
	}
}

func TestMuxNormalReceiveBacklogAbortReleasesBudget(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), Secret: "local-abort-fixture", SessionID: [16]byte{1}, ClientID: "client", RunID: "run"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mux.closeAll)
	stream := mux.registerStream(1, "client", "run", nil)
	for range cap(stream.inbound) {
		prefillMuxInbound(t, stream, []byte("occupied"))
	}
	payload := make([]byte, 1<<20)
	for seq := uint64(1); seq <= 10; seq++ {
		stream.acceptFrame(t.Context(), muxFrame{Kind: muxFrameData, Seq: seq, StreamID: stream.id, Payload: payload, normalReceive: true})
	}
	if !stream.recvWriterBacklog.Load() || stream.isDone() {
		t.Fatal("stream did not retain its ordered backlog")
	}
	stream.acceptFrame(t.Context(), muxFrame{Kind: muxFrameRST, StreamID: stream.id})
	if !stream.isDone() || stream.recvWriterBacklog.Load() {
		t.Fatal("remote reset did not close the stream and clear the retained tail")
	}
	mux.pendingMu.Lock()
	frames, bytes := mux.reassemblyFrames, mux.reassemblyBytes
	mux.pendingMu.Unlock()
	if frames != 0 || bytes != 0 {
		t.Fatalf("reassembly credit leaked after reset: %d frames/%d bytes", frames, bytes)
	}
	if frames, bytes, streams := inboundBudgetSnapshot(mux); frames != 0 || bytes != 0 || streams != 0 {
		t.Fatalf("primary credit leaked after reset: %d frames/%d bytes/%d streams", frames, bytes, streams)
	}
}

func TestMuxReceiveGapRearmsAfterWriterBacklogDrains(t *testing.T) {
	const streamID = 42
	sid := [16]byte{1}
	clientID, runID := "client", "run"
	missing := muxObjectNameWithStreamIDs(sid, DirectionDown, clientID, runID, "epoch-a", streamID, []uint64{streamID}, 0, 2, 1, 2, 2, muxMinBatch, false)
	store := &classFreshStatusStore{
		MemoryStore:  NewMemoryStore(),
		targetNeedle: fmt.Sprintf("%016x", streamID),
		targeted:     ObjectListInfo{Objects: []ObjectInfo{{Name: missing, Updated: time.Now().Format(time.RFC3339Nano)}}},
	}
	mux, err := newDriveMux(&Tunnel{Data: store, SessionID: sid, ClientID: clientID, RunID: runID, PollInterval: time.Second}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mux.startedAt = now.Add(-time.Minute)
	local, peer := net.Pipe()
	t.Cleanup(func() { peer.Close(); mux.closeAll() })
	stream := mux.registerStream(streamID, clientID, runID, local)
	for range cap(stream.inbound) + muxStreamDeferredFrames {
		prefillMuxInbound(t, stream, []byte{'a'})
	}
	stream.acceptFrame(t.Context(), muxFrame{Kind: muxFrameData, Seq: 1, StreamID: streamID, Payload: []byte{'b'}, normalReceive: true})
	if !stream.recvWriterBacklog.Load() {
		t.Fatal("frame 1 did not wait behind the full handoff")
	}
	later := muxObjectMeta{
		Name:     muxObjectNameWithStreamIDs(sid, DirectionDown, clientID, runID, "epoch-a", streamID, []uint64{streamID}, 0, 3, 1, 3, 3, muxMinBatch, false),
		ClientID: clientID, RunID: runID, StreamID: streamID, Seq: 3,
		PlainBytes: muxMinBatch, FrameMinSeq: 3, FrameMaxSeq: 3, FrameRangeKnown: true, Updated: now,
	}
	if !mux.enqueueMuxObject(t.Context(), later) {
		t.Fatal("later object not queued")
	}
	key := stream.key()
	mux.recvNormalMu.Lock()
	_, tracked := mux.recvGaps[key]
	mux.recvNormalMu.Unlock()
	if tracked || mux.maintainReceiveGaps(t.Context()) {
		t.Fatal("receive loop tried to repair through local writer pressure")
	}
	if err := peer.SetReadDeadline(now.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	mux.startWriter(stream)
	data := make([]byte, cap(stream.inbound)+muxStreamDeferredFrames+1)
	if _, err := io.ReadFull(peer, data); err != nil || data[len(data)-1] != 'b' {
		t.Fatalf("writer failed to drain through frame 1, error %v", err)
	}
	select {
	case <-mux.recvWake:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not wake receive-gap maintenance after draining")
	}
	if !mux.maintainReceiveGaps(t.Context()) || len(store.calls) != 1 {
		t.Fatal("receive maintenance did not request the missing frame")
	}
	select {
	case ready := <-mux.recvNormalReady:
		meta, ok := mux.takeNormalMuxObject(t.Context(), ready)
		if !ok || meta.Name != missing || meta.FrameMinSeq != 2 {
			t.Fatalf("ready object = %+v, ok=%t, want missing frame 2", meta, ok)
		}
	default:
		t.Fatal("repaired object did not wake the normal receive worker")
	}
}
