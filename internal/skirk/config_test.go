package skirk

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthConfigRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := url.Values{
			"client_id":     {"client-id"},
			"client_secret": {"client-secret"},
			"refresh_token": {"refresh-token"},
			"grant_type":    {"refresh_token"},
		}
		for key, values := range want {
			if got := r.PostForm.Get(key); got != values[0] {
				t.Fatalf("%s = %q, want %q", key, got, values[0])
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	token, err := (AuthConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RefreshToken: "refresh-token",
		TokenURL:     server.URL,
	}).Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-token" {
		t.Fatalf("token = %q, want access-token", token)
	}
}

func tokenSOCKSProxyFixture(t *testing.T) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	targets := make(chan string, 4)
	proxy := &SOCKSServer{Handler: func(_ context.Context, target string, client net.Conn) {
		targets <- target
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			t.Errorf("SOCKS upstream dial: %v", err)
			return
		}
		defer upstream.Close()
		go io.Copy(upstream, client)
		_, _ = io.Copy(client, upstream)
	}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxy.handle(context.Background(), conn)
		}
	}()
	return "socks5h://" + listener.Addr().String(), targets
}

func TestCustomTokenURLHonorsSOCKSProxyInDirectAndFrontModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/custom-token" {
			t.Errorf("custom token request method=%q path=%q", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("refresh_token") != "test-refresh-token" {
			t.Errorf("custom token POST body: %v", err)
		}
		_, _ = io.WriteString(w, `{"access_token":"custom-access-token","expires_in":3600}`)
	}))
	defer server.Close()
	proxyURL, targets := tokenSOCKSProxyFixture(t)
	for _, mode := range []string{"direct", "google_front_h3_pinned"} {
		t.Run(mode, func(t *testing.T) {
			auth := AuthConfig{ClientID: "test-client", RefreshToken: "test-refresh-token", TokenURL: server.URL + "/custom-token"}
			token, err := auth.refreshAccessToken(context.Background(), RouteConfig{Mode: mode, Proxy: proxyURL})
			if err != nil || token.Token != "custom-access-token" {
				t.Fatalf("custom token=%+v err=%v", token, err)
			}
			select {
			case target := <-targets:
				if target != strings.TrimPrefix(server.URL, "http://") {
					t.Fatalf("SOCKS target=%q, want token endpoint", target)
				}
			case <-time.After(time.Second):
				t.Fatal("custom token request bypassed configured SOCKS proxy")
			}
		})
	}
}

func TestCustomTokenURLDoesNotFollowCredentialRedirect(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", other.URL+"/receive-credentials")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	proxyURL, targets := tokenSOCKSProxyFixture(t)
	auth := AuthConfig{ClientID: "test-client", RefreshToken: "test-refresh-token", TokenURL: server.URL}
	if _, err := auth.refreshAccessToken(context.Background(), RouteConfig{Mode: "google_front", Proxy: proxyURL}); err == nil {
		t.Fatal("redirect response was accepted as a token")
	}
	select {
	case target := <-targets:
		if target != strings.TrimPrefix(server.URL, "http://") {
			t.Fatalf("SOCKS target=%q, want original endpoint", target)
		}
	case <-time.After(time.Second):
		t.Fatal("custom token request bypassed configured SOCKS proxy")
	}
	if redirected.Load() != 0 {
		t.Fatal("OAuth refresh credentials were resent to redirect target")
	}
	select {
	case target := <-targets:
		t.Fatalf("SOCKS followed token redirect to %q", target)
	default:
	}
}

func TestConfigDefaultsToGoogleFrontButKeepsExplicitDirect(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if cfg.Route.Mode != "google_front_pinned" {
		t.Fatalf("default route = %q, want google_front_pinned", cfg.Route.Mode)
	}
	cfg.Route.Mode = "direct"
	cfg.ApplyDefaults()
	if cfg.Route.Mode != "direct" {
		t.Fatalf("explicit route = %q, want direct", cfg.Route.Mode)
	}
}

func TestConfigAcceptsExplicitGoogleFrontH3Pinned(t *testing.T) {
	cfg := &Config{Secret: strings.Repeat("a", 64), Route: RouteConfig{Mode: "google_front_h3_pinned"}}
	cfg.ApplyDefaults()
	if cfg.Route.GoogleIP == "" {
		t.Fatal("H3 pinned route requires the default pinned Google IP")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit H3 route rejected: %v", err)
	}
	if cfg.Route.Mode != "google_front_h3_pinned" {
		t.Fatalf("route mode=%q", cfg.Route.Mode)
	}
}

func TestConfigValidatesExitProxy(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Tunnel: TunnelConfig{
			ExitProxy: "socks5h://127.0.0.1:40000",
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Tunnel.ExitProxy = "ftp://127.0.0.1:21"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exit_proxy scheme") {
		t.Fatalf("err = %v, want exit_proxy scheme error", err)
	}
}

