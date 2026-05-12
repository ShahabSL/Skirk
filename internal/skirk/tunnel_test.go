package skirk

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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
	clientTunnel, err := NewTunnel(data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exitTunnel, err := NewTunnel(data, cfg)
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

func TestTunnelExitProxyWithMemoryStores(t *testing.T) {
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

	proxyTargets := make(chan string, 1)
	proxy := SOCKSServer{
		Listen: freeTCPAddr(t),
		Handler: func(ctx context.Context, target string, client net.Conn) {
			proxyTargets <- target
			remote, err := net.DialTimeout("tcp", target, 2*time.Second)
			if err != nil {
				_ = client.Close()
				return
			}
			defer remote.Close()
			defer client.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(remote, client)
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(client, remote)
				done <- struct{}{}
			}()
			select {
			case <-ctx.Done():
			case <-done:
			}
		},
	}

	data := NewMemoryStore()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			Listen:           freeTCPAddr(t),
			ExitProxy:        "socks5h://" + proxy.Listen,
			ChunkSize:        64,
			PollIntervalMS:   10,
			CleanupProcessed: true,
		},
	}
	cfg.ApplyDefaults()
	clientTunnel, err := NewTunnel(data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exitTunnel, err := NewTunnel(data, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = proxy.Serve(ctx) }()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnel.ServeClient(ctx, cfg.Tunnel.Listen) }()
	time.Sleep(75 * time.Millisecond)

	conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+cfg.Tunnel.Listen, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("proxy")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "proxy" {
		t.Fatalf("got %q", buf)
	}
	select {
	case target := <-proxyTargets:
		if target != echo.Addr().String() {
			t.Fatalf("proxy target = %q, want %q", target, echo.Addr().String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive target")
	}
}

func TestTunnelMultiplexesConcurrentSOCKSStreams(t *testing.T) {
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
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			Listen:           freeTCPAddr(t),
			ChunkSize:        4096,
			PollIntervalMS:   5,
			CleanupProcessed: true,
		},
	}
	cfg.ApplyDefaults()
	clientTunnel, err := NewTunnel(data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exitTunnel, err := NewTunnel(data, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnel.ServeClient(ctx, cfg.Tunnel.Listen) }()
	time.Sleep(75 * time.Millisecond)

	const streams = 24
	var wg sync.WaitGroup
	errCh := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+cfg.Tunnel.Listen, echo.Addr().String())
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			msg := []byte(fmt.Sprintf("stream-%02d", i))
			if _, err := conn.Write(msg); err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- err
				return
			}
			if string(buf) != string(msg) {
				errCh <- fmt.Errorf("stream %d got %q, want %q", i, buf, msg)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("concurrent streams timed out")
	}
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
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

