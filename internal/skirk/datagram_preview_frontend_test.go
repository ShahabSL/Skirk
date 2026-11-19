package skirk

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func openLocalPreviewFrontend(t *testing.T, p *datagramPreview, command byte) (net.Conn, *net.UDPConn, <-chan struct{}, byte) {
	t.Helper()
	app, server := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = server.Close() })
	_ = app.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		p.serveClient(t.Context(), &driveMux{}, socksRequest{Command: command, Target: "0.0.0.0:0"}, leaseTestLoopbackConn{server}, nil)
	}()
	var reply [10]byte
	if _, err := io.ReadFull(app, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 || command != socksCommandUDPAssociate {
		return app, nil, done, reply[1]
	}
	udp, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(binary.BigEndian.Uint16(reply[8:]))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	_ = udp.SetDeadline(time.Now().Add(3 * time.Second))
	return app, udp, done, 0
}

func TestUDPPreviewFrontendAdmissionBoundsUnusedRFCAndHEV(t *testing.T) {
	p := &datagramPreview{sharedUDP: true, sharedLimit: 2, frontendIdle: time.Minute}
	limit := p.frontendLimit()
	if limit <= p.sharedLimit {
		t.Fatal("shared local frontends have no capacity above the media association budget")
	}
	first, _, firstDone, code := openLocalPreviewFrontend(t, p, socksCommandUDPAssociate)
	if code != 0 {
		t.Fatal("first RFC association rejected")
	}
	type connection struct {
		control net.Conn
		done    <-chan struct{}
	}
	others := make([]connection, 0, limit-1)
	for i := 1; i < limit; i++ {
		control, _, done, code := openLocalPreviewFrontend(t, p, socksCommandUDPInTCP)
		if code != 0 {
			t.Fatalf("HEV frontend %d rejected below the local limit", i+1)
		}
		others = append(others, connection{control, done})
	}
	_, _, deniedDone, code := openLocalPreviewFrontend(t, p, socksCommandUDPInTCP)
	if code == 0 {
		t.Fatal("unused associations bypassed the local frontend limit")
	}
	<-deniedDone
	_ = first.Close()
	<-firstDone
	replacement, _, replacementDone, code := openLocalPreviewFrontend(t, p, socksCommandUDPInTCP)
	if code != 0 {
		t.Fatal("closed RFC association did not release local admission")
	}
	for _, other := range others {
		_ = other.control.Close()
		<-other.done
	}
	_ = replacement.Close()
	<-replacementDone
	metrics := (&Tunnel{udpPreview: p}).UDPPreviewSnapshot()
	if metrics.ActiveFrontends != 0 || metrics.LocalFrontendLimitHits != 1 || metrics.ExitCarrierLimitHits != 0 || metrics.AdmissionLimitHits != 1 {
		t.Fatalf("unexpected local admission metrics: %+v", metrics)
	}
}

func TestUDPPreviewRFCUnusedAndDNSOnlyAssociationsExpireLocally(t *testing.T) {
	p := &datagramPreview{sharedUDP: true, sharedLimit: 2, frontendIdle: 400 * time.Millisecond}
	unused, _, unusedDone, code := openLocalPreviewFrontend(t, p, socksCommandUDPAssociate)
	if code != 0 {
		t.Fatal("unused association rejected")
	}
	active, udp, activeDone, code := openLocalPreviewFrontend(t, p, socksCommandUDPAssociate)
	if code != 0 {
		t.Fatal("DNS association rejected")
	}
	for range 6 {
		time.Sleep(100 * time.Millisecond)
		// AAAA is answered locally with NODATA; this fixture never resolves or dials externally.
		if _, err := udp.Write(buildSOCKSUDPDatagram("198.18.0.2", 53, dnsQuestion("fixture.invalid", 28))); err != nil {
			t.Fatal(err)
		}
		var response [512]byte
		if _, err := udp.Read(response[:]); err != nil {
			t.Fatal(err)
		}
		select {
		case <-activeDone:
			t.Fatal("successful DNS traffic did not retain local RFC socket")
		default:
		}
	}
	var b [1]byte
	if _, err := unused.Read(b[:]); err != io.EOF {
		t.Fatalf("unused control did not expire: %v", err)
	}
	<-unusedDone
	if _, err := active.Read(b[:]); err != io.EOF {
		t.Fatalf("idle DNS-only control did not expire: %v", err)
	}
	<-activeDone
	if p.frontendCount != 0 {
		t.Fatal("expired local associations retained admission")
	}
}

func TestUDPPreviewRFCOversizedDatagramKeepsAssociation(t *testing.T) {
	p := &datagramPreview{sharedUDP: true, sharedLimit: 2}
	control, udp, done, code := openLocalPreviewFrontend(t, p, socksCommandUDPAssociate)
	if code != 0 {
		t.Fatal("RFC association rejected")
	}
	defer func() {
		_ = control.Close()
		<-done
	}()
	const dnsHost = "198.18.0.2"
	expectDNS := func(id uint16) {
		t.Helper()
		query := previewDNSFixture(id, dnsmessage.TypeAAAA)
		wire, err := query.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := udp.Write(buildSOCKSUDPDatagram(dnsHost, 53, wire)); err != nil {
			t.Fatal(err)
		}
		var buf [512]byte
		n, err := udp.Read(buf[:])
		if err != nil {
			t.Fatal(err)
		}
		packet, err := parseSOCKSUDPDatagram(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		var reply dnsmessage.Message
		if err := reply.Unpack(packet.Payload); err != nil {
			t.Fatal(err)
		}
		if packet.Host != dnsHost || packet.Port != 53 || !reply.Response || reply.ID != id {
			t.Fatalf("unexpected reply: %#v, ID=%d", packet, reply.ID)
		}
	}
	expectDNS(1) // The local AAAA fixture answers without network resolution.
	oversized := previewDNSFixture(2, dnsmessage.TypeAAAA)
	oversized.Additionals = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Class: 4096},
		Body: &dnsmessage.OPTResource{Options: []dnsmessage.Option{
			{Code: 12, Data: make([]byte, previewDNSMaxQuery)},
		}},
	}}
	wire, err := oversized.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := udp.Write(buildSOCKSUDPDatagram(dnsHost, 53, wire)); err != nil {
		t.Fatal(err)
	}
	expectDNS(3)
}

