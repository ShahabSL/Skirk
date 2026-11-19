package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestUDPPreviewProxySetupResetPreservesHealthySourcePort(t *testing.T) {
	relay, relayAddr := stableUDPTestPeer(t)
	fixture, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	proxyReady := make(chan error, 1)
	go func() {
		control, err := fixture.Accept()
		if err != nil {
			proxyReady <- err
			return
		}
		defer control.Close()
		err = testAcceptResetProxyAssociation(control, relayAddr)
		proxyReady <- err
		if err != nil {
			return
		}
		// Only new association attempts are refused. Keep the first SOCKS
		// association's control open for the lifetime of the fixture.
		for range pilotSharedSocketAttempts {
			conn, err := fixture.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
		var unused [1]byte
		_, _ = control.Read(unused[:])
	}()

	mailbox := stableUDPTestMailbox(relayAddr)
	mailbox.exitProxy = "socks5://" + fixture.Addr().String()
	mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
	ctx, cancel := context.WithCancel(t.Context())
	resets := make(chan pilotAssociationReset, pilotSharedMaxAssociations)
	failed := make(chan error, 1)
	manager := newPilotSharedUDPExitWithResets(ctx, mailbox, pilotSharedMaxAssociations, resets, func(err error) { failed <- err })
	defer func() { cancel(); <-manager.done }()
	send := func(id uint32, body string) {
		manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(id, 1), sent: time.Now(), target: relayAddr, body: []byte(body)})
	}
	send(1, "healthy")
	select {
	case err := <-proxyReady:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy fixture did not negotiate")
	}
	origin := testReadResetProxyPacket(t, relay, "healthy")
	// Model a previously queued response for the association being retired.
	mailbox.offer(sharedExitTestFlow(2, 1), relayAddr, []byte("queued reply"))
	for attempt := uint64(1); attempt <= pilotSharedSocketAttempts; attempt++ {
		send(2, "setup refused")
		stableUDPWait(t, func() bool {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			group := manager.groups[2]
			return manager.ctx.Err() != nil || manager.retired.contains(2) ||
				group != nil && !group.opening && mailbox.originDialErrors.Load() >= attempt
		})
		manager.mu.Lock()
		if group := manager.groups[2]; group != nil {
			group.retryAfter = time.Time{}
		}
		manager.mu.Unlock()
	}
	select {
	case reset := <-resets:
		if reset.id != 2 || mailbox.fairQueue.len() != 0 {
			t.Fatalf("reset announced before queue purge: id=%d queued=%d", reset.id, mailbox.fairQueue.len())
		}
		reset.written()
	case err := <-failed:
		t.Fatalf("local failure reset the lease: %v", err)
	case <-time.After(time.Second):
		t.Fatal("failed association was not reset")
	}
	send(2, "late authenticated datagram")
	if mailbox.rejected.Load() != 1 || mailbox.originDialErrors.Load() != pilotSharedSocketAttempts {
		t.Fatal("late datagram reopened a retired association")
	}
	send(1, "healthy after reset")
	if after := testReadResetProxyPacket(t, relay, "healthy after reset"); after != origin {
		t.Fatalf("healthy association changed its proxy source endpoint from %v to %v", origin, after)
	}
}

