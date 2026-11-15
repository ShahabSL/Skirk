package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gate visible Drive RoundTripper dispatches across all independent pools.
// Lower transports can retry internally; WroteRequest callbacks expose completed
// write attempts, not Google acceptance or an exact billable request count.
// Stopping acquires the write lock after in-flight HTTP calls finish. Future
// mux goroutines fail locally; they cannot race cleanup with a late upload.
type hybridDriveGate struct {
	mu      sync.RWMutex
	stopped bool
	base    http.RoundTripper
	budget  *hybridBudget
	cleanup bool
	onGuard func()
}

func (g *hybridDriveGate) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.RLock()
	if g.stopped {
		g.mu.RUnlock()
		g.budget.mu.Lock()
		g.budget.StoppedDenials++
		g.budget.mu.Unlock()
		return nil, context.Canceled
	}
	cost := 5
	if r.Method == "GET" && strings.Contains(r.URL.Path, "/files") {
		if r.URL.Query().Get("alt") == "media" {
			cost = 200
		} else if strings.HasSuffix(r.URL.Path, "/files") {
			cost = 100
		}
	} else if r.Method != "GET" {
		cost = 50
	}
	if e := g.budget.reserveDrive(cost, g.cleanup); e != nil {
		g.mu.RUnlock()
		if g.onGuard != nil {
			g.onGuard()
		}
		return nil, e
	}
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		g.budget.mu.Lock()
		g.budget.WriteCallbacks++
		if info.Err != nil {
			g.budget.WriteErrorCallbacks++
		}
		g.budget.mu.Unlock()
	}}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	response, err := g.base.RoundTrip(r)
	if err != nil || response == nil || response.Body == nil {
		g.mu.RUnlock()
		return response, err
	}
	response.Body = &hybridGateBody{ReadCloser: response.Body, release: g.mu.RUnlock}
	return response, nil
}

type hybridGateBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *hybridGateBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if e != nil {
		b.once.Do(b.release)
	}
	return n, e
}
func (b *hybridGateBody) Close() error { e := b.ReadCloser.Close(); b.once.Do(b.release); return e }
func (g *hybridDriveGate) stop()       { g.mu.Lock(); g.stopped = true; g.mu.Unlock() }

func (g *hybridDriveGate) CloseIdleConnections() {
	if c, ok := g.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// The shared budget reserves a conservative unit weight for each physical
// request. Per-peer observed calls/units come from DriveStore.QuotaSnapshot.
func hybridGateDrive(store *DriveStore, b *hybridBudget, cleanup bool) []*hybridDriveGate {
	seen := map[*GoogleHTTPClient]bool{}
	var gates []*hybridDriveGate
	for _, c := range []*GoogleHTTPClient{store.http, store.normalUploadHTTP, store.normalDownloadHTTP} {
		if c == nil || seen[c] {
			continue
		}
		seen[c] = true
		g := &hybridDriveGate{base: c.client.Transport, budget: b, cleanup: cleanup}
		c.client.Transport = g
		gates = append(gates, g)
	}
	return gates
}

type hybridLiveRow struct {
	Kind         string  `json:"kind"`
	Pool         int     `json:"pool,omitempty"`
	Sequence     int     `json:"sequence,omitempty"`
	Bytes        int64   `json:"bytes"`
	PublishMS    float64 `json:"publish_response_ms,omitempty"`
	TTFBMS       float64 `json:"ttfb_ms,omitempty"`
	TotalMS      float64 `json:"total_ms"`
	SHA256       string  `json:"sha256"`
	OK           bool    `json:"ok"`
	Failure      string  `json:"failure_category,omitempty"`
	RequestBytes int64   `json:"request_bytes,omitempty"`
}
type hybridBoundedLog struct {
	mu                      sync.Mutex
	writer                  io.Writer
	limit, written, dropped int64
}

func (b *hybridBoundedLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	allowed := int64(n)
	if b.written+allowed > b.limit {
		allowed = b.limit - b.written
	}
	if allowed > 0 {
		got, e := b.writer.Write(data[:allowed])
		b.written += int64(got)
		if e != nil {
			return got, e
		}
	}
	b.dropped += int64(n) - allowed
	return n, nil
}
func (b *hybridBoundedLog) stats() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]int64{"written_bytes": b.written, "dropped_bytes": b.dropped}
}

