package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewAuthenticatedUpstreamCarriesTCPAndUDPWithoutBypass(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tcpOrigin, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpOrigin.Close()
	udpOrigin, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpOrigin.Close()
	udpTarget := udpOrigin.LocalAddr().(*net.UDPAddr).AddrPort()
	var tcpAccepted, udpReceived atomic.Uint64
	go func() {
		for {
			conn, err := tcpOrigin.Accept()
			if err != nil {
				return
			}
			tcpAccepted.Add(1)
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	go func() {
		var buffer [2048]byte
		for {
			n, source, err := udpOrigin.ReadFromUDPAddrPort(buffer[:])
			if err != nil {
				return
			}
			udpReceived.Add(1)
			_, _ = udpOrigin.WriteToUDPAddrPort(buffer[:n], source)
		}
	}()
	upstream := &testPreviewUpstream{tcpTarget: tcpOrigin.Addr().String(), udpTarget: udpTarget}
	proxyURL, proxyErrors := upstream.start(t, ctx)
	listener := startPreviewUpstreamTestPeers(t, ctx, proxyURL, udpTarget)
	clientSOCKS := "socks5://" + listener

	stream, err := dialViaSOCKS5(ctx, clientSOCKS, upstream.tcpTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(8 * time.Second))
	if err := writeAll(stream, []byte("owned TCP")); err != nil {
		t.Fatal(err)
	}
	if err := expectSOCKSUDPProxyBytes(stream, []byte("owned TCP")); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if upstream.connects.Load() != 1 || tcpAccepted.Load() != 1 {
		t.Fatalf("TCP did not go through the authenticated proxy: connects=%d origin=%d", upstream.connects.Load(), tcpAccepted.Load())
	}

	control, udp := openPreviewAssociation(t, listener, socksCommandUDPAssociate)
	defer control.Close()
	defer udp.Close()
	_ = udp.SetDeadline(time.Now().Add(10 * time.Second))
	writePreviewPacket(t, control, udp, udpTarget.Addr().String(), int(udpTarget.Port()), []byte("owned UDP"))
	reply, err := readPreviewPacket(control, udp)
	if err != nil || reply.Host != udpTarget.Addr().String() || reply.Port != int(udpTarget.Port()) || !bytes.Equal(reply.Payload, []byte("owned UDP")) {
		t.Fatalf("UDP through shared proxy failed: %v", err)
	}
	if upstream.associates.Load() != 1 || upstream.udpForwards.Load() != 1 || udpReceived.Load() != 1 {
		t.Fatalf("UDP did not go through the authenticated proxy: associates=%d forwards=%d origin=%d", upstream.associates.Load(), upstream.udpForwards.Load(), udpReceived.Load())
	}
	_ = control.Close()
	_ = udp.Close()

	upstream.refuseTCP.Store(true)
	if optimistic, err := dialViaSOCKS5(ctx, clientSOCKS, upstream.tcpTarget); err == nil {
		defer optimistic.Close()
		// The client frontend can reply to CONNECT before the remote mux
		// reports a late open failure. The stream must then end without data.
		_ = optimistic.SetDeadline(time.Now().Add(8 * time.Second))
		_, _ = optimistic.Write([]byte("must not reach TCP origin"))
		var byteFromOrigin [1]byte
		n, readErr := optimistic.Read(byteFromOrigin[:])
		if n != 0 || !errors.Is(readErr, io.EOF) {
			t.Fatalf("refused TCP stream did not close without origin data: n=%d err=%v", n, readErr)
		}
	}
	upstream.refuseUDP.Store(true)
	failedControl, failedUDP := openPreviewAssociation(t, listener, socksCommandUDPAssociate)
	defer failedControl.Close()
	defer failedUDP.Close()
	_ = failedControl.SetReadDeadline(time.Now().Add(10 * time.Second))
	writePreviewPacket(t, failedControl, failedUDP, udpTarget.Addr().String(), int(udpTarget.Port()), []byte("must not reach origin"))
	var marker [1]byte
	if _, err := failedControl.Read(marker[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("UDP ASSOCIATE refusal did not close the local association: %v", err)
	}
	if upstream.connects.Load() != 2 || upstream.associates.Load() != 2 || upstream.udpForwards.Load() != 1 ||
		tcpAccepted.Load() != 1 || udpReceived.Load() != 1 {
		t.Fatalf("refused traffic bypassed upstream: connects=%d associates=%d forwards=%d tcp=%d udp=%d",
			upstream.connects.Load(), upstream.associates.Load(), upstream.udpForwards.Load(), tcpAccepted.Load(), udpReceived.Load())
	}
	select {
	case err := <-proxyErrors:
		t.Fatal(err)
	default:
	}
}

func TestPreviewUpstreamResetPreservesHealthyAssociation(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	origin, target := stableUDPTestPeer(t)
	seen := make(chan netip.AddrPort, 4)
	go func() {
		var data [512]byte
		for {
			n, source, err := origin.ReadFromUDPAddrPort(data[:])
			if err != nil {
				return
			}
			select {
			case seen <- source:
			case <-ctx.Done():
				return
			}
			_, _ = origin.WriteToUDPAddrPort(data[:n], source)
		}
	}()
	upstream := &testPreviewUpstream{udpTarget: target}
	proxyURL, proxyErrors := upstream.start(t, ctx)
	listen := startPreviewUpstreamTestPeers(t, ctx, proxyURL, target)
	healthyControl, healthyUDP := openPreviewAssociation(t, listen, socksCommandUDPAssociate)
	defer healthyControl.Close()
	defer healthyUDP.Close()
	checkHealthy := func(payload string) netip.AddrPort {
		t.Helper()
		_ = healthyUDP.SetDeadline(time.Now().Add(5 * time.Second))
		writePreviewPacket(t, healthyControl, healthyUDP, target.Addr().String(), int(target.Port()), []byte(payload))
		reply, err := readPreviewPacket(healthyControl, healthyUDP)
		if err != nil || reply.Host != target.Addr().String() || reply.Port != int(target.Port()) || string(reply.Payload) != payload {
			t.Fatalf("healthy proxy association did not echo %q: %v", payload, err)
		}
		select {
		case source := <-seen:
			return source
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return netip.AddrPort{}
		}
	}
	source := checkHealthy("before reset")
	upstream.dropNewUDP.Store(true)
	failedControl, failedUDP := openPreviewAssociation(t, listen, socksCommandUDPAssociate)
	defer failedControl.Close()
	defer failedUDP.Close()
	_ = failedControl.SetReadDeadline(time.Now().Add(10 * time.Second))
	failed := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, err := failedControl.Read(marker[:])
		failed <- err
	}()
	retry := time.NewTicker(200 * time.Millisecond)
	defer retry.Stop()
	writeFailed := func() {
		writePreviewPacket(t, failedControl, failedUDP, target.Addr().String(), int(target.Port()), []byte("refused association"))
	}
	writeFailed()
waitForReset:
	for {
		select {
		case err := <-failed:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("failed proxy association did not receive its reset: %v", err)
			}
			break waitForReset
		case <-retry.C:
			if upstream.associates.Load() < 1+pilotSharedSocketAttempts {
				writeFailed()
			}
		case err := <-proxyErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if after := checkHealthy("after reset"); after != source {
		t.Fatalf("healthy association changed its upstream relay source from %v to %v", source, after)
	}
	if attempts, forwarded := upstream.associates.Load(), upstream.udpForwards.Load(); attempts != 1+pilotSharedSocketAttempts || forwarded != 2 {
		t.Fatalf("unexpected attempts or origin writes: attempts=%d forwarded=%d", attempts, forwarded)
	}

	// A definitive unsupported response is a capability error for this proxy.
	upstream.dropNewUDP.Store(false)
	upstream.refuseUDP.Store(true)
	unsupportedControl, unsupportedUDP := openPreviewAssociation(t, listen, socksCommandUDPAssociate)
	defer unsupportedControl.Close()
	defer unsupportedUDP.Close()
	_ = healthyControl.SetReadDeadline(time.Now().Add(6 * time.Second))
	_ = unsupportedControl.SetReadDeadline(time.Now().Add(6 * time.Second))
	writePreviewPacket(t, unsupportedControl, unsupportedUDP, target.Addr().String(), int(target.Port()), []byte("unsupported"))
	for _, control := range []net.Conn{unsupportedControl, healthyControl} {
		var marker [1]byte
		if _, err := control.Read(marker[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("unsupported proxy capability did not close the lease: %v", err)
		}
	}
}

func TestPreviewUpstreamRejectsTCPOnlyProxyConfiguration(t *testing.T) {
	valid := Config{Drive: DriveConfig{FolderID: "isolated-drive-folder"}, Tunnel: TunnelConfig{
		Transport: "muxv5", UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true,
	}}
	for _, scheme := range []string{"http", "https"} {
		valid.Tunnel.ExitProxy = scheme + "://127.0.0.1:12345"
		if _, err := previewUDPAllowlist(&valid); !errors.Is(err, errSOCKSUDPUnsupported) {
			t.Fatalf("shared UDP accepted %s upstream: %v", scheme, err)
		}
	}
}

func TestPreviewUpstreamCloseInterruptsPendingUDPAssociation(t *testing.T) {
	started := make(chan struct{})
	address, server := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
		if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 1, 0}); err != nil {
			return err
		}
		close(started)
		var extra [1]byte
		_, err := conn.Read(extra[:])
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("pending SOCKS handshake did not close: %w", err)
		}
		return nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mailbox := &pilotMailbox{publicIPv4: true, exitProxy: "socks5://" + address}
	shared := newPilotSharedUDPExit(ctx, mailbox, pilotSharedMaxAssociations, nil)
	packet := pilotPacket{flow: (9 << 32) | 1, target: netip.MustParseAddrPort("127.0.0.1:1234"), sent: time.Now(), body: []byte("pending")}
	// This test never dials the target. Only the upstream fixture sees a
	// greeting, which it holds until association close cancels the setup.
	mailbox.allowed = map[netip.AddrPort]bool{packet.target: true}
	shared.send(ctx, packet)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("asynchronous proxy setup did not start")
	}
	if err := shared.closeAssociation(9); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("association close waited for the upstream to answer")
	}
	shared.send(ctx, packet)
	if mailbox.originSent.Load() != 0 || mailbox.rejected.Load() != 1 {
		t.Fatal("a stale packet reopened the closed association")
	}
}

