package skirk

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const appsScriptEchoResponseLimit = 1500000

type appsScriptEchoSettings struct {
	Route         RouteConfig `json:"route"`
	DeploymentURL string      `json:"deployment_url"`
	Auth          string      `json:"auth"`
}
type appsScriptEchoEnvelope struct {
	Auth  string `json:"auth,omitempty"`
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
	OK    bool   `json:"ok,omitempty"`
}
type appsScriptEchoHop struct {
	Authority string  `json:"authority"`
	Method    string  `json:"method"`
	Status    int     `json:"status"`
	Proto     string  `json:"protocol"`
	MS        float64 `json:"duration_ms"`
	Reused    bool    `json:"connection_reused"`
	Attempts  int     `json:"http_attempts"`
}
type appsScriptEchoRow struct {
	PlainBytes int                 `json:"plain_bytes"`
	Cold       bool                `json:"new_client"`
	MS         float64             `json:"round_trip_ms"`
	Verified   bool                `json:"verified"`
	Hops       []appsScriptEchoHop `json:"hops"`
	Failure    string              `json:"failure,omitempty"`
}

func appsScriptEchoEndpoint(raw string, first bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") || len(raw) > 16384 {
		return nil, errors.New("invalid echo endpoint")
	}
	host := strings.ToLower(u.Hostname())
	if host != "script.google.com" && (first || host != "script.googleusercontent.com") {
		return nil, errors.New("echo endpoint outside approved Google hosts")
	}
	if first && (!strings.HasPrefix(u.Path, "/macros/s/") || !strings.HasSuffix(u.Path, "/exec") || u.RawQuery != "") {
		return nil, errors.New("echo endpoint is not a deployed script")
	}
	return u, nil
}

func runAppsScriptEcho(ctx context.Context, client *GoogleHTTPClient, endpoint *url.URL, request []byte) ([]byte, []appsScriptEchoHop, error) {
	method, body := http.MethodPost, request
	current := endpoint
	var hops []appsScriptEchoHop
	for step := 0; step < 6; step++ {
		var reused atomic.Bool
		hopCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }})
		hopCtx = withGoogleResponseLimit(hopCtx, appsScriptEchoResponseLimit)
		started := time.Now()
		result, err := client.RequestSingleAttempt(hopCtx, method, current.Hostname(), current.RequestURI(), map[string]string{"Content-Type": "application/json"}, body)
		if err != nil {
			return nil, hops, errors.New("echo HTTP request failed")
		}
		hops = append(hops, appsScriptEchoHop{current.Hostname(), method, result.Status, result.Proto, float64(time.Since(started).Microseconds()) / 1000, reused.Load(), result.Attempts})
		switch result.Status {
		case http.StatusOK:
			return result.Body, hops, nil
		case 301, 302, 303, 307, 308:
			location := result.Header.Get("Location")
			u, err := url.Parse(location)
			if err != nil || location == "" {
				return nil, hops, errors.New("echo redirect missing or invalid")
			}
			current, err = appsScriptEchoEndpoint(current.ResolveReference(u).String(), false)
			if err != nil {
				return nil, hops, err
			}
			if result.Status == 301 || result.Status == 302 || result.Status == 303 {
				method, body = http.MethodGet, nil
			}
		default:
			return nil, hops, errors.New("echo returned unexpected HTTP status")
		}
	}
	return nil, hops, errors.New("echo redirect limit exceeded")
}

func verifyAppsScriptEcho(aead cipher.AEAD, expected, response []byte, nonce []byte) bool {
	var envelope appsScriptEchoEnvelope
	if json.Unmarshal(response, &envelope) != nil || !envelope.OK {
		return false
	}
	gotNonce, err := base64.StdEncoding.DecodeString(envelope.Nonce)
	if err != nil || !bytes.Equal(gotNonce, nonce) {
		return false
	}
	sealed, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return false
	}
	plain, err := aead.Open(nil, nonce, sealed, []byte("skirk-apps-script-echo-stage1"))
	return err == nil && bytes.Equal(plain, expected)
}

