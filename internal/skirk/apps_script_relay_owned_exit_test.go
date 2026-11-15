package skirk

// These opt-in test fixtures are not included in any product binary.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
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
	DurationSeconds        int         `json:"duration_seconds"`
	MaxCalls               int64       `json:"max_calls"`
	MaxEncodedBytes        int64       `json:"max_encoded_bytes"`
	BulkBytes              int         `json:"bulk_bytes"`
	ScriptURL              string      `json:"script_url,omitempty"`
}

func (s relayResearchPrivateSettings) relayConfig() (relayResearchConfig, error) {
	return (relayResearchConfig{FrameBytes: s.FrameBytes, MaxBytes: s.MaxBytes, PriorityReserveBytes: s.PriorityReserveBytes, PriorityReserveObjects: s.PriorityReserveObjects}).normalized()
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
	mu                    sync.Mutex
	maxCalls, maxBytes    int64
	calls, bytes, blocked int64
	cancel                context.CancelFunc
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
	return map[string]int64{"http_calls": b.calls, "encoded_body_bytes": b.bytes, "budget_rejections": b.blocked}
}
func (b *relayResearchOwnedBudget) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
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

type relayResearchCountedBody struct {
	io.ReadCloser
	budget *relayResearchOwnedBudget
}

func (r *relayResearchCountedBody) Read(p []byte) (int, error) {
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
	b, err := newRelayResearchBroker(ctx, key, epoch, rc)
	if err != nil {
		return err
	}
	defer b.Close()
	payload := make([]byte, s.BulkBytes)
	if _, err = rand.Read(payload); err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	var gets, posts, echoes, postedBytes atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
	if err = rrWritePrivateJSON(dir, "runtime.json", map[string]any{"broker_local_url": rpc.URL + "/exchange", "origin_url": origin.URL, "origin_target": originAddr, "download_sha256": hex.EncodeToString(hash[:]), "bulk_bytes": s.BulkBytes, "pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
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
	s := relayResearchPrivateSettings{OuterKeyHex: strings.Repeat("11", 32), EpochHex: strings.Repeat("22", 16), MuxSecretHex: strings.Repeat("33", 32), SessionID: strings.Repeat("44", 16), HTTPAuth: strings.Repeat("55", 32), ScriptAuth: strings.Repeat("66", 32), ClientID: "research-client", RunID: "research-run", Route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, FrameBytes: 128 << 10, ChunkBytes: 4 << 20, DurationSeconds: 5, MaxCalls: 8, MaxEncodedBytes: 4 << 20, BulkBytes: 4096}
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
	if !strings.HasPrefix(runtime.Broker, "http://127.0.0.1:") || !strings.HasPrefix(runtime.Origin, "http://127.0.0.1:") {
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
}
