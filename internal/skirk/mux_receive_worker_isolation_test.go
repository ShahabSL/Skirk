package skirk

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// A stalled local socket can fill a stream's inbound channel. The shared
// priority workers must still deliver an unrelated stream's authenticated
// packet without waiting for that socket to become readable.
func TestMuxPriorityReceiveWorkersDoNotBlockBehindStalledLocalReaders(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{
		Data: NewMemoryStore(), Secret: secret, SessionID: [16]byte{1},
		ClientID: "client", RunID: "run", Transport: "muxv5",
		MetadataOnlyMaxBytes: 8 << 10,
	}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() { defer workers.Done(); mux.runReceiveWorker(ctx, true) }()
	}
	defer func() { cancel(); workers.Wait() }()

	streams := make([]*muxStream, 3)
	for i := range streams {
		streams[i] = mux.registerStream(uint64(i+1), "client", "run", nil)
		defer streams[i].close()
	}
	for _, stream := range streams[:2] {
		for range cap(stream.inbound) {
			stream.inbound <- []byte("stalled")
		}
	}

	makeObject := func(streamID, objectSeq uint64) muxObjectMeta {
		t.Helper()
		raw, err := encodeMuxBatch([]muxFrame{{
			Kind: muxFrameData, StreamID: streamID, Seq: 1,
			Payload: []byte(fmt.Sprintf("live-%d", streamID)),
		}})
		if err != nil {
			t.Fatal(err)
		}
		key, err := DeriveMuxLaneKeyV5(secret, tunnel.SessionID, DirectionDown, "client", "run", 0, mux.epoch)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := Seal(key, tunnel.SessionID, DirectionDown, objectSeq, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		return muxObjectMeta{
			Name: fmt.Sprintf("object-%d", objectSeq), ID: fmt.Sprintf("id-%d", objectSeq),
			ClientID: "client", RunID: "run", Epoch: mux.epoch, Transport: "muxv5",
			StreamID: streamID, Lane: 0, Seq: objectSeq, Priority: true,
			MetadataOnly: true, InlineData: sealed,
		}
	}
	mux.recvUrgent <- makeObject(1, 1)
	mux.recvUrgent <- makeObject(2, 2)
	deadline := time.Now().Add(2 * time.Second)
	for streams[0].expectedRecvSeq() != 2 || streams[1].expectedRecvSeq() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("stalled streams were not selected by both workers")
		}
		time.Sleep(time.Millisecond)
	}
	mux.recvUrgent <- makeObject(3, 3)
	select {
	case payload := <-streams[2].inbound:
		if string(payload) != "live-3" {
			t.Fatalf("unrelated stream payload %q", payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unrelated priority stream was blocked by two stalled local readers")
	}
}

func TestMuxDeferredInboundPreservesOrderAndPausesNormalScheduling(t *testing.T) {
	mux := &driveMux{t: &Tunnel{}, streams: map[muxStreamKey]*muxStream{}, pending: map[muxStreamKey][]muxFrame{}}
	stream := mux.registerStream(7, "client", "run", nil)
	defer stream.close()
	for i := range cap(stream.inbound) {
		stream.inbound <- []byte{byte(i)}
	}
	for i := uint64(1); i <= 5; i++ {
		stream.acceptFrame(context.Background(), muxFrame{
			Kind: muxFrameData, StreamID: stream.id, Seq: i,
			Payload: []byte{byte(100 + i)},
		})
	}
	if got := stream.deferredInboundCount.Load(); got != 5 {
		t.Fatalf("deferred frames = %d, want 5", got)
	}
	for i := range cap(stream.inbound) {
		data, ok := stream.nextInbound()
		if !ok || len(data) != 1 || data[0] != byte(i) {
			t.Fatalf("channel item %d = %v, ok=%t", i, data, ok)
		}
	}
	if _, paused := normalReceivePauseStateForStream(stream); !paused {
		t.Fatal("normal receive resumed while deferred frames remained")
	}
	for i := 1; i <= 5; i++ {
		data, ok := stream.nextInbound()
		if !ok || len(data) != 1 || data[0] != byte(100+i) {
			t.Fatalf("deferred item %d = %v, ok=%t", i, data, ok)
		}
	}
	if got := stream.deferredInboundCount.Load(); got != 0 {
		t.Fatalf("deferred frames after drain = %d", got)
	}
	mux.deferredBudgetMu.Lock()
	frames, bytes := mux.deferredBudgetFrames, mux.deferredBudgetBytes
	mux.deferredBudgetMu.Unlock()
	if frames != 0 || bytes != 0 {
		t.Fatalf("shared budget after drain = %d frames/%d bytes", frames, bytes)
	}
	if _, paused := normalReceivePauseStateForStream(stream); paused {
		t.Fatal("normal receive stayed paused after handoff drained")
	}
}

func TestMuxDeferredInboundOverflowClosesOnlyStalledStream(t *testing.T) {
	mux := &driveMux{t: &Tunnel{}, streams: map[muxStreamKey]*muxStream{}, pending: map[muxStreamKey][]muxFrame{}}
	stalled := mux.registerStream(7, "client", "run", nil)
	live := mux.registerStream(8, "client", "run", nil)
	defer stalled.close()
	defer live.close()
	for range cap(stalled.inbound) {
		stalled.inbound <- []byte("filled")
	}
	payload := make([]byte, 2<<20)
	for seq := uint64(1); seq <= 5; seq++ {
		stalled.acceptFrame(context.Background(), muxFrame{
			Kind: muxFrameData, StreamID: stalled.id, Seq: seq, Payload: payload,
		})
	}
	select {
	case <-stalled.done:
	default:
		t.Fatal("stalled stream remained open past the 8 MiB deferred hard cap")
	}
	select {
	case <-live.done:
		t.Fatal("overflow closed an unrelated stream")
	default:
	}
	live.acceptFrame(context.Background(), muxFrame{
		Kind: muxFrameData, StreamID: live.id, Seq: 1, Payload: []byte("still-live"),
	})
	select {
	case data := <-live.inbound:
		if string(data) != "still-live" {
			t.Fatalf("live stream payload = %q", data)
		}
	default:
		t.Fatal("unrelated stream stopped delivering after overflow")
	}
}

func TestMuxDeferredInboundGlobalByteBudgetAndReclaim(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), Secret: "deferred-budget", SessionID: [16]byte{1}, ClientID: "client", RunID: "run"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.closeAll()
	payload := make([]byte, 1<<20)
	registerStalled := func(id uint64) *muxStream {
		stream := mux.registerStream(id, "client", "run", nil)
		for range cap(stream.inbound) {
			stream.inbound <- []byte("filled")
		}
		return stream
	}
	streams := make([]*muxStream, 8)
	for i := range streams {
		stream := registerStalled(uint64(i + 1))
		streams[i] = stream
		for seq := uint64(1); seq <= 8; seq++ {
			stream.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: stream.id, Seq: seq, Payload: payload})
		}
		select {
		case <-stream.done:
			t.Fatalf("stream %d closed before the 64 MiB shared budget filled", i)
		default:
		}
	}
	mux.deferredBudgetMu.Lock()
	bytes, frames := mux.deferredBudgetBytes, mux.deferredBudgetFrames
	mux.deferredBudgetMu.Unlock()
	if bytes != muxGlobalDeferredBytes || frames != 64 {
		t.Fatalf("shared deferred usage = %d bytes/%d frames", bytes, frames)
	}

	overflow := registerStalled(9)
	overflow.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: overflow.id, Seq: 1, Payload: payload})
	select {
	case <-overflow.done:
	default:
		t.Fatal("ninth stalled stream exceeded the shared deferred byte budget")
	}
	live := mux.registerStream(11, "client", "run", nil)
	live.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: live.id, Seq: 1, Payload: []byte("live")})
	select {
	case data := <-live.inbound:
		if string(data) != "live" {
			t.Fatalf("unrelated live stream payload = %q", data)
		}
	default:
		t.Fatal("global deferred budget blocked unrelated local delivery")
	}
	streams[0].close()
	replacement := registerStalled(10)
	for seq := uint64(1); seq <= 8; seq++ {
		replacement.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: replacement.id, Seq: seq, Payload: payload})
	}
	select {
	case <-replacement.done:
		t.Fatal("closing one stalled stream did not return its shared budget")
	default:
	}
	mux.closeAll()
	mux.deferredBudgetMu.Lock()
	bytes, frames, streamCount := mux.deferredBudgetBytes, mux.deferredBudgetFrames, len(mux.deferredByStream)
	mux.deferredBudgetMu.Unlock()
	if bytes != 0 || frames != 0 || streamCount != 0 {
		t.Fatalf("shared deferred budget leaked after close: %d bytes/%d frames/%d streams", bytes, frames, streamCount)
	}
}