// Explicit environment gate keeps ordinary Go validation completely offline.
// The private settings contain only the temporary echo deployment and its
// generated secret; this test never obtains OAuth tokens or Drive data.
func TestLiveAppsScriptOpaqueEcho(t *testing.T) {
	if os.Getenv("SKIRK_LIVE_APPS_SCRIPT_ECHO") != "1" {
		t.Skip("bounded owned Apps Script echo requires explicit live opt-in")
	}
	dir := os.Getenv("SKIRK_APPS_SCRIPT_ECHO_PRIVATE_DIR")
	abs, err := filepath.Abs(dir)
	if err != nil || !strings.Contains(filepath.ToSlash(abs)+"/", "/.skirk-runs/") {
		t.Fatal("private .skirk-runs directory required")
	}
	raw, err := os.ReadFile(filepath.Join(abs, "settings.json"))
	if err != nil {
		t.Fatal("cannot read private echo settings")
	}
	var cfg appsScriptEchoSettings
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.Auth) < 32 || cfg.Route.Mode != "google_front_pinned" || net.ParseIP(cfg.Route.GoogleIP) == nil {
		t.Fatal("invalid private echo configuration or unpinned route")
	}
	endpoint, err := appsScriptEchoEndpoint(cfg.DeploymentURL, true)
	if err != nil {
		t.Fatal("invalid private deployment endpoint")
	}
	cfg.Route.TimeoutSeconds = 20
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal("cannot generate local AEAD key")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	client := NewGoogleHTTPClient(cfg.Route)
	defer client.client.CloseIdleConnections()
	rows := []appsScriptEchoRow{}
	defer func() {
		report := map[string]any{"route_mode": cfg.Route.Mode, "tls_front": "www.google.com", "maximum_posts": 12,
			"rows": rows, "scope": "owned opaque echo only; no URL Fetch or TCP-exit hop"}
		encoded, _ := json.MarshalIndent(report, "", "  ")
		if os.WriteFile(filepath.Join(abs, "measurements.json"), append(encoded, '\n'), 0600) != nil {
			t.Error("cannot persist private measurements")
		}
	}()
	sizes := []int{64, 64, 64, 64, 64, 64, 16384, 16384, 262144, 262144, 1048576, 1048576}
	for i, size := range sizes {
		plain := make([]byte, size)
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(plain); err != nil {
			t.Fatal("random payload generation failed")
		}
		if _, err := rand.Read(nonce); err != nil {
			t.Fatal("random nonce generation failed")
		}
		sealed := aead.Seal(nil, nonce, plain, []byte("skirk-apps-script-echo-stage1"))
		request, _ := json.Marshal(appsScriptEchoEnvelope{Auth: cfg.Auth, Nonce: base64.StdEncoding.EncodeToString(nonce), Data: base64.StdEncoding.EncodeToString(sealed)})
		row := appsScriptEchoRow{PlainBytes: size, Cold: i == 0}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		started := time.Now()
		response, hops, err := runAppsScriptEcho(ctx, client, endpoint, request)
		cancel()
		row.MS, row.Hops = float64(time.Since(started).Microseconds())/1000, hops
		row.Verified = err == nil && verifyAppsScriptEcho(aead, plain, response, nonce)
		if !row.Verified {
			if err != nil {
				row.Failure = err.Error() // Helpers deliberately exclude URL/body/secret values.
			} else {
				row.Failure = "opaque echo integrity or authentication failure"
			}
		}
		rows = append(rows, row)
		if !row.Verified {
			t.Fatalf("owned echo stopped after request %d: %s", i+1, row.Failure)
		}
	}
}

func TestAppsScriptEchoEndpointBounds(t *testing.T) {
	for _, raw := range []string{"http://script.google.com/macros/s/test/exec", "https://accounts.google.com/", "https://script.google.com.evil.test/macros/s/test/exec", "https://user@script.google.com/macros/s/test/exec", "https://script.google.com:444/macros/s/test/exec", "https://script.google.com/macros/s/test/dev", "https://script.google.com/macros/s/test/exec?auth=secret"} {
		if _, err := appsScriptEchoEndpoint(raw, true); err == nil {
			t.Fatal("accepted unexpected echo endpoint")
		}
	}
	if _, err := appsScriptEchoEndpoint("https://script.google.com/macros/s/test/exec", true); err != nil {
		t.Fatal(err)
	}
	if _, err := appsScriptEchoEndpoint("https://script.googleusercontent.com/macros/echo?owned=opaque", false); err != nil {
		t.Fatal(err)
	}
}

func TestAppsScriptEchoPreservesFrontAcrossRedirects(t *testing.T) {
	var calls int
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Hostname() != "www.google.com" {
			t.Error("redirect escaped selected Google front")
		}
		if calls == 1 {
			if req.Host != "script.google.com" || req.Method != http.MethodPost {
				t.Error("invalid initial logical host or method")
			}
			result := stringResponse(302, "")
			result.Header = http.Header{"Location": {"https://script.googleusercontent.com/macros/echo?opaque=test"}}
			return result, nil
		}
		if req.Host != "script.googleusercontent.com" || req.Method != http.MethodGet {
			t.Error("redirect did not preserve authority semantics")
		}
		if req.Body != nil {
			body, _ := io.ReadAll(req.Body)
			if len(body) != 0 {
				t.Error("303-style redirect leaked initial secret in body")
			}
		}
		return stringResponse(200, `{"ok":true}`), nil
	}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
	endpoint, _ := appsScriptEchoEndpoint("https://script.google.com/macros/s/test/exec", true)
	body, hops, err := runAppsScriptEcho(context.Background(), client, endpoint, []byte(`{"auth":"synthetic-test-only"}`))
	if err != nil || calls != 2 || len(hops) != 2 || string(body) != `{"ok":true}` {
		t.Fatalf("redirect = %d calls, %d hops, %v", calls, len(hops), err)
	}
}

func TestAppsScriptEchoIntegrityRejectsAlteration(t *testing.T) {
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, _ := cipher.NewGCM(block)
	nonce, plain := make([]byte, aead.NonceSize()), []byte("synthetic echo")
	sealed := aead.Seal(nil, nonce, plain, []byte("skirk-apps-script-echo-stage1"))
	envelope := appsScriptEchoEnvelope{OK: true, Nonce: base64.StdEncoding.EncodeToString(nonce), Data: base64.StdEncoding.EncodeToString(sealed)}
	valid, _ := json.Marshal(envelope)
	if !verifyAppsScriptEcho(aead, plain, valid, nonce) {
		t.Fatal("valid synthetic echo rejected")
	}
	sealed[0] ^= 1
	envelope.Data = base64.StdEncoding.EncodeToString(sealed)
	invalid, _ := json.Marshal(envelope)
	if verifyAppsScriptEcho(aead, plain, invalid, nonce) {
		t.Fatal("altered ciphertext accepted")
	}
}