func TestUDPPreviewMetricsWaitForLateFrontendCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := publicPreviewForLifetimeTest(t)
		p.sharedUDP = true
		p.tunnel.udpPreview = p
		mailbox, _, finish, err := p.startMailbox(t.Context(), [16]byte{1}, true, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		stopped := make(chan struct{})
		detach, ok := mailbox.attachFrontend(func() { close(stopped) })
		if !ok {
			t.Fatal("live mailbox rejected frontend")
		}
		finished := make(chan struct{})
		go func() { finish(); close(finished) }()
		<-stopped
		synctest.Wait()
		select {
		case <-finished:
			t.Fatal("metrics retired before frontend cleanup")
		default:
		}
		if _, ok := mailbox.attachFrontend(func() {}); ok {
			t.Fatal("closing mailbox admitted a frontend")
		}
		mailbox.offer(uint64(1)<<32|1, netip.MustParseAddrPort("8.8.8.8:3478"), []byte("late local packet"))
		mailbox.closeQueuedAssociation(1)
		detach()
		<-finished
		counters := p.tunnel.UDPPreviewSnapshot().Packets
		if counters.Offered != 1 || counters.Dropped != 1 || len(p.metricsMailboxes) != 0 || len(mailbox.frontends) != 0 {
			t.Fatalf("late frontend counters lost or references retained: %+v", counters)
		}
	})
}

func TestUDPPreviewFailedFrontendAttachCannotMutateRetiredMailbox(t *testing.T) {
	mailbox := stableUDPTestMailbox()
	mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
	mailbox.offer(uint64(1)<<32|1, netip.MustParseAddrPort("8.8.8.8:3478"), []byte("queued"))
	mailbox.stopFrontends()
	finished := false
	relay := &pilotClientRelay{mailbox: mailbox, association: 1, finish: func() { finished = true }}
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	if relay.attachFrontend(local) {
		t.Fatal("closing mailbox accepted relay")
	}
	relay.close(nil)
	if !finished || mailbox.dropped.Load() != 0 || relay.mailbox != nil {
		t.Fatal("unattached relay retained access to retired mailbox accounting")
	}
}

func TestUDPPreviewLocalIdleCancellationJoinsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop, touch := pilotFrontendIdleContext(context.Background(), time.Minute)
		time.Sleep(50 * time.Second)
		touch()
		time.Sleep(50 * time.Second)
		if ctx.Err() != nil {
			t.Fatal("activity did not extend local lifetime")
		}
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Fatal("idle association survived its deadline")
		}
		stop()
	})
}

func TestUDPPreviewSharedIdleCancelsInlineHEVDNSAndRetiresFrontend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p := publicPreviewForLifetimeTest(t)
		p.sharedUDP = true
		p.tunnel.udpPreview = p
		var mailbox *pilotMailbox
		coordinator := &pilotSharedClient{ctx: ctx, open: func(leaseCtx context.Context) (*pilotClientLease, error) {
			m, flows, finish, err := p.startMailbox(leaseCtx, [16]byte{2}, true, nil, nil, nil)
			if err != nil {
				return nil, err
			}
			mailbox = m
			lease, remote := sharedClientTestLease(leaseCtx)
			closeLease := lease.finish
			lease.mailbox, lease.flows = m, flows
			lease.finish = func(reason string) { finish(); closeLease(reason); _ = remote.Close() }
			return lease, nil
		}}
		queryStarted, queryCanceled := make(chan struct{}), make(chan struct{})
		relay := &pilotClientRelay{publicIPv4: true, dns: true, shared: coordinator,
			remoteDNS: func(queryCtx context.Context, _ []byte, _ bool) ([]byte, error) {
				close(queryStarted)
				<-queryCtx.Done()
				close(queryCanceled)
				return nil, queryCtx.Err()
			},
		}
		app, server := net.Pipe()
		defer app.Close()
		defer server.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{server})
		}()
		var reply [10]byte
		if _, err := io.ReadFull(app, reply[:]); err != nil {
			t.Fatal(err)
		}
		if err := writeAll(app, buildUDPInTCPFrame("8.8.8.8", 3478, []byte("initial"))); err != nil {
			t.Fatal(err)
		}
		if err := writeAll(app, buildUDPInTCPFrame("198.18.0.2", 53, dnsQuestion("fixture.invalid", 28))); err != nil {
			t.Fatal(err)
		}
		<-queryStarted
		time.Sleep(udpPreviewStableIdle + 2*time.Second)
		<-queryCanceled
		<-done
		synctest.Wait()
		if coordinator.current != nil || len(p.metricsMailboxes) != 0 || len(mailbox.frontends) != 0 {
			t.Fatal("idle HEV DNS retained the carrier or frontend accounting")
		}
		if p.tunnel.UDPPreviewSnapshot().Packets.Offered != 1 {
			t.Fatal("final local packet accounting lost")
		}
	})
}