func testAcceptResetProxyAssociation(control net.Conn, relay netip.AddrPort) error {
	if err := expectSOCKSUDPProxyBytes(control, []byte{5, 1, 0}); err != nil {
		return err
	}
	if err := writeAll(control, []byte{5, 0}); err != nil {
		return err
	}
	if err := expectSOCKSUDPProxyBytes(control, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	return writeAll(control, append([]byte{5, 0, 0}, buildSOCKSAddress(relay.Addr().String(), int(relay.Port()))...))
}

func testReadResetProxyPacket(t *testing.T, relay *net.UDPConn, want string) netip.AddrPort {
	t.Helper()
	if err := relay.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var wire [256]byte
	n, source, err := relay.ReadFromUDPAddrPort(wire[:])
	if err != nil {
		t.Fatal(err)
	}
	packet, err := parseSOCKSUDPDatagram(wire[:n])
	if err != nil || string(packet.Payload) != want {
		t.Fatalf("relay received %q, want %q, error %v", packet.Payload, want, err)
	}
	return source
}

func TestUDPPreviewAssociationResetIgnoresPriorMappingCallbacks(t *testing.T) {
	peer, endpoint := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(endpoint)
	mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
	ctx, cancel := context.WithCancel(t.Context())
	resets := make(chan pilotAssociationReset, pilotSharedMaxAssociations)
	manager := newPilotSharedUDPExitWithResets(ctx, mailbox, pilotSharedMaxAssociations, resets, nil)
	defer func() { cancel(); <-manager.done }()
	send := func(body string) {
		manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(1, 1), sent: time.Now(), target: endpoint, body: []byte(body)})
		stableUDPRead(t, peer, body)
	}
	send("old attempt")
	manager.mu.Lock()
	group := manager.groups[1]
	first, oldCtx := group.mapping, group.ctx
	manager.mu.Unlock()
	manager.reap(time.Now().Add(udpPreviewStableIdle + time.Second))
	stableUDPWait(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return group.mapping == nil
	})
	send("new attempt")
	manager.mappingFailed(1, group, oldCtx, errors.New("late callback"))
	first.offer(sharedExitTestFlow(1, 1), endpoint, []byte("late reply callback"))
	if len(resets) != 0 || mailbox.fairQueue.len() != 0 || manager.ctx.Err() != nil {
		t.Fatal("old mapping callback affected the replacement")
	}
	send("after late callback")
	manager.mu.Lock()
	current, currentCtx := group.mapping, group.ctx
	manager.mu.Unlock()
	current.offer(sharedExitTestFlow(1, 1), endpoint, []byte("queued before reset"))
	manager.mappingFailed(1, group, currentCtx, errors.New("current reader failed"))
	current.offer(sharedExitTestFlow(1, 1), endpoint, []byte("raced retirement"))
	select {
	case reset := <-resets:
		if reset.id != 1 || mailbox.fairQueue.len() != 0 || manager.ctx.Err() != nil {
			t.Fatal("current mapping reset did not cleanly retire its association")
		}
		reset.written()
	default:
		t.Fatal("current reader failure was not sent to client")
	}
}

func TestUDPPreviewAssociationResetQueueOverflowClosesLease(t *testing.T) {
	peer, endpoint := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(endpoint)
	ctx, cancel := context.WithCancel(t.Context())
	resets := make(chan pilotAssociationReset, 1)
	resets <- pilotAssociationReset{id: 50}
	failed := make(chan error, 1)
	manager := newPilotSharedUDPExitWithResets(ctx, mailbox, pilotSharedMaxAssociations, resets, func(err error) { failed <- err })
	defer func() { cancel(); <-manager.done }()

	manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(2, 1), sent: time.Now(), target: endpoint, body: []byte("started")})
	stableUDPRead(t, peer, "started")
	manager.mu.Lock()
	socket := manager.groups[2].mapping.socket
	manager.mu.Unlock()
	// Closing the actual native socket exercises the reader's fatal callback.
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, errSharedUDPResetQueueFull) || manager.ctx.Err() == nil {
			t.Fatalf("failed signaling did not close the lease: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked reverse reset did not close the lease")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.retired.contains(2) || len(resets) != 1 {
		t.Fatal("blocked notification lost retirement or exceeded queue capacity")
	}
}

