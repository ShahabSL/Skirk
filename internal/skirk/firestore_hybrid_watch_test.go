package skirk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"syscall"
	"time"
)

type fsHybridWatchEvent struct {
	Kind       string  `json:"kind"`
	Name       string  `json:"name,omitempty"`
	Wire       string  `json:"wire_base64,omitempty"`
	UpdateTime string  `json:"update_time,omitempty"`
	At         float64 `json:"received_unix_ms,omitempty"`
	Code       string  `json:"code,omitempty"`
	Change     string  `json:"change,omitempty"`
	IDs        []int   `json:"ids,omitempty"`
}

type fsHybridWatchBridge struct {
	carrier                  *fsHybridGoogleCarrier
	auth                     string
	server                   *httptest.Server
	mu                       sync.Mutex
	closed                   bool
	wg                       sync.WaitGroup
	closeOnce                sync.Once
	requestSlots, eventSlots chan struct{}
	events                   chan fsHybridWatchEvent
	failed                   chan error
}

func newFSHybridWatchBridge(c *fsHybridGoogleCarrier, auth string) *fsHybridWatchBridge {
	b := &fsHybridWatchBridge{carrier: c, auth: auth, requestSlots: make(chan struct{}, 4), eventSlots: make(chan struct{}, 1), events: make(chan fsHybridWatchEvent, 32), failed: make(chan error, 1)}
	b.server = httptest.NewUnstartedServer(http.HandlerFunc(b.serve))
	b.server.Config.ReadHeaderTimeout = 5 * time.Second
	b.server.Config.ReadTimeout = 5 * time.Second
	b.server.Config.WriteTimeout = 25 * time.Second
	b.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.server.Config.BaseContext = func(net.Listener) context.Context { return c.ctx }
	b.server.Start()
	return b
}
func (b *fsHybridWatchBridge) fail(err error) {
	select {
	case b.failed <- err:
	default:
	}
}
func (b *fsHybridWatchBridge) close() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.server.CloseClientConnections()
		b.server.Close()
		b.wg.Wait()
	})
}

func (c *fsHybridGoogleCarrier) watchPath(path string) bool {
	if len(path) > 16384 || strings.Contains(path, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || parsed.Path != firestoreListenPath || parsed.Fragment != "" || len(parsed.Query()["database"]) != 1 || parsed.Query().Get("database") != c.cfg.Database {
		return false
	}
	for _, values := range parsed.Query() {
		if len(values) != 1 {
			return false
		}
	}
	decoded := parsed.RawQuery
	for range 4 {
		next, e := url.QueryUnescape(decoded)
		if e != nil {
			return false
		}
		lower := strings.ToLower(next)
		if strings.Contains(lower, "authorization") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "$httpheaders") || strings.Contains(next, c.cfg.OwnerToken) {
			return false
		}
		if next == decoded {
			return true
		}
		decoded = next
	}
	return false
}

var fsHybridDataField = regexp.MustCompile(`^req[0-9]{1,3}___data__$`)
var fsHybridDecimal = regexp.MustCompile(`^[0-9]{1,16}$`)