type hybridBodyProgress struct {
	base        io.Reader
	bytes, next int64
	emit        func(int64)
}

func (p *hybridBodyProgress) Read(data []byte) (int, error) {
	n, e := p.base.Read(data)
	p.bytes += int64(n)
	if p.bytes >= p.next {
		p.next = (p.bytes/(1<<20) + 1) * (1 << 20)
		if p.emit != nil {
			p.emit(p.bytes)
		}
	}
	return n, e
}
func hybridHTTPFailure(e error) string {
	if errors.Is(e, context.Canceled) {
		return "cancelled"
	}
	var timeout net.Error
	if errors.Is(e, context.DeadlineExceeded) || (errors.As(e, &timeout) && timeout.Timeout()) {
		return "http_timeout"
	}
	return "http_error"
}

type hybridProgress struct {
	mu     sync.Mutex
	file   *os.File
	err    error
	cancel context.CancelFunc
}

func (p *hybridProgress) write(value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	raw, e := json.Marshal(value)
	if e == nil {
		_, e = p.file.Write(append(raw, '\n'))
	}
	if e == nil {
		e = p.file.Sync()
	}
	if e != nil {
		p.err = e
		if p.cancel != nil {
			p.cancel()
		}
	}
}
func (p *hybridProgress) error() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }

type hybridCloudResources struct {
	topics, subs []string
	control      hybridCall
}

func (c *hybridCloudResources) create(ctx context.Context, label string) error {
	for _, direction := range []string{"up", "down"} {
		topic := "projects/skirk-p/topics/" + label + "-" + direction
		sub := "projects/skirk-p/subscriptions/" + label + "-" + direction
		c.topics = append(c.topics, topic)
		c.subs = append(c.subs, sub)
	}
	for i, topic := range c.topics {
		r, e := c.control(ctx, "PUT", "/v1/"+topic, map[string]any{})
		if e = pubsubStreamRequire(r, e); e != nil {
			return e
		}
		if e = checkDriveServerClock(r.Header, r.RequestStarted, r.ResponseReceived); e != nil {
			return e
		}
		r, e = c.control(ctx, "PUT", "/v1/"+c.subs[i], map[string]any{"topic": topic, "ackDeadlineSeconds": 30, "messageRetentionDuration": "600s", "expirationPolicy": map[string]any{"ttl": "86400s"}})
		if e = pubsubStreamRequire(r, e); e != nil {
			return e
		}
	}
	return nil
}
func (c *hybridCloudResources) cleanup(ctx context.Context, delay time.Duration) bool {
	names := append(append([]string{}, c.subs...), c.topics...)
	ok := true
	for _, name := range names {
		r, e := c.control(ctx, "DELETE", "/v1/"+name, nil)
		if e != nil || r == nil || (r.Status != 200 && r.Status != 404) {
			ok = false
		}
	}
	missing := func() {
		for _, name := range names {
			r, e := c.control(ctx, "GET", "/v1/"+name, nil)
			if e != nil || r == nil || r.Status != 404 {
				ok = false
			}
		}
	}
	missing()
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
	}
	missing()
	return ok
}
func hybridBindMux(store *hybridStore, mux *driveMux, ctx context.Context) {
	store.startedAt = mux.startedAt
	store.receive = func(info ObjectInfo) bool {
		if !controlIsFresh(info, mux.startedAt) {
			return false
		}
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			return false
		}
		return mux.enqueueMuxObject(ctx, meta)
	}
}
func hybridPoolMeasurements(ctx context.Context, client, exit *hybridStore, record func(hybridLiveRow)) ([]hybridLiveRow, error) {
	var rows []hybridLiveRow
	sid := client.sid
	seq := uint64(0)
	for _, pool := range []int{1, 2, 4, 4, 2, 1} {
		received := make(chan struct {
			info ObjectInfo
			at   time.Time
		}, 32)
		exit.receive = func(info ObjectInfo) bool {
			select {
			case received <- struct {
				info ObjectInfo
				at   time.Time
			}{info, time.Now()}:
				return true
			default:
				return false
			}
		}
		if e := exit.start(ctx, pool); e != nil {
			return rows, e
		}
		time.Sleep(200 * time.Millisecond)
		for i := 0; i < 3; i++ {
			seq++
			payload := make([]byte, 256)
			if _, e := rand.Read(payload); e != nil {
				exit.stop()
				return rows, e
			}
			raw, e := encodeMuxBatch([]muxFrame{{Kind: muxFrameData, StreamID: 7, Seq: seq, Payload: payload}})
			if e != nil {
				exit.stop()
				return rows, e
			}
			key, e := DeriveMuxLaneKeyV4(client.secret, sid, DirectionUp, client.clientID, client.runID, 0)
			if e != nil {
				exit.stop()
				return rows, e
			}
			sealed, e := Seal(key, sid, DirectionUp, seq, raw, false)
			if e != nil {
				exit.stop()
				return rows, e
			}
			name := muxObjectNameWithStreamIDs(sid, DirectionUp, client.clientID, client.runID, "pool", 7, []uint64{7}, 0, seq, 1, seq, seq, len(raw), true)
			start := time.Now()
			_, e = client.PutObject(ctx, name, sealed)
			publish := time.Since(start)
			if e != nil {
				exit.stop()
				return rows, e
			}
			select {
			case got := <-received:
				if got.info.Name != name {
					exit.stop()
					return rows, errors.New("hybrid pool reordered unexpected object")
				}
				b, e := exit.GetByID(ctx, got.info.ID)
				if e != nil || !bytes.Equal(b, sealed) {
					exit.stop()
					return rows, errors.New("hybrid pool exact authenticated bytes mismatch")
				}
				if e = exit.DeleteID(ctx, got.info.ID); e != nil {
					exit.stop()
					return rows, e
				}
				h := sha256.Sum256(payload)
				row := hybridLiveRow{Kind: "one_way_256b", Pool: pool, Sequence: int(seq), Bytes: 256, PublishMS: float64(publish.Microseconds()) / 1000, TotalMS: float64(got.at.Sub(start).Microseconds()) / 1000, SHA256: hex.EncodeToString(h[:]), OK: true}
				rows = append(rows, row)
				if record != nil {
					record(row)
				}
			case <-ctx.Done():
				exit.stop()
				return rows, ctx.Err()
			}
		}
		exit.stop()
		if e := exit.error(); e != nil {
			return rows, e
		}
	}
	return rows, nil
}

