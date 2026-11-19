package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestSetupProxyUDPProbeDistinguishesRefusalAndUncertainty(t *testing.T) {
	t.Run("explicit SOCKS5 UDP refusal", func(t *testing.T) {
		addr, done := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
			return mockSOCKSUDPProxyHandshake(conn, false, []byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		ok, err := probeExitProxyUDP(ctx, "socks5h://"+addr, netip.MustParseAddrPort("192.0.2.2:53"))
		if err != nil || ok {
			t.Fatalf("explicit UDP refusal returned (%t, %v)", ok, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	for _, reply := range []bool{true, false} {
		name := "valid DNS response"
		if !reply {
			name = "relay accepts UDP but does not reply"
		}
		t.Run(name, func(t *testing.T) {
			relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			endpoint := netip.MustParseAddrPort("192.0.2.2:53")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			addr, done := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
				if err := mockSOCKSUDPProxyHandshake(conn, false, append([]byte{5, 0, 0}, buildSOCKSAddress("127.0.0.1", relay.LocalAddr().(*net.UDPAddr).Port)...)); err != nil {
					return err
				}
				var extra [1]byte
				_, err := conn.Read(extra[:])
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			})
			dataDone := make(chan error, 1)
			go func() {
				buf := make([]byte, 2048)
				_ = relay.SetReadDeadline(time.Now().Add(time.Second))
				n, peer, err := relay.ReadFromUDPAddrPort(buf)
				if err != nil {
					dataDone <- err
					return
				}
				packet, err := parseSOCKSUDPDatagram(buf[:n])
				var message dnsmessage.Message
				if err != nil || packet.Host != endpoint.Addr().String() || packet.Port != int(endpoint.Port()) || message.Unpack(packet.Payload) != nil || message.Response {
					dataDone <- errors.New("expected a genuine IPv4 UDP DNS request relayed through SOCKS5")
					return
				}
				if !reply {
					// Cancel only once the proxy has accepted and received the UDP
					// query, which avoids a timing-dependent handshake failure.
					cancel()
					dataDone <- nil
					return
				}
				message.Response = true
				wire, err := message.Pack()
				if err == nil {
					_, err = relay.WriteToUDPAddrPort(buildSOCKSUDPDatagram(packet.Host, packet.Port, wire), peer)
				}
				dataDone <- err
			}()
			probe, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			ok, err := probeExitProxyUDP(probe, "socks5h://"+addr, endpoint)
			if reply && (!ok || err != nil) {
				t.Fatalf("verified relay returned (%t, %v)", ok, err)
			}
			if !reply && (ok || !errors.Is(err, context.Canceled)) {
				t.Fatalf("silent relay should leave UDP uncertain, got (%t, %v)", ok, err)
			}
			if err := <-dataDone; err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupIPv6RequiresRepliesOverTCPAndUDP(t *testing.T) {
	tcp, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer tcp.Close()
	endpoint := tcp.Addr().(*net.TCPAddr).AddrPort()
	udp, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	reply := func(query []byte) []byte {
		var m dnsmessage.Message
		if m.Unpack(query) != nil {
			return nil
		}
		m.Response = true
		wire, _ := m.Pack()
		return wire
	}
	go func() {
		for range 2 {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if query, err := readPreviewDNSFrame(conn, 4096); err == nil {
				_ = writePreviewDNSFrame(conn, reply(query))
			}
			_ = conn.Close()
		}
	}()
	go func() {
		var wire [4096]byte
		if n, addr, err := udp.ReadFromUDPAddrPort(wire[:]); err == nil {
			_, _ = udp.WriteToUDPAddrPort(reply(wire[:n]), addr)
		}
	}()
	defer udp.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if !probeExitIPv6DNS(ctx, "", endpoint) {
		t.Fatal("healthy dual-transport IPv6 was not enabled")
	}
	_ = udp.Close()
	if probeExitIPv6DNS(ctx, "", endpoint) {
		t.Fatal("a TCP reply alone enabled IPv6")
	}
	if probeExitIPv6DNS(ctx, "", netip.MustParseAddrPort("127.0.0.1:53")) {
		t.Fatal("IPv4 endpoint qualified for IPv6")
	}
}