func (c *fsHybridGoogleCarrier) watchPayload(method string, raw []byte) bool {
	if len(raw) > 128<<10 {
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if method != http.MethodPost {
		return false
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || len(form) > 16 {
		return false
	}
	for key, values := range form {
		if len(values) != 1 {
			return false
		}
		value := values[0]
		switch {
		case key == "count" || key == "ofs":
			if !fsHybridDecimal.MatchString(value) {
				return false
			}
		case key == "headers":
			if len(value) > 32768 || !strings.Contains(value, "Bearer "+c.cfg.OwnerToken) {
				return false
			}
		case fsHybridDataField.MatchString(key):
			var packet struct {
				Database string `json:"database"`
				Add      *struct {
					Documents struct {
						Documents []string `json:"documents"`
					} `json:"documents"`
					TargetID int `json:"targetId"`
				} `json:"addTarget"`
				Remove int `json:"removeTarget"`
			}
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&packet) != nil || decoder.Decode(new(any)) != io.EOF || packet.Database != c.cfg.Database {
				return false
			}
			c.mu.Lock()
			closing := c.closing
			c.mu.Unlock()
			if packet.Add != nil {
				if closing || packet.Remove != 0 || packet.Add.TargetID != 2 || len(packet.Add.Documents.Documents) != 1 || packet.Add.Documents.Documents[0] != c.cfg.PeerDocument {
					return false
				}
			} else if packet.Remove != 2 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (b *fsHybridWatchBridge) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		http.Error(w, "bridge_closed", 503)
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	reject := func(status int, code string) {
		b.carrier.mu.Lock()
		b.carrier.stats.RejectedLocal++
		b.carrier.mu.Unlock()
		http.Error(w, code, status)
	}
	if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.auth)) != 1 {
		reject(403, "unauthorized")
		return
	}
	if r.URL.Path != "/event" && r.URL.Path != "/request" {
		reject(404, "unknown_route")
		return
	}
	slots := b.requestSlots
	if r.URL.Path == "/event" {
		slots = b.eventSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		reject(429, "local_concurrency_bound")
		b.fail(errFSHybridBounds)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		reject(413, "local_body_bound")
		b.fail(errFSHybridBounds)
		return
	}
	if r.URL.Path == "/event" {
		var event fsHybridWatchEvent
		if json.Unmarshal(raw, &event) != nil || len(event.Name) > 512 || len(event.Wire) > base64.StdEncoding.EncodedLen(fsHybridWireMax) || len(event.IDs) > 8 || len(event.Code) > 64 || len(event.UpdateTime) > 64 {
			reject(400, "event_shape")
			b.fail(errFSHybridIntegrity)
			return
		}
		switch event.Kind {
		case "document":
			wire, e := base64.StdEncoding.Strict().DecodeString(event.Wire)
			if event.Name != b.carrier.cfg.PeerDocument || e != nil || base64.StdEncoding.EncodeToString(wire) != event.Wire || len(wire) > fsHybridWireMax || !fsHybridServerTimestamp(event.UpdateTime) {
				reject(400, "document_identity")
				b.fail(errFSHybridIntegrity)
				return
			}
		case "target":
			if event.Change != "NO_CHANGE" && event.Change != "ADD" && event.Change != "REMOVE" && event.Change != "CURRENT" && event.Change != "RESET" {
				reject(400, "target_shape")
				b.fail(errFSHybridIntegrity)
				return
			}
			for _, id := range event.IDs {
				if id != 2 {
					reject(400, "target_identity")
					b.fail(errFSHybridIntegrity)
					return
				}
			}
		case "error":
			if !regexp.MustCompile(`^[A-Z_0-9]{1,64}$`).MatchString(event.Code) {
				reject(400, "error_shape")
				b.fail(errFSHybridIntegrity)
				return
			}
		default:
			reject(400, "event_kind")
			b.fail(errFSHybridIntegrity)
			return
		}
		b.carrier.mu.Lock()
		b.carrier.stats.Events++
		seen := b.carrier.stats.Events
		if event.Kind == "document" {
			b.carrier.stats.Documents++
		}
		b.carrier.mu.Unlock()
		if seen > int64(b.carrier.cfg.MaxWatchEvents) {
			reject(413, "event_count_bound")
			b.fail(errFSHybridBounds)
			return
		}
		select {
		case b.events <- event:
			w.WriteHeader(204)
		case <-r.Context().Done():
			return
		default:
			reject(429, "event_queue_bound")
			b.fail(errFSHybridBounds)
		}
		return
	}
	var request struct {
		Method, Path string
		Headers      map[string]string
		Body         string `json:"body_base64"`
	}
	if json.Unmarshal(raw, &request) != nil || !b.carrier.watchPath(request.Path) || request.Method != http.MethodGet && request.Method != http.MethodPost || len(request.Headers) > 32 {
		reject(403, "owned_listener_only")
		b.fail(errFSHybridIntegrity)
		return
	}
	for k, v := range request.Headers {
		if len(k) > 128 || len(v) > 16384 {
			reject(413, "header_bound")
			b.fail(errFSHybridBounds)
			return
		}
	}
	body, err := base64.StdEncoding.Strict().DecodeString(request.Body)
	if err != nil || !b.carrier.watchPayload(request.Method, body) {
		reject(403, "owned_payload_only")
		b.fail(errFSHybridIntegrity)
		return
	}
	b.carrier.mu.Lock()
	cleanup := b.carrier.closing
	b.carrier.mu.Unlock()
	result, err := b.carrier.call(r.Context(), "listen", request.Method, request.Path, request.Headers, body, cleanup)
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
	_ = json.NewEncoder(w).Encode(map[string]any{"status": result.Status, "headers": headers, "body_base64": base64.StdEncoding.EncodeToString(result.Body)})
}

func (c *fsHybridGoogleCarrier) stopWatch() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.watchStop) })
	_ = firestoreListenWrite(c.cfg.PrivateDir, "stop-hybrid-watch.json", map[string]bool{"stop": true})
}

