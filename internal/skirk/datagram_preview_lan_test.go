package skirk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestUDPPreviewLANRelayBind(t *testing.T) {
	for _, tt := range []struct{ peer, listener, bind string }{
		{"10.0.0.21", "10.0.0.20:1080", "10.0.0.20"},
		{"::ffff:172.18.1.21", "[::ffff:172.18.1.20]:1080", "172.18.1.20"},
		{"fd00::21", "[fd00::20]:1080", "fd00::20"},
		{"192.168.1.21", "0.0.0.0:1080", ""},
		{"8.8.4.4", "192.168.1.20:1080", ""},
		{"192.168.1.21", "8.8.4.4:1080", ""},
		{"fd00::21", "192.168.1.20:1080", ""},
	} {
		peer := netip.MustParseAddr(tt.peer)
		listener := net.TCPAddrFromAddrPort(netip.MustParseAddrPort(tt.listener))
		want, _ := netip.ParseAddr(tt.bind)
		if bind := pilotUDPRelayBind(peer, listener); bind != want {
			t.Errorf("relay bind for %s via %s = %s, want %s", tt.peer, tt.listener, bind, want)
		}
	}
}

func TestUDPPreviewPrivateListenerServesOnlyControlPeersUDPPort(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var local netip.Addr
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && p.Addr().Is4() && p.Addr().IsPrivate() {
			local = p.Addr()
			break
		}
	}
	if !local.IsValid() {
		t.Skip("fixture requires one private IPv4 interface")
	}
	listener, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	newUDPSocket := func() *net.UDPConn {
		t.Helper()
		conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	client, otherPort := newUDPSocket(), newUDPSocket()
	port := client.LocalAddr().(*net.UDPAddr).Port
	control, err := net.DialTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(local, 0)), listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	server, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); _ = server.Close(); <-done }()
	go func() {
		defer close(done)
		preview := &datagramPreview{sharedUDP: true}
		preview.serveClient(ctx, &driveMux{}, socksRequest{
			Command: socksCommandUDPAssociate, Target: net.JoinHostPort(local.String(), strconv.Itoa(port)),
		}, server, nil)
	}()
	_ = control.SetDeadline(time.Now().Add(2 * time.Second))
	var reply [10]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil || !bytes.Equal(reply[:4], []byte{5, 0, 0, 1}) {
		t.Fatalf("private listener refused a standard UDP association: %v", err)
	}
	relayHost, relayPort, err := parseSOCKSAddress(reply[3:])
	if err != nil || relayHost != local.String() || relayPort == 0 {
		t.Fatal("relay did not advertise its concrete control interface")
	}
	relay := netip.AddrPortFrom(local, uint16(relayPort))
	query := dnsQuestion("fixture.invalid", 28)
	payload := buildSOCKSUDPDatagram("198.18.0.2", 53, query)
	if _, err := otherPort.WriteToUDPAddrPort(payload, relay); err != nil {
		t.Fatal(err)
	}
	if _, err := client.WriteToUDPAddrPort(payload, relay); err != nil {
		t.Fatal(err)
	}
	var buf [512]byte
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := client.ReadFromUDPAddrPort(buf[:])
	if err != nil || from != relay {
		t.Fatalf("authorized client did not get a UDP reply: %v", err)
	}
	packet, err := parseSOCKSUDPDatagram(buf[:n])
	if err != nil || packet.Port != 53 || len(packet.Payload) < 12 || !bytes.Equal(packet.Payload[:2], query[:2]) {
		t.Fatal("authorized client did not get the expected DNS reply")
	}
	_ = otherPort.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := otherPort.ReadFromUDPAddrPort(buf[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("a different UDP source port could reach the relay")
	}
}