func TestMuxDeferredInboundGlobalFrameBudget(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), Secret: "deferred-frames", SessionID: [16]byte{2}, ClientID: "client", RunID: "run"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.closeAll()
	for id := uint64(1); id <= 16; id++ {
		stream := mux.registerStream(id, "client", "run", nil)
		for range cap(stream.inbound) {
			stream.inbound <- []byte("filled")
		}
		for seq := uint64(1); seq <= muxStreamDeferredFrames; seq++ {
			stream.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: stream.id, Seq: seq, Payload: []byte{1}})
		}
		select {
		case <-stream.done:
			t.Fatalf("stream %d closed before shared frame budget filled", id)
		default:
		}
	}
	overflow := mux.registerStream(17, "client", "run", nil)
	for range cap(overflow.inbound) {
		overflow.inbound <- []byte("filled")
	}
	overflow.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: overflow.id, Seq: 1, Payload: []byte{1}})
	select {
	case <-overflow.done:
	default:
		t.Fatal("seventeenth stalled stream exceeded the shared deferred frame budget")
	}
}

func TestMuxDeferredInboundBudgetConcurrentCloseAndDelivery(t *testing.T) {
	mux := &driveMux{t: &Tunnel{}, streams: map[muxStreamKey]*muxStream{}, pending: map[muxStreamKey][]muxFrame{}}
	defer mux.closeAll()
	streams := make([]*muxStream, 32)
	var workers sync.WaitGroup
	for i := range streams {
		stream := mux.registerStream(uint64(i+1), "client", "run", nil)
		streams[i] = stream
		for range cap(stream.inbound) {
			stream.inbound <- []byte("filled")
		}
		progress := make(chan struct{})
		workers.Add(1)
		go func() {
			defer workers.Done()
			for seq := uint64(1); seq <= muxStreamDeferredFrames; seq++ {
				stream.acceptFrame(context.Background(), muxFrame{Kind: muxFrameData, StreamID: stream.id, Seq: seq, Payload: []byte{1}})
				if seq == 16 {
					close(progress)
				}
			}
		}()
		if i%2 == 0 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-progress
				stream.close()
			}()
		}
	}
	workers.Wait()
	frames, bytes := 0, 0
	for _, stream := range streams {
		stream.deliverMu.Lock()
		frames += len(stream.deferredInbound)
		bytes += stream.deferredInboundBytes
		stream.deliverMu.Unlock()
	}
	mux.deferredBudgetMu.Lock()
	budgetFrames, budgetBytes := mux.deferredBudgetFrames, mux.deferredBudgetBytes
	mux.deferredBudgetMu.Unlock()
	if frames != budgetFrames || bytes != budgetBytes || frames > muxGlobalDeferredFrames || bytes > muxGlobalDeferredBytes {
		t.Fatalf("shared budget %d frames/%d bytes, stream totals %d frames/%d bytes", budgetFrames, budgetBytes, frames, bytes)
	}
	mux.closeAll()
	mux.deferredBudgetMu.Lock()
	budgetFrames, budgetBytes, streamCount := mux.deferredBudgetFrames, mux.deferredBudgetBytes, len(mux.deferredByStream)
	mux.deferredBudgetMu.Unlock()
	if budgetFrames != 0 || budgetBytes != 0 || streamCount != 0 {
		t.Fatalf("concurrent close leaked budget: %d frames/%d bytes/%d streams", budgetFrames, budgetBytes, streamCount)
	}
}

func TestMuxDeferredInboundDrainsToLocalSocketInFrameOrder(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	mux := &driveMux{t: &Tunnel{}, streams: map[muxStreamKey]*muxStream{}, pending: map[muxStreamKey][]muxFrame{}}
	stream := mux.registerStream(7, "client", "run", left)
	defer stream.close()
	want := make([]byte, 0, muxStreamInbound+300)
	for i := range cap(stream.inbound) {
		value := byte(i)
		stream.inbound <- []byte{value}
		want = append(want, value)
	}
	mux.startWriter(stream)
	for seq := uint64(1); seq <= 300; seq++ {
		value := byte(int(seq) + muxStreamInbound - 1)
		stream.acceptFrame(context.Background(), muxFrame{
			Kind: muxFrameData, StreamID: stream.id, Seq: seq, Payload: []byte{value},
		})
		want = append(want, value)
	}
	if got := stream.deferredInboundCount.Load(); got == 0 {
		t.Fatal("producer did not exercise deferred handoff")
	}
	if err := right.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("local socket byte %d = %d, want %d", i, got[i], want[i])
		}
	}
}