// Exercise real TCP/SOCKS/mux code. User traffic and external targets are absent.
func hybridSOCKSMeasurements(ctx context.Context, cfg *Config, clientData, exitData BlobStore, clientHybrid, exitHybrid *hybridStore, bulkBytes int, record func(hybridLiveRow), logger *log.Logger, event func(map[string]any)) ([]hybridLiveRow, error) {
	payload := make([]byte, bulkBytes)
	if _, e := rand.Read(payload); e != nil {
		return nil, e
	}
	wantHash := sha256.Sum256(payload)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bulk":
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		case "/upload":
			h := sha256.New()
			n, e := io.Copy(h, io.LimitReader(r.Body, int64(bulkBytes)+1))
			if e != nil || n != int64(bulkBytes) || !bytes.Equal(h.Sum(nil), wantHash[:]) {
				http.Error(w, "exact upload mismatch", 400)
				return
			}
			_, _ = io.WriteString(w, hex.EncodeToString(h.Sum(nil)))
		case "/text":
			w.Header().Set("Content-Length", "64")
			_, _ = w.Write(bytes.Repeat([]byte("t"), 64))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	client, e := NewTunnel(clientData, cfg)
	if e != nil {
		return nil, e
	}
	exit, e := NewTunnel(exitData, cfg)
	if e != nil {
		return nil, e
	}
	client.Logger = logger
	if client.Logger == nil {
		client.Logger = log.New(io.Discard, "", 0)
	}
	exit.Logger = client.Logger
	client.setRole("client")
	exit.setRole("exit")
	cm, e := newDriveMux(client, "client", DirectionUp, DirectionDown)
	if e != nil {
		return nil, e
	}
	em, e := newDriveMux(exit, "exit", DirectionDown, DirectionUp)
	if e != nil {
		return nil, e
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer cm.closeAll()
	defer em.closeAll()
	if clientHybrid != nil {
		hybridBindMux(clientHybrid, cm, runCtx)
		hybridBindMux(exitHybrid, em, runCtx)
		if e = clientHybrid.start(runCtx, 2); e != nil {
			return nil, e
		}
		defer clientHybrid.stop()
		if e = exitHybrid.start(runCtx, 2); e != nil {
			return nil, e
		}
		defer exitHybrid.stop()
	}
	cm.start(runCtx)
	em.start(runCtx)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	address := listener.Addr().String()
	listener.Close()
	socks := SOCKSServer{Listen: address, Logger: client.Logger, Handler: func(connCtx context.Context, target string, conn net.Conn) {
		_ = cm.openClientStream(connCtx, target, conn)
	}}
	listenerDone := make(chan error, 1)
	go func() { listenerDone <- socks.Serve(runCtx) }()
	defer func() { cancel(); cm.closeAll(); em.closeAll(); <-listenerDone }()
	transport := &http.Transport{DisableKeepAlives: false, MaxConnsPerHost: 32, DialContext: func(dialCtx context.Context, _, target string) (net.Conn, error) {
		return dialViaSOCKS5(dialCtx, "socks5h://"+address, target)
	}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 3 * time.Minute}
	// Waiting for the local listener does not create a remote tunnel stream.
	ready := false
	for i := 0; i < 100; i++ {
		conn, e := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if e == nil {
			conn.Close()
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !ready {
		return nil, errors.New("hybrid SOCKS listener unavailable")
	}
	var mu sync.Mutex
	var rows []hybridLiveRow
	var failures []error
	request := func(kind, path string, body []byte, sequence int) {
		start := time.Now()
		method := "GET"
		if body != nil {
			method = "POST"
		}
		finish := func(row hybridLiveRow, e error) {
			mu.Lock()
			rows = append(rows, row)
			if e != nil {
				failures = append(failures, e)
			}
			mu.Unlock()
			if record != nil {
				record(row)
			}
		}
		emit := func(direction string, n int64) {
			if event != nil {
				event(map[string]any{"phase": "bulk_progress", "kind": kind, "sequence": sequence, "direction": direction, "bytes": n})
			}
		}
		var requestBody io.Reader = bytes.NewReader(body)
		if len(body) > 0 && strings.HasPrefix(kind, "bulk_") {
			requestBody = &hybridBodyProgress{base: requestBody, next: 1 << 20, emit: func(n int64) { emit("request_body_read_by_http_transport", n) }}
		}
		req, e := http.NewRequestWithContext(ctx, method, origin.URL+path, requestBody)
		if e != nil {
			finish(hybridLiveRow{Kind: kind, Sequence: sequence, RequestBytes: int64(len(body)), TotalMS: float64(time.Since(start).Microseconds()) / 1000, Failure: "request_build"}, errors.New("hybrid HTTP request build failed"))
			return
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}
		response, e := httpClient.Do(req)
		ttfb := time.Since(start)
		if e != nil {
			category := hybridHTTPFailure(e)
			finish(hybridLiveRow{Kind: kind, Sequence: sequence, RequestBytes: int64(len(body)), TotalMS: float64(ttfb.Microseconds()) / 1000, Failure: category}, fmt.Errorf("hybrid HTTP %s", category))
			return
		}
		var responseBody io.Reader = response.Body
		if strings.HasPrefix(kind, "bulk_") {
			responseBody = &hybridBodyProgress{base: responseBody, next: 1 << 20, emit: func(n int64) { emit("response_body_read_by_application", n) }}
		}
		data, e := io.ReadAll(io.LimitReader(responseBody, int64(bulkBytes)+1))
		response.Body.Close()
		elapsed := time.Since(start)
		expected := payload
		if path == "/text" {
			expected = bytes.Repeat([]byte("t"), 64)
		}
		if path == "/upload" {
			expected = []byte(hex.EncodeToString(wantHash[:]))
		}
		ok := e == nil && response.StatusCode == 200 && bytes.Equal(data, expected)
		h := sha256.Sum256(data)
		row := hybridLiveRow{Kind: kind, Sequence: sequence, Bytes: int64(len(data)), RequestBytes: int64(len(body)), TTFBMS: float64(ttfb.Microseconds()) / 1000, TotalMS: float64(elapsed.Microseconds()) / 1000, SHA256: hex.EncodeToString(h[:]), OK: ok}
		if path == "/upload" && ok {
			row.Bytes = int64(len(body))
		}
		if !ok {
			row.Failure = "body_mismatch"
			if e != nil {
				row.Failure = "body_" + hybridHTTPFailure(e)
			} else if response.StatusCode != 200 {
				row.Failure = "response_status"
			}
			finish(row, fmt.Errorf("hybrid HTTP %s", row.Failure))
			return
		}
		finish(row, nil)
	}
	request("warm_text", "/text", nil, 0)
	for _, direction := range []string{"download", "upload"} {
		var bulk sync.WaitGroup
		bulk.Add(1)
		go func(direction string) {
			defer bulk.Done()
			if direction == "download" {
				request("bulk_download", "/bulk", nil, 0)
			} else {
				request("bulk_upload", "/upload", payload, 0)
			}
		}(direction)
		// Warm persistent text shares the normal application HTTP pool while a
		// distinct connection carries the active bulk request.
		for i := 1; i <= 10; i++ {
			request(direction+"_text", "/text", nil, i)
			select {
			case <-ctx.Done():
				bulk.Wait()
				return rows, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
		bulk.Wait()
	}
	if clientHybrid != nil {
		if e = clientHybrid.error(); e != nil {
			failures = append(failures, e)
		}
		if e = exitHybrid.error(); e != nil {
			failures = append(failures, e)
		}
	}
	if len(failures) > 0 {
		return rows, errors.Join(failures...)
	}
	return rows, nil
}

func TestLivePinnedPubSubHybrid(t *testing.T) {
	if os.Getenv("SKIRK_PUBSUB_HYBRID_LIVE") != "1" {
		t.Skip("guarded isolated Pub/Sub/Drive SOCKS hybrid")
	}
	if os.Getenv("SKIRK_PUBSUB_HYBRID_ACCOUNT") != "isolated-secondary" {
		t.Fatal("explicit isolated secondary-account token assertion required")
	}
	mode := os.Getenv("SKIRK_PUBSUB_HYBRID_MODE")
	if mode != "pools" && mode != "hybrid" && mode != "drive" {
		t.Fatal("explicit pools/hybrid/drive mode required")
	}
	out := os.Getenv("SKIRK_PUBSUB_HYBRID_OUT")
	tokenPath := os.Getenv("SKIRK_PUBSUB_HYBRID_TOKEN_FILE")
	configPath := os.Getenv("SKIRK_PUBSUB_HYBRID_ROUTE_CONFIG")
	if !strings.Contains(filepath.Clean(out), ".skirk-runs/") || !strings.Contains(filepath.Clean(tokenPath), ".skirk-runs/") || !strings.Contains(filepath.Clean(configPath), ".skirk-runs/personal-test/quota-kit/") {
		t.Fatal("isolated private paths required")
	}
	if _, e := os.Lstat(out); !os.IsNotExist(e) {
		t.Fatal("fresh private output required")
	}
	info, e := os.Stat(tokenPath)
	if e != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private secondary token required")
	}
	token, e := os.ReadFile(tokenPath)
	if e != nil {
		t.Fatal("token read failed")
	}
	defer os.Remove(tokenPath)
	cfg, e := LoadConfig(configPath)
	if e != nil {
		t.Fatal("isolated config load failed")
	}
	if cfg.Route.Mode != "google_front_pinned" || cfg.Route.GoogleIP == "" || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" {
		t.Fatal("pinned Google front and isolated explicit folder required")
	}
	newSID, sidErr := NewSessionID()
	if sidErr != nil {
		t.Fatal(sidErr)
	}
	cfg.SessionID = hex.EncodeToString(newSID[:])
	cfg.Secret, _ = RandomSecret()
	nonce := make([]byte, 12)
	if _, e = rand.Read(nonce); e != nil {
		t.Fatal(e)
	}
	label := "skirk-hybrid-" + hex.EncodeToString(nonce)
	cfg.Client = ClientConfig{ID: "research", RunID: label}
	cfg.Tunnel.PollIntervalMS = 250
	cfg.Tunnel.CleanupProcessed = true
	cfg.Tunnel.SparsePriority = true
	cfg.Tunnel.InteractionEpoch = true
	cfg.Drive.MetadataOnlyPriority = false
	cfg.Drive.MetadataSlotV1 = false
	cfg.Drive.MetadataSlotDeliveryV1 = false
	cfg.Drive.MetadataSlotAsyncV1 = false
	cfg.Drive.CombinedMuxDiscovery = false
	cfg.Drive.AdaptiveMuxDiscovery = false
	cfg.Drive.InlineSmallObjects = true
	cfg.Drive.NormalListPageSize = 100
	cfg.Drive.SeparateUploadPool = true
	cfg.Drive.SeparateBulkMediaPool = true
	cfg.Drive.UploadHedgeDelayMS = 0
	sid, e := ParseSessionID(cfg.SessionID)
	if e != nil {
		t.Fatal(e)
	}
	prefix := "muxv4/" + cfg.SessionID + "/"
	ledger := out + ".recovery-resources"
	resources := []string{prefix, "projects/skirk-p/topics/" + label + "-up", "projects/skirk-p/topics/" + label + "-down", "projects/skirk-p/subscriptions/" + label + "-up", "projects/skirk-p/subscriptions/" + label + "-down"}
	if e = os.MkdirAll(filepath.Dir(out), 0700); e != nil {
		t.Fatal(e)
	}
	file, e := os.OpenFile(ledger, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = io.WriteString(file, strings.Join(resources, "\n")+"\n")
	if e == nil {
		e = file.Sync()
	}
	file.Close()
	if e != nil {
		t.Fatal(e)
	}
	route := cfg.Route
	route.TimeoutSeconds = 30
	logFile, logErr := os.OpenFile(out+".mux.log", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if logErr != nil {
		t.Fatal("fresh private mux log required")
	}
	defer logFile.Close()
	boundedLog := &hybridBoundedLog{writer: logFile, limit: 8 << 20}
	privateLogger := log.New(boundedLog, "", log.Ldate|log.Ltime|log.Lmicroseconds)
	cfg.Tunnel.Observe = true
	var abortActive func(string)
	budget := &hybridBudget{MaxCalls: 2000, MaxBytes: 128 << 20}
	newAPI := func(cleanup bool) *hybridREST {
		a := &hybridREST{client: NewGoogleHTTPClient(route), token: strings.TrimSpace(string(token)), budget: budget, cleanup: cleanup}
		if !cleanup {
			a.onGuard = func() {
				if abortActive != nil {
					abortActive("pubsub_guard")
				}
			}
		}
		return a
	}
	var apis []*hybridREST
	makeAPI := func(cleanup bool) hybridCall { a := newAPI(cleanup); apis = append(apis, a); return a.call }
	control := makeAPI(false)
	cloud := &hybridCloudResources{control: control}
	// Physical Drive requests are atomically capped at 2,000; active unit
	// reservations stop at 40,000, keeping 10,000 units for exact cleanup.
	driveBudget := &hybridBudget{MaxCalls: 2000, MaxBytes: 50000}
	var stores []*DriveStore
	var gates []*hybridDriveGate
	newDrive := func(cleanup bool) *DriveStore {
		store := NewDriveStoreWithTokenSource(NewGoogleHTTPClient(cfg.Route), NewAccessTokenSource(cfg.Auth, cfg.Route), cfg.Drive)
		store.Logger = privateLogger
		stores = append(stores, store)
		newGates := hybridGateDrive(store, driveBudget, cleanup)
		if !cleanup {
			for _, gate := range newGates {
				gate.onGuard = func() {
					if abortActive != nil {
						abortActive("drive_guard")
					}
				}
			}
		}
		gates = append(gates, newGates...)
		return store
	}
	clientDrive, exitDrive := newDrive(false), newDrive(false)
	clientHybrid, _ := newHybridStore(clientDrive, cfg.Secret, sid, DirectionUp, cfg.Client.ID, cfg.Client.RunID)
	exitHybrid, _ := newHybridStore(exitDrive, cfg.Secret, sid, DirectionDown, cfg.Client.ID, cfg.Client.RunID)
	clientHybrid.publish = makeAPI(false)
	clientHybrid.pull = makeAPI(false)
	clientHybrid.ack = makeAPI(false)
	exitHybrid.publish = makeAPI(false)
	exitHybrid.pull = makeAPI(false)
	exitHybrid.ack = makeAPI(false)
	var rows []hybridLiveRow
	var runErr error
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	journalFile, journalErr := os.OpenFile(out+".progress.jsonl", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if journalErr != nil {
		cancel()
		t.Fatal("fresh private progress journal required")
	}
	defer journalFile.Close()
	progress := &hybridProgress{file: journalFile, cancel: cancel}
	began := time.Now()
	abortActive = func(kind string) {
		progress.write(map[string]any{"phase": "guard_abort", "kind": kind, "elapsed_ms": time.Since(began).Milliseconds()})
		cancel()
	}
	progress.write(map[string]any{"phase": "setup", "mode": mode, "elapsed_ms": 0})
	clientHybrid.event = func(kind string) {
		progress.write(map[string]any{"phase": "operation", "receiver": "client", "kind": kind, "elapsed_ms": time.Since(began).Milliseconds()})
	}
	exitHybrid.event = func(kind string) {
		progress.write(map[string]any{"phase": "operation", "receiver": "exit", "kind": kind, "elapsed_ms": time.Since(began).Milliseconds()})
	}
	record := func(row hybridLiveRow) {
		progress.write(map[string]any{"phase": "sample", "elapsed_ms": time.Since(began).Milliseconds(), "row": row})
		t.Logf("hybrid sample kind=%s pool=%d sequence=%d bytes=%d total_ms=%.3f ok=%t", row.Kind, row.Pool, row.Sequence, row.Bytes, row.TotalMS, row.OK)
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				progress.write(map[string]any{"phase": "heartbeat", "elapsed_ms": time.Since(began).Milliseconds(), "pubsub": budget.snapshot(), "client_published": clientHybrid.published.Load(), "exit_published": exitHybrid.published.Load(), "client_delivered": clientHybrid.delivered.Load(), "exit_delivered": exitHybrid.delivered.Load(), "empty_pulls": clientHybrid.emptyPulls.Load() + exitHybrid.emptyPulls.Load(), "drive_guard": driveBudget.snapshot(), "client_drive_active": clientDrive.QuotaSnapshot(), "exit_drive_active": exitDrive.QuotaSnapshot()})
			}
		}
	}()
	defer func() { stopHeartbeat(); <-heartbeatDone }()

	event := func(value map[string]any) {
		value["elapsed_ms"] = time.Since(began).Milliseconds()
		progress.write(value)
	}
	if mode != "drive" {
		runErr = cloud.create(ctx, label)
		clientHybrid.topic = cloud.topics[0]
		clientHybrid.subscription = cloud.subs[1]
		exitHybrid.topic = cloud.topics[1]
		exitHybrid.subscription = cloud.subs[0]
	}
	if runErr == nil {
		if mode == "pools" {
			rows, runErr = hybridPoolMeasurements(ctx, clientHybrid, exitHybrid, record)
		} else if mode == "hybrid" {
			rows, runErr = hybridSOCKSMeasurements(ctx, cfg, clientHybrid, exitHybrid, clientHybrid, exitHybrid, 16<<20, record, privateLogger, event)
		} else {
			rows, runErr = hybridSOCKSMeasurements(ctx, cfg, clientDrive, exitDrive, nil, nil, 16<<20, record, privateLogger, event)
		}
	}
	progress.write(map[string]any{"phase": "active_complete", "elapsed_ms": time.Since(began).Milliseconds(), "rows": len(rows)})
	cancel()
	clientHybrid.stop()
	exitHybrid.stop()
	for _, gate := range gates {
		gate.stop()
	}
	activeClientQuota, activeExitQuota := clientDrive.QuotaSnapshot(), exitDrive.QuotaSnapshot()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cleanupCancel()
	cleanupDrive := newDrive(true)
	cleanupOK := true
	var deletedIDs []string
	// Resolve exact own names locally. Drive contains/name queries can overmatch.
	scan := func() ([]ObjectInfo, error) {
		r, e := cleanupDrive.ListFreshContainsPageStatus(cleanupCtx, []string{prefix}, time.Time{}, "", 16)
		if e != nil || r.Truncated || r.Incomplete {
			return nil, errors.New("hybrid cleanup prefix scan incomplete")
		}
		var own []ObjectInfo
		for _, info := range r.Objects {
			if strings.HasPrefix(info.Name, prefix) {
				own = append(own, info)
			}
		}
		return own, nil
	}
	if mode != "pools" {
		objects, e := scan()
		if e != nil {
			cleanupOK = false
		} else {
			for _, info := range objects {
				if e = cleanupDrive.DeleteID(cleanupCtx, info.ID); e != nil {
					cleanupOK = false
				}
				deletedIDs = append(deletedIDs, info.ID)
			}
		}
		objects, e = scan()
		if e != nil || len(objects) != 0 {
			cleanupOK = false
		}
	}
	cloud.control = makeAPI(true)
	cloudCleanup := true
	if mode != "drive" {
		cloudCleanup = cloud.cleanup(cleanupCtx, 31*time.Second)
	} else {
		select {
		case <-cleanupCtx.Done():
			cleanupOK = false
		case <-time.After(31 * time.Second):
		}
	}
	if mode != "pools" {
		objects, e := scan()
		if e != nil || len(objects) != 0 {
			cleanupOK = false
		}
		for _, id := range deletedIDs {
			_, _, e = cleanupDrive.GetMuxObjectMetadataByID(cleanupCtx, id)
			if !isDriveNotFound(e) {
				cleanupOK = false
			}
		}
	}
	for _, a := range apis {
		a.client.client.CloseIdleConnections()
	}
	for _, s := range stores {
		for _, c := range []*GoogleHTTPClient{s.http, s.normalUploadHTTP, s.normalDownloadHTTP} {
			if c != nil {
				c.client.CloseIdleConnections()
			}
		}
	}
	report := map[string]any{"mode": mode, "route": cfg.Route.Mode, "sni": "www.google.com", "project": "skirk-p", "rows": rows, "pubsub": budget.snapshot(), "client_drive_active": activeClientQuota, "exit_drive_active": activeExitQuota, "drive_cleanup": cleanupDrive.QuotaSnapshot(), "drive_guard_reserved_units": driveBudget.snapshot()["request_body_bytes"], "drive_transport_guard": driveBudget.snapshot(), "private_mux_log": boundedLog.stats(), "cleanup_verified": cleanupOK && cloudCleanup, "client_pubsub_published": clientHybrid.published.Load(), "exit_pubsub_published": exitHybrid.published.Load(), "client_pubsub_delivered": clientHybrid.delivered.Load(), "exit_pubsub_delivered": exitHybrid.delivered.Load(), "pubsub_duplicates": clientHybrid.duplicates.Load() + exitHybrid.duplicates.Load(), "pubsub_ack_ids": clientHybrid.acked.Load() + exitHybrid.acked.Load(), "pubsub_empty_pulls": clientHybrid.emptyPulls.Load() + exitHybrid.emptyPulls.Load(), "experimental": true, "no_product_scope_export": true}
	stopHeartbeat()
	<-heartbeatDone
	progress.write(map[string]any{"phase": "cleanup_complete", "elapsed_ms": time.Since(began).Milliseconds(), "verified": cleanupOK && cloudCleanup})
	if journalErr := progress.error(); journalErr != nil {
		runErr = errors.Join(runErr, errors.New("private progress journal write failed"))
	}
	ok := runErr == nil && cleanupOK && cloudCleanup
	if runErr != nil {
		report["failure"] = runErr.Error()
	}
	report["ok"] = ok
	if e := logFile.Sync(); e != nil {
		report["log_sync_failed"] = true
		report["ok"] = false
		ok = false
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	if e = os.WriteFile(out, append(raw, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	if cleanupOK && cloudCleanup {
		if e = os.Remove(ledger); e != nil {
			t.Fatal(e)
		}
	}
	if !ok {
		t.Fatalf("hybrid experiment failed; inspect private report (cleanup verified=%t)", cleanupOK && cloudCleanup)
	}
}
