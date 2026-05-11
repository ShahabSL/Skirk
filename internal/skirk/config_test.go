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
		Sheets: SheetsConfig{SpreadsheetID: "sheet-id", Range: "skirk!A:D"},
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
	if fromFile.Sheets.SpreadsheetID != cfg.Sheets.SpreadsheetID {
		t.Fatalf("spreadsheet id = %q, want %q", fromFile.Sheets.SpreadsheetID, cfg.Sheets.SpreadsheetID)
	}
	fromInline, err := LoadConfig("SKIRK_CONFIG=" + text)
	if err != nil {
		t.Fatal(err)
	}
	if fromInline.Route.Mode != cfg.Route.Mode {
		t.Fatalf("route mode = %q, want %q", fromInline.Route.Mode, cfg.Route.Mode)
	}
}
