package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMuxExitAdmissionRetainsResourcesUntilCanceledDialExits(t *testing.T) {
	tunnel := &Tunnel{Data: NewMemoryStore(), ExitMaxStreams: 2}
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	healthy, application := net.Pipe()
	replacement, replacementPeer := net.Pipe()
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{}, 1)
	openingDone := make(chan struct{})
	mux.dialExit = func(_ context.Context, target string) (net.Conn, error) {
		switch target {
		case "healthy.test:80":
			return healthy, nil
		case "replacement.test:80":
			return replacement, nil
		case "waiting.test:80":
			close(dialStarted)
			select {
			case <-releaseDial:
			case <-ctx.Done():
			}
			return nil, errors.New("test dial completed without a connection")
		default:
			return nil, errors.New("over-capacity request unexpectedly reached dial")
		}
	}
	t.Cleanup(func() {
		cancel()
		mux.closeAll()
		_ = application.Close()
		_ = replacementPeer.Close()
	})
	open := func(client, target string) muxFrame {
		return muxFrame{Kind: muxFrameOpen, ClientID: client, RunID: "run", StreamID: 1, Payload: encodeMuxOpenPayload(target, nil)}
	}
	healthyOpen := open("healthy", "healthy.test:80")
	mux.openExitStream(ctx, healthyOpen)
	if tunnel.activeStreams.Load() != 1 {
		t.Fatal("expected the first stream to be active")
	}
	waitingOpen := open("waiting", "waiting.test:80")
	go func() {
		defer close(openingDone)
		mux.openExitStream(ctx, waitingOpen)
	}()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("pending dial did not start")
	}
	assertCapacityRST := func(frame muxFrame) {
		t.Helper()
		lane := mux.lanes[mux.frameLane(frame)]
		select {
		case rst := <-lane.urgent:
			if rst.Kind != muxFrameRST || rst.key() != frame.key() || string(rst.Payload) != "exit_stream_capacity" {
				t.Fatalf("unexpected capacity response: %+v", rst)
			}
		default:
			t.Fatal("expected a capacity reset on the idle control queue")
		}
	}
	overflow := open("overflow", "overflow.test:80")
	mux.openExitStream(ctx, overflow)
	assertCapacityRST(overflow)
	if !mux.isClosedStream(overflow.key()) {
		t.Fatal("refused request did not become closed")
	}
	mux.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: waitingOpen.ClientID, RunID: waitingOpen.RunID, StreamID: waitingOpen.StreamID})
	if !mux.isOpeningStream(waitingOpen.key()) || !mux.isClosedStream(waitingOpen.key()) {
		t.Fatal("canceled live worker lost ownership before it returned")
	}
	beforeDone := open("overflow-later", "overflow.test:80")
	mux.openExitStream(ctx, beforeDone)
	assertCapacityRST(beforeDone)

	// The healthy client still receives data while admission is saturated.
	payload := []byte("uninterrupted")
	mux.handleFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: healthyOpen.ClientID, RunID: healthyOpen.RunID, StreamID: healthyOpen.StreamID, Seq: 1, Payload: payload})
	_ = application.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(application, got); err != nil || string(got) != string(payload) {
		t.Fatalf("healthy stream read %q, %v", got, err)
	}

	releaseDial <- struct{}{}
	select {
	case <-openingDone:
	case <-time.After(time.Second):
		t.Fatal("canceled dial did not exit")
	}
	if mux.isOpeningStream(waitingOpen.key()) {
		t.Fatal("canceled dial retained capacity after it exited")
	}
	next := open("replacement", "replacement.test:80")
	mux.openExitStream(ctx, next)
	if mux.isOpeningStream(next.key()) || mux.streamByKey(next.key()) == nil || tunnel.activeStreams.Load() != 2 {
		t.Fatal("the freed slot was not transferred to the replacement stream")
	}
}

func TestMuxExitAdmissionBoundsPendingDials(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ExitMaxStreams: defaultExitStreamLimit}}
	key := func(id uint64) muxStreamKey { return muxStreamKey{ClientID: "client", RunID: "run", StreamID: id} }
	for id := uint64(1); id <= muxExitOpenPendingLimit; id++ {
		if got := mux.claimExitOpen(key(id)); got != muxExitOpenAdmitted {
			t.Fatalf("pending request %d admission = %d", id, got)
		}
	}
	if got := mux.claimExitOpen(key(muxExitOpenPendingLimit + 1)); got != muxExitOpenAtCapacity {
		t.Fatalf("full pending admission = %d; want capacity rejection", got)
	}
	mux.releaseExitOpenClaim(key(1))
	if got := mux.claimExitOpen(key(muxExitOpenPendingLimit + 2)); got != muxExitOpenAdmitted {
		t.Fatalf("admission after pending release = %d", got)
	}
}

func TestMuxExitResetCancelsOpeningDial(t *testing.T) {
	for _, phase := range []string{"waiting for permit", "dialing upstream"} {
		t.Run(phase, func(t *testing.T) {
			dialing := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != "origin.test:443" {
					t.Errorf("unexpected upstream request %s %s", r.Method, r.Host)
					return
				}
				close(dialing)
				<-r.Context().Done()
			}))
			t.Cleanup(proxy.Close)
			m, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), ExitProxy: proxy.URL, ExitMaxStreams: 1}, "exit", DirectionDown, DirectionUp)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			m.exitOpenSlots = make(chan struct{}, 1)
			if phase == "waiting for permit" {
				m.exitOpenSlots <- struct{}{}
			}
			open := muxFrame{Kind: muxFrameOpen, ClientID: "client", RunID: "run", StreamID: 1, Payload: encodeMuxOpenPayload("origin.test:443", nil)}
			if !m.claimExitOpenFrame(ctx, open) {
				t.Fatal("first OPEN was refused")
			}
			finished := make(chan struct{})
			t.Cleanup(func() { cancel(); <-finished })
			go func() {
				defer close(finished)
				m.openClaimedExitStream(ctx, open)
			}()
			if phase == "dialing upstream" {
				select {
				case <-dialing:
				case <-time.After(time.Second):
					t.Fatal("upstream dial did not start")
				}
			}
			m.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: open.ClientID, RunID: open.RunID, StreamID: open.StreamID})
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("reset left the canceled OPEN occupying capacity")
			}
			if phase == "waiting for permit" {
				select {
				case <-dialing:
					t.Fatal("canceled waiter dialed the upstream")
				default:
				}
				<-m.exitOpenSlots
			}
			if len(m.exitOpenSlots) != 0 || !m.isClosedStream(open.key()) || m.isOpeningStream(open.key()) {
				t.Fatal("reset failed to retire the opening dial or return its permit")
			}
			open.StreamID++
			if !m.claimExitOpenFrame(ctx, open) {
				t.Fatal("next OPEN did not get the returned capacity")
			}
		})
	}
}