func TestUDPPreviewAssociationResetKeepsSlotsUntilWritten(t *testing.T) {
	const limit = 64
	peer, endpoint := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(endpoint)
	ctx, cancel := context.WithCancel(t.Context())
	resets := make(chan pilotAssociationReset, limit)
	manager := newPilotSharedUDPExitWithResets(ctx, mailbox, limit, resets, nil)
	defer func() { cancel(); <-manager.done }()

	send := func(id uint32, body string) {
		manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(id, 1), sent: time.Now(), target: endpoint, body: []byte(body)})
	}
	send(1, "held")
	source := stableUDPRead(t, peer, "held")
	for id := uint32(2); id <= limit; id++ {
		send(id, "neighbor")
		stableUDPRead(t, peer, "neighbor")
	}
	manager.mu.Lock()
	var sockets []pilotUDPSocket
	for id := uint32(2); id <= limit; id++ {
		sockets = append(sockets, manager.groups[id].mapping.socket)
	}
	manager.mu.Unlock()
	for _, socket := range sockets {
		if err := socket.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Halt the writer after it has dequeued one RESET. Idle/failed sockets
	// finish teardown and both queued and in-flight notifications hold slots.
	stableUDPWait(t, func() bool { return len(resets) == limit-1 })
	first := <-resets
	stableUDPWait(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		for id := uint32(2); id <= limit; id++ {
			if group := manager.groups[id]; group != nil && group.mapping != nil {
				return false
			}
		}
		return true
	})
	for id := uint32(limit + 1); id <= 2*limit; id++ {
		send(id, "blocked until reset sent")
	}
	if mailbox.flowCap.Load() != limit || manager.ctx.Err() != nil {
		t.Fatalf("control backpressure lost bounds or retired the carrier: refusals=%d err=%v", mailbox.flowCap.Load(), manager.ctx.Err())
	}
	send(1, "held with reset backlog")
	if actual := stableUDPRead(t, peer, "held with reset backlog"); actual != source {
		t.Fatalf("held source changed: %v", actual)
	}
	first.written()
	send(limit+1, "replacement after reset")
	stableUDPRead(t, peer, "replacement after reset")
	for len(resets) > 0 {
		reset := <-resets
		reset.written()
	}
	for id := uint32(2); id <= limit; id++ {
		send(id, "late after reset")
	}
	if mailbox.rejected.Load() != limit-1 || sharedExitTestGroupCount(manager) != 2 {
		t.Fatalf("late datagrams affected a retired group: rejected=%d groups=%d", mailbox.rejected.Load(), sharedExitTestGroupCount(manager))
	}
}

type resetTestControl struct {
	net.Conn
	handler func(uint32)
}

func (c *resetTestControl) setAssociationResetHandler(fn func(uint32)) { c.handler = fn }

func TestUDPPreviewClientResetClosesOnlyMatchingGenerationAssociation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &pilotSharedClient{ctx: ctx}
	client.open = func(parent context.Context) (*pilotClientLease, error) {
		lease, remote := sharedClientTestLease(parent)
		lease.control = &resetTestControl{Conn: lease.control}
		finish := lease.finish
		lease.finish = func(reason string) { finish(reason); _ = remote.Close() }
		return lease, nil
	}
	open := func() (*resetTestControl, net.Conn, uint32, func()) {
		t.Helper()
		local, peer := net.Pipe()
		t.Cleanup(func() { _ = local.Close(); _ = peer.Close() })
		lease, id, release, _, err := client.acquire(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		return lease.control.(*resetTestControl), peer, id, release
	}
	control, first, firstID, releaseFirst := open()
	defer releaseFirst()
	_, second, secondID, releaseSecond := open()
	defer releaseSecond()
	control.handler(secondID)
	if err := testReadResetFrontend(second); !errors.Is(err, io.EOF) {
		t.Fatalf("failed frontend did not close: %v", err)
	}
	if err := testReadResetFrontend(first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("healthy frontend closed: %v", err)
	}
	releaseSecond()
	client.mu.Lock()
	oldGeneration := client.current
	client.mu.Unlock()
	releaseFirst()
	<-oldGeneration.done
	next, replacement, newID, releaseNext := open()
	defer releaseNext()
	if newID != firstID {
		t.Fatalf("fixture did not reuse an ID: old=%d new=%d", firstID, newID)
	}
	control.handler(firstID)
	if err := testReadResetFrontend(replacement); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("obsolete reset affected the matching ID on the next lease: %v", err)
	}
	next.handler(newID)
	if err := testReadResetFrontend(replacement); !errors.Is(err, io.EOF) {
		t.Fatalf("current reset did not close its frontend: %v", err)
	}
}

func testReadResetFrontend(conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var one [1]byte
	_, err := conn.Read(one[:])
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}
