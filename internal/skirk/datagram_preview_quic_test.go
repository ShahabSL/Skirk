package skirk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// Expose only PacketConn methods to QUIC. Some UDP optimizations would use
// recvmsg/sendmsg directly and skip the RFC 1928 association envelope.
type quicSOCKSTestConn struct {
	net.PacketConn
	relay  *net.UDPConn
	origin *net.UDPAddr
}

func (c quicSOCKSTestConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	var framed [2048]byte
	n, err := c.relay.Read(framed[:])
	if err != nil {
		return 0, nil, err
	}
	packet, err := parseSOCKSUDPDatagram(framed[:n])
	if err != nil || packet.Host != c.origin.IP.String() || packet.Port != c.origin.Port || len(packet.Payload) > len(buf) {
		return 0, nil, errors.New("unexpected SOCKS datagram in QUIC fixture")
	}
	return copy(buf, packet.Payload), c.origin, nil
}

func (c quicSOCKSTestConn) WriteTo(buf []byte, remote net.Addr) (int, error) {
	if remote.String() != c.origin.String() {
		return 0, errors.New("QUIC fixture attempted an unexpected origin")
	}
	if _, err := c.relay.Write(buildSOCKSUDPDatagram(c.origin.IP.String(), c.origin.Port, buf)); err != nil {
		return 0, err
	}
	return len(buf), nil
}

func TestUDPPreviewCarriesAuthenticatedQUICStream(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 32*time.Second)
	defer cancel()
	const alpn = "skirk-owned-quic-test"
	tlsFixture := httptest.NewTLSServer(nil)
	certs := tlsFixture.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(tlsFixture.Certificate())
	tlsFixture.Close()
	config := &quic.Config{HandshakeIdleTimeout: 12 * time.Second, MaxIdleTimeout: 20 * time.Second, DisablePathMTUDiscovery: true}
	origin, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: certs, NextProtos: []string{alpn}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	route := origin.Addr().(*net.UDPAddr)
	request := bytes.Repeat([]byte("verified QUIC request\n"), 384)
	response := bytes.Repeat([]byte("verified QUIC response\n"), 384)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- serveQUICTestStream(ctx, origin, request, response)
	}()
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	_, exit, socks := startUDPPreviewTestPeers(t, ctx, "matching shared peers", route.AddrPort(), NewMemoryStore(), metadata)
	control, relay := openPreviewAssociation(t, socks, socksCommandUDPAssociate)
	defer control.Close()
	defer relay.Close()
	if err := relay.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	conn, err := quic.Dial(ctx, quicSOCKSTestConn{PacketConn: relay, relay: relay, origin: route}, route,
		&tls.Config{RootCAs: roots, ServerName: "127.0.0.1", NextProtos: []string{alpn}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "fixture complete")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(17 * time.Second))
	if err := writeAll(stream, request); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(stream, int64(len(response)+1)))
	if err != nil || !bytes.Equal(got, response) {
		t.Fatalf("QUIC response changed or stopped after %d bytes: %v", len(got), err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if counters := exit.UDPPreviewSnapshot().Packets; counters.OriginSent == 0 || counters.OriginReceived == 0 {
		t.Fatal("QUIC transfer did not traverse the exit's UDP path")
	}
}

func serveQUICTestStream(ctx context.Context, listener *quic.Listener, request, response []byte) error {
	conn, err := listener.Accept(ctx)
	if err != nil {
		return err
	}
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return err
	}
	_ = stream.SetDeadline(time.Now().Add(17 * time.Second))
	got, err := io.ReadAll(io.LimitReader(stream, int64(len(request)+1)))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, request) {
		return errors.New("QUIC origin received different application bytes")
	}
	if err := writeAll(stream, response); err != nil {
		return err
	}
	return stream.Close()
}