func TestConfigValidatesClientNamespace(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Client: ClientConfig{ID: "client-a", RunID: "run-a"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Client.ID = "bad/client"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "config.client.id") {
		t.Fatalf("err = %v, want client id validation error", err)
	}
	cfg.Client.ID = "client-a"
	cfg.Client.RunID = "x"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "config.client.run_id") {
		t.Fatalf("err = %v, want run id validation error", err)
	}
}

func TestValidateGoogleProjectID(t *testing.T) {
	for _, id := range []string{"abcdef", "a-1234", "test-project-123456"} {
		if err := ValidateGoogleProjectID(id); err != nil {
			t.Errorf("valid project ID %q: %v", id, err)
		}
	}
	for _, id := range []string{"", "abcde", "1abcde", "Abcdef", "abcde-", "abc_def", " abcdef", "abcdef\n", strings.Repeat("a", 31)} {
		if err := ValidateGoogleProjectID(id); err == nil {
			t.Errorf("accepted invalid project ID %q", id)
		}
	}

	cfg := &Config{Secret: strings.Repeat("a", 64), Drive: DriveConfig{QuotaProject: "test-project-123456"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid quota project rejected: %v", err)
	}
	cfg.Drive.QuotaProject = "bad_project"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "config.drive.quota_project") {
		t.Fatalf("invalid quota project error = %v", err)
	}
}

func TestConfigNormalListPageSizeChoices(t *testing.T) {
	cfg := &Config{Secret: strings.Repeat("a", 64)}
	cfg.ApplyDefaults()
	for _, size := range []int{0, 100, 500, 1000} {
		cfg.Drive.NormalListPageSize = size
		if err := cfg.Validate(); err != nil {
			t.Errorf("page size %d rejected: %v", size, err)
		}
	}
	for _, size := range []int{-1, 1, 999, 1001} {
		cfg.Drive.NormalListPageSize = size
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "config.drive.normal_list_page_size") {
			t.Errorf("page size %d error = %v", size, err)
		}
	}
}

