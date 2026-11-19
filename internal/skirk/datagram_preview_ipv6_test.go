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

func TestUDPPreviewIPv6ConfigurationAndPolicy(t *testing.T) {
	cfg := Config{Drive: DriveConfig{FolderID: "isolated-drive-folder"}, Tunnel: TunnelConfig{
		UDPPreviewTargets: []string{"127.0.0.1:3478", "[::1]:3478"}, UDPPreviewIPv6: true, UDPPreviewPublicIPv4: true,
		UDPPreviewStableUDP: true, UDPPreviewRemoteDNS: true,
	}}
	if _, err := previewUDPBatchBytes(cfg.Tunnel); err != nil {
		t.Fatal(err)
	}
	for _, disable := range []func(*TunnelConfig){
		func(c *TunnelConfig) { c.UDPPreviewStableUDP = false },
		func(c *TunnelConfig) { c.UDPPreviewPublicIPv4 = false },
		func(c *TunnelConfig) { c.UDPPreviewRemoteDNS = false },
	} {
		candidate := cfg.Tunnel
		disable(&candidate)
		if _, err := previewUDPBatchBytes(candidate); err == nil {
			t.Fatal("IPv6 accepted without all required opt-ins")
		}
	}
	allowed, err := previewUDPAllowlist(&cfg)
	if err != nil || len(allowed) != 2 {
		t.Fatalf("owned dual-stack targets rejected: %v", err)
	}
	cfg.Tunnel.UDPPreviewIPv6 = false
	if _, err := previewUDPAllowlist(&cfg); err == nil {
		t.Fatal("IPv4-only configuration accepted an IPv6 fixture")
	}
	configCases := []struct {
		address string
		want    bool
	}{
		{"[::1]:3478", true},
		{"[::1]:3479", false},
		{"127.0.0.1:3478", true},
		{"[2606:4700::1]:3478", true},
		{"[2001:4860::1]:3478", true},
		{"[2400:1234::1]:853", false},
		{"[2400:1234::1]:53", false},
		{"[2400:1234::1%eth0]:3478", false},
		{"[::]:3478", false},
		{"[::ffff:100.64.0.1]:3478", false},
		{"[::c0a8:101]:3478", false},
		{"[64:ff9b::192.168.1.1]:3478", false},
		{"[fc00::1]:3478", false},
		{"[fe80::1]:3478", false},
		{"[ff02::1]:3478", false},
		{"[2001:db8::1]:3478", false},
		{"[2001:2::1]:3478", false},
		{"[2002:c0a8:101::1]:3478", false},
		{"[3ffe::1]:3478", false},
		{"[3fff::1]:3478", false},
	}
	for _, tc := range configCases {
		if got := permitPreviewTarget(allowed, true, true, netip.MustParseAddrPort(tc.address)); got != tc.want {
			t.Errorf("permit %s = %t, want %t", tc.address, got, tc.want)
		}
	}
	for _, input := range []string{"2606:4700::1", "relay.invalid", "::1"} {
		if _, ok := pilotAllowedTarget(allowed, true, false, socksUDPPacket{Host: input, Port: 3478}); ok {
			t.Errorf("IPv4 preview unexpectedly accepted %s", input)
		}
	}
	packet := pilotPacket{flow: 1, sent: time.Now(), target: netip.MustParseAddrPort("[::1]:3478"), body: []byte("fixture")}
	wire := pilotMarshal([]pilotPacket{packet})
	if _, ok := pilotUnmarshal(wire, pilotMaxPlain, pilotMaxBatch, false); ok {
		t.Fatal("legacy decoder accepted an IPv6 datagram")
	}
	if got, ok := pilotUnmarshal(wire, pilotMaxPlain, pilotMaxBatch, true); !ok || got[0].target != packet.target || string(got[0].body) != "fixture" {
		t.Fatal("dual-stack codec changed the IPv6 datagram")
	}
}