func startPreviewUpstreamTestPeers(t *testing.T, ctx context.Context, proxyURL string, target netip.AddrPort) string {
	t.Helper()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "proxy-test-client", RunID: "proxy-test-run"},
		Drive:  DriveConfig{FolderID: "isolated-drive-folder"},
		Tunnel: TunnelConfig{
			Transport: "muxv5", Listen: freeTCPAddr(t), PollIntervalMS: 10, CleanupProcessed: true,
			UDPPreviewTargets: []string{target.String()}, UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true,
		},
	}
	cfg.ApplyDefaults()
	store := NewMemoryStore()
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	makeTunnel := func(url string) *Tunnel {
		t.Helper()
		cfg.Tunnel.ExitProxy = url
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
		drive := NewDriveStore(httpClient, "owned-test-token", cfg.Drive)
		peer, err := NewTunnel(drive, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer.Data, peer.Logger, peer.udpPreview.http = store, nil, httpClient
		return peer
	}
	client, exit := makeTunnel(""), makeTunnel(proxyURL)
	go func() { _ = exit.ServeExit(ctx) }()
	go func() { _ = client.ServeClient(ctx, cfg.Tunnel.Listen) }()
	waitForProbeListener(t, cfg.Tunnel.Listen)
	return cfg.Tunnel.Listen
}