func TestConfigUploadHedgeDelayIsOptInAndBounded(t *testing.T) {
	cfg := &Config{Secret: strings.Repeat("a", 64)}
	cfg.ApplyDefaults()
	if cfg.Drive.UploadHedgeDelayMS != 0 {
		t.Fatalf("default upload hedge delay = %d, want disabled", cfg.Drive.UploadHedgeDelayMS)
	}
	if cfg.Drive.SeparateUploadPool {
		t.Fatal("separate upload pool enabled by default")
	}
	for _, delay := range []int{0, 1000, 3000, 10000} {
		cfg.Drive.UploadHedgeDelayMS = delay
		if err := cfg.Validate(); err != nil {
			t.Errorf("delay %d rejected: %v", delay, err)
		}
	}
	for _, delay := range []int{-1, 1, 999, 10001} {
		cfg.Drive.UploadHedgeDelayMS = delay
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "upload_hedge_delay_ms") {
			t.Errorf("delay %d error = %v, want hedge validation", delay, err)
		}
	}
	var decoded struct {
		Drive DriveConfig `json:"drive"`
	}
	if err := json.Unmarshal([]byte(`{"drive":{"upload_hedge_delay_ms":3000}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Drive.UploadHedgeDelayMS != 3000 {
		t.Fatalf("decoded hedge delay = %d, want 3000", decoded.Drive.UploadHedgeDelayMS)
	}
	if err := json.Unmarshal([]byte(`{"drive":{"separate_upload_pool":true}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Drive.SeparateUploadPool {
		t.Fatal("separate upload pool JSON option was ignored")
	}
	if err := json.Unmarshal([]byte(`{"drive":{"separate_bulk_media_pool":true}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Drive.SeparateBulkMediaPool {
		t.Fatal("separate bulk media pool JSON option was ignored")
	}
}

func TestConfigRejectsNonPositiveFixedPollInterval(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
	}
	cfg.ApplyDefaults()
	cfg.Tunnel.Profile = "fixed"
	cfg.Tunnel.PollIntervalMS = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "poll_interval_ms") {
		t.Fatalf("err = %v, want poll_interval_ms validation error", err)
	}
}

func TestConfigAutoProfileClampsFastPollInterval(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Tunnel: TunnelConfig{
			Profile:        "auto",
			PollIntervalMS: 750,
		},
	}
	cfg.ApplyDefaults()
	if got, want := cfg.PollInterval(), time.Second; got != want {
		t.Fatalf("PollInterval() = %s, want %s", got, want)
	}
}

func TestConfigFixedProfileAllowsFastPollInterval(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Tunnel: TunnelConfig{
			Profile:        "fixed",
			PollIntervalMS: 750,
		},
	}
	cfg.ApplyDefaults()
	if got, want := cfg.PollInterval(), 750*time.Millisecond; got != want {
		t.Fatalf("PollInterval() = %s, want %s", got, want)
	}
}

func TestConfigRejectsExperimentalTransport(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Tunnel: TunnelConfig{
			Transport: "experimental",
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "muxv4") {
		t.Fatalf("err = %v, want transport validation error mentioning muxv4", err)
	}
	if got := normalizeMuxTransport(cfg.Tunnel.Transport); got != "muxv4" {
		t.Fatalf("normalizeMuxTransport(experimental) = %q, want muxv4", got)
	}
}

func TestAccessTokenSourceRefreshesBeforeExpiry(t *testing.T) {
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := atomic.AddInt32(&count, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token-" + strconv.Itoa(int(token)),
			"expires_in":   300,
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	source := NewAccessTokenSource(AuthConfig{
		ClientID:     "client-id",
		RefreshToken: "refresh-token",
		TokenURL:     server.URL,
	}, RouteConfig{Mode: "direct"})

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("token was cached inside proactive refresh margin: first=%q second=%q", first, second)
	}
	if count != 2 {
		t.Fatalf("refresh count = %d, want 2", count)
	}
}

func TestAccessTokenSourceUsesCachedTokenDuringHostileRefresh(t *testing.T) {
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := atomic.AddInt32(&count, 1)
		expiresIn := 900
		if token > 1 {
			expiresIn = 3600
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token-" + strconv.Itoa(int(token)),
			"expires_in":   expiresIn,
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	source := NewAccessTokenSource(AuthConfig{
		ClientID:     "client-id",
		RefreshToken: "refresh-token",
		TokenURL:     server.URL,
	}, RouteConfig{Mode: "google_front"})

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "access-token-1" || second != first {
		t.Fatalf("cached hostile token behavior first=%q second=%q", first, second)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&count) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&count); got < 2 {
		t.Fatalf("background refresh count = %d, want at least 2", got)
	}
	third, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third != "access-token-2" {
		t.Fatalf("third token = %q, want background-refreshed token", third)
	}
}

func TestAccessTokenSourceLate401KeepsNewToken(t *testing.T) {
	source := NewAccessTokenSource(AuthConfig{}, RouteConfig{})
	source.token = "new-token"
	source.expiresAt = time.Now().Add(time.Hour)
	source.invalidateRejectedToken("old-token")
	if token, err := source.Token(context.Background()); err != nil || token != "new-token" {
		t.Fatalf("late 401 discarded newer token: token=%q err=%v", token, err)
	}
	source.invalidateRejectedToken("new-token")
	if source.token != "" {
		t.Fatal("rejected current token remained cached")
	}
}

func TestAccessTokenSourceOldBackgroundRefreshCannotOverwriteNewGrant(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := "new-token"
		if requests.Add(1) == 1 {
			close(entered)
			<-release
			token = "superseded-background-token"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 3600})
	}))
	defer server.Close()
	source := NewAccessTokenSource(AuthConfig{ClientID: "test", RefreshToken: "test", TokenURL: server.URL}, RouteConfig{Mode: "direct"})
	source.token, source.refreshing = "rejected-token", true
	done := make(chan struct{})
	go func() {
		defer close(done)
		source.refreshInBackground(0)
	}()
	<-entered
	source.invalidateRejectedToken("rejected-token")
	token, err := source.Token(context.Background())
	close(release)
	<-done
	if err != nil || token != "new-token" {
		t.Fatalf("foreground refresh token=%q error=%v", token, err)
	}
	if got, err := source.Token(context.Background()); err != nil || got != "new-token" {
		t.Fatalf("old background result replaced new token: token=%q error=%v", got, err)
	}
}

func TestOAuthTokenAttemptsStayWithinUserSelectedRoute(t *testing.T) {
	route := RouteConfig{
		Mode:           "google_front",
		Proxy:          "socks5h://127.0.0.1:11093",
		GoogleIP:       "216.239.38.120",
		TimeoutSeconds: 240,
	}
	for _, mode := range []string{"google_front", "google_front_pinned", "google_front_h1", "google_front_h1_pinned", "google_front_h3_pinned"} {
		withMode := route
		withMode.Mode = mode
		attempts := oauthTokenAttempts(withMode)
		wantCount := 2
		if mode == "google_front_h3_pinned" {
			wantCount = 3
		}
		if len(attempts) != wantCount {
			t.Fatalf("route=%s attempt count=%d, want %d", mode, len(attempts), wantCount)
		}
		if attempts[0].host != "oauth2.googleapis.com" || attempts[len(attempts)-1].host != "accounts.google.com" {
			t.Fatalf("route=%s endpoints=%+v", mode, attempts)
		}
		for _, attempt := range attempts {
			if !isGoogleFrontRoute(attempt.route.Mode) || attempt.route.Proxy != route.Proxy {
				t.Fatalf("route=%s silently changed route/proxy: %+v", mode, attempt)
			}
		}
		if mode == "google_front_h3_pinned" && attempts[1].route.Mode != "google_front_pinned" {
			t.Fatalf("H3 route missing fronted H2 endpoint fallback: %+v", attempts)
		}
	}
	for _, mode := range []string{"direct", "real_pinned"} {
		withMode := route
		withMode.Mode = mode
		attempts := oauthTokenAttempts(withMode)
		if len(attempts) != 2 || attempts[1].host != "accounts.google.com" {
			t.Fatalf("direct route=%s attempts=%+v", mode, attempts)
		}
		for _, attempt := range attempts {
			if attempt.route.Mode != mode || attempt.route.Proxy != route.Proxy {
				t.Fatalf("direct route=%s silently changed route/proxy: %+v", mode, attempt)
			}
		}
	}
}

func TestTerminalOAuthTokenError(t *testing.T) {
	if !terminalOAuthTokenError(http.StatusBadRequest, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("invalid_grant should be terminal")
	}
	if terminalOAuthTokenError(http.StatusNotFound, []byte(`<html>wrong edge</html>`)) {
		t.Fatal("wrong-edge response should allow another token attempt")
	}
	_, err := parseOAuthTokenResponse(http.StatusBadRequest, []byte(`{"error":"invalid_grant","error_description":"expired"}`))
	if !IsTerminalOAuthTokenError(err) {
		t.Fatalf("parsed error = %v, want terminal OAuth token error", err)
	}
	_, err = parseOAuthTokenResponse(http.StatusBadRequest, []byte(`{"error":"temporarily_unavailable"}`))
	if IsTerminalOAuthTokenError(err) {
		t.Fatalf("parsed error = %v, want retryable OAuth token error", err)
	}
}

func TestTextConfigRoundTrip(t *testing.T) {
	cfg := &Config{
		Secret:    "0123456789abcdef0123456789abcdef",
		SessionID: "session",
		Auth: AuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RefreshToken: "refresh-token",
			TokenURL:     "https://oauth2.googleapis.com/token",
		},
		Route:  RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120"},
		Drive:  DriveConfig{FolderID: "drive-folder"},
		Tunnel: TunnelConfig{Listen: "127.0.0.1:18080", ChunkSize: 1024 * 1024, PollIntervalMS: 1200, Concurrency: 4, CleanupProcessed: true},
	}

	text, err := EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, ConfigTextPrefix) {
		t.Fatalf("text prefix = %q, want %q", text[:len(ConfigTextPrefix)], ConfigTextPrefix)
	}

	decoded, err := DecodeConfigText(text)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Secret != cfg.Secret || decoded.Auth.RefreshToken != cfg.Auth.RefreshToken || decoded.Drive.FolderID != cfg.Drive.FolderID {
		t.Fatalf("decoded config mismatch: %#v", decoded)
	}

	path := filepath.Join(t.TempDir(), "client.skirk")
	if err := os.WriteFile(path, []byte(text+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.Secret != cfg.Secret {
		t.Fatalf("file config secret = %q, want %q", fromFile.Secret, cfg.Secret)
	}
	fromInline, err := LoadConfig("SKIRK_CONFIG=" + text)
	if err != nil {
		t.Fatal(err)
	}
	if fromInline.Route.Mode != cfg.Route.Mode {
		t.Fatalf("route mode = %q, want %q", fromInline.Route.Mode, cfg.Route.Mode)
	}

	wrapped := ConfigTextPrefix + wrapConfigPayload(strings.TrimPrefix(text, ConfigTextPrefix), 71)
	fromWrapped, err := LoadConfig(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if fromWrapped.SessionID != cfg.SessionID {
		t.Fatalf("wrapped session id = %q, want %q", fromWrapped.SessionID, cfg.SessionID)
	}

	fullCommand := "skirk serve-client --config '" + wrapped + "' --listen 127.0.0.1:18080"
	fromCommand, err := LoadConfig(fullCommand)
	if err != nil {
		t.Fatal(err)
	}
	if fromCommand.Drive.FolderID != cfg.Drive.FolderID {
		t.Fatalf("command folder id = %q, want %q", fromCommand.Drive.FolderID, cfg.Drive.FolderID)
	}
}

func wrapConfigPayload(payload string, width int) string {
	var b strings.Builder
	for len(payload) > width {
		b.WriteString(payload[:width])
		b.WriteString("\n  ")
		payload = payload[width:]
	}
	b.WriteString(payload)
	return b.String()
}
