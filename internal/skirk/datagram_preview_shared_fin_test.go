package skirk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSharedUDPRemoteMuxFINClosesLocalAssociationsAndAllowsReopen(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	origin, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	go func() {
		var buf [2048]byte
		for {
			n, peer, err := origin.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				return
			}
			_, _ = origin.WriteToUDPAddrPort(buf[:n], peer)
		}
	}()
	target := origin.LocalAddr().(*net.UDPAddr).AddrPort()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "fin-test-client", RunID: "fin-test-run"},
		Drive:  DriveConfig{FolderID: "isolated-drive-folder"},
		Tunnel: TunnelConfig{
			Transport: "muxv5", Listen: freeTCPAddr(t), PollIntervalMS: 10, CleanupProcessed: true,
			UDPPreviewTargets: []string{target.String()}, UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true,
		},
	}
	cfg.ApplyDefaults()
	store := NewMemoryStore()
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	makePeer := func() *Tunnel {
		t.Helper()
		httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
		drive := NewDriveStore(httpClient, "owned-test-token", cfg.Drive)
		peer, err := NewTunnel(drive, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer.Data, peer.Logger, peer.udpPreview.http = store, nil, httpClient
		return peer
	}
	client, exit := makePeer(), makePeer()
	exit.setRole("exit")
	exitMux, err := newDriveMux(exit, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	exitMux.start(ctx)
	defer exitMux.closeAll()
	go func() { _ = client.ServeClient(ctx, cfg.Tunnel.Listen) }()
	waitForProbeListener(t, cfg.Tunnel.Listen)
	control, udp := openPreviewAssociation(t, cfg.Tunnel.Listen, socksCommandUDPAssociate)
	defer control.Close()
	defer udp.Close()
	hev, _ := openPreviewAssociation(t, cfg.Tunnel.Listen, socksCommandUDPInTCP)
	defer hev.Close()
	echo := func(control net.Conn, udp *net.UDPConn, tag string) {
		t.Helper()
		body := []byte(tag)
		writePreviewPacket(t, control, udp, target.Addr().String(), int(target.Port()), body)
		reply, err := readPreviewPacket(control, udp)
		if err != nil || !bytes.Equal(reply.Payload, body) {
			t.Fatalf("association %s did not echo: %v", tag, err)
		}
	}
	echo(control, udp, "rfc1928")
	echo(hev, nil, "hev")
	exitMux.streamsMu.Lock()
	var old *muxStream
	count := len(exitMux.streams)
	for _, stream := range exitMux.streams {
		old = stream
	}
	exitMux.streamsMu.Unlock()
	if count != 1 {
		t.Fatalf("the two associations opened %d mux carriers", count)
	}
	// Close the real exit endpoint. Its mux reader publishes FIN as it
	// would after a terminal origin socket failure or a lease cancellation.
	_ = old.conn.Close()
	deadline := time.Now().Add(7 * time.Second)
	for _, local := range []net.Conn{control, hev} {
		_ = local.SetReadDeadline(deadline)
		var b [1]byte
		if _, err := local.Read(b[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("remote FIN did not promptly close a local association: %v", err)
		}
	}
	fresh, freshUDP := openPreviewAssociation(t, cfg.Tunnel.Listen, socksCommandUDPAssociate)
	defer fresh.Close()
	defer freshUDP.Close()
	echo(fresh, freshUDP, "fresh generation")
	exitMux.streamsMu.Lock()
	var renewed bool
	for _, stream := range exitMux.streams {
		renewed = renewed || stream.id != old.id
	}
	exitMux.streamsMu.Unlock()
	if !renewed {
		t.Fatal("the reopened association did not acquire a fresh mux carrier")
	}
}
