package skirk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
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
		Client:    ClientConfig{ID: "client-a", RunID: "run-a"},
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
	proxyTargets := make(chan string, 1)
	proxy := SOCKSServer{
		Listen: freeTCPAddr(t),
		Handler: func(ctx context.Context, target string, client net.Conn) {
			proxyTargets <- target
			defer client.Close()
			done := make(chan struct{}, 1)
			go func() {
				_, _ = io.Copy(client, client)
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
		Client:    ClientConfig{ID: "client-a", RunID: "run-a"},
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

	targetAddr := "198.51.100.10:443"
	conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+cfg.Tunnel.Listen, targetAddr)
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
		if target != targetAddr {
			t.Fatalf("proxy target = %q, want %q", target, targetAddr)
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
		Client:    ClientConfig{ID: "client-a", RunID: "run-a"},
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

func TestTunnelHTTPProxyMediaBurstUnderBulk(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bulk":
			writeTestHTTPBody(w, 4*1024*1024, 16*1024, time.Millisecond)
		case "/media":
			writeTestHTTPRange(w, 256*1024, 0)
		case "/abort":
			writeTestHTTPRange(w, 256*1024, 250*time.Millisecond)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	exitProxy := SOCKSServer{
		Listen: freeTCPAddr(t),
		Handler: func(ctx context.Context, _ string, client net.Conn) {
			upstream, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", originURL.Host)
			if err != nil {
				_ = client.Close()
				return
			}
			copyConnPair(ctx, client, upstream)
		},
	}

	data := &delayedBlobStore{
		inner:       NewMemoryStore(),
		putDelay:    20 * time.Millisecond,
		getDelay:    8 * time.Millisecond,
		listDelay:   12 * time.Millisecond,
		deleteDelay: time.Millisecond,
	}
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Client:    ClientConfig{ID: "client-a", RunID: "run-a"},
		Tunnel: TunnelConfig{
			Listen:              freeTCPAddr(t),
			ExitProxy:           "socks5h://" + exitProxy.Listen,
			ChunkSize:           256 * 1024,
			PollIntervalMS:      2,
			UploadConcurrency:   16,
			DownloadConcurrency: 16,
			CleanupProcessed:    true,
			Observe:             true,
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
	var logs lockedBuffer
	logger := log.New(&logs, "", 0)
	clientTunnel.Logger = logger
	exitTunnel.Logger = logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitProxy.Serve(ctx) }()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	httpProxyListen := freeTCPAddr(t)
	go func() { _ = clientTunnel.ServeHTTPProxyClient(ctx, httpProxyListen) }()
	time.Sleep(100 * time.Millisecond)

	proxyURL, err := url.Parse("http://" + httpProxyListen)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
		Timeout: 10 * time.Second,
	}

	bulkDone := make(chan httpHammerResult, 1)
	go func() {
		bulkDone <- doHTTPHammerRequest(context.Background(), client, "bulk", "http://bulk.example/bulk", "")
	}()
	time.Sleep(25 * time.Millisecond)

	const fullMedia = 24
	const abortishMedia = 16
	results := make(chan httpHammerResult, fullMedia+abortishMedia)
	var wg sync.WaitGroup
	for i := 0; i < fullMedia; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- doHTTPHammerRequest(context.Background(), client, "media", fmt.Sprintf("http://media.example/media?seg=%d", i), "bytes=0-262143")
		}(i)
	}
	for i := 0; i < abortishMedia; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reqCtx, reqCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer reqCancel()
			results <- doHTTPHammerRequest(reqCtx, client, "abort", fmt.Sprintf("http://media.example/abort?seg=%d", i), "bytes=0-262143")
		}(i)
	}
	wg.Wait()
	close(results)

	var mediaDurations []time.Duration
	abortErrors := 0
	for res := range results {
		switch res.kind {
		case "media":
			if res.err != nil {
				t.Fatalf("media request failed after %s: %v", res.duration, res.err)
			}
			if res.status != http.StatusPartialContent || res.bytes != 256*1024 {
				t.Fatalf("media response status=%d bytes=%d, want 206 and 262144", res.status, res.bytes)
			}
			mediaDurations = append(mediaDurations, res.duration)
		case "abort":
			if res.err != nil {
				abortErrors++
			}
		}
	}
	if abortErrors < abortishMedia/2 {
		t.Fatalf("abort-shaped requests canceled=%d, want at least %d", abortErrors, abortishMedia/2)
	}
	if p95 := durationPercentile(mediaDurations, 95); p95 > 5*time.Second {
		t.Fatalf("media burst p95=%s, want <=5s", p95)
	}

	select {
	case bulk := <-bulkDone:
		if bulk.err != nil {
			t.Fatalf("bulk request failed after %s: %v", bulk.duration, bulk.err)
		}
		if bulk.status != http.StatusOK || bulk.bytes != 4*1024*1024 {
			t.Fatalf("bulk response status=%d bytes=%d, want 200 and 4194304", bulk.status, bulk.bytes)
		}
		if bulk.duration > 10*time.Second {
			t.Fatalf("bulk duration=%s, want <=10s", bulk.duration)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("bulk request timed out under media burst")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
	logText := logs.String()
	for _, bad := range []string{"urgent_queue_full", "retry budget exhausted", "mux process terminal failure", "mux upload terminal failure"} {
		if strings.Contains(logText, bad) {
			t.Fatalf("unexpected mux failure marker %q in logs:\n%s", bad, logText)
		}
	}
}

type httpHammerResult struct {
	kind     string
	status   int
	bytes    int64
	duration time.Duration
	err      error
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func doHTTPHammerRequest(ctx context.Context, client *http.Client, kind, rawURL, byteRange string) httpHammerResult {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return httpHammerResult{kind: kind, duration: time.Since(started), err: err}
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := client.Do(req)
	if err != nil {
		return httpHammerResult{kind: kind, duration: time.Since(started), err: err}
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return httpHammerResult{kind: kind, status: resp.StatusCode, bytes: n, duration: time.Since(started), err: err}
}

func writeTestHTTPRange(w http.ResponseWriter, size int, firstByteDelay time.Duration) {
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", size-1, size))
	w.WriteHeader(http.StatusPartialContent)
	if firstByteDelay > 0 {
		time.Sleep(firstByteDelay)
	}
	writePattern(w, size, 16*1024, 0)
}

func writeTestHTTPBody(w http.ResponseWriter, size, chunk int, delay time.Duration) {
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.WriteHeader(http.StatusOK)
	writePattern(w, size, chunk, delay)
}

func writePattern(w io.Writer, size, chunk int, delay time.Duration) {
	if chunk <= 0 {
		chunk = size
	}
	buf := bytes.Repeat([]byte("s"), chunk)
	remaining := size
	for remaining > 0 {
		n := chunk
		if n > remaining {
			n = remaining
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		remaining -= n
		if delay > 0 && remaining > 0 {
			time.Sleep(delay)
		}
	}
}

func copyConnPair(ctx context.Context, a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

type delayedBlobStore struct {
	inner       BlobStore
	putDelay    time.Duration
	getDelay    time.Duration
	listDelay   time.Duration
	deleteDelay time.Duration
}

func (s *delayedBlobStore) Put(ctx context.Context, name string, data []byte) error {
	if err := sleepContext(ctx, s.putDelay); err != nil {
		return err
	}
	return s.inner.Put(ctx, name, data)
}

func (s *delayedBlobStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if err := sleepContext(ctx, s.putDelay); err != nil {
		return ObjectInfo{}, err
	}
	if store, ok := s.inner.(ObjectPutStore); ok {
		return store.PutObject(ctx, name, data)
	}
	if err := s.inner.Put(ctx, name, data); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: name, Size: int64(len(data))}, nil
}

func (s *delayedBlobStore) Get(ctx context.Context, name string) ([]byte, error) {
	if err := sleepContext(ctx, s.getDelay); err != nil {
		return nil, err
	}
	return s.inner.Get(ctx, name)
}

func (s *delayedBlobStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := sleepContext(ctx, s.listDelay); err != nil {
		return nil, err
	}
	return s.inner.List(ctx, prefix)
}

func (s *delayedBlobStore) Delete(ctx context.Context, name string) error {
	if err := sleepContext(ctx, s.deleteDelay); err != nil {
		return err
	}
	return s.inner.Delete(ctx, name)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func durationPercentile(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if percentile <= 0 {
		return sorted[0]
	}
	if percentile >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := (len(sorted)*percentile + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

func TestTunnelSupportsTwoClientNamespacesOnOneExit(t *testing.T) {
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
	base := &Config{
		Secret:    secret,
		SessionID: "00112233445566778899aabbccddeeff",
		Tunnel: TunnelConfig{
			ChunkSize:        4096,
			PollIntervalMS:   5,
			CleanupProcessed: true,
		},
	}
	base.ApplyDefaults()
	exitTunnel, err := NewTunnel(data, base)
	if err != nil {
		t.Fatal(err)
	}
	clientA := *base
	clientA.Client = ClientConfig{ID: "client-a", RunID: "run-a"}
	clientA.Tunnel.Listen = freeTCPAddr(t)
	clientB := *base
	clientB.Client = ClientConfig{ID: "client-b", RunID: "run-b"}
	clientB.Tunnel.Listen = freeTCPAddr(t)
	clientTunnelA, err := NewTunnel(data, &clientA)
	if err != nil {
		t.Fatal(err)
	}
	clientTunnelB, err := NewTunnel(data, &clientB)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnelA.ServeClient(ctx, clientA.Tunnel.Listen) }()
	go func() { _ = clientTunnelB.ServeClient(ctx, clientB.Tunnel.Listen) }()
	time.Sleep(75 * time.Millisecond)

	type clientCase struct {
		name string
		addr string
		msg  string
	}
	cases := []clientCase{
		{name: "a", addr: clientA.Tunnel.Listen, msg: "from-client-a"},
		{name: "b", addr: clientB.Tunnel.Listen, msg: "from-client-b"},
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(cases))
	for _, tc := range cases {
		tc := tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+tc.addr, echo.Addr().String())
			if err != nil {
				errCh <- fmt.Errorf("%s dial: %w", tc.name, err)
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write([]byte(tc.msg)); err != nil {
				errCh <- fmt.Errorf("%s write: %w", tc.name, err)
				return
			}
			buf := make([]byte, len(tc.msg))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- fmt.Errorf("%s read: %w", tc.name, err)
				return
			}
			if string(buf) != tc.msg {
				errCh <- fmt.Errorf("%s got %q, want %q", tc.name, buf, tc.msg)
			}
		}()
	}
	wg.Wait()
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
	limiter.release(false, nil, 150*time.Millisecond, 0)
	if limiter.limit != 3 {
		t.Fatalf("slow success limit = %d, want 3", limiter.limit)
	}
	limiter.inFlight = 1
	limiter.release(false, nil, 250*time.Millisecond, 0)
	if limiter.limit != 3 {
		t.Fatalf("same-epoch slow success limit = %d, want 3", limiter.limit)
	}
	limiter.backoffUntil = time.Time{}
	limiter.inFlight = 1
	limiter.release(false, nil, 250*time.Millisecond, 0)
	if limiter.limit != 3 {
		t.Fatalf("next-epoch slow success floor limit = %d, want 3", limiter.limit)
	}
}

func TestAdaptiveLimiterBacksOffOncePerCongestionEpoch(t *testing.T) {
	limiter := newAdaptiveLimiter(16, 32, time.Second, "test", nil)
	limiter.inFlight = 16
	limiter.release(false, nil, 3*time.Second, 0)
	if got, want := limiter.limit, 8; got != want {
		t.Fatalf("first severe slow limit = %d, want %d", got, want)
	}
	limiter.inFlight = 15
	limiter.release(false, nil, 3*time.Second, 0)
	if got, want := limiter.limit, 8; got != want {
		t.Fatalf("same congestion epoch limit = %d, want %d", got, want)
	}
	limiter.backoffUntil = time.Time{}
	limiter.inFlight = 14
	limiter.release(false, nil, 3*time.Second, 0)
	if got, want := limiter.limit, 6; got != want {
		t.Fatalf("next congestion epoch floor limit = %d, want %d", got, want)
	}
}

func TestAdaptiveLimiterUsesByteAwareSlowThresholdForBulk(t *testing.T) {
	limiter := newAdaptiveLimiter(4, 8, 5*time.Second, "test", nil)
	limiter.inFlight = 1
	limiter.release(false, nil, 4*time.Second, 4*1024*1024)
	if limiter.limit != 4 {
		t.Fatalf("bulk transfer at acceptable byte rate limit = %d, want 4", limiter.limit)
	}
	limiter.inFlight = 1
	limiter.release(false, nil, 6*time.Second, 4*1024*1024)
	if limiter.limit != 3 {
		t.Fatalf("slow bulk transfer limit = %d, want 3", limiter.limit)
	}
	limiter.backoffUntil = time.Time{}
	limiter.inFlight = 1
	limiter.release(false, nil, 20*time.Second, 4*1024*1024)
	if limiter.limit != 3 {
		t.Fatalf("very slow bulk transfer floor limit = %d, want 3", limiter.limit)
	}
}

func TestAdaptiveLimiterKeepsPriorityLatencyByteInsensitive(t *testing.T) {
	limiter := newAdaptiveLimiter(4, 8, 5*time.Second, "test", nil)
	limiter.inFlight = 1
	limiter.release(true, nil, 6*time.Second, 4*1024*1024)
	if limiter.limit != 3 {
		t.Fatalf("priority slow transfer limit = %d, want 3", limiter.limit)
	}
}

func TestAdaptiveLimiterKeepsSmallPriorityReserve(t *testing.T) {
	limiter := newAdaptiveLimiter(8, 8, time.Second, "test", nil)
	limiter.inFlight = 6
	if !limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should use non-reserved capacity")
	}
	limiter.inFlight = 7
	if limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should stop before consuming the priority reserve")
	}
	if !limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should be allowed while reserve is protected")
	}
}

func TestAdaptiveLimiterKeepsNormalFloorWhenPriorityIsWaiting(t *testing.T) {
	limiter := newAdaptiveLimiter(8, 8, time.Second, "test", nil)
	limiter.inFlight = 1
	limiter.priorityWait = 1
	if !limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should keep the non-reserved floor while priority is queued")
	}
	limiter.inFlight = 7
	if limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should still stop before consuming the priority reserve")
	}
	if !limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should still acquire while below the limit")
	}
}

