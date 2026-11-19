package skirk

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestProbeExitThroughMux(t *testing.T) {
	store := NewMemoryStore()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "probe-client", RunID: "probe-run"},
		Tunnel: TunnelConfig{Listen: freeTCPAddr(t), ChunkSize: 256, PollIntervalMS: 10, CleanupProcessed: true},
	}
	cfg.ApplyDefaults()
	client, err := NewTunnel(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := NewTunnel(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exit.ServeExit(ctx) }()
	go func() { _ = client.ServeClient(ctx, cfg.Tunnel.Listen) }()
	waitForProbeListener(t, cfg.Tunnel.Listen)
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer probeCancel()
	duration, err := ProbeExit(probeCtx, cfg.Tunnel.Listen, secret)
	if err != nil {
		t.Fatalf("authenticated exit probe failed: %v", err)
	}
	if duration <= 0 || duration > 5*time.Second {
		t.Fatalf("unexpected sample duration %s", duration)
	}
	otherSecret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeExit(probeCtx, cfg.Tunnel.Listen, otherSecret); !errors.Is(err, ErrExitNotVerified) {
		t.Fatalf("wrong-secret proof result = %v, want invalid proof", err)
	}
}

func TestProbeExitTimesOutWithoutExit(t *testing.T) {
	store := NewMemoryStore()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "probe-client", RunID: "probe-run"},
		Tunnel: TunnelConfig{Listen: freeTCPAddr(t), ChunkSize: 256, PollIntervalMS: 10},
	}
	cfg.ApplyDefaults()
	client, err := NewTunnel(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.ServeClient(ctx, cfg.Tunnel.Listen) }()
	waitForProbeListener(t, cfg.Tunnel.Listen)
	probeCtx, probeCancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer probeCancel()
	if _, err := ProbeExit(probeCtx, cfg.Tunnel.Listen, secret); err == nil {
		t.Fatal("probe verified an absent exit")
	}
}

func TestProbeExitRejectsLocalSOCKSImpersonation(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	addr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := SOCKSServer{Listen: addr, Handler: func(_ context.Context, target string, conn net.Conn) {
		if target != exitProbeTarget {
			t.Errorf("target = %q", target)
		}
		fake := make([]byte, len(exitProbeResponseMagic)+32)
		copy(fake, exitProbeResponseMagic)
		_, _ = conn.Write(fake)
	}}
	go func() { _ = server.Serve(ctx) }()
	waitForProbeListener(t, addr)
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()
	if _, err := ProbeExit(probeCtx, addr, secret); err == nil {
		t.Fatal("fake local listener produced an authenticated exit proof")
	}
}

func TestProbeExitOldPeerWithoutProof(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	addr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := SOCKSServer{Listen: addr, Handler: func(_ context.Context, _ string, _ net.Conn) {
		// An older exit tries to dial the .invalid target and never sends a proof.
	}}
	go func() { _ = server.Serve(ctx) }()
	waitForProbeListener(t, addr)
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()
	if _, err := ProbeExit(probeCtx, addr, secret); err == nil {
		t.Fatal("old peer without proof was verified")
	}
}

func waitForProbeListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("SOCKS listener %s did not start", addr)
}