func TestUDPPreviewIPv6BothSOCKSFrontends(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	ipv4, v4Target := stableUDPTestPeer(t)
	ipv6a, v6TargetA := previewIPv6TestPeer(t)
	ipv6b, v6TargetB := previewIPv6TestPeer(t)
	v4Sources := echoPreviewUDP(ctx, ipv4)
	v6SourcesA := echoPreviewUDP(ctx, ipv6a)
	v6SourcesB := echoPreviewUDP(ctx, ipv6b)
	meta := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	_, _, socksAddress := startUDPPreviewTestPeers(t, ctx, "matching dual-stack peers", v4Target, NewMemoryStore(), meta, v6TargetA, v6TargetB)
	for _, command := range []byte{socksCommandUDPAssociate, socksCommandUDPInTCP} {
		control, local := openPreviewAssociation(t, socksAddress, command)
		defer control.Close()
		if local != nil {
			defer local.Close()
		}
		host4 := v4Target.Addr().String()
		if command == socksCommandUDPInTCP {
			host4 = pilotHEVLoopbackAlias
		}
		cases := []struct {
			host string
			port int
			body string
		}{
			{host4, int(v4Target.Port()), "first IPv4 payload"},
			{"::1", int(v6TargetA.Port()), "first IPv6 payload"},
			{"::1", int(v6TargetB.Port()), "second IPv6 payload"},
		}
		for _, want := range cases {
			writePreviewPacket(t, control, local, want.host, want.port, []byte(want.body))
		}
		received := make(map[string]socksUDPPacket)
		for range cases {
			packet, err := readPreviewPacket(control, local)
			if err != nil {
				t.Fatalf("SOCKS command %d rejected a dual-stack datagram: %v", command, err)
			}
			received[string(packet.Payload)] = packet
		}
		for _, want := range cases {
			if got := received[want.body]; got.Host != want.host || got.Port != want.port {
				t.Fatalf("SOCKS command %d changed the source or payload: got host %q, port %d", command, got.Host, got.Port)
			}
		}
		var sources []netip.AddrPort
		for _, c := range []<-chan netip.AddrPort{v4Sources, v6SourcesA, v6SourcesB} {
			select {
			case source := <-c:
				sources = append(sources, source)
			case <-ctx.Done():
				t.Fatal("origin did not receive a datagram")
			}
		}
		if !sources[0].Addr().Is4() || !sources[1].Addr().Is6() || sources[1] != sources[2] {
			t.Fatalf("stable source endpoint was not reused in the IPv6 family: %v", sources)
		}
		_ = control.Close()
	}
}

func TestUDPPreviewIPv6PolicyMismatchRefused(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	previewIPv6TestPeer(t) // Require IPv6 support before starting this fixture.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	peer, target := stableUDPTestPeer(t)
	got := echoPreviewUDP(ctx, peer)
	meta := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	_, _, socksAddress := startUDPPreviewTestPeers(t, ctx, "different IPv6 policy", target, NewMemoryStore(), meta)
	control, local := openPreviewAssociation(t, socksAddress, socksCommandUDPAssociate)
	defer control.Close()
	defer local.Close()
	writePreviewPacket(t, control, local, target.Addr().String(), int(target.Port()), []byte("refused"))
	_ = control.SetReadDeadline(time.Now().Add(udpPreviewHandshake + time.Second))
	var b [1]byte
	if _, err := control.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("UDP lease did not reject a mismatched IPv6 policy: %v", err)
	}
	select {
	case <-got:
		t.Fatal("mismatched policy sent a packet to the origin")
	default:
	}
}

func TestUDPPreviewIPv6StableMappingFiltersAndCloses(t *testing.T) {
	peer, target := previewIPv6TestPeer(t)
	unsolicited, unsolicitedTarget := previewIPv6TestPeer(t)
	mailbox := stableUDPTestMailbox(target, unsolicitedTarget)
	mailbox.publicIPv6 = true
	mapping, cancel := stableUDPTestMapping(t, mailbox)
	mapping.send(t.Context(), pilotPacket{flow: 1, target: target, body: []byte("request")})
	source := stableUDPRead(t, peer, "request")
	stableUDPWrite(t, unsolicited, source, []byte("unsolicited"))
	stableUDPWait(t, func() bool { return mailbox.rejected.Load() == 1 })
	stableUDPWrite(t, peer, source, []byte("solicited"))
	select {
	case got := <-mailbox.queue:
		if got.flow != 1 || got.target != target || string(got.body) != "solicited" {
			t.Fatal("unverified or misrouted IPv6 reply")
		}
	case <-time.After(time.Second):
		t.Fatal("solicited IPv6 reply did not reach the mailbox")
	}
	cancel()
	select {
	case <-mapping.done:
	case <-time.After(time.Second):
		t.Fatal("dual-stack readers did not close promptly")
	}
	for _, socket := range []pilotUDPSocket{mapping.socket, mapping.socket6} {
		if err := socket.SetWriteDeadline(time.Now()); err == nil {
			t.Fatal("closed lease retained an active socket")
		}
	}
}

func previewIPv6TestPeer(t *testing.T) (*net.UDPConn, netip.AddrPort) {
	t.Helper()
	peer, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { peer.Close() })
	return peer, peer.LocalAddr().(*net.UDPAddr).AddrPort()
}

func echoPreviewUDP(ctx context.Context, peer *net.UDPConn) <-chan netip.AddrPort {
	sources := make(chan netip.AddrPort, 4)
	go func() {
		var payload [512]byte
		for {
			n, source, err := peer.ReadFromUDPAddrPort(payload[:])
			if err != nil {
				return
			}
			select {
			case sources <- source:
			case <-ctx.Done():
				return
			}
			_, _ = peer.WriteToUDPAddrPort(payload[:n], source)
		}
	}()
	return sources
}