func TestAdaptiveLimiterReservesNormalSlotsWhenNormalIsWaiting(t *testing.T) {
	limiter := newAdaptiveLimiter(8, 8, time.Second, "test", nil)
	limiter.normalWait = 1
	limiter.inFlight = 6
	limiter.priorityBusy = 6
	if limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should not consume normal reserve while normal is waiting")
	}
	if !limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should acquire its reserved slot")
	}

	limiter.normalWait = 0
	if !limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should use idle normal reserve when no normal waiter exists")
	}
}

func TestAdaptiveLimiterPriorityCanUseReserveAfterNormalShrink(t *testing.T) {
	limiter := newAdaptiveLimiter(8, 8, time.Second, "test", nil)
	limiter.limit = 4
	limiter.inFlight = 7
	limiter.priorityBusy = 0
	if limiter.canAcquireLocked(false) {
		t.Fatal("normal traffic should not acquire when old normal work is above the shrunken window")
	}
	if !limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should still use reserved capacity while total work is below max")
	}
	limiter.priorityBusy = limiter.priorityReserveLocked()
	if limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should stop after consuming the reserved capacity")
	}
}

func TestAdaptiveLimiterBulkFloorReservesPrioritySlots(t *testing.T) {
	limiter := newAdaptiveLimiter(32, 32, time.Second, "test", nil)
	for limiter.limit > limiter.minimumLimitLocked() {
		limiter.backoffUntil = time.Time{}
		limiter.inFlight = 1
		limiter.release(false, nil, 7*time.Second, 4*1024*1024)
	}
	if got, want := limiter.limit, 6; got != want {
		t.Fatalf("slow bulk floor limit = %d, want %d", got, want)
	}
	if got, want := limiter.priorityReserveLocked(), 4; got != want {
		t.Fatalf("priority reserve at bulk floor = %d, want %d", got, want)
	}
	limiter.inFlight = 1
	if !limiter.canAcquireLocked(false) {
		t.Fatal("normal bulk traffic should keep a second non-reserved floor slot")
	}
	limiter.inFlight = 2
	if limiter.canAcquireLocked(false) {
		t.Fatal("normal bulk traffic should stop after two non-reserved floor slots are occupied")
	}
	limiter.inFlight = limiter.limit
	limiter.priorityBusy = limiter.priorityReserveLocked() - 1
	if !limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should still use reserved capacity at the bulk floor")
	}
	limiter.priorityBusy = limiter.priorityReserveLocked()
	if limiter.canAcquireLocked(true) {
		t.Fatal("priority traffic should stop after consuming the floor reserve")
	}
}

func TestAutoProfileUsesConservativeUploadWindows(t *testing.T) {
	tests := []struct {
		name        string
		role        string
		routeProxy  string
		wantWorkers int
		wantInitial int
	}{
		{
			name:        "client direct",
			role:        "client",
			wantWorkers: autoClientUploadWorkers,
			wantInitial: autoClientUploadWindow,
		},
		{
			name:        "client proxy",
			role:        "client",
			routeProxy:  "socks5h://127.0.0.1:1080",
			wantWorkers: autoClientProxyUploadWorkers,
			wantInitial: autoClientProxyUploadWindow,
		},
		{
			name:        "exit",
			role:        "exit",
			wantWorkers: autoExitUploadWorkers,
			wantInitial: autoExitUploadWindow,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			tunnel := &Tunnel{Profile: "auto", role: tt.role, RouteProxy: tt.routeProxy}
			max := tunnel.uploadWorkerCount()
			if max != tt.wantWorkers {
				t.Fatalf("auto upload workers = %d, want %d", max, tt.wantWorkers)
			}
			if got := tunnel.initialUploadWindow(max); got != tt.wantInitial {
				t.Fatalf("auto initial upload window = %d, want %d", got, tt.wantInitial)
			}
		})
	}
}

func TestAutoProfileExplicitUploadConcurrencyIsUpperCap(t *testing.T) {
	tunnel := &Tunnel{Profile: "auto", role: "exit", UploadConcurrency: 20}
	max := tunnel.uploadWorkerCount()
	if max != 20 {
		t.Fatalf("explicit upload workers = %d, want 20", max)
	}
	if got := tunnel.initialUploadWindow(max); got != autoExitExplicitUploadWindow {
		t.Fatalf("explicit auto initial upload window = %d, want %d", got, autoExitExplicitUploadWindow)
	}
}

func TestAutoProfileClientExplicitUploadConcurrencyStartsWide(t *testing.T) {
	tunnel := &Tunnel{Profile: "auto", role: "client", UploadConcurrency: 16}
	max := tunnel.uploadWorkerCount()
	if max != 16 {
		t.Fatalf("explicit client upload workers = %d, want 16", max)
	}
	if got := tunnel.initialUploadWindow(max); got != autoClientExplicitUploadWindow {
		t.Fatalf("explicit client initial upload window = %d, want %d", got, autoClientExplicitUploadWindow)
	}
}

func TestAutoProfileExitUploadConcurrencyHasStableCeiling(t *testing.T) {
	tunnel := &Tunnel{Profile: "auto", role: "exit", UploadConcurrency: 64}
	max := tunnel.uploadWorkerCount()
	if max != autoExitUploadMaxWorkers {
		t.Fatalf("auto exit upload cap = %d, want %d", max, autoExitUploadMaxWorkers)
	}
	if got := tunnel.initialUploadWindow(max); got != autoExitExplicitUploadWindow {
		t.Fatalf("auto exit initial upload window = %d, want %d", got, autoExitExplicitUploadWindow)
	}
}

func TestFixedProfileExplicitUploadConcurrencyStartsAtRequestedWindow(t *testing.T) {
	tunnel := &Tunnel{Profile: "fixed", role: "exit", UploadConcurrency: 20}
	max := tunnel.uploadWorkerCount()
	if max != 20 {
		t.Fatalf("explicit upload workers = %d, want 20", max)
	}
	if got := tunnel.initialUploadWindow(max); got != 20 {
		t.Fatalf("fixed initial upload window = %d, want 20", got)
	}
}

func TestExitAutoProfileKeepsFullDownloadWindow(t *testing.T) {
	tunnel := &Tunnel{Profile: "auto", role: "exit"}
	if got, want := tunnel.initialDownloadWindow(tunnel.downloadWorkerCount()), 16; got != want {
		t.Fatalf("exit auto initial download window = %d, want %d", got, want)
	}
}

func TestMuxReceiveWorkerCountsScalePriorityWithDownloadConcurrency(t *testing.T) {
	mux := &driveMux{t: &Tunnel{Profile: "auto", DownloadConcurrency: 32}}
	priority, normal := mux.receiveWorkerCounts()
	if priority != 8 || normal != 24 {
		t.Fatalf("receive workers priority=%d normal=%d, want 8/24", priority, normal)
	}

	mux.t.DownloadConcurrency = 8
	priority, normal = mux.receiveWorkerCounts()
	if priority != 4 || normal != 4 {
		t.Fatalf("receive workers at 8 priority=%d normal=%d, want 4/4", priority, normal)
	}

	mux.t.DownloadConcurrency = 2
	priority, normal = mux.receiveWorkerCounts()
	if priority != 1 || normal != 1 {
		t.Fatalf("receive workers at 2 priority=%d normal=%d, want 1/1", priority, normal)
	}
}

func TestMuxPriorityUploadWorkersScaleWithPerLaneCapacity(t *testing.T) {
	mux := &driveMux{t: &Tunnel{Profile: "auto", UploadConcurrency: 8}, lanes: make([]*muxLane, muxLaneCount)}
	if got := mux.priorityUploadWorkersPerLane(); got != 1 {
		t.Fatalf("priority upload workers at per-lane 2 = %d, want 1", got)
	}

	mux.t.UploadConcurrency = 16
	if got := mux.priorityUploadWorkersPerLane(); got != 2 {
		t.Fatalf("priority upload workers at per-lane 4 = %d, want 2", got)
	}

	mux.t.UploadConcurrency = 32
	if got := mux.priorityUploadWorkersPerLane(); got != 2 {
		t.Fatalf("priority upload workers at per-lane 8 = %d, want 2", got)
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
	name := muxObjectName(sid, DirectionDown, "client-a", "run-a", "cafebabedeadbeef", 0x1234, 3, 9, 2, 7, 8, 1234, true)
	if !strings.Contains(name, "/down/client-a/run-a/cafebabedeadbeef/p0/s0000000000001234/l03/") {
		t.Fatalf("name = %q, want client/run/epoch segment", name)
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name, ID: "file-id"})
	if !ok {
		t.Fatalf("parse failed for %q", name)
	}
	if meta.ID != "file-id" || meta.ClientID != "client-a" || meta.RunID != "run-a" || meta.StreamID != 0x1234 || meta.Lane != 3 || meta.Seq != 9 || !meta.Priority {
		t.Fatalf("meta = %+v, want priority client/run stream lane=3 seq=9 id=file-id", meta)
	}
	if meta.PlainBytes != 1234 || !meta.FrameRangeKnown || meta.FrameMinSeq != 7 || meta.FrameMaxSeq != 8 {
		t.Fatalf("meta = %+v, want plain bytes and frame range", meta)
	}
}

func TestMuxExitOpenClaimSuppressesDuplicatesAndRecentlyClosed(t *testing.T) {
	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[muxStreamKey]*muxStream{},
		opening: map[muxStreamKey]struct{}{},
		closed:  map[muxStreamKey]time.Time{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 7}

	if !mux.claimExitOpen(key) {
		t.Fatal("first open claim failed")
	}
	if mux.claimExitOpen(key) {
		t.Fatal("duplicate open should be blocked while first open is in progress")
	}
	mux.finishExitOpenClaim(key, false)

	left, right := net.Pipe()
	defer right.Close()
	stream := mux.registerStream(key.StreamID, key.ClientID, key.RunID, left)
	if mux.claimExitOpen(key) {
		t.Fatal("duplicate open should be blocked while stream is active")
	}
	stream.close()
	if mux.claimExitOpen(key) {
		t.Fatal("duplicate open should be blocked shortly after stream closes")
	}

	mux.streamsMu.Lock()
	mux.closed[key] = time.Now().Add(-time.Second)
	mux.streamsMu.Unlock()
	if !mux.claimExitOpen(key) {
		t.Fatal("open should be allowed after the recently closed guard expires")
	}
}

func TestMuxRSTBeforeOpenRemembersClosedStream(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:       &Tunnel{},
		role:    "exit",
		streams: map[muxStreamKey]*muxStream{},
		opening: map[muxStreamKey]struct{}{},
		closed:  map[muxStreamKey]time.Time{},
		pending: map[muxStreamKey][]muxFrame{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 42}
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("late")})

	mux.handleFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID})
	if !mux.isClosedStream(key) {
		t.Fatal("unknown RST should remember the stream as closed")
	}
	if mux.claimExitOpen(key) {
		t.Fatal("open claim succeeded after earlier RST")
	}
	mux.pendingMu.Lock()
	pending := len(mux.pending[key])
	mux.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending frames = %d, want dropped after RST", pending)
	}
}

func TestMuxPendingFramesCapBytesAndCloseOnOverflow(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:            &Tunnel{},
		role:         "exit",
		streams:      map[muxStreamKey]*muxStream{},
		opening:      map[muxStreamKey]struct{}{},
		closed:       map[muxStreamKey]time.Time{},
		pending:      map[muxStreamKey][]muxFrame{},
		pendingBytes: map[muxStreamKey]int{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 42}
	payload := make([]byte, muxPendingStreamBytes/2)
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: payload})
	if got := len(mux.pending[key]); got != 1 {
		t.Fatalf("pending frames = %d, want 1", got)
	}
	if got := mux.pendingTotalBytes; got == 0 {
		t.Fatal("pending bytes were not tracked")
	}

	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, Payload: payload})
	if !mux.isClosedStream(key) {
		t.Fatal("pending byte overflow should close the stream key")
	}
	mux.pendingMu.Lock()
	pendingFrames := len(mux.pending[key])
	pendingBytes := mux.pendingBytes[key]
	pendingTotal := mux.pendingTotalBytes
	mux.pendingMu.Unlock()
	if pendingFrames != 0 || pendingBytes != 0 || pendingTotal != 0 {
		t.Fatalf("pending state after overflow: frames=%d stream_bytes=%d total=%d, want cleared", pendingFrames, pendingBytes, pendingTotal)
	}
}

func TestMuxPendingFramesGlobalOverflowClosesAllPendingStreams(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:            &Tunnel{},
		role:         "exit",
		streams:      map[muxStreamKey]*muxStream{},
		opening:      map[muxStreamKey]struct{}{},
		closed:       map[muxStreamKey]time.Time{},
		pending:      map[muxStreamKey][]muxFrame{},
		pendingBytes: map[muxStreamKey]int{},
	}
	keyA := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 1}
	keyB := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 2}
	keyC := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 3}
	frameA := muxFrame{Kind: muxFrameData, ClientID: keyA.ClientID, RunID: keyA.RunID, StreamID: keyA.StreamID, Seq: 1, Payload: []byte("a")}
	frameB := muxFrame{Kind: muxFrameData, ClientID: keyB.ClientID, RunID: keyB.RunID, StreamID: keyB.StreamID, Seq: 1, Payload: []byte("b")}
	mux.pending[keyA] = []muxFrame{frameA}
	mux.pending[keyB] = []muxFrame{frameB}
	mux.pendingBytes[keyA] = pendingFrameBytes(frameA)
	mux.pendingBytes[keyB] = pendingFrameBytes(frameB)
	mux.pendingTotalBytes = muxPendingGlobalBytes

	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: keyC.ClientID, RunID: keyC.RunID, StreamID: keyC.StreamID, Seq: 1, Payload: []byte("c")})

	if got := mux.pendingTotalBytes; got != 0 {
		t.Fatalf("pending total bytes = %d, want 0", got)
	}
	if len(mux.pending) != 0 {
		t.Fatalf("pending streams = %d, want 0", len(mux.pending))
	}
	if len(mux.pendingBytes) != 0 {
		t.Fatalf("pending byte streams = %d, want 0", len(mux.pendingBytes))
	}
	for _, key := range []muxStreamKey{keyA, keyB, keyC} {
		if !mux.isClosedStream(key) {
			t.Fatalf("stream %d was not terminal-closed after global pending overflow", key.StreamID)
		}
	}
}