func fsHybridBlobDigest(blob []byte) string {
	hash := sha256.Sum256(blob)
	return hex.EncodeToString(hash[:])
}

func (c *fsHybridGoogleCarrier) WatchSnapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := make(map[string]any, len(c.watchState))
	for k, v := range c.watchState {
		copy[k] = v
	}
	return copy
}

func (c *fsHybridGoogleCarrier) WatchPeer(ctx context.Context, callback func(fsPriorityDocument) error) (runErr error) {
	if ctx == nil {
		return errFSHybridBounds
	}
	c.mu.Lock()
	if c.closed || c.closing || c.watchStarted || callback == nil {
		c.mu.Unlock()
		return errFSHybridClosed
	}
	c.watchStarted = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.watchErr = runErr; c.mu.Unlock(); close(c.watchDone) }()
	blob, err := firestoreListenPrivate(c.cfg.PrivateDir, "webchannel_blob_es2018.js")
	if err != nil {
		return errFSHybridBounds
	}
	// Keep this check local: no downloading or unpinned SDK is permitted.
	if fsHybridBlobDigest(blob) != firestoreWebChannelSHA {
		return errFSHybridIntegrity
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return &fsHybridControlError{err}
	}
	bridge := newFSHybridWatchBridge(c, hex.EncodeToString(secret))
	c.mu.Lock()
	c.bridge = bridge
	c.mu.Unlock()
	defer bridge.close()
	configPath := filepath.Join(c.cfg.PrivateDir, "hybrid-watch-config.json")
	config := map[string]any{"hybrid_research": true, "bridge_url": bridge.server.URL, "bridge_auth": bridge.auth, "database": c.cfg.Database, "document": c.cfg.PeerDocument, "access_token": c.cfg.OwnerToken, "webchannel_blob": filepath.Join(c.cfg.PrivateDir, "webchannel_blob_es2018.js"), "duration_ms": c.cfg.WatchDuration.Milliseconds(), "request_limit": c.cfg.MaxWatchRequests, "event_limit": c.cfg.MaxWatchEvents}
	if err = firestoreListenWrite(c.cfg.PrivateDir, "hybrid-watch-config.json", config); err != nil {
		return errFSHybridBounds
	}
	defer os.Remove(configPath)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../experiments/firestore-hybrid/watch.mjs"))
	command := exec.Command("node", script, configPath)
	command.WaitDelay = 2 * time.Second
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NODE_OPTIONS", "NODE_PATH", "NODE_USE_ENV_PROXY", "SKIRK_FIRESTORE_LISTEN_OBSERVER", "SKIRK_FIRESTORE_HYBRID_WATCH":
			continue
		}
		command.Env = append(command.Env, env)
	}
	command.Env = append(command.Env, "SKIRK_FIRESTORE_HYBRID_WATCH=1", "NO_PROXY=127.0.0.1,localhost,::1")
	output, err := os.OpenFile(filepath.Join(c.cfg.PrivateDir, "hybrid-watch.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errFSHybridBounds
	}
	defer output.Close()
	command.Stdout = output
	command.Stderr = output
	c.mu.Lock()
	closed := c.closed || c.closing
	c.mu.Unlock()
	if closed {
		return errFSHybridClosed
	}
	if err = command.Start(); err != nil {
		return &fsHybridControlError{errors.New("owned Node observer start failed")}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	defer func() {
		c.stopWatch()
		if !joined {
			select {
			case err := <-done:
				joined = true
				if runErr == nil {
					runErr = err
				}
			case <-time.After(6 * time.Second):
				_ = command.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
					joined = true
				case <-time.After(time.Second):
					_ = command.Process.Kill()
					select {
					case <-done:
						joined = true
					case <-time.After(2 * time.Second):
					}
				}
			}
		}
		if !joined {
			runErr = errors.Join(runErr, errors.New("owned Node observer did not join"))
		}
		raw, readErr := firestoreListenPrivate(c.cfg.PrivateDir, "hybrid-watch-result.json")
		var state map[string]any
		if readErr != nil || json.Unmarshal(raw, &state) != nil {
			runErr = errors.Join(runErr, errors.New("owned observer result missing"))
			return
		}
		c.mu.Lock()
		c.watchState = state
		c.mu.Unlock()
		if state["stopped"] != true || state["target_remove_acknowledged"] != true || state["channel_closed"] != true || state["error"] != "" || state["pending_requests"] != float64(0) || state["pending_events"] != float64(0) || state["pending_sdk_timers"] != float64(0) {
			runErr = errors.Join(runErr, errors.New("owned observer shutdown proof incomplete"))
		}
	}()
	timer := time.NewTimer(c.cfg.WatchDuration + 5*time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-bridge.events:
			if event.Kind == "error" {
				return errors.New("owned Firestore listener reported failure")
			}
			if event.Kind == "document" {
				wire, _ := base64.StdEncoding.Strict().DecodeString(event.Wire)
				if err = callback(fsPriorityDocument{Ciphertext: wire, UpdateTime: event.UpdateTime}); err != nil {
					return err
				}
			}
		case err = <-bridge.failed:
			return err
		case err = <-done:
			joined = true
			return errors.Join(err, errors.New("owned observer exited before Close"))
		case <-ctx.Done():
			return ctx.Err()
		case <-c.parent.Done():
			return c.parent.Err()
		case <-c.ctx.Done():
			return errFSHybridClosed
		case <-timer.C:
			return errFSHybridBounds
		case <-c.watchStop:
			return nil
		}
	}
}

func (c *fsHybridGoogleCarrier) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		started := c.watchStarted
		c.mu.Unlock()
		c.stopWatch()
		if started {
			select {
			case <-c.watchDone:
			case <-time.After(11 * time.Second):
				c.closeErr = errors.New("owned watch owner did not join")
			}
		}
		c.mu.Lock()
		c.finished = true
		bridge := c.bridge
		c.mu.Unlock()
		c.cancel()
		if bridge != nil {
			bridge.close()
		}
		c.client.client.CloseIdleConnections()
		requestsDone := make(chan struct{})
		go func() { c.requestWG.Wait(); c.operationWG.Wait(); close(requestsDone) }()
		select {
		case <-requestsDone:
		case <-time.After(2 * time.Second):
			c.closeErr = errors.Join(c.closeErr, errors.New("owned control requests did not join"))
		}
		c.mu.Lock()
		c.closeErr = errors.Join(c.closeErr, c.watchErr)
		c.mu.Unlock()
	})
	return c.closeErr
}