type testPreviewUpstream struct {
	tcpTarget                         string
	udpTarget                         netip.AddrPort
	connects, associates, udpForwards atomic.Uint64
	refuseTCP, refuseUDP              atomic.Bool
	dropNewUDP                        atomic.Bool
}

func (p *testPreviewUpstream) start(t *testing.T, ctx context.Context) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	errorsSeen := make(chan error, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				if err := p.serve(ctx, conn); err != nil && ctx.Err() == nil {
					errorsSeen <- err
				}
			}()
		}
	}()
	address := url.URL{Scheme: "socks5h", Host: listener.Addr().String(), User: url.UserPassword("u:r", "p@s")}
	return address.String(), errorsSeen
}

func (p *testPreviewUpstream) serve(ctx context.Context, control net.Conn) error {
	if err := expectSOCKSUDPProxyBytes(control, []byte{5, 1, 2}); err != nil {
		return err
	}
	if err := writeAll(control, []byte{5, 2}); err != nil {
		return err
	}
	if err := expectSOCKSUDPProxyBytes(control, []byte{1, 3, 'u', ':', 'r', 3, 'p', '@', 's'}); err != nil {
		return err
	}
	if err := writeAll(control, []byte{1, 0}); err != nil {
		return err
	}
	var request [10]byte
	if _, err := io.ReadFull(control, request[:]); err != nil {
		return err
	}
	if request[0] != 5 || request[2] != 0 || request[3] != 1 {
		return errors.New("unexpected owned proxy request")
	}
	switch request[1] {
	case 1:
		p.connects.Add(1)
		host, port, err := parseSOCKSAddress(request[3:])
		if err != nil || fmt.Sprintf("%s:%d", host, port) != p.tcpTarget {
			return errors.New("unexpected owned TCP target")
		}
		if p.refuseTCP.Load() {
			return socksReplyUDP(control, 2, net.IPv4zero, 0)
		}
		dialer := net.Dialer{}
		origin, err := dialer.DialContext(ctx, "tcp4", p.tcpTarget)
		if err != nil {
			return err
		}
		defer origin.Close()
		if err := socksReplyUDP(control, 0, net.IPv4zero, 0); err != nil {
			return err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.Copy(origin, control)
			_ = origin.(*net.TCPConn).CloseWrite()
		}()
		_, _ = io.Copy(control, origin)
		_ = control.Close()
		<-done
		return nil
	case 3:
		p.associates.Add(1)
		if !bytes.Equal(request[4:], make([]byte, 6)) {
			return errors.New("UDP ASSOCIATE did not request a wildcard client endpoint")
		}
		if p.refuseUDP.Load() {
			return socksReplyUDP(control, 7, net.IPv4zero, 0)
		}
		if p.dropNewUDP.Load() {
			return nil
		}
		return p.serveUDP(control)
	default:
		return errors.New("unexpected owned proxy command")
	}
}

