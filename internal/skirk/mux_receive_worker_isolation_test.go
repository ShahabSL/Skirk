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
