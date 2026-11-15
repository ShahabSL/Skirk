package skirk

// These opt-in test fixtures are not included in any product binary.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type relayResearchPrivateSettings struct {
	OuterKeyHex            string      `json:"outer_key_hex"`
	EpochHex               string      `json:"epoch_hex"`
	MuxSecretHex           string      `json:"mux_secret_hex"`
	SessionID              string      `json:"session_id"`
	HTTPAuth               string      `json:"http_auth"`
	ScriptAuth             string      `json:"script_auth"`
	ClientID               string      `json:"client_id"`
	RunID                  string      `json:"run_id"`
	Route                  RouteConfig `json:"route"`
	FrameBytes             int         `json:"frame_bytes"`
	ChunkBytes             int         `json:"chunk_bytes"`
	MaxBytes               int64       `json:"max_bytes"`
	PriorityReserveBytes   int64       `json:"priority_reserve_bytes"`
	PriorityReserveObjects int         `json:"priority_reserve_objects"`
	PackPriority           bool        `json:"pack_priority,omitempty"`
	DurationSeconds        int         `json:"duration_seconds"`
	MaxCalls               int64       `json:"max_calls"`
	MaxEncodedBytes        int64       `json:"max_encoded_bytes"`
	MaxScriptPosts         int64       `json:"max_script_posts"`
	MaxHTTPAttemptsUpper   int64       `json:"max_http_attempts_upper"`
	MaxHTTPEncodedUpper    int64       `json:"max_http_encoded_upper"`
	BulkBytes              int         `json:"bulk_bytes"`
	ScriptURL              string      `json:"script_url,omitempty"`
	PayloadSeedHex         string      `json:"payload_seed_hex"`
}

// A public test-corpus seed, not a key used by either authentication layer.
func rrCampaignPayload(s relayResearchPrivateSettings) ([]byte, error) {
	seed, err := hex.DecodeString(s.PayloadSeedHex)
	if err != nil || len(seed) != 32 || s.BulkBytes < 1 || s.BulkBytes > 16<<20 {
		return nil, errRelayResearchBounds
	}
	block, err := aes.NewCipher(seed)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, s.BulkBytes)
	cipher.NewCTR(block, make([]byte, aes.BlockSize)).XORKeyStream(payload, payload)
	return payload, nil
}

func rrNewOwnedTLSOrigin(handler http.Handler) (*httptest.Server, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Skirk owned relay research origin"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(3 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.MaxHeaderBytes = 16 << 10
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	return server, nil
}

func (s relayResearchPrivateSettings) relayConfig() (relayResearchConfig, error) {
	return (relayResearchConfig{FrameBytes: s.FrameBytes, MaxBytes: s.MaxBytes, PriorityReserveBytes: s.PriorityReserveBytes, PriorityReserveObjects: s.PriorityReserveObjects, PackPriority: s.PackPriority}).normalized()
}