func (p *testPreviewUpstream) serveUDP(control net.Conn) error {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer relay.Close()
	if err := socksReplyUDP(control, 0, net.IPv4zero, relay.LocalAddr().(*net.UDPAddr).Port); err != nil {
		return err
	}
	go func() {
		var marker [1]byte
		_, _ = control.Read(marker[:])
		_ = relay.Close()
	}()
	var client netip.AddrPort
	var buffer [2048]byte
	for {
		n, source, err := relay.ReadFromUDPAddrPort(buffer[:])
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if source == p.udpTarget && client.IsValid() {
			frame := buildSOCKSUDPDatagram(source.Addr().String(), int(source.Port()), buffer[:n])
			if _, err := relay.WriteToUDPAddrPort(frame, client); err != nil {
				return err
			}
			continue
		}
		packet, err := parseSOCKSUDPDatagram(buffer[:n])
		if err != nil || packet.Host != p.udpTarget.Addr().String() || packet.Port != int(p.udpTarget.Port()) ||
			client.IsValid() && client != source {
			return errors.New("unexpected owned UDP target or client")
		}
		client = source
		p.udpForwards.Add(1)
		if _, err := relay.WriteToUDPAddrPort(packet.Payload, p.udpTarget); err != nil {
			return err
		}
	}
}
