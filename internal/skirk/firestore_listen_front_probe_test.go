package skirk

// A bounded owned-document Listen primitive, not a product transport.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const firestoreListenPath = "/google.firestore.v1.Firestore/Listen/channel"
const firestoreWebChannelSHA = "c820e65a0254c9765ab8564cc01faf2778dc87dfd72425421029f82aa8087467"

type firestoreListenJournal struct {
	mu   sync.Mutex
	file *os.File
	err  error
}

func (j *firestoreListenJournal) write(value any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	j.err = json.NewEncoder(j.file).Encode(value)
	if j.err == nil {
		j.err = j.file.Sync()
	}
	return j.err
}

type firestoreListenDispatch struct {
	mu           sync.Mutex
	client       *GoogleHTTPClient
	token        string
	calls        int
	encodedUpper int64
	journal      *firestoreListenJournal
}

func (d *firestoreListenDispatch) call(ctx context.Context, kind, method, path string, headers map[string]string, body []byte, cleanup bool) (*HTTPResult, error) {
	d.mu.Lock()
	limit := 36
	if cleanup {
		limit = 40
	}
	if d.calls >= limit || d.encodedUpper+8*int64(len(body)) > 16<<20 {
		d.mu.Unlock()
		return nil, errors.New("Firestore Listen dispatch bound")
	}
	d.calls++
	id := d.calls
	d.encodedUpper += 8 * int64(len(body))
	d.mu.Unlock()
	if err := d.journal.write(map[string]any{"kind": "dispatch_start", "id": id, "operation": kind, "method": method, "request_body_bytes": len(body), "http_attempts_reserved": 8, "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		return nil, err
	}
	selected := map[string]string{"Authorization": "Bearer " + d.token, "Accept-Encoding": "identity"}
	for key, value := range headers {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "X-Goog-Api-Client") {
			selected[key] = value
		}
	}
	callCtx, cancel := context.WithTimeout(withGoogleResponseLimit(ctx, 1<<20), 15*time.Second)
	defer cancel()
	started := time.Now()
	result, err := d.client.RequestSingleAttempt(callCtx, method, "firestore.googleapis.com", path, selected, body)
	status, protocol, responseBytes := 0, "", 0
	if result != nil {
		status, protocol, responseBytes = result.Status, result.Proto, len(result.Body)
	}
	journalErr := d.journal.write(map[string]any{"kind": "dispatch_result", "id": id, "operation": kind, "method": method, "status": status, "protocol": protocol, "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000, "response_body_bytes": responseBytes, "transport_error": err != nil && !errors.Is(err, context.Canceled), "canceled": errors.Is(err, context.Canceled)})
	if err == nil && result != nil && result.Proto != "HTTP/2.0" {
		err = errors.New("Firestore response did not preserve HTTP2")
	}
	return result, errors.Join(err, journalErr)
}
func (d *firestoreListenDispatch) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{"visible_calls": d.calls, "http_attempts_upper": d.calls * 8, "encoded_request_bytes_upper": d.encodedUpper, "active_call_limit": 36, "total_call_limit_including_cleanup": 40, "response_limit_bytes": 1 << 20}
}

type firestoreListenEvent struct {
	Kind   string  `json:"kind"`
	Name   string  `json:"name,omitempty"`
	Wire   string  `json:"wire_base64,omitempty"`
	At     float64 `json:"received_unix_ms,omitempty"`
	Code   string  `json:"code,omitempty"`
	Change string  `json:"change,omitempty"`
	IDs    []int   `json:"ids,omitempty"`
}
type firestoreListenBridge struct {
	ctx            context.Context
	cancel         context.CancelFunc
	dispatch       *firestoreListenDispatch
	database, auth string
	mu             sync.Mutex
	closed         bool
	eventsSeen     int
	wg             sync.WaitGroup
	requestSlots   chan struct{}
	eventSlots     chan struct{}
	events         chan firestoreListenEvent
	server         *httptest.Server
}

func firestoreListenBridgePath(path, database string) bool {
	if strings.Contains(path, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || parsed.Path != firestoreListenPath || parsed.Fragment != "" || len(path) > 16384 {
		return false
	}
	query, err := url.QueryUnescape(parsed.RawQuery)
	return err == nil && parsed.Query().Get("database") == database && !strings.Contains(query, "Authorization") && !strings.Contains(query, "Bearer ") && !parsed.Query().Has("$httpHeaders")
}
func newFirestoreListenBridge(parent context.Context, d *firestoreListenDispatch, database, auth string) *firestoreListenBridge {
	ctx, cancel := context.WithCancel(parent)
	b := &firestoreListenBridge{ctx: ctx, cancel: cancel, dispatch: d, database: database, auth: auth, requestSlots: make(chan struct{}, 4), eventSlots: make(chan struct{}, 1), events: make(chan firestoreListenEvent, 64)}
	b.server = httptest.NewUnstartedServer(http.HandlerFunc(b.serve))
	b.server.Config.ReadHeaderTimeout = 5 * time.Second
	b.server.Config.ReadTimeout = 5 * time.Second
	b.server.Config.WriteTimeout = 25 * time.Second
	b.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	b.server.Start()
	return b
}
func (b *firestoreListenBridge) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.server.CloseClientConnections()
	b.server.Close()
	b.wg.Wait()
}
func (b *firestoreListenBridge) serve(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	route, reason, status := "other", "ok", 500
	if r.URL.Path == "/request" || r.URL.Path == "/event" {
		route = r.URL.Path
	}
	var admitted chan struct{}
	registered := false
	defer func() {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		// Constant route/reason enums only: never persist request URLs, headers,
		// bodies, owner tokens or the loopback authorization value.
		if err := b.dispatch.journal.write(map[string]any{"kind": "local_http_result", "phase": route, "status": status, "reason": reason, "canceled": r.Context().Err() != nil, "bridge_closed": closed, "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000}); err != nil {
			b.cancel()
		}
		if admitted != nil {
			<-admitted
		}
		if registered {
			b.wg.Done()
		}
	}()
	reject := func(code int, cause string) {
		status, reason = code, cause
		http.Error(w, cause, code)
	}
	if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.auth)) != 1 {
		reject(403, "unauthorized")
		return
	}
	if route == "other" {
		reject(404, "unknown_route")
		return
	}
	slots := b.requestSlots
	if route == "/event" {
		// One independent local ACK slot cannot be starved by four longpolls.
		// This adds no Google concurrency or API budget.
		slots = b.eventSlots
	}
	select {
	case slots <- struct{}{}:
		admitted = slots
	default:
		reject(429, "local_concurrency_bound")
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		reject(503, "bridge_closed")
		return
	}
	b.wg.Add(1)
	registered = true
	b.mu.Unlock()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		reject(413, "body_bound")
		return
	}
	if r.URL.Path == "/event" {
		var event firestoreListenEvent
		if json.Unmarshal(body, &event) != nil || (event.Kind != "document" && event.Kind != "target" && event.Kind != "error") || len(event.Wire) > 128<<10 || len(event.Name) > 512 || len(event.Code) > 32 || len(event.IDs) > 8 || strings.Contains(event.Code, b.dispatch.token) {
			reject(400, "event_bound")
			return
		}
		b.mu.Lock()
		b.eventsSeen++
		bounded := b.eventsSeen <= 64
		b.mu.Unlock()
		if !bounded {
			reject(413, "event_count_bound")
			b.cancel()
			return
		}
		wireHash := sha256.Sum256([]byte(event.Wire))
		if err := b.dispatch.journal.write(map[string]any{"kind": "observer_event", "event_kind": event.Kind, "code": event.Code, "target_change": event.Change, "target_ids": event.IDs, "received_unix_ms": event.At, "encoded_ciphertext_sha256": hex.EncodeToString(wireHash[:])}); err != nil {
			reject(500, "journal_failed")
			b.cancel()
			return
		}
		select {
		case b.events <- event:
			status = 204
			w.WriteHeader(204)
		case <-b.ctx.Done():
			reject(503, "bridge_closed")
		default:
			reject(429, "event_queue_bound")
			b.cancel()
		}
		return
	}
	var request struct {
		Method, Path string
		Headers      map[string]string
		Body         string `json:"body_base64"`
	}
	if json.Unmarshal(body, &request) != nil || !firestoreListenBridgePath(request.Path, b.database) || (request.Method != http.MethodGet && request.Method != http.MethodPost) || len(request.Headers) > 32 {
		reject(403, "owned_listener_only")
		return
	}
	decodedPath, _ := url.QueryUnescape(request.Path)
	if b.dispatch.token != "" && (strings.Contains(request.Path, b.dispatch.token) || strings.Contains(decodedPath, b.dispatch.token)) {
		reject(403, "url_credentials")
		return
	}
	for k, v := range request.Headers {
		if len(k) > 128 || len(v) > 16384 {
			reject(413, "header_bound")
			return
		}
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(request.Body)
	if err != nil || len(payload) > 128<<10 {
		reject(413, "payload_bound")
		return
	}
	result, err := b.dispatch.call(r.Context(), "listen", request.Method, request.Path, request.Headers, payload, false)
	if err != nil || result == nil {
		reject(502, "front_request_failed")
		return
	}
	headers := map[string]string{}
	for _, key := range []string{"Content-Type", "X-HTTP-Session-Id", "X-Client-Wire-Protocol", "X-HTTP-Initial-Response"} {
		if value := result.Header.Get(key); value != "" {
			headers[key] = value
		}
	}
	w.Header().Set("Content-Type", "application/json")
	status = 200
	if err := json.NewEncoder(w).Encode(map[string]any{"status": result.Status, "headers": headers, "body_base64": base64.StdEncoding.EncodeToString(result.Body)}); err != nil {
		reason = "response_write"
	}
}

