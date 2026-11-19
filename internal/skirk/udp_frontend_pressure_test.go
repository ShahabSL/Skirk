package skirk

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestUDPPreviewHEVMediaStartsWithIdleAndDNSFrontendPressure(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	origin, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = origin.Close() })
	go func() {
		var buf [2048]byte
		for {
			n, peer, err := origin.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				return
			}
			if _, err := origin.WriteToUDPAddrPort(buf[:n], peer); err != nil {
				return
			}
		}
	}()

	target := origin.LocalAddr().(*net.UDPAddr).AddrPort()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "frontend-pressure", RunID: "fixture-run"},
		Drive:  DriveConfig{FolderID: "isolated-drive-folder"},
		Tunnel: TunnelConfig{
			Transport: "muxv5", Listen: freeTCPAddr(t), PollIntervalMS: 10, CleanupProcessed: true,
			UDPPreviewTargets: []string{target.String()}, UDPPreviewPublicIPv4: true,
			UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true, UDPPreviewSharedAssociations: 32,
		},
	}
	cfg.ApplyDefaults()
	store := NewMemoryStore()
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
	newPeer := func() *Tunnel {
		t.Helper()
		peer, err := NewTunnel(NewDriveStore(httpClient, "owned-test-token", cfg.Drive), &cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer.Data, peer.Logger, peer.udpPreview.http = store, nil, httpClient
		return peer
	}
	client, exit := newPeer(), newPeer()
	clientDone, exitDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exitDone)
		_ = exit.ServeExit(ctx)
	}()
	go func() {
		defer close(clientDone)
		_ = client.ServeClient(ctx, cfg.Tunnel.Listen)
	}()
	t.Cleanup(func() { cancel(); <-clientDone; <-exitDone })
	waitForProbeListener(t, cfg.Tunnel.Listen)

	openHEV := func() net.Conn {
		t.Helper()
		control, _ := openPreviewAssociation(t, cfg.Tunnel.Listen, socksCommandUDPInTCP)
		t.Cleanup(func() { _ = control.Close() })
		return control
	}
	const background = 40
	for i := range background {
		control := openHEV()
		if i%2 != 0 {
			continue
		}
		query := dnsQuestion("fixture.invalid", 28) // Local AAAA NODATA, with no external DNS.
		writePreviewPacket(t, control, nil, "198.18.0.2", 53, query)
		reply, err := readPreviewPacket(control, nil)
		if err != nil || reply.Port != 53 || len(reply.Payload) < 12 || !bytes.Equal(reply.Payload[:2], query[:2]) {
			t.Fatalf("local DNS reply on frontend %d failed: %v", i, err)
		}
	}
	if metrics := client.UDPPreviewSnapshot(); metrics.ActiveFrontends != background || metrics.ActiveCarriers != 0 || metadata.calls() != 0 {
		t.Fatalf("idle and DNS frontends used a Drive carrier or did not stay open: %+v", metrics)
	}

	media := openHEV()
	for _, size := range []int{160, 1200} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		writePreviewPacket(t, media, nil, pilotHEVLoopbackAlias, int(target.Port()), payload)
		if reply, err := readPreviewPacket(media, nil); err != nil || !bytes.Equal(reply.Payload, payload) {
			t.Fatalf("%d-byte UDP round trip under frontend pressure failed: %v", size, err)
		}
	}
	if metrics := client.UDPPreviewSnapshot(); metrics.ActiveFrontends != background+1 || metrics.ActiveCarriers != 1 || metrics.AdmissionLimitHits != 0 {
		t.Fatalf("media should use one carrier while every local frontend remains open: %+v", metrics)
	}
	if metrics := exit.UDPPreviewSnapshot(); metrics.ActiveCarriers != 1 || metrics.Packets.OriginSent != 2 || metrics.Packets.OriginReceived != 2 || metrics.Packets.FlowLimitHits != 0 {
		t.Fatalf("unexpected origin traffic after opening media: %+v", metrics)
	}
}
