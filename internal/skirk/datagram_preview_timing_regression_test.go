package skirk

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func timingRawExit(t *testing.T) (net.Conn, *pilotTimingControl, *pilotTimingState, *atomic.Uint32) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	client, server := net.Pipe()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	state := newPilotTimingState()
	base := time.Now().Add(-time.Second)
	if err := state.install(1, base, base); err != nil {
		t.Fatal(err)
	}
	if err := state.activate(1); err != nil {
		t.Fatal(err)
	}
	failed := &atomic.Uint32{}
	control, err := newPilotTimingControl(ctx, server, &Tunnel{Secret: secret}, [16]byte{8}, state, false, func(uint32) error { return nil }, nil, func() { failed.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = client.Close(); _ = control.wait() })
	return client, control, state, failed
}

func timingRawWrite(t *testing.T, conn net.Conn, control *pilotTimingControl, kind byte, epoch uint64) {
	t.Helper()
	wire, err := pilotControlEncode(control.key, pilotControlFrame{kind: kind, epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAll(conn, wire); err != nil {
		t.Fatal(err)
	}
}

func timingRawRead(t *testing.T, conn net.Conn, control *pilotTimingControl, want byte) {
	t.Helper()
	frame, err := pilotControlRead(conn, control.key)
	if err != nil || frame.kind != want {
		t.Fatalf("control frame=%+v err=%v want=%d", frame, err, want)
	}
}

func TestPilotTimingControlDelayedACKKeepsPreparedReceiveAndOldSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, control, state, failed := timingRawExit(t)
		timingRawWrite(t, conn, control, pilotControlPrepare, 2)
		timingRawRead(t, conn, control, pilotControlReply)
		if !state.accept(2, 0, 0, time.Now()) {
			t.Fatal("exit rejected new upstream before ACK")
		}
		time.Sleep(13 * time.Second)
		id, elapsed, _, ok := state.stamp(time.Now())
		if !ok || id != 1 {
			t.Fatal("delayed ACK stopped old downlink")
		}
		if !state.accept(2, 13*time.Second, 13*time.Second, time.Now()) {
			t.Fatal("prepared receive map expired at request timeout")
		}
		timingRawWrite(t, conn, control, pilotControlAck, 2)
		synctest.Wait()
		if timingControlActive(state) != 2 || !state.valid(1, time.Now()) {
			t.Fatal("ACK failed or evicted fresh queued old downlink")
		}
		if elapsed <= 0 || failed.Load() != 0 {
			t.Fatal("renewal closed the existing control/socket owner")
		}
	})
}

func TestPilotTimingControlRefreshIsRateLimitedWhileACKIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, control, state, failed := timingRawExit(t)
		state.invalidate()
		timingRawRead(t, conn, control, pilotControlRefresh)
		timingRawWrite(t, conn, control, pilotControlPrepare, 2)
		timingRawRead(t, conn, control, pilotControlReply)
		for range 1000 {
			state.stamp(time.Now())
		}
		synctest.Wait()
		_ = conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		if _, err := pilotControlRead(conn, control.key); err == nil {
			t.Fatal("origin traffic flooded control REFRESH while ACK was pending")
		}
		_ = conn.SetReadDeadline(time.Time{})
		time.Sleep(udpPreviewHandshake)
		timingRawRead(t, conn, control, pilotControlRefresh)
		timingRawWrite(t, conn, control, pilotControlAck, 2)
		synctest.Wait()
		if timingControlActive(state) != 2 || failed.Load() != 0 {
			t.Fatal("rate limiting prevented successful ACK recovery")
		}
	})
}

func TestPilotTimingFilterPrecedesMetadataMediaClassification(t *testing.T) {
	for _, fair := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue", true: "fair queue"}[fair], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotUploadTestProvider{}
				mailbox := newAdaptiveTestMailbox(t, provider)
				mailbox.mediaBytes = pilotMediaWidePlain
				mailbox.timing = newPilotTimingState()
				origin := time.Now().Add(-time.Second)
				if err := mailbox.timing.install(2, origin, origin); err != nil {
					t.Fatal(err)
				}
				if err := mailbox.timing.activate(2); err != nil {
					t.Fatal(err)
				}
				if fair {
					mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
					mailbox.queue = nil
				}
				enqueue := func(packet pilotPacket) {
					if fair {
						mailbox.fairQueue.offer(packet)
					} else {
						mailbox.queue <- packet
					}
				}
				target := netip.MustParseAddrPort("8.8.8.8:3478")
				enqueue(pilotPacket{flow: 1<<32 | 1, sent: time.Now(), target: target, body: make([]byte, 400-pilotPacketHeader), timingEpoch: 1, elapsed: time.Second})
				for range 65 {
					enqueue(pilotPacket{flow: 1<<32 | 1, sent: time.Now(), target: target, body: make([]byte, 246-pilotPacketHeader), timingEpoch: 2, elapsed: time.Second})
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go mailbox.publishLoop(ctx)
				time.Sleep(2*pilotPublishTick + time.Millisecond)
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				if len(attempts) != 2 || attempts[0].pool != 0 || attempts[1].pool != 0 || attempts[0].plain != 4+64*246 || attempts[1].plain != 4+246 {
					t.Fatalf("invalid epoch changed final upload class/count: %+v", attempts)
				}
				if mailbox.published.Load() != 65 || mailbox.stale.Load() != 1 || mailbox.mediaUploaded.Load() != 0 {
					t.Fatal("classification/filter counters did not match actual uploads")
				}
			})
		})
	}
}

func TestPilotTimingLegacyRequestRejectedBeforeMailbox(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(udpPreviewRequest) != len(exitProbeRequestMagic) {
		t.Fatal("versioned request changed fixed exit control frame length")
	}
	var called atomic.Bool
	conn, err := newExitControlConnection(secret, func(net.Conn, [exitProbeNonceBytes]byte) { called.Store(true) })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeAll(conn, append([]byte("skirk-udp-lease-req:"), make([]byte, exitProbeNonceBytes)...)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := conn.Read(one[:]); err != io.EOF {
		t.Fatalf("legacy request not refused: %v", err)
	}
	if called.Load() {
		t.Fatal("legacy request allocated a v2 mailbox")
	}
}
