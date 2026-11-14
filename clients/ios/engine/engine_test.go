package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func testProfile(t *testing.T) string {
	t.Helper()
	secret, err := skirk.RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &skirk.Config{Secret: secret, SessionID: "00112233445566778899aabbccddeeff"}
	cfg.ApplyDefaults()
	profile, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func testStore(context.Context, *skirk.Config) (skirk.BlobStore, error) {
	return skirk.NewMemoryStore(), nil
}

func TestPinnedGoogleIP(t *testing.T) {
	for _, tt := range []struct {
		mode string
		want string
	}{
		{"real_pinned", "216.239.38.120"},
		{"google_front_pinned", "216.239.38.120"},
		{"google_front_h1_pinned", "216.239.38.120"},
		{"direct", ""},
		{"google_front", ""},
		{"google_front_h1", ""},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			cfg, err := skirk.ParseConfig([]byte(testProfile(t)))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Route.Mode = tt.mode
			profile, err := skirk.EncodeConfigText(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := PinnedGoogleIP(profile)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("pinned Google IP = %q, want %q", got, tt.want)
			}
		})
	}
	cfg, err := skirk.ParseConfig([]byte(testProfile(t)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Route.GoogleIP = "192.0.2.42"
	customProfile, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := PinnedGoogleIP(customProfile); err != nil || got != "192.0.2.42" {
		t.Fatalf("custom pinned IP = %q, error %v", got, err)
	}
	cfg.Route.Proxy = "socks5h://127.0.0.1:1080"
	proxiedProfile, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := PinnedGoogleIP(proxiedProfile); err != nil || got != "" {
		t.Fatalf("proxied pinned IP = %q, error %v; want empty", got, err)
	}
	if _, err := PinnedGoogleIP("invalid"); err == nil {
		t.Fatal("invalid profile should fail")
	}
}

func TestSelectedGoogleRouteOverridesProfile(t *testing.T) {
	profile := testProfile(t)
	for _, tt := range []struct {
		mode   string
		pinned string
	}{
		{mode: "google_front_pinned", pinned: "216.239.38.120"},
		{mode: "direct", pinned: ""},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			address, err := PinnedGoogleIPWithRoute(profile, tt.mode)
			if err != nil || address != tt.pinned {
				t.Fatalf("pinned address = %q, error %v; want %q", address, err, tt.pinned)
			}
			var selected string
			err = startWithRoute(profile, "ios-route-test", freePort(t), 0, tt.mode,
				func(_ context.Context, cfg *skirk.Config) (skirk.BlobStore, error) {
					selected = cfg.Route.Mode
					return skirk.NewMemoryStore(), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = Stop() })
			if selected != tt.mode {
				t.Fatalf("engine route = %q, want %q", selected, tt.mode)
			}
			if err := Stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := PinnedGoogleIPWithRoute(profile, "unknown"); err == nil {
		t.Fatal("unknown Google route should fail")
	}
	cfg, err := skirk.ParseConfig([]byte(profile))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Route.Mode = "google_front"
	customProfile, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if address, err := PinnedGoogleIPWithRoute(customProfile, "profile"); err != nil || address != "" {
		t.Fatalf("profile route address = %q, error %v; want no pinned IP", address, err)
	}
	var selected string
	err = startWithRoute(customProfile, "ios-profile-route-test", freePort(t), 0, "profile",
		func(_ context.Context, cfg *skirk.Config) (skirk.BlobStore, error) {
			selected = cfg.Route.Mode
			return skirk.NewMemoryStore(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer Stop()
	if selected != "google_front" {
		t.Fatalf("profile route = %q, want google_front", selected)
	}
}

func TestStartStopLoopbackListeners(t *testing.T) {
	defer Stop()
	socksPort, httpPort := freePort(t), freePort(t)
	if socksPort == httpPort {
		httpPort = freePort(t)
	}
	if err := start(testProfile(t), "ios-test-client", socksPort, httpPort, testStore); err != nil {
		t.Fatal(err)
	}
	if !IsRunning() {
		t.Fatal("bridge should report running")
	}
	for _, port := range []int{socksPort, httpPort} {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("port %d is not listening: %v", port, err)
		}
		_ = conn.Close()
	}
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if IsRunning() {
		t.Fatal("bridge still reports running after Stop")
	}
	for _, port := range []int{socksPort, httpPort} {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("port %d remains open after Stop", port)
		}
	}
}

func TestBridgeProxiesThroughMemoryExit(t *testing.T) {
	defer Stop()
	profile := testProfile(t)
	cfg, err := skirk.ParseConfig([]byte(profile))
	if err != nil {
		t.Fatal(err)
	}
	store := skirk.NewMemoryStore()
	exit, err := skirk.NewTunnel(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exit.ServeExit(ctx) }()
	socksPort, httpPort := freePort(t), freePort(t)
	if socksPort == httpPort {
		httpPort = freePort(t)
	}
	if err := start(profile, "ios-test-client", socksPort, httpPort, func(context.Context, *skirk.Config) (skirk.BlobStore, error) {
		return store, nil
	}); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("through-skirk"))
	}))
	defer origin.Close()
	for _, proxy := range []string{
		"socks5h://" + loopbackAddr(socksPort),
		"http://" + loopbackAddr(httpPort),
	} {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 15 * time.Second}
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("request through %s: %v", proxyURL.Scheme, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(body) != "through-skirk" {
			t.Fatalf("response through %s = %q, error %v", proxyURL.Scheme, body, err)
		}
	}
}

func TestStartRejectsInvalidPortAndDuplicateStart(t *testing.T) {
	defer Stop()
	profile := testProfile(t)
	if err := start(profile, "ios-test-client", 0, 0, testStore); err == nil {
		t.Fatal("zero SOCKS port should be rejected")
	}
	port := freePort(t)
	if err := start(profile, "ios-test-client", port, port, testStore); err == nil {
		t.Fatal("duplicate ports should be rejected")
	}
	if err := start(profile, "ios-test-client", port, 0, testStore); err != nil {
		t.Fatal(err)
	}
	if err := start(profile, "ios-test-client", freePort(t), 0, testStore); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate start error = %v", err)
	}
}

func TestStopCancelsPendingStart(t *testing.T) {
	defer Stop()
	storeReady := make(chan struct{})
	startErr := make(chan error, 1)
	profile, port := testProfile(t), freePort(t)
	go func() {
		startErr <- start(profile, "ios-test-client", port, 0, func(ctx context.Context, _ *skirk.Config) (skirk.BlobStore, error) {
			close(storeReady)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-storeReady:
	case <-time.After(3 * time.Second):
		t.Fatal("start did not reach store initialization")
	}
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-startErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start error = %v, want context cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending start did not cancel")
	}
}