func TestMuxPendingFramesFlushReleasesByteAccounting(t *testing.T) {
	ctx := context.Background()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	mux := &driveMux{
		t:            &Tunnel{},
		role:         "exit",
		streams:      map[muxStreamKey]*muxStream{},
		opening:      map[muxStreamKey]struct{}{},
		closed:       map[muxStreamKey]time.Time{},
		pending:      map[muxStreamKey][]muxFrame{},
		pendingBytes: map[muxStreamKey]int{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 42}
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, Payload: []byte("b")})
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("a")})
	if mux.pendingTotalBytes == 0 {
		t.Fatal("pending bytes were not tracked before flush")
	}

	stream := mux.registerStream(key.StreamID, key.ClientID, key.RunID, left)
	defer stream.close()
	mux.flushPendingFrames(ctx, stream)
	if got := mux.pendingTotalBytes; got != 0 {
		t.Fatalf("pending total bytes = %d, want 0 after flush", got)
	}
	if _, ok := mux.pendingBytes[key]; ok {
		t.Fatal("pending stream bytes still tracked after flush")
	}
	if len(stream.inbound) != 2 {
		t.Fatalf("stream inbound frames = %d, want 2", len(stream.inbound))
	}
}

func TestMuxTerminalCloseClearsPendingBeforeBadOpen(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:                &Tunnel{},
		role:             "exit",
		streams:          map[muxStreamKey]*muxStream{},
		opening:          map[muxStreamKey]struct{}{},
		closed:           map[muxStreamKey]time.Time{},
		pending:          map[muxStreamKey][]muxFrame{},
		pendingBytes:     map[muxStreamKey]int{},
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 42}
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("late")})
	mux.handleFrame(ctx, muxFrame{Kind: muxFrameOpen, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: []byte("bad-open")})

	for i := 0; i < 50 && !mux.isClosedStream(key); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !mux.isClosedStream(key) {
		t.Fatal("bad open should close stream key")
	}
	if got := mux.pendingTotalBytes; got != 0 {
		t.Fatalf("pending total bytes = %d, want 0", got)
	}
	if len(mux.pending[key]) != 0 {
		t.Fatal("pending frames were not cleared")
	}
	if _, ok := mux.pendingBytes[key]; ok {
		t.Fatal("pending byte entry was not cleared")
	}
}

func TestMuxTerminalFailureClearsPendingWithoutRegisteredStream(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:                &Tunnel{CleanupProcessed: true},
		role:             "client",
		streams:          map[muxStreamKey]*muxStream{},
		opening:          map[muxStreamKey]struct{}{},
		closed:           map[muxStreamKey]time.Time{},
		pending:          map[muxStreamKey][]muxFrame{},
		pendingBytes:     map[muxStreamKey]int{},
		seen:             map[string]struct{}{},
		queued:           map[string]struct{}{},
		cleanupQueue:     make(chan cleanupTask, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 42}
	mux.queuePendingFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("late")})
	meta := muxObjectMeta{Name: "muxv4/failed", ID: "id", ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Attempts: muxProcessMaxRetries}
	mux.failMuxObject(ctx, meta, fmt.Errorf("boom"))

	if !mux.isClosedStream(key) {
		t.Fatal("failed object should close stream key")
	}
	if got := mux.pendingTotalBytes; got != 0 {
		t.Fatalf("pending total bytes = %d, want 0", got)
	}
	if !mux.isKnown(meta.Name) {
		t.Fatal("failed object should be marked known")
	}
}