func firestoreListenPrivate(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	s, err := os.Lstat(p)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 1<<20 {
		return nil, errors.New("private research file required")
	}
	return os.ReadFile(p)
}
func firestoreListenWrite(dir, name string, value any) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	return errors.Join(err, f.Close())
}

func TestLiveOwnedFirestoreListenPinnedFront(t *testing.T) {
	if os.Getenv("SKIRK_LIVE_FIRESTORE_LISTEN") != "1" {
		t.Skip("explicit owned Listen research")
	}
	dir := filepath.Clean(os.Getenv("SKIRK_FIRESTORE_LISTEN_DIR"))
	info, err := os.Lstat(dir)
	if !filepath.IsAbs(dir) || !strings.Contains(dir, string(os.PathSeparator)+".skirk-runs"+string(os.PathSeparator)) || err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private research directory required")
	}
	var ledger struct {
		Database string `json:"database_name"`
		Created  bool   `json:"database_created"`
		Free     bool   `json:"database_free_eligible"`
	}
	raw, err := firestoreListenPrivate(dir, "recovery-ledger.json")
	if err != nil || json.Unmarshal(raw, &ledger) != nil || !ledger.Created || !ledger.Free || !regexp.MustCompile(`^projects/skirk-p/databases/skirk-front-probe-[a-f0-9]{12}$`).MatchString(ledger.Database) {
		t.Fatal("fresh owned free database proof required")
	}
	token, err := firestoreListenPrivate(dir, "access-token")
	if err != nil || len(token) == 0 || len(token) > 16384 {
		t.Fatal("secondary owner research token required")
	}
	blob, err := firestoreListenPrivate(dir, "webchannel_blob_es2018.js")
	hash := sha256.Sum256(blob)
	if err != nil || hex.EncodeToString(hash[:]) != firestoreWebChannelSHA {
		t.Fatal("pinned official WebChannel source required")
	}
	if err = firestoreListenWrite(dir, "listen-started.json", map[string]any{"pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal("research allocation already consumed")
	}
	f, err := os.OpenFile(filepath.Join(dir, "listen-journal.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("cannot retain private journal")
	}
	defer f.Close()
	journal := &firestoreListenJournal{file: f}
	client := NewGoogleHTTPClient(RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120", TimeoutSeconds: 20})
	defer client.client.CloseIdleConnections()
	dispatch := &firestoreListenDispatch{client: client, token: strings.TrimSpace(string(token)), journal: journal}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result, runErr := runFirestoreListenOwned(ctx, cancel, dir, ledger.Database, dispatch, journal)
	result["dispatch"] = dispatch.snapshot()
	result["passed"] = runErr == nil
	result["scope"] = "six owned-document AEAD states via official WebChannel longpoll; not a TCP transport or mixed messenger claim"
	if runErr != nil {
		result["error"] = errorSummary(runErr)
	}
	if err = firestoreListenWrite(dir, "listen-result.json", result); err != nil {
		t.Fatal("cannot retain bounded probe result")
	}
	if runErr != nil {
		t.Fatal("owned Listen primitive failed; private journal preserves partial results")
	}
}

func runFirestoreListenOwned(ctx context.Context, cancel context.CancelFunc, dir, database string, dispatch *firestoreListenDispatch, journal *firestoreListenJournal) (result map[string]any, runErr error) {
	result = map[string]any{}
	namespace := make([]byte, 12)
	if _, err := rand.Read(namespace); err != nil {
		return result, err
	}
	name := database + "/documents/skirk_listen_" + hex.EncodeToString(namespace) + "/record"
	result["document"] = name
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return result, err
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	bridgeSecret := make([]byte, 32)
	if _, err := rand.Read(bridgeSecret); err != nil {
		return result, err
	}
	bridge := newFirestoreListenBridge(ctx, dispatch, database, hex.EncodeToString(bridgeSecret))
	defer bridge.close()
	var command *exec.Cmd
	var nodeDone chan error
	var nodeStarted, nodeJoined bool
	var observer map[string]any
	defer func() {
		waitStarted := time.Now()
		forcedTermination, killFailed := false, false
		if nodeStarted && !nodeJoined {
			_ = firestoreListenWrite(dir, "stop-listen.json", map[string]bool{"stop": true})
			select {
			case err := <-nodeDone:
				nodeJoined = true
				if err != nil {
					runErr = errors.Join(runErr, errors.New("official observer exited with error"))
				}
			case <-time.After(5 * time.Second):
				forcedTermination = true
				killFailed = command.Process.Kill() != nil
				select {
				case <-nodeDone:
					nodeJoined = true
				case <-time.After(2 * time.Second):
					runErr = errors.Join(runErr, errors.New("official observer was not joined after termination"))
				}
				runErr = errors.Join(runErr, errors.New("official observer required bounded termination"))
			}
		}
		result["observer_started"] = nodeStarted
		result["observer_joined"] = nodeJoined
		result["observer_forced_termination"] = forcedTermination
		result["observer_kill_failed"] = killFailed
		result["observer_wait_ms"] = float64(time.Since(waitStarted).Microseconds()) / 1000
		if nodeJoined && command.ProcessState != nil {
			result["observer_exit_code"] = command.ProcessState.ExitCode()
		}
		bridge.close()
		if data, err := firestoreListenPrivate(dir, "observer-result.json"); err == nil {
			_ = json.Unmarshal(data, &observer)
		}
		result["observer"] = observer
		if nodeStarted && (observer == nil || observer["stopped"] != true || observer["error"] != "") {
			runErr = errors.Join(runErr, errors.New("official observer did not report joined successful shutdown"))
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 50*time.Second)
		defer cleanupCancel()
		deleted, err := dispatch.call(cleanupCtx, "cleanup_delete", http.MethodDelete, "/v1/"+name, nil, nil, true)
		result["delete_ok"] = err == nil && deleted != nil && deleted.Status == 200
		immediate, e := dispatch.call(cleanupCtx, "cleanup_immediate", http.MethodGet, "/v1/"+name, nil, nil, true)
		result["immediate_404"] = e == nil && immediate != nil && immediate.Status == 404
		select {
		case <-time.After(31 * time.Second):
		case <-cleanupCtx.Done():
			runErr = errors.Join(runErr, cleanupCtx.Err())
			return
		}
		late, e2 := dispatch.call(cleanupCtx, "cleanup_delayed", http.MethodGet, "/v1/"+name, nil, nil, true)
		result["delayed_404"] = e2 == nil && late != nil && late.Status == 404
		if result["delete_ok"] != true || result["immediate_404"] != true || result["delayed_404"] != true {
			runErr = errors.Join(runErr, err, e, e2, errors.New("exact owned document cleanup not proven"))
		}
	}()
	type sample struct {
		Sequence             int `json:"sequence"`
		WriteAckMS, ListenMS float64
		Verified             bool
	}
	samples := make([]sample, 0, 6)
	seen := map[string]bool{}
	duplicates := 0
	result["duplicate_prior_events"] = 0
	for seq := 0; seq < 6; seq++ {
		plain := make([]byte, 2048)
		_, err := rand.Read(plain)
		if err != nil {
			return result, err
		}
		binary.BigEndian.PutUint64(plain, uint64(seq))
		nonce := make([]byte, aead.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return result, err
		}
		wire := aead.Seal(nonce, nonce, plain, []byte("skirk-owned-firestore-listen-v1"))
		wire64 := base64.StdEncoding.EncodeToString(wire)
		plainHash, wireHash := sha256.Sum256(plain), sha256.Sum256(wire)
		if err = journal.write(map[string]any{"kind": "state_prepared", "sequence": seq, "plaintext_sha256": hex.EncodeToString(plainHash[:]), "ciphertext_sha256": hex.EncodeToString(wireHash[:])}); err != nil {
			return result, err
		}
		body, _ := json.Marshal(map[string]any{"fields": map[string]any{"sealed": map[string]string{"bytesValue": wire64}}})
		path := "/v1/" + name
		if seq == 0 {
			path += "?currentDocument.exists=false"
		}
		started := time.Now()
		response, err := dispatch.call(ctx, fmt.Sprintf("write_%d", seq), http.MethodPatch, path, map[string]string{"Content-Type": "application/json"}, body, false)
		ack := time.Since(started)
		if err != nil || response == nil || response.Status != 200 {
			return result, errors.Join(err, errors.New("owned document update failed"))
		}
		if seq == 0 {
			_, file, _, _ := runtime.Caller(0)
			script := filepath.Clean(filepath.Join(filepath.Dir(file), "../../experiments/firestore-listen/observer.mjs"))
			config := map[string]any{"bridge_url": bridge.server.URL, "bridge_auth": bridge.auth, "database": database, "document": name, "access_token": dispatch.token, "webchannel_blob": filepath.Join(dir, "webchannel_blob_es2018.js"), "duration_ms": 60000}
			if err = firestoreListenWrite(dir, "observer-config.json", config); err != nil {
				return result, err
			}
			command = exec.CommandContext(ctx, "node", script, filepath.Join(dir, "observer-config.json"))
			for _, env := range os.Environ() {
				key, _, _ := strings.Cut(env, "=")
				if key == "HTTP_PROXY" || key == "HTTPS_PROXY" || key == "ALL_PROXY" || key == "http_proxy" || key == "https_proxy" || key == "all_proxy" || key == "NODE_OPTIONS" || key == "NODE_USE_ENV_PROXY" || key == "SKIRK_FIRESTORE_LISTEN_OBSERVER" {
					continue
				}
				command.Env = append(command.Env, env)
			}
			command.Env = append(command.Env, "SKIRK_FIRESTORE_LISTEN_OBSERVER=1", "NO_PROXY=127.0.0.1,localhost,::1")
			if err = command.Start(); err != nil {
				return result, err
			}
			nodeDone = make(chan error, 1)
			nodeStarted = true
			go func() { nodeDone <- command.Wait() }()
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
		var delivered firestoreListenEvent
		for delivered.Kind == "" {
			select {
			case err := <-nodeDone:
				nodeJoined = true
				waitCancel()
				return result, errors.Join(err, errors.New("official observer exited before expected state"))
			case event := <-bridge.events:
				if event.Kind == "error" {
					waitCancel()
					return result, fmt.Errorf("official listener rejected code=%s", event.Code)
				}
				if event.Kind != "document" {
					continue
				}
				hash := sha256.Sum256([]byte(event.Wire))
				_ = journal.write(map[string]any{"kind": "document_event", "encoded_ciphertext_sha256": hex.EncodeToString(hash[:]), "received_unix_ms": event.At})
				if event.Name != name {
					waitCancel()
					return result, errors.New("unexpected listener namespace")
				}
				if seen[event.Wire] {
					duplicates++
					result["duplicate_prior_events"] = duplicates
					continue
				}
				if event.Wire != wire64 {
					waitCancel()
					return result, errors.New("unexpected ciphertext version")
				}
				delivered = event
			case <-waitCtx.Done():
				waitCancel()
				return result, errors.New("owned document notification timed out")
			}
		}
		waitCancel()
		gotWire, err := base64.StdEncoding.Strict().DecodeString(delivered.Wire)
		if err != nil || !bytes.Equal(gotWire, wire) {
			return result, errors.New("listener ciphertext mismatch")
		}
		got, err := aead.Open(nil, gotWire[:aead.NonceSize()], gotWire[aead.NonceSize():], []byte("skirk-owned-firestore-listen-v1"))
		if err != nil || !bytes.Equal(got, plain) {
			return result, errors.New("listener AEAD mismatch")
		}
		seen[wire64] = true
		sample := sample{Sequence: seq, WriteAckMS: float64(ack.Microseconds()) / 1000, ListenMS: delivered.At - float64(started.UnixMicro())/1000, Verified: true}
		if sample.ListenMS < 0 || sample.ListenMS > 120000 {
			return result, errors.New("listener event timing outside probe lifetime")
		}
		samples = append(samples, sample)
		result["samples"] = samples
		if err = journal.write(map[string]any{"kind": "verified_sample", "sample": sample}); err != nil {
			return result, err
		}
	}
	return result, nil
}
