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

func TestPreviewProxyPendingSlowSetupKeepsFreshPackets(t *testing.T) {
	relay, relayAddr := stableUDPTestPeer(t)
	gotGreeting, proceed := make(chan struct{}), make(chan struct{})
	address, server := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
		if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 1, 0}); err != nil {
			return err
		}
		close(gotGreeting)
		select {
		case <-proceed:
		case <-time.After(4 * time.Second):
			return errors.New("fixture never resumed proxy setup")
		}
		if err := writeAll(conn, []byte{5, 0}); err != nil {
			return err
		}
		if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return err
		}
		if err := writeAll(conn, append([]byte{5, 0, 0}, buildSOCKSAddress(relayAddr.Addr().String(), int(relayAddr.Port()))...)); err != nil {
			return err
		}
		var extra [1]byte
		_, err := conn.Read(extra[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	target := netip.MustParseAddrPort("127.0.0.1:1234") // Negotiated relay sees this target; the test never dials it.
	mailbox := stableUDPTestMailbox(target)
	mailbox.exitProxy = "socks5://" + address
	s := newPilotSharedUDPExit(ctx, mailbox, pilotSharedMaxAssociations, nil)
	defer func() { cancel(); <-s.done }()
	send := func(body string) {
		s.send(ctx, pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, sent: time.Now(), body: []byte(body)})
	}
	send("expires before another arrival")
	select {
	case <-gotGreeting:
	case <-time.After(time.Second):
		t.Fatal("proxy setup never reached the controlled wait")
	}
	s.mu.Lock()
	s.groups[1].pending[0].sent = time.Now().Add(-pilotLifetime - time.Second)
	s.mu.Unlock()
	send("fresh-a")
	if mailbox.stale.Load() != 1 {
		t.Fatal("a fresh arrival did not remove an expired pending packet")
	}
	send("expires during drain")
	send("fresh-b")
	s.mu.Lock()
	s.groups[1].pending[1].sent = time.Now().Add(-pilotLifetime - time.Second)
	s.mu.Unlock()
	close(proceed)
	_ = relay.SetReadDeadline(time.Now().Add(time.Second))
	var received [256]byte
	for _, want := range []string{"fresh-a", "fresh-b"} {
		n, _, err := relay.ReadFromUDPAddrPort(received[:])
		if err != nil {
			t.Fatal(err)
		}
		packet, err := parseSOCKSUDPDatagram(received[:n])
		if err != nil || string(packet.Payload) != want {
			t.Fatalf("proxy drain received %q, want %q, error %v", packet.Payload, want, err)
		}
	}
	if mailbox.stale.Load() != 2 || mailbox.dropped.Load() != 0 {
		t.Fatalf("wrong expiry accounting: stale=%d drops=%d", mailbox.stale.Load(), mailbox.dropped.Load())
	}
	if err := s.closeAssociation(1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed association kept the upstream control connection")
	}
}

func TestPreviewProxyPendingCapsAndDiscard(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The owned listener withholds the SOCKS greeting reply so the real
	// opener remains in flight until the manager cancels it.
	ctx, cancel := context.WithCancel(t.Context())
	target := netip.MustParseAddrPort("127.0.0.1:1234")
	mailbox := stableUDPTestMailbox(target)
	mailbox.exitProxy = "socks5://" + listener.Addr().String()
	s := newPilotSharedUDPExit(ctx, mailbox, 4, nil)
	defer func() { cancel(); <-s.done }()
	send := func(id uint32, tag byte) {
		payload := make([]byte, pilotMaxPayload)
		payload[0] = tag
		s.send(ctx, pilotPacket{flow: sharedExitTestFlow(id, 1), target: target, sent: time.Now(), body: payload})
	}
	for i := range pilotSharedProxyPending + 1 {
		send(1, byte(i))
	}
	s.mu.Lock()
	group := s.groups[1]
	newestKept := group != nil && len(group.pending) == pilotSharedProxyPending && group.pending[0].body[0] == 1 &&
		group.pending[len(group.pending)-1].body[0] == pilotSharedProxyPending
	s.mu.Unlock()
	if !newestKept || mailbox.dropped.Load() != 1 {
		t.Fatal("per-association pending pressure did not preserve the newer packet")
	}
	for i := range pilotSharedProxyPending {
		send(2, byte(i))
	}
	s.mu.Lock()
	packets, pendingBytes := s.pendingPackets, s.pendingBytes
	s.mu.Unlock()
	if packets != pilotSharedProxyPackets || pendingBytes != pilotSharedProxyBytes {
		t.Fatalf("pending bounds not reached: packets=%d bytes=%d", packets, pendingBytes)
	}
	send(3, 99)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		group := s.groups[3]
		if s.pendingPackets != pilotSharedProxyPackets || s.pendingBytes != pilotSharedProxyBytes ||
			group == nil || len(group.pending) != 1 || group.pending[0].body[0] != 99 || mailbox.dropped.Load() != 2 {
			t.Fatal("global pressure rejected fresh traffic or exceeded the aggregate budget")
		}
		for id := uint32(1); id <= 3; id++ {
			group := s.groups[id]
			if group == nil || !group.opening || group.cancel == nil {
				t.Fatalf("association %d did not start a cancellable opener", id)
			}
			if err := s.retireAssociationLocked(id); err != nil {
				t.Fatal(err)
			}
			// A canceled worker cannot finish while the manager lock is
			// held, so its resource slot must still be reserved here.
			if s.groups[id] != group || !group.closed || group.ctx.Err() != context.Canceled {
				t.Fatalf("association %d lost its slot before its opener finished", id)
			}
		}
		if s.pendingPackets != 0 || s.pendingBytes != 0 || mailbox.dropped.Load() != pilotSharedProxyPackets+2 {
			t.Fatal("closed associations retained or double-counted pending packets")
		}
	}()
	stableUDPWait(t, func() bool { return sharedExitTestGroupCount(s) == 0 })
}