func TestFairOrderMuxMetasInterleavesClientRuns(t *testing.T) {
	ordered := fairOrderMuxMetas([]muxObjectMeta{
		{ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 1},
		{ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 2},
		{ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 3},
		{ClientID: "client-b", RunID: "run-b", Lane: 0, Seq: 1},
		{ClientID: "client-b", RunID: "run-b", Lane: 0, Seq: 2},
	})
	if len(ordered) != 5 {
		t.Fatalf("ordered len = %d", len(ordered))
	}
	got := []string{}
	for _, meta := range ordered {
		got = append(got, meta.ClientID+":"+fmt.Sprint(meta.Seq))
	}
	want := []string{"client-a:1", "client-b:1", "client-a:2", "client-b:2", "client-a:3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}

func TestFairOrderMuxMetasInterleavesStreams(t *testing.T) {
	ordered := fairOrderMuxMetas([]muxObjectMeta{
		{ClientID: "client-a", RunID: "run-a", StreamID: 1, Lane: 0, Seq: 1},
		{ClientID: "client-a", RunID: "run-a", StreamID: 1, Lane: 0, Seq: 2},
		{ClientID: "client-a", RunID: "run-a", StreamID: 2, Lane: 0, Seq: 1},
		{ClientID: "client-a", RunID: "run-a", StreamID: 2, Lane: 0, Seq: 2},
	})
	got := []string{}
	for _, meta := range ordered {
		got = append(got, fmt.Sprintf("%d:%d", meta.StreamID, meta.Seq))
	}
	want := []string{"1:1", "2:1", "1:2", "2:2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}

func TestOrderMuxMetasPrioritizesInteractiveObjects(t *testing.T) {
	base := time.Now()
	ordered := orderMuxMetas([]muxObjectMeta{
		{Name: "bulk-old", ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 1, Updated: base.Add(-time.Second)},
		{Name: "interactive-new", ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 2, Priority: true, Updated: base},
		{Name: "bulk-new", ClientID: "client-a", RunID: "run-a", Lane: 0, Seq: 3, Updated: base.Add(time.Second)},
	})
	if len(ordered) != 3 || ordered[0].Name != "interactive-new" {
		t.Fatalf("ordered = %+v, want priority object first", ordered)
	}
}

func TestMuxPriorityFramePromotesControlAndTinyFirstData(t *testing.T) {
	for _, kind := range []byte{muxFrameOpen, muxFrameFIN, muxFrameRST} {
		if !muxPriorityFrame(muxFrame{Kind: kind}) {
			t.Fatalf("kind %d should stay strict priority", kind)
		}
	}

	frame := muxFrame{Kind: muxFrameData, Seq: 1, Payload: make([]byte, muxPriorityFirstDataMax)}
	if !muxPriorityFrame(frame) {
		t.Fatal("tiny first data frame should use priority capacity")
	}
	frame.Payload = make([]byte, muxPriorityFirstDataMax+1)
	if muxPriorityFrame(frame) {
		t.Fatal("large data frame should not consume priority capacity")
	}
	frame.Seq = 2
	frame.Payload = make([]byte, muxPriorityFirstDataMax)
	if muxPriorityFrame(frame) {
		t.Fatal("continuing data frame should use the fair data scheduler")
	}
	frame.PriorityHint = true
	if !muxPriorityFrame(frame) {
		t.Fatal("bounded bootstrap hint should promote tiny continuation data")
	}
	frame.Payload = make([]byte, muxPriorityBootstrapChunk+1)
	if muxPriorityFrame(frame) {
		t.Fatal("bootstrap hint should not promote oversized data frames")
	}
}

func TestMuxPriorityBatchRequiresAllFramesPriority(t *testing.T) {
	frames := []muxFrame{
		{Kind: muxFrameOpen, StreamID: 1},
		{Kind: muxFrameData, StreamID: 1, Seq: 2, Payload: make([]byte, inlineDataThreshold+1)},
	}
	if muxPriorityBatch(frames) {
		t.Fatal("mixed priority and bulk batch should not be classified as priority")
	}
}

func TestMuxFrameTotalDelayIncludesQueueAndUploadTime(t *testing.T) {
	enqueuedAt := time.Unix(100, 0)
	pickedAt := enqueuedAt.Add(2 * time.Second)
	loggedAt := pickedAt.Add(3 * time.Second)
	frame := muxFrame{EnqueuedAt: enqueuedAt}

	if got, want := muxFrameTotalDelayAt(frame, loggedAt, pickedAt), 5*time.Second; got != want {
		t.Fatalf("total delay = %s, want %s", got, want)
	}
	if got, want := muxFrameTotalDelayAt(muxFrame{}, loggedAt, pickedAt), 3*time.Second; got != want {
		t.Fatalf("fallback total delay = %s, want %s", got, want)
	}
}

func TestMuxSendDataPayloadQueuesDataThroughFairScheduler(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}

	if err := mux.sendDataPayload(context.Background(), stream, make([]byte, 320*1024)); err != nil {
		t.Fatalf("send data payload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	if got := len(lane.urgent); got != 0 {
		t.Fatalf("urgent frames = %d, want data to stay out of strict priority", got)
	}

	lane.normalMu.Lock()
	normal := append([]muxFrame(nil), lane.normalQueues[stream.key()]...)
	queued := lane.normalQueuedFrames
	lane.normalMu.Unlock()
	if queued != 1 || len(normal) != 1 || normal[0].Seq != 1 || len(normal[0].Payload) != 320*1024 {
		t.Fatalf("normal queue = queued %d frames %+v, want one fair data frame", queued, normal)
	}
	if muxPriorityFrame(normal[0]) {
		t.Fatal("data frame should stay normal")
	}
}

func TestMuxSendDataPayloadPromotesTinyFirstResponse(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}

	if err := mux.sendDataPayload(context.Background(), stream, make([]byte, muxPriorityFirstDataMax)); err != nil {
		t.Fatalf("send data payload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	if got := len(lane.urgent); got != 1 {
		t.Fatalf("urgent frames = %d, want tiny first data in priority queue", got)
	}
	got := <-lane.urgent
	if got.Seq != 1 || len(got.Payload) != muxPriorityFirstDataMax {
		t.Fatalf("urgent frame seq=%d len=%d, want seq 1 len %d", got.Seq, len(got.Payload), muxPriorityFirstDataMax)
	}

	lane.normalMu.Lock()
	queued := lane.normalQueuedFrames
	lane.normalMu.Unlock()
	if queued != 0 {
		t.Fatalf("normal frames = %d, want none for tiny first response", queued)
	}
}

func TestMuxSendDataPayloadPromotesBoundedBootstrapBudget(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}
	stream.enablePriorityBootstrap()

	payload := make([]byte, muxPriorityBootstrapBudget+64*1024)
	if err := mux.sendDataPayload(context.Background(), stream, payload); err != nil {
		t.Fatalf("send data payload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	if got, want := len(lane.urgent), muxPriorityBootstrapBudget/muxPriorityBootstrapChunk; got != want {
		t.Fatalf("urgent frames = %d, want %d bootstrap chunks", got, want)
	}
	for i := 0; i < muxPriorityBootstrapBudget/muxPriorityBootstrapChunk; i++ {
		frame := <-lane.urgent
		if !frame.PriorityHint || !muxPriorityFrame(frame) || len(frame.Payload) != muxPriorityBootstrapChunk {
			t.Fatalf("bootstrap frame %d = seq %d len %d hint %t", i, frame.Seq, len(frame.Payload), frame.PriorityHint)
		}
	}

	lane.normalMu.Lock()
	normal := append([]muxFrame(nil), lane.normalQueues[stream.key()]...)
	queued := lane.normalQueuedFrames
	lane.normalMu.Unlock()
	if queued != 1 || len(normal) != 1 || len(normal[0].Payload) != 64*1024 {
		t.Fatalf("normal queue = queued %d frames %+v, want one fair remainder", queued, normal)
	}
	if normal[0].PriorityHint || muxPriorityFrame(normal[0]) {
		t.Fatal("remainder should not consume priority capacity")
	}
}

func TestMuxSendDataPayloadDoesNotPromoteIdleBurst(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{
		id:       1,
		clientID: "client-a",
		runID:    "run-a",
		mux:      mux,
	}
	stream.sendSeq.Store(10)

	if err := mux.sendDataPayload(context.Background(), stream, make([]byte, 192*1024)); err != nil {
		t.Fatalf("send data payload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	if got := len(lane.urgent); got != 0 {
		t.Fatalf("urgent frames = %d, want idle data to stay fair", got)
	}

	lane.normalMu.Lock()
	normal := append([]muxFrame(nil), lane.normalQueues[stream.key()]...)
	queued := lane.normalQueuedFrames
	lane.normalMu.Unlock()
	if queued != 1 || len(normal) != 1 || normal[0].Seq != 11 {
		t.Fatalf("normal queue = queued %d frames %+v, want one non-priority seq 11", queued, normal)
	}
}

func TestMuxSendDataPayloadSplitsNormalChunksToFairBatch(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{
		id:       1,
		clientID: "client-a",
		runID:    "run-a",
		mux:      mux,
	}
	stream.sendSeq.Store(4)

	payload := make([]byte, 3*muxNormalFairBatch+123)
	if err := mux.sendDataPayload(ctx, stream, payload); err != nil {
		t.Fatalf("sendDataPayload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	if got := len(lane.urgent); got != 0 {
		t.Fatalf("urgent frames = %d, want no bulk-tail promotion", got)
	}
	lane.normalMu.Lock()
	normal := append([]muxFrame(nil), lane.normalQueues[stream.key()]...)
	queued := lane.normalQueuedFrames
	queuedBytes := lane.normalQueuedBytes
	lane.normalMu.Unlock()
	if queued != 4 || len(normal) != 4 {
		t.Fatalf("normal queue = queued %d frames %+v, want four fair chunks including tail", queued, normal)
	}
	if normal[0].Seq != 5 || normal[3].Seq != 8 {
		t.Fatalf("normal frame seqs = %d..%d, want contiguous after initial priority", normal[0].Seq, normal[3].Seq)
	}
	for i, frame := range normal {
		if len(frame.Payload) > muxNormalFairBatch {
			t.Fatalf("frame %d payload = %d, want <= fair batch %d", i, len(frame.Payload), muxNormalFairBatch)
		}
	}
	if queuedBytes != muxBatchPlainBytes(normal) {
		t.Fatalf("queued bytes = %d, want %d", queuedBytes, muxBatchPlainBytes(normal))
	}
}

func TestMuxWriterClosesBeforeBestEffortRSTWhenUrgentQueueBlocked(t *testing.T) {
	left, right := net.Pipe()
	_ = right.Close()
	mux := &driveMux{
		t:       &Tunnel{},
		role:    "client",
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
		lanes:   make([]*muxLane, muxLaneCount),
	}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
		mux.lanes[i].urgent = make(chan muxFrame)
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	mux.startWriter(stream)

	stream.inbound <- []byte("x")
	select {
	case <-stream.done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("writer did not close stream while best-effort RST lane was blocked")
	}
}

func TestPriorityMuxDownloadHedgesSlowFirstAttempt(t *testing.T) {
	store := &hedgedObjectStore{firstExited: make(chan struct{})}
	tunnel := &Tunnel{
		Data:                store,
		DownloadConcurrency: 8,
		Profile:             "auto",
		role:                "client",
	}
	mux := &driveMux{t: tunnel}

	started := time.Now()
	sealed, err := mux.downloadMuxObject(context.Background(), muxObjectMeta{Name: "obj", ID: "file-id", Priority: true})
	if err != nil {
		t.Fatalf("download mux object: %v", err)
	}
	if string(sealed) != "hedged" {
		t.Fatalf("sealed = %q, want hedged response", sealed)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("hedged download took %s, want under 3s", elapsed)
	}
	if got := store.calls.Load(); got < 2 {
		t.Fatalf("store calls = %d, want hedged second attempt", got)
	}

	select {
	case <-store.firstExited:
	case <-time.After(2 * time.Second):
		t.Fatal("first hedged attempt did not exit after winner canceled the hedge")
	}

	tunnel.downloadLimiter.mu.Lock()
	limit := tunnel.downloadLimiter.limit
	tunnel.downloadLimiter.mu.Unlock()
	if limit != 8 {
		t.Fatalf("download limiter window = %d, want canceled loser not to shrink it", limit)
	}
}

type hedgedObjectStore struct {
	calls       atomic.Int32
	firstExited chan struct{}
}

func (s *hedgedObjectStore) Put(context.Context, string, []byte) error { return nil }

func (s *hedgedObjectStore) Get(ctx context.Context, name string) ([]byte, error) {
	return s.GetByID(ctx, name)
}

func (s *hedgedObjectStore) List(context.Context, string) ([]ObjectInfo, error) { return nil, nil }

func (s *hedgedObjectStore) Delete(context.Context, string) error { return nil }

func (s *hedgedObjectStore) GetByID(ctx context.Context, _ string) ([]byte, error) {
	if s.calls.Add(1) == 1 {
		defer close(s.firstExited)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte("hedged"), nil
}

func (s *hedgedObjectStore) DeleteID(context.Context, string) error { return nil }

func TestMuxFramesStayOnHomeLane(t *testing.T) {
	mux := &driveMux{lanes: make([]*muxLane, muxLaneCount)}
	first := muxFrame{Kind: muxFrameData, StreamID: 9, Seq: 1, Payload: []byte("small")}
	second := muxFrame{Kind: muxFrameData, StreamID: 9, Seq: 2, Payload: []byte("small")}
	if got, want := mux.frameLane(first), mux.frameLane(second); got != want {
		t.Fatalf("frames used lanes %d and %d, want same home lane", got, want)
	}
	fin := muxFrame{Kind: muxFrameFIN, StreamID: 9, Seq: 3}
	if got, want := mux.frameLane(fin), mux.frameLane(first); got != want {
		t.Fatalf("fin lane = %d, want home lane %d", got, want)
	}
	bulkA := muxFrame{Kind: muxFrameData, StreamID: 9, Seq: 2, Payload: make([]byte, inlineDataThreshold+1)}
	bulkB := muxFrame{Kind: muxFrameData, StreamID: 9, Seq: 3, Payload: make([]byte, inlineDataThreshold+1)}
	if got, want := mux.frameLane(bulkA), mux.frameLane(bulkB); got != want {
		t.Fatalf("bulk frames used lanes %d and %d, want same home lane", got, want)
	}
}

func TestMuxBatchLoopSeparatesUrgentFromBulk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame)
	lane.urgentUpload = make(chan []muxFrame, 2)
	lane.upload = make(chan []muxFrame, 2)
	go lane.runBatchLoop(ctx)

	bulk := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 2, Payload: make([]byte, inlineDataThreshold+1)}
	urgent := muxFrame{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 3, Payload: []byte("open")}

	if err := lane.enqueueNormalFrame(ctx, bulk); err != nil {
		t.Fatalf("enqueue bulk frame: %v", err)
	}
	select {
	case lane.urgent <- urgent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending urgent frame")
	}

	var normalBatch []muxFrame
	select {
	case normalBatch = <-lane.upload:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for normal upload batch")
	}
	if len(normalBatch) != 1 || normalBatch[0].StreamID != bulk.StreamID || muxPriorityBatch(normalBatch) {
		t.Fatalf("normal batch = %+v, want only the bulk frame", normalBatch)
	}

	var urgentBatch []muxFrame
	select {
	case urgentBatch = <-lane.urgentUpload:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for urgent upload batch")
	}
	if len(urgentBatch) != 1 || urgentBatch[0].StreamID != urgent.StreamID || !muxPriorityBatch(urgentBatch) {
		t.Fatalf("urgent batch = %+v, want only the urgent frame", urgentBatch)
	}
}

func TestMuxUrgentBatchLoopCoalescesBrowserOpenBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const opens = 24
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame, opens)
	lane.urgentUpload = make(chan []muxFrame, 1)
	for i := 0; i < opens; i++ {
		lane.urgent <- muxFrame{
			Kind:     muxFrameOpen,
			ClientID: "client-a",
			RunID:    "run-a",
			StreamID: uint64(1000 + i),
			Payload:  encodeMuxOpenPayload("example.com:443", make([]byte, 1800)),
		}
	}

	go lane.runUrgentBatchLoop(ctx)

	select {
	case batch := <-lane.urgentUpload:
		if len(batch) != opens {
			t.Fatalf("urgent batch frames = %d, want %d browser opens coalesced", len(batch), opens)
		}
		if !muxPriorityBatch(batch) {
			t.Fatal("coalesced open burst should stay priority")
		}
		seen := map[uint64]bool{}
		for _, frame := range batch {
			seen[frame.StreamID] = true
		}
		if len(seen) != opens {
			t.Fatalf("coalesced batch streams = %d, want %d distinct streams", len(seen), opens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for coalesced urgent open burst")
	}
}

func TestMuxUrgentBatchLoopCapsBrowserOpenBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const opens = muxUrgentCoalesceStreams + 4
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame, opens)
	lane.urgentUpload = make(chan []muxFrame, 2)
	for i := 0; i < opens; i++ {
		lane.urgent <- muxFrame{
			Kind:     muxFrameOpen,
			ClientID: "client-a",
			RunID:    "run-a",
			StreamID: uint64(2000 + i),
			Payload:  encodeMuxOpenPayload("example.com:443", make([]byte, 1800)),
		}
	}

	go lane.runUrgentBatchLoop(ctx)

	select {
	case batch := <-lane.urgentUpload:
		if len(batch) != muxUrgentCoalesceStreams {
			t.Fatalf("urgent batch streams = %d, want cap %d", len(batch), muxUrgentCoalesceStreams)
		}
		raw, err := encodeMuxBatch(batch)
		if err != nil {
			t.Fatalf("encode urgent batch: %v", err)
		}
		if len(raw) > muxUrgentCoalesceBatch {
			t.Fatalf("urgent batch bytes = %d, want <= %d", len(raw), muxUrgentCoalesceBatch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capped urgent open burst")
	}

	select {
	case batch := <-lane.urgentUpload:
		if len(batch) != opens-muxUrgentCoalesceStreams {
			t.Fatalf("second urgent batch frames = %d, want capped remainder %d", len(batch), opens-muxUrgentCoalesceStreams)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for urgent cap remainder")
	}
}

func TestMuxPriorityBootstrapBacklogCountsCoalescedUrgentFrames(t *testing.T) {
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame, muxUrgentFrameQueue)
	lane.urgentUpload = make(chan []muxFrame, 1)
	frames := make([]muxFrame, muxPriorityBootstrapBacklog)
	lane.noteUrgentUploadEnqueued(frames)

	if !lane.priorityBootstrapBacklogHigh() {
		t.Fatal("coalesced urgent upload frames should count toward bootstrap demotion")
	}

	lane.noteUrgentUploadDequeued(frames[:muxPriorityBootstrapBacklog-1])
	if lane.priorityBootstrapBacklogHigh() {
		t.Fatal("bootstrap backlog should clear after queued urgent frames are drained below threshold")
	}
}

func TestMuxReceiveUploadBatchPrefersNormalAfterUrgentBurst(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgentUpload = make(chan []muxFrame, 1)
	lane.upload = make(chan []muxFrame, 1)

	urgent := []muxFrame{{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 1}}
	normal := []muxFrame{{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 2, Seq: 2, Payload: make([]byte, inlineDataThreshold+1)}}
	lane.noteUrgentUploadEnqueued(urgent)
	lane.urgentUpload <- urgent
	lane.upload <- normal

	got, done := lane.receiveUploadBatch(ctx, false, true)
	if done {
		t.Fatal("receiveUploadBatch returned done")
	}
	if muxPriorityBatch(got) || got[0].StreamID != normal[0].StreamID {
		t.Fatalf("received batch = %+v, want normal batch after urgent burst", got)
	}
	if lane.urgentUploadBacklogFrames() != len(urgent) {
		t.Fatalf("urgent backlog frames = %d, want urgent batch still queued", lane.urgentUploadBacklogFrames())
	}
}

func TestMuxReceiveUploadBatchKeepsWorkerHeldUrgentBacklogVisible(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgentUpload = make(chan []muxFrame, 1)
	urgent := []muxFrame{
		{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 1},
		{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 2},
	}
	lane.noteUrgentUploadEnqueued(urgent)
	lane.urgentUpload <- urgent

	got, done := lane.receiveUploadBatch(ctx, false, false)
	if done {
		t.Fatal("receiveUploadBatch returned done")
	}
	if len(got) != len(urgent) {
		t.Fatalf("received %d urgent frames, want %d", len(got), len(urgent))
	}
	if lane.urgentUploadBacklogFrames() != len(urgent) {
		t.Fatalf("worker-held urgent frames = %d, want %d still visible", lane.urgentUploadBacklogFrames(), len(urgent))
	}
	lane.noteUrgentUploadDequeued(got)
	if lane.urgentUploadBacklogFrames() != 0 {
		t.Fatalf("urgent backlog after completion = %d, want 0", lane.urgentUploadBacklogFrames())
	}
}

func TestMuxUrgentBatchLoopKeepsClientRunsSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame, 2)
	lane.urgentUpload = make(chan []muxFrame, 2)
	lane.urgent <- muxFrame{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 1, Payload: encodeMuxOpenPayload("a.example:443", nil)}
	lane.urgent <- muxFrame{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-b", StreamID: 2, Payload: encodeMuxOpenPayload("b.example:443", nil)}

	go lane.runUrgentBatchLoop(ctx)

	for i := 0; i < 2; i++ {
		select {
		case batch := <-lane.urgentUpload:
			if len(batch) != 1 {
				t.Fatalf("urgent batch %d frames = %d, want separate client/run namespace", i, len(batch))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for separated urgent batches")
		}
	}
}

func TestNormalSendAdmissionAllowsNewStreamPastSoftQueueCap(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	streams := muxNormalFrameQueue / muxNormalStreamQueue
	for stream := 0; stream < streams; stream++ {
		for seq := 1; seq <= muxNormalStreamQueue; seq++ {
			frame := muxFrame{
				Kind:     muxFrameData,
				ClientID: "client-a",
				RunID:    "run-a",
				StreamID: uint64(stream + 1),
				Seq:      uint64(seq),
				Payload:  make([]byte, inlineDataThreshold+1),
			}
			if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
				t.Fatalf("fill stream %d seq %d: %v", stream, seq, err)
			}
		}
	}
	lane.normalMu.Lock()
	queued := lane.normalQueuedFrames
	lane.normalMu.Unlock()
	if queued != muxNormalFrameQueue {
		t.Fatalf("queued frames = %d, want soft cap %d", queued, muxNormalFrameQueue)
	}

	newStreamCtx, cancelNewStream := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelNewStream()
	if err := lane.enqueueNormalFrame(newStreamCtx, muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 999, Seq: 1, Payload: []byte("new-stream")}); err != nil {
		t.Fatalf("new stream admission should bypass soft cap: %v", err)
	}

	fullStreamCtx, cancelFullStream := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelFullStream()
	err := lane.enqueueNormalFrame(fullStreamCtx, muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 99, Payload: []byte("full-stream")})
	if err == nil {
		t.Fatal("existing full stream admitted beyond per-stream cap")
	}
}

func TestNormalSendAdmissionCapsStreamBytes(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}, 0)
	payload := make([]byte, 4*1024*1024)
	for seq := uint64(1); seq <= 3; seq++ {
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: seq, Payload: payload}
		if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
			t.Fatalf("enqueue frame %d: %v", seq, err)
		}
	}
	lane.normalMu.Lock()
	streamBytes := lane.normalQueueBytes[muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 1}]
	totalBytes := lane.normalQueuedBytes
	lane.normalMu.Unlock()
	if streamBytes != totalBytes || streamBytes <= 0 {
		t.Fatalf("queued bytes stream=%d total=%d, want matching positive byte accounting", streamBytes, totalBytes)
	}

	fullStreamCtx, cancelFullStream := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelFullStream()
	err := lane.enqueueNormalFrame(fullStreamCtx, muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 4, Payload: payload})
	if err == nil {
		t.Fatal("stream admitted beyond byte cap")
	}

	if _, ok := lane.takeNormalBatch(ctx); !ok {
		t.Fatal("takeNormalBatch returned false")
	}
	lane.normalMu.Lock()
	remainingBytes := lane.normalQueuedBytes
	lane.normalMu.Unlock()
	if remainingBytes < 0 || remainingBytes >= totalBytes {
		t.Fatalf("remaining queued bytes = %d, want lower than initial %d", remainingBytes, totalBytes)
	}
}

