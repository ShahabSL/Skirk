package skirk

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestPilotIntervalExactFiveSecondPacketBothDirections(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{5}}
	target := netip.MustParseAddrPort("127.0.0.1:3478")
	allowed := map[netip.AddrPort]bool{target: true}
	client, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{6}, true, allowed)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{6}, false, allowed)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1700000000, 0)
	clientBase, exitBase := base.Add(24*time.Hour), base.Add(-24*time.Hour)
	cn, en := clientBase.Add(3*time.Second), exitBase.Add(3*time.Second)
	client.timing = timingTestState(t, true, &cn, clientBase.Add(3*time.Second), clientBase)
	exit.timing = timingTestState(t, false, &en, exitBase.Add(1500*time.Millisecond), exitBase.Add(1500*time.Millisecond))
	var up, down int
	exit.deliver = func(context.Context, pilotPacket) { up++ }
	client.deliver = func(context.Context, pilotPacket) { down++ }
	cn, en = clientBase.Add(4*time.Second), exitBase.Add(4*time.Second)
	client.offer(1, target, []byte("up exact five seconds"))
	exit.offer(1, target, []byte("down exact five seconds"))
	upPacket, downPacket := <-client.queue, <-exit.queue
	cn, en = clientBase.Add(9*time.Second), exitBase.Add(9*time.Second)
	for _, direction := range []struct {
		sender, receiver *pilotMailbox
		packet           pilotPacket
	}{
		{client, exit, upPacket}, {exit, client, downPacket},
	} {
		wire, err := Seal(direction.sender.sendKey, tunnel.SessionID, direction.sender.sendDir, 1, pilotMarshal([]pilotPacket{direction.packet}), false)
		if err != nil {
			t.Fatal(err)
		}
		direction.receiver.acceptBatch(t.Context(), "owned-five-second", 1, wire, pilotMaxPlain, pilotMaxBatch)
	}
	if up != 1 || down != 1 || client.stale.Load() != 0 || exit.stale.Load() != 0 {
		t.Fatalf("causal gap expired fresh packet: up=%d down=%d", up, down)
	}
}

func TestPilotIntervalExpiryDiagnosticsSeparateCauses(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{7}}
	target := netip.MustParseAddrPort("127.0.0.1:3478")
	mailbox, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{8}, false, map[netip.AddrPort]bool{target: true})
	if err != nil {
		t.Fatal(err)
	}
	mailbox.fairQueue = newPilotFairQueue(1, pilotFairQuantum)
	mailbox.fairQueue.offer(pilotPacket{flow: 1<<32 | 1, target: target, sent: time.Now().Add(-7 * time.Second), body: []byte("old queue")})
	mailbox.offer(1<<32|1, target, []byte("replacement"))
	now := time.Now()
	base := now.Add(-10 * time.Second)
	mailbox.timing = timingTestState(t, false, &now, base, base)
	mailbox.deliver = func(context.Context, pilotPacket) { t.Fatal("certainly stale delivered") }
	packet := pilotPacket{flow: 1, target: target, timingEpoch: 1, elapsed: time.Second, receiveElapsed: time.Second, body: []byte("old receive")}
	wire, err := Seal(mailbox.recvKey, tunnel.SessionID, mailbox.recvDir, 1, pilotMarshal([]pilotPacket{packet}), false)
	if err != nil {
		t.Fatal(err)
	}
	mailbox.acceptBatch(t.Context(), "owned-expired", 1, wire, pilotMaxPlain, pilotMaxBatch)
	mailbox.timing.invalidate()
	mailbox.offer(1<<32|1, target, []byte("invalid epoch"))
	var totals UDPPreviewPacketCounters
	totals.addMailbox(mailbox)
	if totals.Expired != 3 || totals.LocalQueueExpired != 1 || totals.IncomingCertainlyStale != 1 || totals.InvalidTiming != 1 {
		t.Fatalf("expiry causes lost in process metrics: %+v", totals)
	}
}

func TestPilotIntervalWireAuthenticatesBothElapsedStamps(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{3}}
	target := netip.MustParseAddrPort("127.0.0.1:3478")
	mailbox, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{4}, false, map[netip.AddrPort]bool{target: true})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	now := base.Add(11 * time.Second)
	mailbox.timing = timingTestState(t, false, &now, base, base)
	packet := pilotPacket{flow: 1, target: target, body: []byte("owned interval"), timingEpoch: 1, elapsed: 10 * time.Second, receiveElapsed: 10 * time.Second}
	plain := pilotMarshal([]pilotPacket{packet})
	decoded, ok := pilotUnmarshal(plain, pilotMaxPlain, pilotMaxBatch, false)
	if !ok || len(plain) != len(pilotDatagramMagic)+52+len(packet.body) || decoded[0].elapsed != packet.elapsed || decoded[0].receiveElapsed != packet.receiveElapsed {
		t.Fatal("interval wire lost either immutable timestamp")
	}
	var delivered int
	mailbox.deliver = func(context.Context, pilotPacket) { delivered++ }
	seal := func(seq uint64, body []byte) []byte {
		wire, err := Seal(mailbox.recvKey, tunnel.SessionID, mailbox.recvDir, seq, body, false)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	wire := seal(1, plain)
	for range 2 {
		mailbox.acceptBatch(t.Context(), "owned", 1, wire, pilotMaxPlain, pilotMaxBatch)
	}
	if delivered != 1 {
		t.Fatal("valid interval or duplicate replay handling failed")
	}
	forged := seal(2, plain)
	forged[headerLen+len(pilotDatagramMagic)+24] ^= 1
	mailbox.acceptBatch(t.Context(), "forged", 2, forged, pilotMaxPlain, pilotMaxBatch)
	if delivered != 1 || mailbox.rejected.Load() != 1 {
		t.Fatal("second timestamp was not authenticated")
	}
	// A correctly authenticated but impossible pair is a timing rejection.
	bad := append([]byte(nil), plain...)
	binary.BigEndian.PutUint64(bad[len(pilotDatagramMagic)+24:], 0)
	mailbox.acceptBatch(t.Context(), "inconsistent", 3, seal(3, bad), pilotMaxPlain, pilotMaxBatch)
	if delivered != 1 || mailbox.invalidTiming.Load() != 1 || mailbox.incomingCertainlyStale.Load() != 0 {
		t.Fatal("inconsistent bounds were treated as age expiry")
	}
	copy(bad[:4], "SDP2")
	if _, ok := pilotUnmarshal(bad, pilotMaxPlain, pilotMaxBatch, false); ok {
		t.Fatal("v2 packet silently accepted")
	}
}

func TestPilotIntervalV2RequestDoesNotStartLease(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	var started atomic.Bool
	conn, err := newExitControlConnection(secret, func(net.Conn, [exitProbeNonceBytes]byte) { started.Store(true) })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	request := append([]byte("skirk-udp-lease-rq2:"), make([]byte, exitProbeNonceBytes)...)
	if err := writeAll(conn, request); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := conn.Read(one[:]); err != io.EOF || started.Load() {
		t.Fatalf("mixed peer allocated lease: %v", err)
	}
}
