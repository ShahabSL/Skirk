package skirk

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTunnelSOCKSToExitWithMemoryStores(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	data := NewMemoryStore()
	control := NewMemoryStore()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			Listen:           freeTCPAddr(t),
			ChunkSize:        64,
			PollIntervalMS:   10,
			CleanupProcessed: true,
		},
	}
	cfg.ApplyDefaults()
	clientTunnel, err := NewTunnel(data, control, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exitTunnel, err := NewTunnel(data, control, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnel.ServeClient(ctx, cfg.Tunnel.Listen) }()
	time.Sleep(75 * time.Millisecond)

	conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+cfg.Tunnel.Listen, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", buf)
	}
}

func TestControlConnIDParsesStreamPrefix(t *testing.T) {
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	prefix := streamControlDirPrefix(sid, DirectionDown)
	name := streamBatchControlName(sid, DirectionDown, "abc123", 1, 8)
	if got := controlConnID(prefix, name); got != "abc123" {
		t.Fatalf("controlConnID() = %q, want abc123", got)
	}
	if got := controlConnID(prefix, "control/other/down/abc123/0000000000000001.DATA"); got != "" {
		t.Fatalf("controlConnID() for wrong prefix = %q, want empty", got)
	}
}

func TestOpenControlEncryptsTargetInFilename(t *testing.T) {
	data := NewMemoryStore()
	control := NewMemoryStore()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			PollIntervalMS: 10,
		},
	}
	cfg.ApplyDefaults()
	tunnel, err := NewTunnel(data, control, cfg)
	if err != nil {
		t.Fatal(err)
	}
	target := "secret-target.example:443"
	if err := tunnel.sendEvent(context.Background(), DirectionUp, "abc123", 0, "OPEN", "", target, 0, false, ""); err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	var name string
	for key := range control.objects {
		name = key
	}
	control.mu.Unlock()
	if name == "" {
		t.Fatal("expected one OPEN control object")
	}
	if !strings.Contains(name, ".OPENI.") {
		t.Fatalf("open control name = %q, want OPENI", name)
	}
	if strings.Contains(name, target) || strings.Contains(name, base64.RawURLEncoding.EncodeToString([]byte(target))) {
		t.Fatalf("open control name leaks target: %s", name)
	}
	event, ok := tunnel.parseOpenControlInfo(name)
	if !ok {
		t.Fatalf("failed to parse encrypted OPENI control: %s", name)
	}
	if event.Target != target || event.ConnID != "abc123" || event.Sequence != 0 {
		t.Fatalf("parsed event = %+v", event)
	}
	legacy := streamControlPrefix(tunnel.SessionID, DirectionUp, "abc123") + "0000000000000000.OPEN." + base64.RawURLEncoding.EncodeToString([]byte(target))
	if _, ok := tunnel.parseOpenControlInfo(legacy); ok {
		t.Fatal("legacy plaintext OPEN controls should not be accepted")
	}
}

func TestAdaptiveLimiterBacksOffOnSlowSuccess(t *testing.T) {
	limiter := newAdaptiveLimiter(4, 8, 100*time.Millisecond, "test", nil)
	limiter.inFlight = 1
	limiter.release(nil, 150*time.Millisecond)
	if limiter.limit != 3 {
		t.Fatalf("slow success limit = %d, want 3", limiter.limit)
	}
	limiter.inFlight = 1
	limiter.release(nil, 250*time.Millisecond)
	if limiter.limit != 2 {
		t.Fatalf("second slow success limit = %d, want 2", limiter.limit)
	}
}

func TestStreamDownloadWindowCapsPerConnectionReadAhead(t *testing.T) {
	cfg := &Config{
		Secret:    strings.Repeat("a", 64),
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			DownloadConcurrency: 32,
		},
	}
	cfg.ApplyDefaults()
	tunnel, err := NewTunnel(NewMemoryStore(), NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := tunnel.streamDownloadWindow(); got != 16 {
		t.Fatalf("direct stream window = %d, want 16", got)
	}
	tunnel.RouteProxy = "socks5h://127.0.0.1:11093"
	if got := tunnel.streamDownloadWindow(); got != 8 {
		t.Fatalf("proxy stream window = %d, want 8", got)
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