func TestMuxBatchLoopUrgentProgressesWithSaturatedNormalUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.urgent = make(chan muxFrame, 1)
	lane.urgentUpload = make(chan []muxFrame, 1)
	lane.upload = make(chan []muxFrame, 1)

	bulk := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 2, Payload: make([]byte, inlineDataThreshold+1)}
	lane.upload <- []muxFrame{bulk}
	if err := lane.enqueueNormalFrame(ctx, bulk); err != nil {
		t.Fatalf("enqueue bulk frame: %v", err)
	}
	go lane.runBatchLoop(ctx)

	urgent := muxFrame{Kind: muxFrameOpen, ClientID: "client-a", RunID: "run-a", StreamID: 3, Payload: []byte("open")}
	lane.urgent <- urgent

	select {
	case urgentBatch := <-lane.urgentUpload:
		if len(urgentBatch) != 1 || urgentBatch[0].StreamID != urgent.StreamID || !muxPriorityBatch(urgentBatch) {
			t.Fatalf("urgent batch = %+v, want only the urgent frame", urgentBatch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for urgent upload while normal upload was saturated")
	}
}

func TestMuxUploadTerminalFailureClosesAffectedStreams(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	mux := &driveMux{
		t:       &Tunnel{},
		role:    "client",
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
		lanes:   make([]*muxLane, muxLaneCount),
	}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
		mux.lanes[i].urgent = make(chan muxFrame)
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	lane := mux.lanes[mux.frameLane(muxFrame{Kind: muxFrameData, StreamID: stream.id})]
	lane.failUploadBatch(context.Background(), []muxFrame{
		{Kind: muxFrameData, ClientID: stream.clientID, RunID: stream.runID, StreamID: stream.id, Seq: 1, Payload: []byte("x")},
	}, fmt.Errorf("permanent upload failure"), muxUploadMaxRetries)

	select {
	case <-stream.done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("upload terminal failure did not close stream")
	}
	if !mux.isClosedStream(stream.key()) {
		t.Fatal("upload terminal failure did not remember closed stream")
	}
}

func TestNormalSendSchedulerInterleavesStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 1024 * 1024}}, 0)
	lane.upload = make(chan []muxFrame, 8)

	largePayload := make([]byte, 700*1024)
	for seq := uint64(1); seq <= 5; seq++ {
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: seq, Payload: largePayload}
		if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
			t.Fatalf("enqueue bulk stream frame %d: %v", seq, err)
		}
	}
	interactive := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 2, Seq: 1, Payload: []byte("interactive")}
	if err := lane.enqueueNormalFrame(ctx, interactive); err != nil {
		t.Fatalf("enqueue interactive frame: %v", err)
	}

	go lane.runFairNormalBatchLoop(ctx)

	first := receiveMuxBatch(t, lane.upload)
	if first[0].StreamID != 1 {
		t.Fatalf("first batch stream = %d, want bulk stream 1", first[0].StreamID)
	}
	second := receiveMuxBatch(t, lane.upload)
	if second[0].StreamID != 2 {
		t.Fatalf("second batch stream = %d, want interactive stream 2 before more bulk", second[0].StreamID)
	}
}

func TestNormalSendSchedulerCapsBulkBatchSize(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}, 0)
	payload := make([]byte, 256*1024)
	for seq := uint64(1); seq <= muxNormalStreamQueue; seq++ {
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: seq, Payload: payload}
		if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
			t.Fatalf("enqueue normal frame %d: %v", seq, err)
		}
	}

	batch, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("take normal batch returned false")
	}
	if got := muxBatchPlainBytes(batch); got > muxNormalBulkBatch {
		t.Fatalf("batch bytes = %d, want <= %d", got, muxNormalBulkBatch)
	}
	raw, err := encodeMuxBatch(batch)
	if err != nil {
		t.Fatalf("encodeMuxBatch: %v", err)
	}
	if got := len(raw); got > muxNormalBulkBatch {
		t.Fatalf("encoded batch bytes = %d, want <= %d", got, muxNormalBulkBatch)
	}
}

func TestNormalSendSchedulerUsesFairBatchWhenStreamsContend(t *testing.T) {
	ctx := context.Background()
	lane := newMuxLane(&driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}, 0)
	payload := make([]byte, 500*1024)
	for seq := uint64(1); seq <= 6; seq++ {
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: seq, Payload: payload}
		if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
			t.Fatalf("enqueue bulk stream frame %d: %v", seq, err)
		}
	}
	contender := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 2, Seq: 1, Payload: payload}
	if err := lane.enqueueNormalFrame(ctx, contender); err != nil {
		t.Fatalf("enqueue contender frame: %v", err)
	}

	first, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("take first batch returned false")
	}
	if first[0].StreamID != 1 {
		t.Fatalf("first batch stream = %d, want bulk stream 1", first[0].StreamID)
	}
	if got := muxBatchPlainBytes(first); got > muxNormalFairBatch {
		t.Fatalf("first batch bytes = %d, want <= fair cap %d while streams contend", got, muxNormalFairBatch)
	}

	second, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("take second batch returned false")
	}
	if second[0].StreamID != 2 {
		t.Fatalf("second batch stream = %d, want contender stream 2", second[0].StreamID)
	}
}

func TestNormalSendSchedulerObservesContention(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	lane := newMuxLane(&driveMux{
		t:    &Tunnel{ChunkSize: 8 * 1024 * 1024, Observe: true, Logger: log.New(&logs, "", 0)},
		role: "client",
	}, 0)
	payload := make([]byte, 500*1024)
	for seq := uint64(1); seq <= 3; seq++ {
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: seq, Payload: payload}
		if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
			t.Fatalf("enqueue bulk stream frame %d: %v", seq, err)
		}
	}
	contender := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 2, Seq: 1, Payload: payload}
	if err := lane.enqueueNormalFrame(ctx, contender); err != nil {
		t.Fatalf("enqueue contender frame: %v", err)
	}

	if _, ok := lane.takeNormalBatch(ctx); !ok {
		t.Fatal("take normal batch returned false")
	}
	logText := logs.String()
	if !strings.Contains(logText, "mux send scheduler") || !strings.Contains(logText, "contended=true") {
		t.Fatalf("scheduler log = %q, want contention observation", logText)
	}
}

func receiveMuxBatch(t *testing.T, ch <-chan []muxFrame) []muxFrame {
	t.Helper()
	select {
	case frames := <-ch:
		return frames
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mux batch")
		return nil
	}
}

func TestNormalMuxReceiveTuningStaysBelowReassemblyHardCaps(t *testing.T) {
	if got := muxNormalStreamInflightBytes; got >= muxStreamPendingBytes {
		t.Fatalf("normal receive byte window = %d, want < hard pending byte cap %d", got, muxStreamPendingBytes)
	}
	if got := muxNormalStreamInflight * muxMaxFrames; got >= muxStreamPendingFrames {
		t.Fatalf("normal receive frame window = %d, want < hard pending frame cap %d", got, muxStreamPendingFrames)
	}
}

func TestNormalMuxSchedulerProcessesStreamInObjectSequence(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:  make(chan muxStreamKey, 4),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	for _, seq := range []uint64{3, 1, 2} {
		meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: seq}
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue seq %d failed", seq)
		}
	}

	for _, want := range []uint64{1, 2, 3} {
		select {
		case ready := <-mux.recvNormalReady:
			got, ok := mux.takeNormalMuxObject(ctx, ready)
			if !ok {
				t.Fatal("ready stream had no object")
			}
			if got.Seq != want {
				t.Fatalf("got seq %d, want %d", got.Seq, want)
			}
			mux.finishNormalMuxObject(ctx, got)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}

func TestNormalMuxSchedulerOrdersStripedLaneObjectsByFrameSequence(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:  make(chan muxStreamKey, 4),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	metas := []muxObjectMeta{
		{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Lane: 3, Seq: 1, FrameMinSeq: 9, FrameMaxSeq: 9, FrameRangeKnown: true},
		{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Lane: 0, Seq: 10, FrameMinSeq: 7, FrameMaxSeq: 7, FrameRangeKnown: true},
		{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Lane: 2, Seq: 2, FrameMinSeq: 8, FrameMaxSeq: 8, FrameRangeKnown: true},
	}
	for _, meta := range metas {
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue lane=%d seq=%d failed", meta.Lane, meta.Seq)
		}
	}

	for _, wantFrameSeq := range []uint64{7, 8, 9} {
		select {
		case ready := <-mux.recvNormalReady:
			got, ok := mux.takeNormalMuxObject(ctx, ready)
			if !ok {
				t.Fatal("ready stream had no object")
			}
			if got.FrameMinSeq != wantFrameSeq {
				t.Fatalf("got frame min seq %d from lane %d object seq %d, want %d", got.FrameMinSeq, got.Lane, got.Seq, wantFrameSeq)
			}
			mux.finishNormalMuxObject(ctx, got)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for frame seq %d", wantFrameSeq)
		}
	}
}

func TestNormalMuxSchedulerWaitsForMissingEarlierFrameRange(t *testing.T) {
	ctx := context.Background()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:                &Tunnel{},
		streams:          map[muxStreamKey]*muxStream{},
		pending:          map[muxStreamKey][]muxFrame{},
		recvNormalReady:  make(chan muxStreamKey, 2),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	defer stream.close()
	key := stream.key()

	later := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 20, FrameMinSeq: 10, FrameMaxSeq: 11, FrameRangeKnown: true}
	if !mux.enqueueNormalMuxObject(ctx, later) {
		t.Fatal("enqueue later normal object failed")
	}
	select {
	case ready := <-mux.recvNormalReady:
		if got, ok := mux.takeNormalMuxObject(ctx, ready); ok {
			t.Fatalf("scheduler took later object before missing frame range arrived: %+v", got)
		}
	default:
	}

	stream.mu.Lock()
	stream.recvExpected = 10
	stream.mu.Unlock()
	mux.signalNormalMuxObjectIfReady(ctx, key)

	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object after missing frame range was delivered")
		}
		if got.Seq != later.Seq {
			t.Fatalf("got seq %d, want %d", got.Seq, later.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for normal object after missing frame range was delivered")
	}
}

func TestNormalMuxSchedulerPipelinesFrameRangesWhenEarlierObjectIsActive(t *testing.T) {
	ctx := context.Background()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:                &Tunnel{},
		streams:          map[muxStreamKey]*muxStream{},
		pending:          map[muxStreamKey][]muxFrame{},
		recvNormalReady:  make(chan muxStreamKey, 4),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	defer stream.close()
	key := stream.key()

	first := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, FrameMinSeq: 1, FrameMaxSeq: 4, FrameRangeKnown: true}
	second := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, FrameMinSeq: 5, FrameMaxSeq: 8, FrameRangeKnown: true}
	if !mux.enqueueNormalMuxObject(ctx, first) {
		t.Fatal("enqueue first failed")
	}
	if !mux.enqueueNormalMuxObject(ctx, second) {
		t.Fatal("enqueue second failed")
	}

	ready := <-mux.recvNormalReady
	got, ok := mux.takeNormalMuxObject(ctx, ready)
	if !ok || got.Seq != first.Seq {
		t.Fatalf("first take = %+v ok=%t, want seq %d", got, ok, first.Seq)
	}
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object for pipelined range")
		}
		if got.Seq != second.Seq {
			t.Fatalf("got seq %d, want %d", got.Seq, second.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pipelined frame range")
	}
}

func TestMuxNextObjectPrefersUrgentBeforeNormal(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvUrgent:       make(chan muxObjectMeta, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	if !mux.enqueueNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1}) {
		t.Fatal("enqueue normal object failed")
	}
	mux.recvUrgent <- muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: 99, Seq: 7, Priority: true}

	budget := muxNormalStreamInflight
	got, ok := mux.nextMuxObject(ctx, false, &budget)
	if !ok {
		t.Fatal("nextMuxObject returned false")
	}
	if !got.Priority || got.StreamID != 99 {
		t.Fatalf("got %+v, want urgent object before normal", got)
	}
}

func TestMuxNextObjectServesNormalAfterUrgentBudget(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvUrgent:       make(chan muxObjectMeta, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	if !mux.enqueueNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1}) {
		t.Fatal("enqueue normal object failed")
	}
	mux.recvUrgent <- muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: 99, Seq: 7, Priority: true}
	budget := 0

	got, ok := mux.nextMuxObject(ctx, false, &budget)
	if !ok {
		t.Fatal("nextMuxObject returned false")
	}
	if got.Priority || got.StreamID != key.StreamID {
		t.Fatalf("got %+v, want normal object after urgent budget exhausted", got)
	}
	if budget != muxNormalStreamInflight {
		t.Fatalf("budget = %d, want reset to %d", budget, muxNormalStreamInflight)
	}
}