func TestPreviewProxyPendingConcurrentArrivalDuringDrain(t *testing.T) {
	for _, closeDuringDrain := range []bool{false, true} {
		name := "FIFO"
		if closeDuringDrain {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			groupCtx, cancelGroup := context.WithCancel(ctx)
			defer cancelGroup()
			target := netip.MustParseAddrPort("127.0.0.1:1234")
			mailbox := stableUDPTestMailbox(target)
			mailbox.exitProxy = "socks5://controlled-fixture.invalid"
			writes := &pendingTestSocket{ctx: groupCtx, first: make(chan struct{}), proceed: make(chan struct{}), wrote: make(chan string, 3)}
			mapping := &pilotUDPMapping{socket: writes, mailbox: mailbox,
				byFlow: make(map[uint64]netip.AddrPort), byEndpoint: make(map[netip.AddrPort]uint64)}
			group := &pilotSharedUDPAssociation{ctx: groupCtx, cancel: cancelGroup, opening: true, mapping: mapping}
			s := &pilotSharedUDPExit{ctx: ctx, mailbox: mailbox, maxAssociations: 4,
				groups: map[uint32]*pilotSharedUDPAssociation{1: group}}
			send := func(body string) {
				s.send(ctx, pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, sent: time.Now(), body: []byte(body)})
			}
			send("first")
			send("second")
			done := make(chan struct{})
			go func() { defer close(done); s.drainProxyPending(1, group, mapping, groupCtx) }()
			select {
			case <-writes.first:
			case <-time.After(time.Second):
				t.Fatal("first queued packet did not begin flushing")
			}
			newArrival := make(chan struct{})
			go func() { send("new while blocked"); close(newArrival) }()
			select {
			case <-newArrival:
			case <-time.After(time.Second):
				t.Fatal("a new packet waited for or overtook the pending flush")
			}
			if closeDuringDrain {
				if err := s.closeAssociation(1); err != nil {
					t.Fatal(err)
				}
			} else {
				close(writes.proceed)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("drain did not exit")
			}
			if s.pendingPackets != 0 || s.pendingBytes != 0 || group.opening {
				t.Fatal("drain retained pending records or setup gate")
			}
			if closeDuringDrain {
				if len(writes.wrote) != 0 || mailbox.dropped.Load() != 2 {
					t.Fatal("association close sent or missed accounting for queued packets")
				}
				return
			}
			for _, want := range []string{"first", "second", "new while blocked"} {
				select {
				case got := <-writes.wrote:
					if got != want {
						t.Fatalf("pending FIFO changed: got %q, want %q", got, want)
					}
				default:
					t.Fatalf("pending FIFO did not write %q", want)
				}
			}
		})
	}
}

type pendingTestSocket struct {
	ctx            context.Context
	first, proceed chan struct{}
	wrote          chan string
}

func (s *pendingTestSocket) WriteToUDPAddrPort(p []byte, _ netip.AddrPort) (int, error) {
	if string(p) == "first" {
		close(s.first)
		select {
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		case <-s.proceed:
		}
	}
	s.wrote <- string(p)
	return len(p), nil
}

func (s *pendingTestSocket) ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error) {
	return 0, netip.AddrPort{}, io.EOF
}

func (s *pendingTestSocket) SetWriteDeadline(time.Time) error { return nil }
func (s *pendingTestSocket) Close() error                     { return nil }
