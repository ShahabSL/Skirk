package skirk

import (
	"context"
	"encoding/json"
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

func TestConfigValidatesBurstPolling(t *testing.T) {
	cfg := &Config{
		Secret: strings.Repeat("a", 64),
		Tunnel: TunnelConfig{
			BurstPoll:         true,
			BurstPollMS:       75,
			BurstPollWindowMS: 5000,
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Tunnel.BurstPollMS = 10
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "burst_poll_ms") {
		t.Fatalf("err = %v, want burst_poll_ms validation error", err)
	}
	cfg.Tunnel.BurstPollMS = 75
	cfg.Tunnel.BurstPollWindowMS = 500
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "burst_poll_window_ms") {
		t.Fatalf("err = %v, want burst_poll_window_ms validation error", err)
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
	}, RouteConfig{Mode: "google_front", Proxy: "socks5h://127.0.0.1:11093"})

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

func TestOAuthTokenAttemptsIncludeHostileFallbacks(t *testing.T) {
	route := RouteConfig{
		Mode:           "google_front",
		Proxy:          "socks5h://127.0.0.1:11093",
		GoogleIP:       "216.239.38.120",
		TimeoutSeconds: 240,
	}
	attempts := oauthTokenAttempts(route)
	if len(attempts) != 4 {
		t.Fatalf("attempt count = %d, want 4", len(attempts))
	}
	if attempts[0].host != "oauth2.googleapis.com" || attempts[0].route.Mode != "google_front" {
		t.Fatalf("primary attempt = %+v", attempts[0])
	}
	if attempts[1].host != "accounts.google.com" || attempts[1].path != "/o/oauth2/token" || attempts[1].route.Mode != "google_front" || attempts[1].route.Proxy == "" {
		t.Fatalf("fronted accounts fallback = %+v", attempts[1])
	}
	if attempts[2].host != "accounts.google.com" || attempts[2].path != "/o/oauth2/token" || attempts[2].route.Mode != "direct" || attempts[2].route.Proxy == "" {
		t.Fatalf("direct accounts fallback = %+v", attempts[2])
	}
	if attempts[3].host != "oauth2.googleapis.com" || attempts[3].route.Mode != "direct" || attempts[3].route.Proxy == "" {
		t.Fatalf("direct oauth2 fallback = %+v", attempts[3])
	}
}

func TestTerminalOAuthTokenError(t *testing.T) {
	if !terminalOAuthTokenError(http.StatusBadRequest, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("invalid_grant should be terminal")
	}
	if terminalOAuthTokenError(http.StatusNotFound, []byte(`<html>wrong edge</html>`)) {
		t.Fatal("wrong-edge response should allow fallback")
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