func TestNormalMuxSchedulerDropsQueuedObjectsForClosedStream(t *testing.T) {
	ctx := context.Background()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:                &Tunnel{CleanupProcessed: true},
		streams:          map[muxStreamKey]*muxStream{},
		closed:           map[muxStreamKey]time.Time{},
		pending:          map[muxStreamKey][]muxFrame{},
		seen:             map[string]struct{}{},
		queued:           map[string]struct{}{},
		cleanupQueue:     make(chan cleanupTask, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	key := stream.key()
	meta := muxObjectMeta{
		Name:     "muxv4/queued-before-close",
		ClientID: key.ClientID,
		RunID:    key.RunID,
		StreamID: key.StreamID,
		Seq:      1,
	}
	if !mux.enqueueMuxObject(ctx, meta) {
		t.Fatal("enqueue normal object failed")
	}

	stream.close()

	mux.recvNormalMu.Lock()
	_, hasFlow := mux.recvNormalFlows[key]
	_, hasActive := mux.recvNormalActive[key]
	_, hasSent := mux.recvNormalSent[key]
	mux.recvNormalMu.Unlock()
	if hasFlow || hasActive || hasSent {
		t.Fatalf("closed stream left scheduler state: flow=%t active=%t sent=%t", hasFlow, hasActive, hasSent)
	}
	if !mux.isKnown(meta.Name) {
		t.Fatal("closed stream did not mark dropped object as seen")
	}
	select {
	case task := <-mux.cleanupQueue:
		if task.name != meta.Name || task.id != meta.ID {
			t.Fatalf("cleanup task = %+v, want name=%q id=%q", task, meta.Name, meta.ID)
		}
	default:
		t.Fatal("closed stream did not schedule dropped object cleanup")
	}

	select {
	case ready := <-mux.recvNormalReady:
		if got, ok := mux.takeNormalMuxObject(ctx, ready); ok {
			t.Fatalf("stale ready token returned closed-stream object: %+v", got)
		}
	default:
	}
}

func TestNormalMuxSchedulerAllowsBoundedStreamInflight(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:  make(chan muxStreamKey, muxNormalStreamInflight+1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	for seq := uint64(1); seq <= uint64(muxNormalStreamInflight+1); seq++ {
		meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: seq}
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue seq %d failed", seq)
		}
	}

	for want := uint64(1); want <= uint64(muxNormalStreamInflight); want++ {
		select {
		case ready := <-mux.recvNormalReady:
			got, ok := mux.takeNormalMuxObject(ctx, ready)
			if !ok {
				t.Fatal("ready stream had no object")
			}
			if got.Seq != want {
				t.Fatalf("got seq %d, want %d", got.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}

	select {
	case ready := <-mux.recvNormalReady:
		t.Fatalf("received extra ready token while stream window is full: %+v", ready)
	default:
	}

	mux.finishNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1})
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object after finish")
		}
		if got.Seq != uint64(muxNormalStreamInflight+1) {
			t.Fatalf("got seq %d, want %d", got.Seq, uint64(muxNormalStreamInflight+1))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream window refill")
	}
}

func TestNormalMuxSchedulerCapsStreamInflightBytes(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:       make(chan muxStreamKey, 4),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalActiveBytes: map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	objectBytes := muxNormalStreamInflightBytes/2 + 1
	for seq := uint64(1); seq <= 3; seq++ {
		meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: seq, PlainBytes: objectBytes}
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue seq %d failed", seq)
		}
	}

	var first muxObjectMeta
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object")
		}
		first = got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first object")
	}
	select {
	case ready := <-mux.recvNormalReady:
		t.Fatalf("received extra ready token while byte window is full: %+v", ready)
	default:
	}

	mux.finishNormalMuxObject(ctx, first)
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object after byte window refill")
		}
		if got.Seq != 2 {
			t.Fatalf("got seq %d, want 2", got.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for byte window refill")
	}
}

func TestNormalMuxSchedulerCapsQueuedBytesPerStream(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:       make(chan muxStreamKey, 4),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalQueuedBytes: map[muxStreamKey]int{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	objectBytes := muxNormalReceiveQueueBytes/2 + 1
	first := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, PlainBytes: objectBytes}
	second := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, PlainBytes: objectBytes}
	if !mux.enqueueNormalMuxObject(ctx, first) {
		t.Fatal("first enqueue failed")
	}
	if mux.enqueueNormalMuxObject(ctx, second) {
		t.Fatal("second enqueue exceeded per-stream queued byte cap")
	}
	if got := mux.recvNormalQueuedTotal; got != objectBytes {
		t.Fatalf("queued total = %d, want %d", got, objectBytes)
	}
	if got := mux.recvNormalQueuedBytes[key]; got != objectBytes {
		t.Fatalf("queued stream bytes = %d, want %d", got, objectBytes)
	}
}

func TestNormalMuxSchedulerCapsQueuedBytesGlobally(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:       make(chan muxStreamKey, 8),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalQueuedBytes: map[muxStreamKey]int{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
	}
	for i := 0; i < muxNormalReceiveGlobalBytes/muxNormalReceiveQueueBytes; i++ {
		key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: uint64(i + 1)}
		meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, PlainBytes: muxNormalReceiveQueueBytes}
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue stream %d failed before global cap", i+1)
		}
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 99}
	meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, PlainBytes: muxMinBatch}
	if mux.enqueueNormalMuxObject(ctx, meta) {
		t.Fatal("enqueue exceeded global queued byte cap")
	}
	if got := mux.recvNormalQueuedTotal; got != muxNormalReceiveGlobalBytes {
		t.Fatalf("queued total = %d, want %d", got, muxNormalReceiveGlobalBytes)
	}
}

func TestNormalMuxSchedulerTakeReleasesQueuedBytes(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:       make(chan muxStreamKey, 1),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalQueuedBytes: map[muxStreamKey]int{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, PlainBytes: muxMinBatch}
	if !mux.enqueueNormalMuxObject(ctx, meta) {
		t.Fatal("enqueue failed")
	}
	ready := <-mux.recvNormalReady
	if _, ok := mux.takeNormalMuxObject(ctx, ready); !ok {
		t.Fatal("take failed")
	}
	if got := mux.recvNormalQueuedTotal; got != 0 {
		t.Fatalf("queued total = %d, want 0 after take", got)
	}
	if _, ok := mux.recvNormalQueuedBytes[key]; ok {
		t.Fatal("queued stream bytes still tracked after take")
	}
}

func TestNormalMuxSchedulerAllowsGapRecoveryWhileReassemblyPaused(t *testing.T) {
	ctx := context.Background()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:                &Tunnel{},
		streams:          map[muxStreamKey]*muxStream{},
		pending:          map[muxStreamKey][]muxFrame{},
		recvNormalReady:  make(chan muxStreamKey, 4),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	defer stream.close()
	key := stream.key()

	stream.mu.Lock()
	stream.recvPending[2] = muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, Payload: make([]byte, muxStreamPauseBytes)}
	stream.recvPendingBytes = muxStreamPauseBytes
	stream.mu.Unlock()

	if !mux.enqueueNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, FrameMinSeq: 1, FrameMaxSeq: 1, FrameRangeKnown: true}) {
		t.Fatal("enqueue failed")
	}
	if !mux.enqueueNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 3, FrameMinSeq: 3, FrameMaxSeq: 3, FrameRangeKnown: true}) {
		t.Fatal("enqueue follow-up failed")
	}
	var recovery muxObjectMeta
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("scheduler refused the missing object needed to drain reassembly backlog")
		}
		if got.Seq != 1 {
			t.Fatalf("got seq %d, want missing seq 1", got.Seq)
		}
		recovery = got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap recovery object")
	}

	stream.mu.Lock()
	delete(stream.recvPending, 2)
	stream.recvPendingBytes = 0
	stream.recvExpected = 3
	stream.mu.Unlock()
	mux.finishNormalMuxObject(ctx, recovery)

	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object after reassembly drain")
		}
		if got.Seq != 3 {
			t.Fatalf("got seq %d, want 3", got.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for normal receive resume")
	}
}

func TestNormalMuxSchedulerPausesOnInboundBacklog(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:                &Tunnel{},
		streams:          map[muxStreamKey]*muxStream{},
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	stream := mux.registerStream(7, "client-a", "run-a", nil)
	key := stream.key()
	for i := 0; i < muxStreamInboundPause; i++ {
		stream.inbound <- []byte("x")
	}

	if !mux.enqueueNormalMuxObject(ctx, muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1}) {
		t.Fatal("enqueue failed")
	}
	select {
	case ready := <-mux.recvNormalReady:
		if got, ok := mux.takeNormalMuxObject(ctx, ready); ok {
			t.Fatalf("scheduler took object %+v while inbound backlog was paused", got)
		}
	default:
	}

	<-stream.inbound
	mux.signalNormalMuxObjectIfReady(ctx, key)
	select {
	case ready := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, ready)
		if !ok {
			t.Fatal("ready stream had no object after inbound drain")
		}
		if got.Seq != 1 {
			t.Fatalf("got seq %d, want 1", got.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound receive resume")
	}
}

func TestMuxStreamReordersStripedFrames(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	mux.startWriter(stream)
	defer stream.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream.acceptFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 42, Seq: 2, Payload: []byte("b")})
	stream.acceptFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 42, Seq: 1, Payload: []byte("a")})

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

func TestMuxStreamConcurrentAcceptFramePreservesOrder(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	mux.startWriter(stream)
	defer stream.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const count = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for seq := count; seq >= 1; seq-- {
		seq := uint64(seq)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stream.acceptFrame(ctx, muxFrame{
				Kind:     muxFrameData,
				ClientID: "client-a",
				RunID:    "run-a",
				StreamID: 42,
				Seq:      seq,
				Payload:  []byte{byte(seq)},
			})
		}()
	}
	close(start)
	wg.Wait()

	if err := right.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, count)
	if _, err := io.ReadFull(right, got); err != nil {
		t.Fatal(err)
	}
	for i, b := range got {
		if want := byte(i + 1); b != want {
			t.Fatalf("byte %d = %d, want %d; full=%v", i, b, want, got)
		}
	}
}

func TestMuxStreamReassemblyBackpressureDoesNotCloseStream(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	defer stream.close()

	ctx := context.Background()
	payload := make([]byte, 256*1024)
	for seq := uint64(2); seq <= 34; seq++ {
		stream.acceptFrame(ctx, muxFrame{
			Kind:     muxFrameData,
			ClientID: "client-a",
			RunID:    "run-a",
			StreamID: 42,
			Seq:      seq,
			Payload:  payload,
		})
	}

	select {
	case <-stream.done:
		t.Fatal("stream closed at soft reassembly backpressure watermark")
	default:
	}
	frames, bytes := stream.reassemblyBacklog()
	if frames == 0 || bytes < muxStreamPauseBytes {
		t.Fatalf("backlog frames=%d bytes=%d, want soft pause backlog", frames, bytes)
	}
}

func TestMuxStreamReassemblyOverflowClosesStream(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	mux := &driveMux{
		t:       &Tunnel{},
		streams: map[muxStreamKey]*muxStream{},
		pending: map[muxStreamKey][]muxFrame{},
	}
	stream := mux.registerStream(42, "client-a", "run-a", left)
	defer stream.close()

	ctx := context.Background()
	for seq := uint64(2); seq <= uint64(muxStreamPendingFrames+2); seq++ {
		stream.acceptFrame(ctx, muxFrame{
			Kind:     muxFrameData,
			ClientID: "client-a",
			RunID:    "run-a",
			StreamID: 42,
			Seq:      seq,
			Payload:  []byte{1},
		})
	}

	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Fatal("stream did not close after reassembly overflow")
	}
}

func TestMuxReadBufferScalesTowardDriveEfficientObjects(t *testing.T) {
	bulkReadBuffer := 4 * (muxNormalFairBatch - muxBatchHeaderSize - muxFrameHeaderSize)
	tests := []struct {
		name      string
		chunkSize int
		want      int
	}{
		{name: "small", chunkSize: 64 * 1024, want: 32 * 1024},
		{name: "default", chunkSize: 8 * 1024 * 1024, want: bulkReadBuffer},
		{name: "max", chunkSize: 16 * 1024 * 1024, want: bulkReadBuffer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := &driveMux{t: &Tunnel{ChunkSize: tt.chunkSize}}
			if got := mux.readBufferSize(); got != tt.want {
				t.Fatalf("readBufferSize() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSendDataPayloadLargeNormalFrameFitsNormalBatch(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{t: &Tunnel{ChunkSize: 16 * 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	stream := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}
	stream.sendSeq.Store(4)

	payload := make([]byte, mux.readBufferSize())
	if err := mux.sendDataPayload(ctx, stream, payload); err != nil {
		t.Fatalf("sendDataPayload: %v", err)
	}
	lane := mux.lanes[mux.frameLane(muxFrame{StreamID: stream.id})]
	batch, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("takeNormalBatch returned false")
	}
	raw, err := encodeMuxBatch(batch)
	if err != nil {
		t.Fatalf("encodeMuxBatch: %v", err)
	}
	if got, want := len(raw), mux.normalBatchBytes(); got > want {
		t.Fatalf("encoded batch bytes = %d, want <= %d", got, want)
	}
}

func TestMuxTakeNormalBatchLimitsContendedBulkToFairFrame(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{t: &Tunnel{ChunkSize: 16 * 1024 * 1024}, lanes: make([]*muxLane, muxLaneCount)}
	for i := range mux.lanes {
		mux.lanes[i] = newMuxLane(mux, i)
	}
	bulk := &muxStream{id: 1, clientID: "client-a", runID: "run-a", mux: mux}
	peer := &muxStream{id: 5, clientID: "client-a", runID: "run-a", mux: mux}
	bulk.sendSeq.Store(4)
	peer.sendSeq.Store(4)

	if err := mux.sendDataPayload(ctx, bulk, make([]byte, mux.readBufferSize())); err != nil {
		t.Fatalf("send bulk payload: %v", err)
	}
	if err := mux.sendDataPayload(ctx, peer, make([]byte, 32*1024)); err != nil {
		t.Fatalf("send peer payload: %v", err)
	}

	lane := mux.lanes[mux.frameLane(muxFrame{StreamID: bulk.id})]
	batch, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("takeNormalBatch returned false")
	}
	if len(batch) != 1 || batch[0].StreamID != bulk.id {
		t.Fatalf("first contended batch = %+v, want one bulk frame", batch)
	}
	if got := muxBatchPlainBytes(batch); got > muxNormalFairBatch {
		t.Fatalf("contended bulk batch bytes = %d, want <= fair batch %d", got, muxNormalFairBatch)
	}

	batch, ok = lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("second takeNormalBatch returned false")
	}
	if len(batch) != 1 || batch[0].StreamID != peer.id {
		t.Fatalf("second contended batch = %+v, want peer stream before remaining bulk", batch)
	}
}

func TestReadChunkCoalesceClassBudgets(t *testing.T) {
	tests := []struct {
		name      string
		bytes     int
		wantDelay time.Duration
		wantAge   time.Duration
	}{
		{name: "interactive", bytes: 1, wantDelay: interactiveCoalesceDelay, wantAge: interactiveCoalesceMaxAge},
		{name: "medium", bytes: mediumDataThreshold, wantDelay: mediumCoalesceDelay, wantAge: mediumCoalesceMaxAge},
		{name: "bulk", bytes: inlineDataThreshold, wantDelay: bulkCoalesceDelay, wantAge: bulkCoalesceMaxAge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coalesceDelayForBytesWithPolicy(tt.bytes, false); got != tt.wantDelay {
				t.Fatalf("coalesceDelayForBytes(%d) = %s, want %s", tt.bytes, got, tt.wantDelay)
			}
			if got := coalesceMaxAgeForBytesWithPolicy(tt.bytes, false); got != tt.wantAge {
				t.Fatalf("coalesceMaxAgeForBytes(%d) = %s, want %s", tt.bytes, got, tt.wantAge)
			}
		})
	}
}

