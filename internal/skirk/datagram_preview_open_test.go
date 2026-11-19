package skirk

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestMuxExplicitUDPChallengeRejectsOversizedInitialData(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{
		Data: NewMemoryStore(), ClientID: "inline-client", RunID: "inline-run",
	}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	err = mux.openClientStreamWithInitial(ctx, exitProbeTarget, local, make([]byte, muxInlineFirst+1))
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversized prepared OPEN was not refused immediately: %v", err)
	}
	if mux.active.Load() != 0 || len(mux.streams) != 0 || len(mux.closed) != 0 {
		t.Fatal("oversized prepared OPEN registered a stream")
	}
	for _, lane := range mux.lanes {
		if len(lane.urgent) != 0 {
			t.Fatal("oversized prepared OPEN enqueued a frame")
		}
	}
}

func TestMuxExplicitUDPChallengeSkipsDelayedLocalWriter(t *testing.T) {
	nonce := [exitProbeNonceBytes]byte{0x19, 0xa8, 0x4f}
	challenge := append([]byte(udpPreviewRequest), nonce[:]...)
	for _, tc := range []struct {
		name      string
		initial   []byte
		afterOpen []byte
		localWait time.Duration
	}{
		{"prepared UDP challenge", challenge, []byte("later control"), 0},
		{"ordinary opportunistic OPEN", nil, challenge, initialOpenDataWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, stop := context.WithCancel(context.Background())
				defer stop()
				mux, err := newDriveMux(&Tunnel{
					Data: NewMemoryStore(), ClientID: "inline-client", RunID: "inline-run",
				}, "client", DirectionUp, DirectionDown)
				if err != nil {
					t.Fatal(err)
				}
				// Observe the normal mux frames directly while controlling the
				// local writer and the timing of its upload acknowledgment.
				mux.lanes = mux.lanes[:1]
				lane := mux.lanes[0]
				local, peer := net.Pipe()
				defer local.Close()
				defer peer.Close()
				written := make(chan error, 1)
				go func() {
					time.Sleep(4 * initialOpenDataWait)
					written <- writeAll(peer, tc.afterOpen)
				}()
				started := time.Now()
				done := make(chan error, 1)
				go func() {
					if tc.initial == nil {
						done <- mux.openClientStream(ctx, exitProbeTarget, local)
						return
					}
					done <- mux.openClientStreamWithInitial(ctx, exitProbeTarget, local, tc.initial)
				}()
				var open muxFrame
				select {
				case open = <-lane.urgent:
				case <-time.After(time.Second):
					t.Fatal("mux did not queue an OPEN")
				}
				if open.Kind != muxFrameOpen || open.Ack == nil {
					t.Fatalf("first frame kind=%d, want acknowledged OPEN", open.Kind)
				}
				target, initial, err := decodeMuxOpenPayload(open.Payload)
				if err != nil || target != exitProbeTarget || !bytes.Equal(initial, tc.initial) {
					t.Fatalf("OPEN target=%q initial=%x err=%v, want initial=%x", target, initial, err, tc.initial)
				}
				if got := time.Since(started); got != tc.localWait {
					t.Fatalf("OPEN local delay=%v, want %v", got, tc.localWait)
				}
				open.Ack.complete(nil)
				select {
				case later := <-lane.urgent:
					if later.Kind != muxFrameData || later.key() != open.key() || !bytes.Equal(later.Payload, tc.afterOpen) {
						t.Fatalf("post-OPEN frame kind=%d payload=%x, want DATA %x", later.Kind, later.Payload, tc.afterOpen)
					}
				case <-time.After(time.Second):
					t.Fatal("mux did not deliver the delayed local write")
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if len(lane.urgent) != 0 {
					t.Fatal("unexpected duplicate or split mux frame")
				}
				stop()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}