func rrReadPrivateSettings(dir string) (relayResearchPrivateSettings, []byte, [16]byte, error) {
	var s relayResearchPrivateSettings
	var epoch [16]byte
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) || !strings.Contains(dir+string(os.PathSeparator), string(os.PathSeparator)+".skirk-runs"+string(os.PathSeparator)) {
		return s, nil, epoch, errRelayResearchBounds
	}
	dirInfo, dirErr := os.Lstat(dir)
	if dirErr != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0077 != 0 {
		return s, nil, epoch, errRelayResearchBounds
	}
	path := filepath.Join(dir, "settings.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16*1024 {
		return s, nil, epoch, errRelayResearchBounds
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, nil, epoch, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&s) != nil || decoder.Decode(new(any)) != io.EOF {
		return s, nil, epoch, errRelayResearchBounds
	}
	key, err := hex.DecodeString(s.OuterKeyHex)
	epochBytes, epochErr := hex.DecodeString(s.EpochHex)
	muxSecret, muxErr := hex.DecodeString(s.MuxSecretHex)
	httpAuth, httpErr := hex.DecodeString(s.HTTPAuth)
	scriptAuth, scriptErr := hex.DecodeString(s.ScriptAuth)
	if err != nil || epochErr != nil || muxErr != nil || httpErr != nil || scriptErr != nil || len(key) != 32 || len(epochBytes) != 16 || len(muxSecret) != 32 || bytes.Equal(key, muxSecret) || bytes.Equal(httpAuth, scriptAuth) || bytes.Equal(key, httpAuth) || bytes.Equal(key, scriptAuth) || bytes.Equal(muxSecret, httpAuth) || bytes.Equal(muxSecret, scriptAuth) || len(httpAuth) != 32 || len(scriptAuth) != 32 {
		return s, nil, epoch, errRelayResearchBounds
	}
	copy(epoch[:], epochBytes)
	if _, err := ParseSessionID(s.SessionID); err != nil {
		return s, nil, epoch, errRelayResearchBounds
	}
	if !isSafeObjectSegment(s.ClientID) || len(s.ClientID) > 64 || !isSafeObjectSegment(s.RunID) || len(s.RunID) > 64 || s.Route.Mode != "google_front_pinned" || net.ParseIP(s.Route.GoogleIP) == nil || s.Route.Proxy != "" || s.DurationSeconds < 1 || s.DurationSeconds > 7200 || s.MaxCalls < 1 || s.MaxCalls > 1024 || s.MaxEncodedBytes < 1 || s.MaxEncodedBytes > 128<<20 || s.ChunkBytes < 64<<10 || s.ChunkBytes > 4<<20 || s.BulkBytes < 1 || s.BulkBytes > 16<<20 {
		return s, nil, epoch, errRelayResearchBounds
	}
	if _, err := s.relayConfig(); err != nil {
		return s, nil, epoch, err
	}
	if s.MaxScriptPosts < 1 || s.MaxScriptPosts > 256 || s.MaxHTTPAttemptsUpper < 1 || s.MaxHTTPAttemptsUpper > 4096 || s.MaxHTTPEncodedUpper < 1 || s.MaxHTTPEncodedUpper > 1<<30 {
		return s, nil, epoch, errRelayResearchBounds
	}
	seed, seedErr := hex.DecodeString(s.PayloadSeedHex)
	if seedErr != nil || len(seed) != 32 {
		return s, nil, epoch, errRelayResearchBounds
	}
	return s, key, epoch, nil
}

func rrWritePrivateJSON(dir, name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(raw, '\n'))
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}

type relayResearchOwnedBudget struct {
	mu                          sync.Mutex
	maxCalls, maxBytes          int64
	calls, seen, bytes, blocked int64
	cancel                      context.CancelFunc
}

func (b *relayResearchOwnedBudget) claim(n int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	allow := min(int64(n), b.maxBytes-b.bytes)
	if allow <= 0 {
		b.blocked++
		b.cancel()
		return 0
	}
	b.bytes += allow
	return int(allow)
}
func (b *relayResearchOwnedBudget) refund(n int) {
	b.mu.Lock()
	b.bytes -= int64(n)
	b.mu.Unlock()
}
func (b *relayResearchOwnedBudget) snapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]int64{"http_calls": b.seen, "admitted_http_calls": b.calls, "encoded_body_bytes": b.bytes, "budget_rejections": b.blocked}
}
func (b *relayResearchOwnedBudget) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.seen++
		if b.calls >= b.maxCalls {
			b.blocked++
			b.mu.Unlock()
			b.cancel()
			http.Error(w, "research session call bound", http.StatusTooManyRequests)
			return
		}
		b.calls++
		b.mu.Unlock()
		r.Body = &relayResearchCountedBody{ReadCloser: r.Body, budget: b}
		next.ServeHTTP(&relayResearchCountedWriter{ResponseWriter: w, budget: b}, r)
	})
}