func TestReadChunkFastBulkFillsScaledBuffer(t *testing.T) {
	buffer := make([]byte, 2*1024*1024)
	input := bytes.NewReader(make([]byte, len(buffer)))
	n, err := readChunk(input, buffer)
	if err != nil {
		t.Fatalf("readChunk: %v", err)
	}
	if n != len(buffer) {
		t.Fatalf("readChunk bytes = %d, want %d", n, len(buffer))
	}
}

func TestReadChunkInteractiveTrickleRespectsMaxAge(t *testing.T) {
	reader := &trickleDeadlineReader{
		initialChunk: 1,
		nextChunk:    1,
		nextDelay:    time.Millisecond,
	}
	buffer := make([]byte, 32*1024)
	started := time.Now()
	n, err := readChunk(reader, buffer)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("readChunk: %v", err)
	}
	if n >= mediumDataThreshold {
		t.Fatalf("readChunk bytes = %d, want interactive partial chunk", n)
	}
	if elapsed < interactiveCoalesceMaxAge-5*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("readChunk elapsed = %s, want around interactive max age %s", elapsed, interactiveCoalesceMaxAge)
	}
}

func TestReadChunkBulkTrickleRespectsMaxAge(t *testing.T) {
	reader := &trickleDeadlineReader{
		initialChunk: inlineDataThreshold,
		nextChunk:    1024,
		nextDelay:    10 * time.Millisecond,
	}
	buffer := make([]byte, 2*1024*1024)
	started := time.Now()
	n, err := readChunk(reader, buffer)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("readChunk: %v", err)
	}
	if n <= inlineDataThreshold || n >= len(buffer) {
		t.Fatalf("readChunk bytes = %d, want partial bulk chunk", n)
	}
	if elapsed < bulkCoalesceMaxAge-25*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("readChunk elapsed = %s, want around bulk max age %s", elapsed, bulkCoalesceMaxAge)
	}
}

func TestReadChunkForcedBulkCoalescesTinyTrickle(t *testing.T) {
	reader := &trickleDeadlineReader{
		initialChunk: 1,
		nextChunk:    1024,
		nextDelay:    10 * time.Millisecond,
	}
	buffer := make([]byte, 2*1024*1024)
	started := time.Now()
	n, err := readChunkWithPolicy(reader, buffer, true)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("readChunkWithPolicy: %v", err)
	}
	if n <= mediumDataThreshold {
		t.Fatalf("readChunkWithPolicy bytes = %d, want forced bulk coalescing above medium threshold", n)
	}
	if elapsed < forcedBulkCoalesceMaxAge-75*time.Millisecond || elapsed > forcedBulkCoalesceMaxAge+500*time.Millisecond {
		t.Fatalf("readChunkWithPolicy elapsed = %s, want around forced bulk max age %s", elapsed, forcedBulkCoalesceMaxAge)
	}
}

func TestNormalMuxSchedulerCorksSingleTinyNormalFrame(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{t: &Tunnel{ChunkSize: muxMaxBatch}}
	lane := newMuxLane(mux, 0)
	frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 5, Payload: []byte("tiny")}
	if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
		t.Fatalf("enqueueNormalFrame: %v", err)
	}

	started := time.Now()
	frames, ok := lane.takeNormalBatch(ctx)
	elapsed := time.Since(started)
	if !ok {
		t.Fatal("takeNormalBatch returned !ok")
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if elapsed < muxNormalSmallBatchDelay-10*time.Millisecond {
		t.Fatalf("takeNormalBatch elapsed = %s, want cork delay around %s", elapsed, muxNormalSmallBatchDelay)
	}
}

func TestNormalMuxSchedulerCorkBatchesTinyNormalFrames(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{t: &Tunnel{ChunkSize: muxMaxBatch}}
	lane := newMuxLane(mux, 0)
	first := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 5, Payload: []byte("a")}
	second := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 1, Seq: 6, Payload: []byte("b")}
	if err := lane.enqueueNormalFrame(ctx, first); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	go func() {
		time.Sleep(muxNormalSmallBatchDelay / 3)
		_ = lane.enqueueNormalFrame(ctx, second)
	}()

	frames, ok := lane.takeNormalBatch(ctx)
	if !ok {
		t.Fatal("takeNormalBatch returned !ok")
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
}

func TestNormalMuxSchedulerWindowWithMaxBatchObjects(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		recvNormalReady:       make(chan muxStreamKey, muxNormalStreamInflight+1),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalActiveBytes: map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
	}
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 9}
	for seq := uint64(1); seq <= uint64(muxNormalStreamInflight+1); seq++ {
		meta := muxObjectMeta{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: seq, PlainBytes: muxMaxBatch}
		if !mux.enqueueNormalMuxObject(ctx, meta) {
			t.Fatalf("enqueue seq %d failed", seq)
		}
	}
	for want := uint64(1); want <= uint64(muxNormalStreamInflight); want++ {
		select {
		case ready := <-mux.recvNormalReady:
			got, ok := mux.takeNormalMuxObject(ctx, ready)
			if !ok {
				t.Fatal("ready stream had no object")
			}
			if got.Seq != want {
				t.Fatalf("got seq %d, want %d", got.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
	select {
	case ready := <-mux.recvNormalReady:
		t.Fatalf("received extra ready token while max-batch byte window is full: %+v", ready)
	default:
	}
}

type trickleDeadlineReader struct {
	initialChunk int
	nextChunk    int
	nextDelay    time.Duration

	mu       sync.Mutex
	deadline time.Time
	reads    int
}

func (r *trickleDeadlineReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.reads++
	readIndex := r.reads
	deadline := r.deadline
	r.mu.Unlock()

	chunk := r.nextChunk
	delay := r.nextDelay
	if readIndex == 1 {
		chunk = r.initialChunk
		delay = 0
	}
	if chunk <= 0 {
		chunk = 1
	}
	if chunk > len(p) {
		chunk = len(p)
	}
	if delay > 0 {
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, timeoutError{}
			}
			if remaining < delay {
				time.Sleep(remaining)
				return 0, timeoutError{}
			}
		}
		time.Sleep(delay)
	}
	for i := 0; i < chunk; i++ {
		p[i] = byte(i)
	}
	return chunk, nil
}

func (r *trickleDeadlineReader) SetReadDeadline(deadline time.Time) error {
	r.mu.Lock()
	r.deadline = deadline
	r.mu.Unlock()
	return nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

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

func TestBypassExitProxyForLoopbackTargets(t *testing.T) {
	for _, target := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if !bypassExitProxy(target) {
			t.Fatalf("target %q should bypass exit proxy", target)
		}
	}
	for _, target := range []string{"example.com:443", "10.0.0.1:80"} {
		if bypassExitProxy(target) {
			t.Fatalf("target %q should not bypass exit proxy", target)
		}
	}
}

func TestExitTargetIPv4Preference(t *testing.T) {
	tests := []struct {
		target       string
		family       string
		wantPrimary  string
		wantFallback string
	}{
		{target: "example.com:443", family: "prefer_ipv4", wantPrimary: "tcp4", wantFallback: "tcp"},
		{target: "127.0.0.1:8080", family: "prefer_ipv4", wantPrimary: "tcp4", wantFallback: "tcp"},
		{target: "[::1]:8080", family: "prefer_ipv4", wantPrimary: "", wantFallback: "tcp"},
		{target: "2001:db8::1", family: "prefer_ipv4", wantPrimary: "", wantFallback: "tcp"},
		{target: "[::1]:8080", family: "ipv4_only", wantPrimary: "tcp4", wantFallback: ""},
		{target: "127.0.0.1:8080", family: "ipv6_only", wantPrimary: "tcp6", wantFallback: ""},
	}
	for _, tt := range tests {
		gotPrimary, gotFallback := exitDialNetworks(tt.target, tt.family)
		if gotPrimary != tt.wantPrimary || gotFallback != tt.wantFallback {
			t.Fatalf("exitDialNetworks(%q, %q) = (%q, %q), want (%q, %q)", tt.target, tt.family, gotPrimary, gotFallback, tt.wantPrimary, tt.wantFallback)
		}
	}
	for _, target := range []string{"example.com:443", "127.0.0.1:8080", "localhost"} {
		if !targetSupportsIPFamilyPreference(target, "ipv4") {
			t.Fatalf("target %q should allow IPv4 preference", target)
		}
	}
	for _, target := range []string{"[::1]:8080", "2001:db8::1"} {
		if targetSupportsIPFamilyPreference(target, "ipv4") {
			t.Fatalf("target %q should not force IPv4 preference", target)
		}
	}
}

func TestActivePollDelayStaysAtBaseDuringActiveStreams(t *testing.T) {
	mux := &driveMux{
		role: "client",
		t:    &Tunnel{PollInterval: 100 * time.Millisecond},
	}
	mux.active.Add(1)
	if got := mux.pollDelay(); got != 100*time.Millisecond {
		t.Fatalf("active poll delay = %s, want base interval", got)
	}
}

func TestMuxListFreshSinceAdvancesWithLookback(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	mux := &driveMux{startedAt: startedAt, listSince: startedAt}

	newest := startedAt.Add(2 * time.Minute)
	mux.advanceListSince([]ObjectInfo{{Updated: newest.Format(time.RFC3339Nano)}})
	want := newest.Add(-muxListLookback)
	if got := mux.listFreshSince(); !got.Equal(want) {
		t.Fatalf("list since = %s, want %s", got, want)
	}

	mux.advanceListSince([]ObjectInfo{{Updated: startedAt.Add(time.Minute).Format(time.RFC3339Nano)}})
	if got := mux.listFreshSince(); !got.Equal(want) {
		t.Fatalf("list since moved backward to %s, want %s", got, want)
	}
}

func TestMuxListFreshSinceDoesNotAdvanceWhenFreshListIsTruncated(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	since := startedAt.Add(10 * time.Second)
	store := &freshStatusStore{result: ObjectListInfo{Objects: []ObjectInfo{{Updated: startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)}}, Truncated: true, NextPageToken: "page-17"}}
	mux := &driveMux{
		t:         &Tunnel{Data: store},
		startedAt: startedAt,
		listSince: since,
	}

	if _, err := mux.listRecvMuxObjects(context.Background(), "muxv4/session/down/client/run/"); err != nil {
		t.Fatal(err)
	}
	if got := mux.listFreshSince(); !got.Equal(since) {
		t.Fatalf("list since = %s, want unchanged %s after truncated fresh list", got, since)
	}
	if _, err := mux.listRecvMuxObjects(context.Background(), "muxv4/session/down/client/run/"); err != nil {
		t.Fatal(err)
	}
	if len(store.pageCalls) < 2 || store.pageCalls[0] != "" || store.pageCalls[1] != "page-17" {
		t.Fatalf("page calls = %#v, want second poll to resume from truncation token", store.pageCalls)
	}
}

func TestMuxPollContinuesWhenFreshListHasNextPageToken(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	store := &freshStatusStore{result: ObjectListInfo{Truncated: true, NextPageToken: "page-2"}}
	mux := &driveMux{
		t:         &Tunnel{Data: store, ClientID: "client-a", RunID: "run-a", PollInterval: 100 * time.Millisecond},
		role:      "client",
		recvDir:   DirectionDown,
		startedAt: startedAt,
		listSince: startedAt,
		seen:      map[string]struct{}{},
		queued:    map[string]struct{}{},
	}
	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("poll should continue immediately when a fresh-list page token remains")
	}
	if !mux.hasListFreshPageToken() {
		t.Fatal("fresh-list page token was not retained")
	}
}

func TestMuxListFreshIncompleteWithoutNextPageDoesNotAdvance(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	newest := startedAt.Add(2 * time.Minute)
	store := &freshStatusStore{result: ObjectListInfo{
		Objects:    []ObjectInfo{{Updated: newest.Format(time.RFC3339Nano)}},
		Truncated:  true,
		Incomplete: true,
	}}
	mux := &driveMux{
		t:         &Tunnel{Data: store},
		startedAt: startedAt,
		listSince: startedAt,
	}
	if _, err := mux.listRecvMuxObjects(context.Background(), "muxv4/session/down/client/run/"); err != nil {
		t.Fatal(err)
	}
	if got := mux.listFreshSince(); !got.Equal(startedAt) {
		t.Fatalf("list since = %s, want unchanged %s after incomplete page without nextPageToken", got, startedAt)
	}
	if _, err := mux.listRecvMuxObjects(context.Background(), "muxv4/session/down/client/run/"); err != nil {
		t.Fatal(err)
	}
	if len(store.pageCalls) < 2 || store.pageCalls[0] != "" || store.pageCalls[1] != "" {
		t.Fatalf("page calls = %#v, want retry from the same freshness cursor without a page token", store.pageCalls)
	}
}

func TestMuxPollRewindsFreshCursorWhenNormalEnqueueBackpressured(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	clientID := "client-a"
	runID := "run-a"
	olderUpdated := startedAt.Add(2 * time.Minute)
	newerUpdated := startedAt.Add(4 * time.Minute)
	blockedName := muxObjectName(sid, DirectionDown, clientID, runID, "epoch-a", 1, 0, 1, 1, 1, 1, muxMinBatch, false)
	priorityName := muxObjectName(sid, DirectionDown, clientID, runID, "epoch-a", 2, 0, 2, 1, 1, 1, 128, true)
	store := &freshStatusStore{result: ObjectListInfo{Objects: []ObjectInfo{
		{Name: blockedName, Updated: olderUpdated.Format(time.RFC3339Nano)},
		{Name: priorityName, Updated: newerUpdated.Format(time.RFC3339Nano)},
	}}}
	mux, err := newDriveMux(&Tunnel{
		Data:         store,
		SessionID:    sid,
		ClientID:     clientID,
		RunID:        runID,
		PollInterval: time.Second,
	}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.startedAt = startedAt
	mux.listSince = startedAt
	blockedKey := muxStreamKey{ClientID: clientID, RunID: runID, StreamID: 1}
	mux.recvNormalQueuedBytes[blockedKey] = muxNormalReceiveQueueBytes
	mux.recvNormalQueuedTotal = muxNormalReceiveQueueBytes

	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("poll should report the enqueued priority object")
	}
	want := olderUpdated.Add(-muxRepairListLookback)
	if got := mux.listFreshSince(); !got.Equal(want) {
		t.Fatalf("list since = %s, want rewind to %s after blocked enqueue", got, want)
	}
	if mux.isKnown(blockedName) {
		t.Fatal("blocked object stayed claimed; it must remain rediscoverable")
	}
}

