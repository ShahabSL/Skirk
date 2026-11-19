package skirk

import (
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewHEVRechecksQueuedCarrierReplies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        time.Duration
		wait       time.Duration
		clockReset bool
		local      uint64
		stale      uint64
		invalid    uint64
	}{
		{name: "certainly stale after admission", age: pilotLifetime - 50*time.Millisecond, wait: 100 * time.Millisecond, stale: 1},
		{name: "local retention limit", age: time.Millisecond, wait: pilotLifetime, local: 1},
		{name: "invalidated epoch", age: time.Millisecond, clockReset: true, invalid: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				secret, err := RandomSecret()
				if err != nil {
					t.Fatal(err)
				}
				tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{7}}
				target := netip.MustParseAddrPort("127.0.0.1:3478")
				mailbox, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{8}, true, map[netip.AddrPort]bool{target: true})
				if err != nil {
					t.Fatal(err)
				}
				origin := time.Now().Add(-10 * time.Second)
				var elapsed atomic.Int64
				elapsed.Store(int64(10 * time.Second))
				timing := newPilotTimingState()
				timing.now = func() time.Time { return origin.Add(time.Duration(elapsed.Load())) }
				mailbox.timing = timing
				install := func(id uint64, epochOrigin time.Time) {
					t.Helper()
					if err := timing.install(id, epochOrigin, epochOrigin); err != nil {
						t.Fatal(err)
					}
					if err := timing.activate(id); err != nil {
						t.Fatal(err)
					}
				}
				install(1, origin)
				flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow)}
				mailbox.deliver = flows.returnToClient
				server, app := net.Pipe()
				defer app.Close()
				defer server.Close()
				_ = app.SetDeadline(time.Now().Add(time.Second))
				relay := &pilotClientRelay{
					allowed: mailbox.allowed, mailbox: mailbox, flows: flows, publicIPv4: true, dns: true,
				}
				go pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{server})
				var reply [10]byte
				if _, err := io.ReadFull(app, reply[:]); err != nil || reply[1] != 0 {
					t.Fatalf("HEV frontend reply %x: %v", reply, err)
				}
				if err := writeAll(app, buildUDPInTCPFrame(pilotHEVLoopbackAlias, int(target.Port()), []byte("open flow"))); err != nil {
					t.Fatal(err)
				}
				flow := (<-mailbox.queue).flow
				seq := uint64(0)
				inject := func(epoch uint64, stamp time.Duration, body string) {
					t.Helper()
					seq++
					packet := pilotPacket{
						flow: flow, target: target, body: []byte(body),
						timingEpoch: epoch, elapsed: stamp, receiveElapsed: stamp,
					}
					wire, err := Seal(mailbox.recvKey, tunnel.SessionID, mailbox.recvDir, seq, pilotMarshal([]pilotPacket{packet}), false)
					if err != nil {
						t.Fatal(err)
					}
					mailbox.acceptBatch(ctx, "owned-reply-fixture", seq, wire, pilotMaxPlain, pilotMaxBatch)
				}
				inject(1, time.Duration(elapsed.Load()), "already writing")
				// The frontend writer is blocked in net.Pipe.Write. Advance the
				// timing clock independently of the socket's write deadline.
				synctest.Wait()
				inject(1, time.Duration(elapsed.Load())-tc.age, "retained reply")
				if got := mailbox.delivered.Load(); got != 2 {
					t.Fatalf("test requires both records to pass initial admission, got %d", got)
				}
				elapsed.Add(int64(tc.wait))
				if tc.clockReset {
					timing.invalidate()
					install(2, timing.nowTime())
					inject(2, 0, "fresh reply")
				} else {
					inject(1, time.Duration(elapsed.Load()), "fresh reply")
				}
				for _, want := range []string{"already writing", "fresh reply"} {
					frame, err := readUDPInTCPFrame(app)
					if err != nil || string(frame.Payload) != want ||
						frame.Host != pilotHEVLoopbackAlias || frame.Port != int(target.Port()) {
						t.Fatalf("HEV reply %+v: %v, want %q at the loopback alias", frame, err, want)
					}
				}
				if mailbox.stale.Load() != 1 || mailbox.localQueueExpired.Load() != tc.local ||
					mailbox.incomingCertainlyStale.Load() != tc.stale || mailbox.invalidTiming.Load() != tc.invalid ||
					mailbox.hevReplyDropped.Load() != 0 {
					t.Fatalf("wrong reply-queue accounting: expired=%d local=%d stale=%d invalid=%d overflow=%d",
						mailbox.stale.Load(), mailbox.localQueueExpired.Load(), mailbox.incomingCertainlyStale.Load(),
						mailbox.invalidTiming.Load(), mailbox.hevReplyDropped.Load())
				}
			})
		})
	}
}