func TestRelayOwnedExitBudgetBoundsReadsWritesAndAdmittedCalls(t *testing.T) {
	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := &relayResearchOwnedBudget{maxCalls: 1, maxBytes: 6, cancel: cancel}
			if mode == "read" {
				body := &relayResearchCountedBody{ReadCloser: io.NopCloser(strings.NewReader("12345678")), budget: budget}
				raw, err := io.ReadAll(body)
				if len(raw) != 6 || !errors.Is(err, errRelayResearchBounds) {
					t.Fatal("read crossed encoded-body bound", len(raw), err)
				}
			} else {
				recorder := httptest.NewRecorder()
				writer := &relayResearchCountedWriter{ResponseWriter: recorder, budget: budget}
				n, err := writer.Write([]byte("12345678"))
				if n != 6 || recorder.Body.Len() != 6 || !errors.Is(err, errRelayResearchBounds) {
					t.Fatal("write crossed encoded-body bound", n, err)
				}
			}
			if ctx.Err() == nil || budget.snapshot()["encoded_body_bytes"] != 6 {
				t.Fatal("exhausted body budget did not cancel or account", budget.snapshot())
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := &relayResearchOwnedBudget{maxCalls: 2, maxBytes: 64, cancel: cancel}
	var executed int
	handler := budget.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { executed++; _, _ = w.Write([]byte{1}) }))
	for i := 0; i < 3; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/exchange", nil))
	}
	s := budget.snapshot()
	if executed != 2 || s["http_calls"] != 3 || s["admitted_http_calls"] != 2 || s["budget_rejections"] != 1 || ctx.Err() == nil {
		t.Fatal("admitted-call budget or observed-call accounting incorrect", executed, s)
	}
}

type relayResearchCountedBody struct {
	io.ReadCloser
	budget *relayResearchOwnedBudget
}

func (r *relayResearchCountedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	allowed := r.budget.claim(len(p))
	if allowed == 0 {
		return 0, errRelayResearchBounds
	}
	n, err := r.ReadCloser.Read(p[:allowed])
	r.budget.refund(allowed - n)
	return n, err
}

type relayResearchCountedWriter struct {
	http.ResponseWriter
	budget *relayResearchOwnedBudget
}

func (w *relayResearchCountedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *relayResearchCountedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	allowed := w.budget.claim(len(p))
	if allowed == 0 {
		return 0, errRelayResearchBounds
	}
	n, err := w.ResponseWriter.Write(p[:allowed])
	w.budget.refund(allowed - n)
	if allowed < len(p) {
		w.budget.cancel()
		err = errors.Join(err, errRelayResearchBounds)
	}
	return n, err
}

// Run only in a separate bounded test process, with a freshly generated private
// directory. The publicly reachable broker can dial exactly one owned origin.
func TestAppsScriptRelayOwnedExit(t *testing.T) {
	dir := os.Getenv("SKIRK_APPS_SCRIPT_RELAY_PRIVATE_DIR")
	if dir == "" {
		t.Skip("opt-in owned relay fixture")
	}
	s, key, epoch, err := rrReadPrivateSettings(dir)
	if err != nil {
		t.Fatal("invalid private relay settings")
	}
	parent, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.DurationSeconds)*time.Second)
	defer cancel()
	if err := rrRunOwnedExit(ctx, cancel, dir, s, key, epoch); err != nil {
		t.Fatal("owned relay fixture failed:", errorSummary(err))
	}
}

