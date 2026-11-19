package skirk

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewSharedCapacityProof(t *testing.T) {
	base := publicPreviewForLifetimeTest(t)
	base.tunnel.Data = base.drive
	cfg := &Config{Drive: DriveConfig{FolderID: base.drive.folderID}, Tunnel: TunnelConfig{
		UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true,
	}}
	var omitted [32]byte
	seen := make(map[[32]byte]bool)
	for _, limit := range []int{0, 16, 32, 64} {
		cfg.Tunnel.UDPPreviewSharedAssociations = limit
		p, err := newDatagramPreview(base.tunnel, cfg)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := p.proof("fixture-client", "fixture-run", [exitProbeNonceBytes]byte{1}, [exitProbeNonceBytes]byte{2})
		if err != nil {
			t.Fatal(err)
		}
		switch limit {
		case 0:
			omitted = proof
		case 16:
			if proof != omitted {
				t.Fatal("explicit default changed the P13 proof")
			}
		default:
			if seen[proof] {
				t.Fatalf("association cap %d did not change the authenticated proof", limit)
			}
		}
		seen[proof] = true
	}
	cfg.Tunnel.UDPPreviewSharedAssociations = 65
	if _, err := previewUDPBatchBytes(cfg.Tunnel); err == nil {
		t.Fatal("config raised the hard cap")
	}
	cfg.Tunnel.UDPPreviewSharedAssociations, cfg.Tunnel.UDPPreviewSharedUDP = 32, false
	if _, err := previewUDPBatchBytes(cfg.Tunnel); err == nil {
		t.Fatal("independent mode accepted a shared cap")
	}
}

func TestUDPPreviewSharedHEVIdleConnectionReleasesWithoutInterruptingReceiveOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		target := netip.MustParseAddrPort("8.8.8.8:3478") // Policy fixture only, no real UDP dial.
		mailbox := stableUDPTestMailbox()
		flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow), sharedMaxFlows: 32 * pilotMaxFlows}
		var upstream net.Conn
		s := &pilotSharedClient{ctx: ctx, maxAssociations: 32, open: func(ctx context.Context) (*pilotClientLease, error) {
			lease, remote := sharedClientTestLease(ctx)
			lease.mailbox, lease.flows, upstream = mailbox, flows, remote
			return lease, nil
		}}
		open := func(body string) (net.Conn, pilotPacket, <-chan struct{}) {
			app, server := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = server.Close() })
			done := make(chan struct{})
			go func() {
				defer close(done)
				relay := &pilotClientRelay{publicIPv4: true, dns: true, shared: s}
				pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{server})
			}()
			var response [10]byte
			if _, err := io.ReadFull(app, response[:]); err != nil || response[1] != 0 {
				t.Fatalf("HEV associate failed: %v", err)
			}
			if err := writeAll(app, buildUDPInTCPFrame(target.Addr().String(), int(target.Port()), []byte(body))); err != nil {
				t.Fatal(err)
			}
			return app, <-mailbox.queue, done
		}
		active, incoming, activeDone := open("receive-only")
		idle, idleUp, idleDone := open("idle")
		defer upstream.Close()
		closed := make(chan uint32, 1)
		go func() {
			var id [4]byte
			if _, err := io.ReadFull(upstream, id[:]); err == nil {
				closed <- binary.BigEndian.Uint32(id[:])
			}
			close(closed)
		}()
		for i := 1; i <= 14; i++ {
			time.Sleep(10 * time.Second)
			incoming.body = []byte("periodic reply")
			flows.returnToClient(ctx, incoming)
			if reply, err := readUDPInTCPFrame(active); err != nil || string(reply.Payload) != "periodic reply" {
				t.Fatalf("receive-only association failed after %ds: %v", i*10, err)
			}
			if i == 11 {
				// DNS-only traffic must not retain a shared media association.
				if err := writeAll(idle, buildUDPInTCPFrame("198.18.0.2", 53, dnsQuestion("fixture.invalid", 28))); err != nil {
					t.Fatal(err)
				}
				if _, err := readUDPInTCPFrame(idle); err != nil {
					t.Fatal(err)
				}
			}
		}
		synctest.Wait()
		select {
		case <-idleDone:
		default:
			t.Fatal("idle local link held its association after 140 seconds")
		}
		if id := <-closed; id != uint32(idleUp.flow>>32) {
			t.Fatalf("idle local teardown did not send the exact association close: %d", id)
		}
		flows.mu.Lock()
		_, alive := flows.client[incoming.flow]
		_, idleRetained := flows.client[idleUp.flow]
		flows.mu.Unlock()
		if !alive || idleRetained {
			t.Fatal("idle reaper damaged another association or leaked a callback")
		}
		_ = active.Close()
		<-activeDone
		synctest.Wait()
		if s.current != nil {
			t.Fatal("final association did not release the carrier")
		}
	})
}

func TestUDPPreviewSharedCapacitySocketsAndDormantBounds(t *testing.T) {
	for _, limit := range []int{32, 64} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			peer, target := previewIPv6TestPeer(t)
			mailbox := stableUDPTestMailbox(target)
			mailbox.publicIPv6 = true
			ctx, cancel := context.WithCancel(t.Context())
			manager := newPilotSharedUDPExit(ctx, mailbox, limit, nil)
			defer func() { cancel(); <-manager.done }()
			sources := make(map[netip.AddrPort]bool)
			for i := range limit {
				manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(uint32(i+1), 1), target: target, body: []byte("request")})
				sources[stableUDPRead(t, peer, "request")] = true
			}
			manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(uint32(limit+1), 1), target: target, body: []byte("over cap")})
			manager.mu.Lock()
			var sockets []pilotUDPSocket
			for _, group := range manager.groups {
				sockets = append(sockets, group.mapping.socket, group.mapping.socket6)
			}
			manager.mu.Unlock()
			if len(sources) != limit || len(sockets) != 2*limit || mailbox.flowCap.Load() != 1 {
				t.Fatalf("runtime cap not observed, sources=%d sockets=%d refusal=%d", len(sources), len(sockets), mailbox.flowCap.Load())
			}
			manager.reap(time.Now().Add(udpPreviewStableIdle + time.Second))
			stableUDPWait(t, func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()
				for _, group := range manager.groups {
					if group.mapping != nil {
						return false
					}
				}
				return len(manager.groups) == limit
			})
			for _, socket := range sockets {
				if err := socket.SetWriteDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("dormant group retained a socket: %v", err)
				}
			}
			for i := range limit {
				if err := manager.closeAssociation(uint32(i + 1)); err != nil {
					t.Fatal(err)
				}
			}
			if sharedExitTestGroupCount(manager) != 0 {
				t.Fatal("explicit closes retained dormant registrations")
			}
			manager.send(ctx, pilotPacket{flow: sharedExitTestFlow(uint32(limit+1), 1), target: target, body: []byte("reused")})
			stableUDPRead(t, peer, "reused")
			t.Logf("%d active groups: %d sockets, %d MiB explicit reader buffers; zero previous sockets after idle", limit, len(sockets), len(sockets)/16)
		})
	}
}