func TestMuxPollBackpressureRewindClearsFreshPageToken(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	clientID := "client-a"
	runID := "run-a"
	updated := startedAt.Add(2 * time.Minute)
	blockedName := muxObjectName(sid, DirectionDown, clientID, runID, "epoch-a", 1, 0, 1, 1, 1, 1, muxMinBatch, false)
	store := &freshStatusStore{result: ObjectListInfo{
		Objects:       []ObjectInfo{{Name: blockedName, Updated: updated.Format(time.RFC3339Nano)}},
		Truncated:     true,
		NextPageToken: "page-2",
	}}
	mux, err := newDriveMux(&Tunnel{
		Data:         store,
		SessionID:    sid,
		ClientID:     clientID,
		RunID:        runID,
		PollInterval: time.Second,
	}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.startedAt = startedAt
	mux.listSince = startedAt
	blockedKey := muxStreamKey{ClientID: clientID, RunID: runID, StreamID: 1}
	mux.recvNormalQueuedBytes[blockedKey] = muxNormalReceiveQueueBytes
	mux.recvNormalQueuedTotal = muxNormalReceiveQueueBytes

	mux.pollMuxObjects(context.Background())
	if mux.hasListFreshPageToken() {
		t.Fatal("backpressure rewind kept a stale page token and could skip the blocked object")
	}
	if got := mux.listFreshSince(); !got.Equal(startedAt) {
		t.Fatalf("list since = %s, want unchanged %s when rewind target is newer", got, startedAt)
	}
}

func TestMuxReceiveGapRepairRewindsFreshCursor(t *testing.T) {
	ctx := context.Background()
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	mux, err := newDriveMux(&Tunnel{PollInterval: time.Second}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.startedAt = startedAt
	mux.listSince = startedAt.Add(5 * time.Minute)
	stream := mux.registerStream(42, "client-a", "run-a", nil)
	defer stream.close()
	key := stream.key()
	stream.mu.Lock()
	stream.recvPending[2] = muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, Payload: make([]byte, muxStreamPauseBytes)}
	stream.recvPendingBytes = muxStreamPauseBytes
	stream.mu.Unlock()

	updated := startedAt.Add(3 * time.Minute)
	meta := muxObjectMeta{
		Name:            "blocked-gap",
		ClientID:        key.ClientID,
		RunID:           key.RunID,
		StreamID:        key.StreamID,
		Seq:             9,
		PlainBytes:      muxMinBatch,
		FrameMinSeq:     3,
		FrameMaxSeq:     3,
		FrameRangeKnown: true,
		Updated:         updated,
	}
	if !mux.enqueueNormalMuxObject(ctx, meta) {
		t.Fatal("enqueue failed")
	}
	want := updated.Add(-muxRepairListLookback)
	if got := mux.listFreshSince(); !got.Equal(want) {
		t.Fatalf("list since = %s, want enqueue-time repair rewind to %s", got, want)
	}
}

func TestMuxPollSplitsPriorityAndNormalFreshLists(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	clientID := "client-a"
	runID := "run-a"
	priorityName := muxObjectName(sid, DirectionDown, clientID, runID, "epoch-a", 1, 0, 1, 1, 1, 1, 128, true)
	normalName := muxObjectName(sid, DirectionDown, clientID, runID, "epoch-a", 2, 0, 2, 1, 1, 1, muxMinBatch, false)
	updated := startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	store := &classFreshStatusStore{
		MemoryStore: NewMemoryStore(),
		priority:    ObjectListInfo{Objects: []ObjectInfo{{Name: priorityName, Updated: updated}}},
		normal:      ObjectListInfo{Objects: []ObjectInfo{{Name: normalName, Updated: updated}}},
	}
	mux, err := newDriveMux(&Tunnel{
		Data:         store,
		SessionID:    sid,
		ClientID:     clientID,
		RunID:        runID,
		PollInterval: time.Second,
	}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.startedAt = startedAt
	mux.listSince = startedAt
	mux.priorityListSince = startedAt

	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("priority poll should enqueue a p0 object")
	}
	if got := len(mux.recvUrgent); got != 1 {
		t.Fatalf("urgent queue = %d, want 1", got)
	}
	if len(store.calls) != 1 || !containsString(store.calls[0].contains, "/p0/") {
		t.Fatalf("first list call = %#v, want priority class filter", store.calls)
	}

	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("normal poll should enqueue a p1 object after priority is known")
	}
	if got := len(mux.recvNormalFlows); got != 1 {
		t.Fatalf("normal flow count = %d, want 1", got)
	}
	if len(store.calls) < 3 || !containsString(store.calls[2].contains, "/p1/") {
		t.Fatalf("third list call = %#v, want normal class filter after priority scan", store.calls)
	}
}

func TestMuxClassFreshListAdvancesSlidingCursorWhenTruncated(t *testing.T) {
	startedAt := time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC)
	newest := startedAt.Add(2 * time.Minute)
	store := &classFreshStatusStore{
		MemoryStore: NewMemoryStore(),
		normal: ObjectListInfo{
			Objects:   []ObjectInfo{{Name: "muxv4/demo/down/client/run/epoch/p1/s0000000000000001/l00/0000000000000001.f1.b65536", Updated: newest.Format(time.RFC3339Nano)}},
			Truncated: true,
		},
	}
	mux := &driveMux{
		t:         &Tunnel{Data: store},
		startedAt: startedAt,
		listSince: startedAt,
	}
	if _, err := mux.listRecvMuxObjectsByClass(context.Background(), "muxv4/demo/down/client/run/", "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true); err != nil {
		t.Fatal(err)
	}
	want := newest.Add(-muxListLookback)
	if got := mux.listFreshSince(); !got.Equal(want) {
		t.Fatalf("list since = %s, want sliding cursor %s after truncated class list", got, want)
	}
	if mux.hasListFreshPageToken() {
		t.Fatal("class sliding list should not keep a stale page token")
	}
}

func TestMuxMarkSeenCompactionPreservesQueuedClaims(t *testing.T) {
	mux := &driveMux{
		seen:   map[string]struct{}{},
		queued: map[string]struct{}{"inflight": struct{}{}},
	}
	for i := 0; i <= 200000; i++ {
		mux.seen[fmt.Sprintf("seen-%d", i)] = struct{}{}
	}
	mux.markSeen("done")
	if !mux.isKnown("inflight") {
		t.Fatal("queued in-flight claim was lost during seen compaction")
	}
	if !mux.isKnown("done") {
		t.Fatal("newly seen object was lost during seen compaction")
	}
}

func TestNormalReceivePausedCountsInboundBacklog(t *testing.T) {
	mux := &driveMux{t: &Tunnel{}}
	stream := mux.registerStream(1, "client-a", "run-a", nil)
	for i := 0; i < muxStreamInboundPause; i++ {
		stream.inbound <- []byte("x")
	}
	if !mux.normalReceivePaused(stream.key()) {
		t.Fatal("normal receive should pause when inbound writer backlog reaches threshold")
	}
	<-stream.inbound
	if mux.normalReceivePaused(stream.key()) {
		t.Fatal("normal receive stayed paused after inbound backlog dropped below threshold")
	}
}

type freshStatusStore struct {
	BlobStore
	result    ObjectListInfo
	err       error
	pageCalls []string
}

func (s *freshStatusStore) ListFreshStatus(context.Context, string, time.Time) (ObjectListInfo, error) {
	return s.result, s.err
}

func (s *freshStatusStore) ListFreshPageStatus(_ context.Context, _ string, _ time.Time, pageToken string) (ObjectListInfo, error) {
	s.pageCalls = append(s.pageCalls, pageToken)
	return s.result, s.err
}

type classFreshStatusCall struct {
	contains  []string
	pageToken string
	maxPages  int
}

type classFreshStatusStore struct {
	*MemoryStore
	priority ObjectListInfo
	normal   ObjectListInfo
	calls    []classFreshStatusCall
}

func (s *classFreshStatusStore) ListFreshContainsPageStatus(_ context.Context, contains []string, _ time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	s.calls = append(s.calls, classFreshStatusCall{contains: append([]string(nil), contains...), pageToken: pageToken, maxPages: maxPages})
	if containsString(contains, "/p0/") {
		return s.priority, nil
	}
	if containsString(contains, "/p1/") {
		return s.normal, nil
	}
	return ObjectListInfo{}, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestMuxProcessFailureBackoffDoesNotImmediateRequeue(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:                &Tunnel{},
		seen:             map[string]struct{}{},
		queued:           map[string]struct{}{},
		recvUrgent:       make(chan muxObjectMeta, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	meta := muxObjectMeta{Name: "muxv4/test-object", Priority: false}
	if !mux.claimQueued(meta.Name) {
		t.Fatal("initial claim failed")
	}

	mux.retryMuxObject(ctx, meta)

	if !mux.isKnown(meta.Name) {
		t.Fatal("failed object should remain claimed during retry backoff")
	}
	if mux.enqueueMuxObject(ctx, meta) {
		t.Fatal("poll rediscovery should not enqueue an object already waiting for retry")
	}
	select {
	case key := <-mux.recvNormalReady:
		if got, ok := mux.takeNormalMuxObject(ctx, key); ok {
			t.Fatalf("retry enqueued immediately: %+v", got)
		}
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case key := <-mux.recvNormalReady:
		got, ok := mux.takeNormalMuxObject(ctx, key)
		if !ok {
			t.Fatal("retry key had no queued object")
		}
		if got.Attempts != 1 {
			t.Fatalf("retry attempts = %d, want 1", got.Attempts)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("retry was not enqueued after backoff")
	}
}

func TestMuxMixedPriorityObjectNameCarriesAllStreams(t *testing.T) {
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	name := muxObjectNameWithStreamIDs(sid, DirectionDown, "client-a", "run-a", "epoch-a", 0x10, []uint64{0x10, 0x20, 0x30}, 2, 9, 3, 0, 0, 8192, true)
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name, ID: "drive-id"})
	if !ok {
		t.Fatalf("failed to parse mixed mux object name %q", name)
	}
	if meta.StreamID != 0x10 {
		t.Fatalf("primary stream = %x, want 10", meta.StreamID)
	}
	if got, want := len(meta.StreamIDs), 3; got != want {
		t.Fatalf("stream id count = %d, want %d", got, want)
	}
	for i, want := range []uint64{0x10, 0x20, 0x30} {
		if meta.StreamIDs[i] != want {
			t.Fatalf("stream id %d = %x, want %x", i, meta.StreamIDs[i], want)
		}
	}
	if meta.FrameRangeKnown {
		t.Fatal("mixed-stream priority object should not advertise per-stream frame range")
	}
	if got := meta.streamKeys(); len(got) != 3 {
		t.Fatalf("stream keys = %d, want 3", len(got))
	}
}

func TestMuxProcessRetryBudgetTerminalFailureDoesNotRequeue(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:                &Tunnel{CleanupProcessed: true},
		seen:             map[string]struct{}{},
		queued:           map[string]struct{}{},
		closed:           map[muxStreamKey]time.Time{},
		cleanupQueue:     make(chan cleanupTask, 1),
		recvUrgent:       make(chan muxObjectMeta, 1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}
	meta := muxObjectMeta{
		Name:     "muxv4/test-terminal-object",
		ID:       "drive-id",
		ClientID: "client-a",
		RunID:    "run-a",
		StreamID: 7,
		Attempts: muxProcessMaxRetries,
	}
	if !mux.claimQueued(meta.Name) {
		t.Fatal("initial claim failed")
	}

	mux.retryMuxObject(ctx, meta)

	if !mux.isKnown(meta.Name) {
		t.Fatal("terminal failed object should be marked known")
	}
	if !mux.isClosedStream(meta.key()) {
		t.Fatal("terminal failed object should close the affected stream key")
	}
	select {
	case task := <-mux.cleanupQueue:
		if task.name != meta.Name || task.id != meta.ID {
			t.Fatalf("cleanup task = %+v, want failed object cleanup", task)
		}
	default:
		t.Fatal("terminal failed object did not schedule cleanup")
	}
	select {
	case ready := <-mux.recvNormalReady:
		if got, ok := mux.takeNormalMuxObject(ctx, ready); ok {
			t.Fatalf("terminal failed object was requeued: %+v", got)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMuxProcessTerminalFailureClosesAllMixedStreams(t *testing.T) {
	ctx := context.Background()
	mux := &driveMux{
		t:            &Tunnel{CleanupProcessed: true},
		seen:         map[string]struct{}{},
		queued:       map[string]struct{}{},
		closed:       map[muxStreamKey]time.Time{},
		cleanupQueue: make(chan cleanupTask, 1),
	}
	meta := muxObjectMeta{
		Name:      "muxv4/test-mixed-terminal-object",
		ID:        "drive-id",
		ClientID:  "client-a",
		RunID:     "run-a",
		StreamID:  7,
		StreamIDs: []uint64{7, 8, 9},
		Attempts:  muxProcessMaxRetries,
	}

	mux.failMuxObject(ctx, meta, fmt.Errorf("decode failed"))

	for _, streamID := range meta.StreamIDs {
		key := muxStreamKey{ClientID: meta.ClientID, RunID: meta.RunID, StreamID: streamID}
		if !mux.isClosedStream(key) {
			t.Fatalf("stream %d was not closed after mixed object failure", streamID)
		}
	}
	if !mux.isKnown(meta.Name) {
		t.Fatal("failed mixed object should be marked known")
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