func rrRunOwnedExit(ctx context.Context, cancel context.CancelFunc, dir string, s relayResearchPrivateSettings, key []byte, epoch [16]byte) error {
	rc, err := s.relayConfig()
	if err != nil {
		return err
	}
	// Never resume an in-memory exactly-once session after a process restart.
	// A fresh campaign arm needs a fresh private directory, key and epoch;
	// retain this marker even if the previous process crashed before ready.
	if err = rrWritePrivateJSON(dir, "server-started.json", map[string]any{"pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano), "restart_policy": "fresh private directory, outer key and epoch required"}); err != nil {
		return err
	}
	b, err := newRelayResearchBroker(ctx, key, epoch, rc)
	if err != nil {
		return err
	}
	defer b.Close()
	payload, err := rrCampaignPayload(s)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	var gets, posts, echoes, postedBytes atomic.Int64
	events, err := os.OpenFile(filepath.Join(dir, "origin-events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer events.Close()
	var eventsMu sync.Mutex
	origin, err := rrNewOwnedTLSOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/bulk" {
			eventsMu.Lock()
			eventErr := json.NewEncoder(events).Encode(map[string]any{"event": "bulk_started", "phase": r.Header.Get("X-Skirk-Research-Phase"), "method": r.Method, "unix_nano": time.Now().UnixNano()})
			eventsMu.Unlock()
			if eventErr != nil {
				cancel()
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/bulk":
			gets.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		case r.Method == http.MethodPost && r.URL.Path == "/bulk":
			h := sha256.New()
			n, e := io.Copy(h, http.MaxBytesReader(w, r.Body, int64(s.BulkBytes)+1))
			if e != nil || n != int64(s.BulkBytes) {
				http.Error(w, "owned body bound", http.StatusBadRequest)
				return
			}
			posts.Add(1)
			postedBytes.Add(n)
			_ = json.NewEncoder(w).Encode(map[string]any{"bytes": n, "sha256": hex.EncodeToString(h.Sum(nil))})
		case r.Method == http.MethodPost && r.URL.Path == "/echo":
			body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 65))
			if e != nil || len(body) != 64 {
				http.Error(w, "owned echo bound", http.StatusBadRequest)
				return
			}
			echoes.Add(1)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	if err != nil {
		return err
	}
	defer origin.Close()
	originAddr := origin.Listener.Addr().String()
	cfg := &Config{Secret: s.MuxSecretHex, SessionID: s.SessionID, Client: ClientConfig{ID: s.ClientID, RunID: s.RunID}, Tunnel: TunnelConfig{Profile: "fixed", PollIntervalMS: 10, ChunkSize: s.ChunkBytes, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
	cfg.ApplyDefaults()
	tunnel, err := NewTunnel(b.exitStore(), cfg)
	if err != nil {
		return err
	}
	tunnel.Logger = log.New(io.Discard, "", 0)
	tunnel.setRole("exit")
	mux, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		return err
	}
	mux.dialExit = func(callCtx context.Context, target string) (net.Conn, error) {
		if target != originAddr {
			return nil, errors.New("research exit target outside owned allowlist")
		}
		return (&net.Dialer{}).DialContext(callCtx, "tcp", target)
	}
	validate, notify := relayResearchMuxHooks(mux, ctx)
	b.setUpHooks(validate, func(info ObjectInfo) { notify(info) })
	mux.start(ctx)
	budget := &relayResearchOwnedBudget{maxCalls: s.MaxCalls, maxBytes: s.MaxEncodedBytes, cancel: cancel}
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.HTTPAuth)) != 1 {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		b.handler(s.HTTPAuth).ServeHTTP(w, r)
	})
	rpc := httptest.NewServer(budget.wrap(protected))
	// Defer cancellation before listener shutdown to release long PULL handlers.
	defer func() { cancel(); mux.closeAll(); b.Close(); rpc.Close() }()
	if err = rrWritePrivateJSON(dir, "runtime.json", map[string]any{"broker_local_url": rpc.URL + "/exchange", "origin_url": origin.URL, "origin_target": originAddr, "origin_certificate_base64": base64.StdEncoding.EncodeToString(origin.Certificate().Raw), "download_sha256": hex.EncodeToString(hash[:]), "bulk_bytes": s.BulkBytes, "pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		return err
	}
	<-ctx.Done()
	beforeClose := b.snapshot()
	cancel()
	mux.closeAll()
	b.Close()
	rpc.Close()
	origin.Close()
	return rrWritePrivateJSON(dir, "exit-result.json", map[string]any{"broker_before_close": beforeClose, "broker_after_close": b.snapshot(), "budget": budget.snapshot(), "origin_gets": gets.Load(), "origin_posts": posts.Load(), "origin_echoes": echoes.Load(), "origin_posted_bytes": postedBytes.Load(), "active_streams_after_close": mux.active.Load(), "stopped_utc": time.Now().UTC().Format(time.RFC3339Nano), "mux_cleanup_scope": "context canceled and all streams closed; standalone process exit bounds unjoined legacy mux goroutines"})
}

func TestRelayOwnedExitFixtureValidatesPrivateSettingsAndJoinsHTTP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := relayResearchPrivateSettings{OuterKeyHex: strings.Repeat("11", 32), EpochHex: strings.Repeat("22", 16), MuxSecretHex: strings.Repeat("33", 32), SessionID: strings.Repeat("44", 16), HTTPAuth: strings.Repeat("55", 32), ScriptAuth: strings.Repeat("66", 32), ClientID: "research-client", RunID: "research-run", Route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, FrameBytes: 128 << 10, ChunkBytes: 4 << 20, DurationSeconds: 5, MaxCalls: 8, MaxEncodedBytes: 4 << 20, MaxScriptPosts: 256, MaxHTTPAttemptsUpper: 4096, MaxHTTPEncodedUpper: 1 << 30, BulkBytes: 4096, PayloadSeedHex: strings.Repeat("77", 32)}
	if err := rrWritePrivateJSON(dir, "settings.json", s); err != nil {
		t.Fatal(err)
	}
	loaded, key, epoch, err := rrReadPrivateSettings(dir)
	if err != nil || loaded.HTTPAuth != s.HTTPAuth {
		t.Fatal("private settings rejected", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = rrReadPrivateSettings(dir); !errors.Is(err, errRelayResearchBounds) {
		t.Fatal("publicly readable settings accepted", err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*relayResearchPrivateSettings){
		func(v *relayResearchPrivateSettings) { v.ClientID = "client/other" },
		func(v *relayResearchPrivateSettings) { v.RunID = "run/other" },
		func(v *relayResearchPrivateSettings) { v.OuterKeyHex = v.HTTPAuth },
		func(v *relayResearchPrivateSettings) { v.OuterKeyHex = v.ScriptAuth },
		func(v *relayResearchPrivateSettings) { v.MuxSecretHex = v.HTTPAuth },
		func(v *relayResearchPrivateSettings) { v.MuxSecretHex = v.ScriptAuth },
		func(v *relayResearchPrivateSettings) { v.MaxScriptPosts = 257 },
		func(v *relayResearchPrivateSettings) { v.MaxHTTPAttemptsUpper = 4097 },
		func(v *relayResearchPrivateSettings) { v.MaxHTTPEncodedUpper = 1<<30 + 1 },
	} {
		bad := s
		mutate(&bad)
		raw, marshalErr := json.Marshal(bad)
		if marshalErr != nil || os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("could not prepare invalid private settings")
		}
		if _, _, _, readErr := rrReadPrivateSettings(dir); !errors.Is(readErr, errRelayResearchBounds) {
			t.Fatal("unsafe identity or reused credentials accepted", readErr)
		}
	}
	raw, _ := json.Marshal(s)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rrRunOwnedExit(ctx, cancel, dir, s, key, epoch) }()
	var runtime struct {
		Broker string `json:"broker_local_url"`
		Origin string `json:"origin_url"`
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(filepath.Join(dir, "runtime.json"))
		if readErr == nil && json.Unmarshal(raw, &runtime) == nil && runtime.Broker != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.HasPrefix(runtime.Broker, "http://127.0.0.1:") || !strings.HasPrefix(runtime.Origin, "https://127.0.0.1:") {
		cancel()
		<-done
		t.Fatal("owned fixtures did not bind only loopback")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post(runtime.Broker, "application/json", strings.NewReader("{}"))
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Error("missing distinct HTTP bearer accepted", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("owned fixture shutdown failed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owned fixture did not join HTTP shutdown")
	}
	if err = rrRunOwnedExit(ctx, cancel, dir, s, key, epoch); !errors.Is(err, os.ErrExist) {
		t.Fatal("in-memory session could restart using consumed private directory", err)
	}
}