func TestMuxBatchAndOpenPayloadRoundTrip(t *testing.T) {
	open := encodeMuxOpenPayload("example.com:443", []byte("hello"))
	target, initial, err := decodeMuxOpenPayload(open)
	if err != nil {
		t.Fatal(err)
	}
	if target != "example.com:443" || string(initial) != "hello" {
		t.Fatalf("open payload target=%q initial=%q", target, initial)
	}

	raw, err := encodeMuxBatch([]muxFrame{
		{Kind: muxFrameOpen, StreamID: 7, Payload: open},
		{Kind: muxFrameData, StreamID: 7, Seq: 1, Payload: []byte("payload")},
		{Kind: muxFrameFIN, StreamID: 7, Seq: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMuxBatch(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].Kind != muxFrameOpen || frames[1].Kind != muxFrameData || frames[1].Seq != 1 || string(frames[1].Payload) != "payload" {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestMuxObjectNameIncludesEpoch(t *testing.T) {
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	name := muxObjectName(sid, DirectionDown, "cafebabedeadbeef", 3, 9, 2, 1234)
	if !strings.Contains(name, "/cafebabedeadbeef/l03/") {
		t.Fatalf("name = %q, want epoch segment", name)
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name, ID: "file-id"})
	if !ok {
		t.Fatalf("parse failed for %q", name)
	}
	if meta.ID != "file-id" || meta.Lane != 3 || meta.Seq != 9 {
		t.Fatalf("meta = %+v, want lane=3 seq=9 id=file-id", meta)
	}
}

func TestMuxStreamReordersStripedFrames(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[uint64]*muxStream{},
		pending: map[uint64][]muxFrame{},
	}
	stream := mux.registerStream(42, left)
	mux.startWriter(stream)
	defer stream.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream.acceptFrame(ctx, muxFrame{Kind: muxFrameData, StreamID: 42, Seq: 2, Payload: []byte("b")})
	stream.acceptFrame(ctx, muxFrame{Kind: muxFrameData, StreamID: 42, Seq: 1, Payload: []byte("a")})

	if err := right.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(right, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ab" {
		t.Fatalf("got %q, want ab", buf)
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
	tunnel, err := NewTunnel(NewMemoryStore(), cfg)
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

func TestCleanupForegroundBusyState(t *testing.T) {
	tunnel := &Tunnel{}
	if tunnel.foregroundBusy() {
		t.Fatal("new tunnel should not be foreground busy")
	}
	tunnel.activeStreams.Add(1)
	if !tunnel.foregroundBusy() {
		t.Fatal("active stream should make cleanup wait")
	}
	tunnel.activeStreams.Add(-1)
	tunnel.markActivity()
	if !tunnel.foregroundBusy() {
		t.Fatal("recent activity should make cleanup wait")
	}
	atomic.StoreInt64(&tunnel.lastActivityNS, time.Now().Add(-cleanupQuietWindow-time.Second).UnixNano())
	if tunnel.foregroundBusy() {
		t.Fatal("old activity should allow cleanup")
	}
}

func TestBurstPollState(t *testing.T) {
	tunnel := &Tunnel{
		BurstPoll:         true,
		BurstPollInterval: 75 * time.Millisecond,
		BurstPollWindow:   5 * time.Second,
	}
	now := time.Now()
	if tunnel.burstPollActive(now) {
		t.Fatal("burst should be inactive before uploads")
	}
	tunnel.activeStreams.Add(1)
	atomic.StoreInt64(&tunnel.lastUploadNS, now.Add(-time.Second).UnixNano())
	atomic.StoreInt64(&tunnel.lastActivityNS, now.UnixNano())
	if !tunnel.burstPollActive(now) {
		t.Fatal("recent upload with active stream should enable burst")
	}
	atomic.StoreInt64(&tunnel.burstDisabledUntilNS, now.Add(time.Minute).UnixNano())
	if tunnel.burstPollActive(now) {
		t.Fatal("cooldown should disable burst")
	}
	atomic.StoreInt64(&tunnel.burstDisabledUntilNS, 0)
	atomic.StoreInt64(&tunnel.lastUploadNS, now.Add(-10*time.Second).UnixNano())
	if tunnel.burstPollActive(now) {
		t.Fatal("old upload should not enable burst")
	}
}

func TestMarkSlowListDisablesBurstTemporarily(t *testing.T) {
	tunnel := &Tunnel{BurstPoll: true}
	tunnel.markSlowList(burstSlowListThreshold + time.Millisecond)
	disabledUntil := atomic.LoadInt64(&tunnel.burstDisabledUntilNS)
	if disabledUntil == 0 || !time.Unix(0, disabledUntil).After(time.Now()) {
		t.Fatal("slow list should set a future cooldown")
	}
}

func TestWakeReceiverIsNonBlocking(t *testing.T) {
	mux := &driveMux{recvWake: make(chan struct{}, 1)}
	mux.wakeReceiver()
	mux.wakeReceiver()
	select {
	case <-mux.recvWake:
	default:
		t.Fatal("wakeReceiver did not queue wake signal")
	}
	select {
	case <-mux.recvWake:
		t.Fatal("wakeReceiver should coalesce duplicate wake signals")
	default:
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
