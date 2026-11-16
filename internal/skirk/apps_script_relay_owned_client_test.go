package skirk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type relayResearchOwnedRuntime struct {
	BrokerLocalURL          string `json:"broker_local_url"`
	OriginURL               string `json:"origin_url"`
	OriginTarget            string `json:"origin_target"`
	OriginCertificateBase64 string `json:"origin_certificate_base64"`
	DownloadSHA256          string `json:"download_sha256"`
	BulkBytes               int    `json:"bulk_bytes"`
	PID                     int    `json:"pid"`
	StartedUTC              string `json:"started_utc"`
}
type relayResearchCampaignReservation struct {
	CampaignID             string `json:"campaign_id"`
	VariantID              string `json:"variant_id"`
	AllocationIndex        int    `json:"allocation_index"`
	ReservedScriptPosts    int64  `json:"reserved_script_posts"`
	CampaignMaxScriptPosts int64  `json:"campaign_max_script_posts"`
	ReservedAtUTC          string `json:"reserved_at_utc"`
}

func rrReadPrivateJSON(dir, name string, result any) error {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return errRelayResearchBounds
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(result) != nil || d.Decode(new(any)) != io.EOF {
		return errRelayResearchBounds
	}
	return nil
}

func rrValidateClientReservation(dir string, s relayResearchPrivateSettings) (relayResearchCampaignReservation, error) {
	var r relayResearchCampaignReservation
	if err := rrReadPrivateJSON(dir, "campaign-reservation.json", &r); err != nil {
		return r, err
	}
	at, err := time.Parse(time.RFC3339, r.ReservedAtUTC)
	if err != nil || at.After(time.Now().Add(time.Minute)) || !isSafeObjectSegment(r.CampaignID) || !isSafeObjectSegment(r.VariantID) || r.AllocationIndex < 1 || r.AllocationIndex > 4 || r.ReservedScriptPosts != s.MaxScriptPosts || r.ReservedScriptPosts < 1 || r.ReservedScriptPosts > 256 || r.CampaignMaxScriptPosts != 1024 {
		return r, errRelayResearchBounds
	}
	return r, nil
}

type relayResearchRows struct {
	mu   sync.Mutex
	file *os.File
	err  error
}

func (r *relayResearchRows) put(row map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.err = json.NewEncoder(r.file).Encode(row)
	if r.err == nil {
		r.err = r.file.Sync()
	}
	return r.err
}

// The only live client entry point. It neither reads Drive credentials nor
// connects directly to the owned origin/relay. All origin traffic uses SOCKS,
// then the selected Google front and the approved Apps Script deployment.
func TestAppsScriptRelayOwnedClient(t *testing.T) {
	if os.Getenv("SKIRK_LIVE_APPS_SCRIPT_RELAY_CLIENT") != "1" {
		t.Skip("opt-in live client")
	}
	dir := os.Getenv("SKIRK_APPS_SCRIPT_RELAY_PRIVATE_DIR")
	s, key, epoch, err := rrReadPrivateSettings(dir)
	if err != nil {
		t.Fatal("invalid private relay settings")
	}
	reservation, err := rrValidateClientReservation(dir, s)
	if err != nil {
		t.Fatal("missing or invalid pre-reserved campaign allocation")
	}
	var runtime relayResearchOwnedRuntime
	if rrReadPrivateJSON(dir, "runtime.json", &runtime) != nil || runtime.BulkBytes != s.BulkBytes {
		t.Fatal("invalid owned exit runtime")
	}
	if err = rrWritePrivateJSON(dir, "client-started.json", map[string]any{"campaign_id": reservation.CampaignID, "variant_id": reservation.VariantID, "allocation_index": reservation.AllocationIndex, "pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal("campaign allocation already consumed")
	}
	f, err := os.OpenFile(filepath.Join(dir, "client-samples.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot create private sample ledger")
	}
	defer f.Close()
	rows := &relayResearchRows{file: f}
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, min(time.Duration(s.DurationSeconds)*time.Second, 8*time.Minute))
	defer cancel()
	codec, err := newRelayResearchCodec(key, epoch, s.FrameBytes)
	if err != nil {
		t.Fatal("invalid codec")
	}
	budget := &relayResearchCarrierBudget{MaxPosts: s.MaxScriptPosts, MaxAttempts: s.MaxHTTPAttemptsUpper, MaxBodyBytes: s.MaxHTTPEncodedUpper}
	google := NewGoogleHTTPClient(s.Route)
	defer google.client.CloseIdleConnections()
	exchange, err := newRelayResearchGoogleExchange(google, s.ScriptURL, s.ScriptAuth, codec, budget)
	if err != nil {
		t.Fatal("invalid front-only Apps Script deployment")
	}
	started := time.Now()
	snapshot, runErr := rrRunOwnedClient(ctx, dir, s, key, epoch, runtime, exchange, rows)
	status := "passed"
	errorSummaryText := ""
	if runErr != nil {
		status = "failed"
		errorSummaryText = errorSummary(runErr)
	}
	result := map[string]any{"status": status, "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000, "pack_priority": s.PackPriority, "frame_bytes": s.FrameBytes, "chunk_bytes": s.ChunkBytes, "bulk_bytes": s.BulkBytes, "campaign_id": reservation.CampaignID, "variant_id": reservation.VariantID, "allocation_index": reservation.AllocationIndex, "client": snapshot, "carrier": budget.snapshot(), "route_mode": s.Route.Mode, "client_error": errorSummaryText, "sample_ledger_error": rows.err != nil, "measurement_scope": "owned TLS origin via actual SOCKS/mux and only selected Google front; no direct-origin fallback"}
	if err = rrWritePrivateJSON(dir, "client-result.json", result); err != nil {
		t.Fatal("cannot persist private client result")
	}
	if runErr != nil {
		t.Fatal("live relay workload failed; private sample ledger retains completed and failed rows")
	}
}

func rrRunOwnedClient(parent context.Context, dir string, s relayResearchPrivateSettings, key []byte, epoch [16]byte, runtime relayResearchOwnedRuntime, exchange relayResearchExchange, rows *relayResearchRows) (map[string]int64, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	parsed, err := url.Parse(runtime.OriginURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != runtime.OriginTarget || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errRelayResearchBounds
	}
	host, _, err := net.SplitHostPort(runtime.OriginTarget)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, errRelayResearchBounds
	}
	der, err := base64.StdEncoding.Strict().DecodeString(runtime.OriginCertificateBase64)
	if err != nil || len(der) > 8<<10 {
		return nil, errRelayResearchBounds
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	tlsConfig := &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}
	payload, err := rrCampaignPayload(s)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(payload)
	if runtime.DownloadSHA256 != hex.EncodeToString(hash[:]) {
		return nil, errRelayResearchConflict
	}
	rc, err := s.relayConfig()
	if err != nil {
		return nil, err
	}
	client, err := newRelayResearchClient(ctx, key, epoch, rc, exchange)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	// A terminal carrier failure must abort TLS/HTTP waits immediately instead
	// of leaving application reads alive until their independent deadlines.
	stopClientFailure := context.AfterFunc(client.ctx, cancel)
	defer stopClientFailure()
	cfg := &Config{Secret: s.MuxSecretHex, SessionID: s.SessionID, Client: ClientConfig{ID: s.ClientID, RunID: s.RunID}, Tunnel: TunnelConfig{Transport: "muxv4", Profile: "fixed", PollIntervalMS: 10, ChunkSize: s.ChunkBytes, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
	cfg.ApplyDefaults()
	tunnel, err := NewTunnel(client, cfg)
	if err != nil {
		return nil, err
	}
	tunnel.Logger = log.New(io.Discard, "", 0)
	tunnel.setRole("client")
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		return nil, err
	}
	validate, notify := relayResearchMuxHooks(mux, ctx)
	client.setDownHooks(validate, notify)
	mux.start(ctx)
	defer mux.closeAll()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	listener.Close()
	socks := SOCKSServer{Listen: address, Logger: tunnel.Logger, Handler: func(c context.Context, target string, conn net.Conn) {
		if target != runtime.OriginTarget {
			conn.Close()
			return
		}
		_ = mux.openClientStream(c, target, conn)
	}}
	serverDone := make(chan error, 1)
	go func() { serverDone <- socks.Serve(ctx) }()
	defer func() { cancel(); mux.closeAll(); client.Close(); <-serverDone }()
	for i := 0; i < 100; i++ {
		c, e := net.DialTimeout("tcp", address, 10*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		if i == 99 {
			return client.snapshot(), errors.New("local SOCKS startup failed")
		}
		if e = relayClientDelay(ctx, time.Millisecond); e != nil {
			return client.snapshot(), e
		}
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), MaxConnsPerHost: 8, DialContext: func(c context.Context, _, target string) (net.Conn, error) {
		if target != runtime.OriginTarget {
			return nil, errRelayResearchConflict
		}
		return dialViaSOCKS5(c, "socks5h://"+address, target)
	}}
	httpClient := &http.Client{Transport: transport, Timeout: 180 * time.Second}
	defer transport.CloseIdleConnections()
	coldAt := time.Now()
	raw, err := dialViaSOCKS5(ctx, "socks5h://"+address, runtime.OriginTarget)
	if err != nil {
		return client.snapshot(), err
	}
	echo := tls.Client(raw, tlsConfig.Clone())
	defer echo.Close()
	stopEcho := rrCloseConnOnCancel(ctx, echo)
	defer stopEcho()
	_ = echo.SetDeadline(time.Now().Add(180 * time.Second))
	handshakeErr := echo.HandshakeContext(ctx)
	if err = rows.put(map[string]any{"kind": "tls_handshake", "phase": "cold", "elapsed_ms": rrMillis(time.Since(coldAt)), "verified": handshakeErr == nil, "timing_scope": "local SOCKS connect and end-to-end TLS handshake"}); err != nil || handshakeErr != nil {
		return client.snapshot(), errors.Join(handshakeErr, err, client.error())
	}
	reader := bufio.NewReader(echo)
	warm := rrOwnedMessage("warm", 0)
	at := time.Now()
	if err = rrWriteOwnedEcho(ctx, echo, runtime.OriginURL, warm); err != nil {
		return client.snapshot(), err
	}
	first, err := rrReadOwnedEcho(reader, warm)
	warmErr := err
	if err = rows.put(map[string]any{"kind": "text", "phase": "warm", "index": 0, "request_ms": rrMillis(time.Since(at)), "first_body_ms": rrMillis(first.Sub(at)), "success": warmErr == nil}); err != nil || warmErr != nil {
		return client.snapshot(), errors.Join(err, warmErr)
	}
	for _, phase := range []string{"download", "upload"} {
		if err = rrOwnedMixedPhase(ctx, dir, phase, runtime.OriginURL, payload, hash, httpClient, echo, reader, rows); err != nil {
			return client.snapshot(), err
		}
	}
	echo.Close()
	transport.CloseIdleConnections()
	cleanupUntil := time.Now().Add(40 * time.Second)
	quiet := time.Time{}
	for time.Now().Before(cleanupUntil) {
		snapshot := client.snapshot()
		if snapshot["up_bytes"] == 0 && snapshot["down_bytes"] == 0 {
			if quiet.IsZero() {
				quiet = time.Now()
			}
			if time.Since(quiet) >= 2*time.Second {
				return snapshot, client.error()
			}
		} else {
			quiet = time.Time{}
		}
		mux.requestCleanupKick()
		if err = relayClientDelay(ctx, 50*time.Millisecond); err != nil {
			return snapshot, err
		}
	}
	return client.snapshot(), errors.New("client cleanup did not reach stable empty payload caches")
}

func rrMillis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func rrCloseConnOnCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = conn.Close() })
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}
func rrOwnedMessage(phase string, index int) []byte {
	v := bytes.Repeat([]byte{byte(len(phase) + index + 1)}, 64)
	binary.BigEndian.PutUint64(v, uint64(index+1))
	return v
}
func rrWriteOwnedEcho(ctx context.Context, conn net.Conn, origin string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/echo", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	return req.Write(conn)
}
func rrReadOwnedEcho(reader *bufio.Reader, want []byte) (time.Time, error) {
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		return time.Time{}, err
	}
	defer response.Body.Close()
	body := make([]byte, 64)
	_, err = io.ReadFull(response.Body, body[:1])
	first := time.Now()
	if err == nil {
		_, err = io.ReadFull(response.Body, body[1:])
	}
	extra := make([]byte, 1)
	n, e := response.Body.Read(extra)
	if err != nil || response.StatusCode != 200 || !bytes.Equal(body, want) || n != 0 || e != io.EOF {
		return first, errors.New("persistent owned echo changed/reordered bytes")
	}
	return first, nil
}

func rrWaitOwnedBulkStart(ctx context.Context, dir, phase string, finished *atomic.Bool) error {
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "origin-events.jsonl"))
		if err == nil && len(raw) <= 64<<10 {
			for _, line := range bytes.Split(raw, []byte{'\n'}) {
				var v struct{ Event, Phase string }
				if json.Unmarshal(line, &v) == nil && v.Event == "bulk_started" && v.Phase == phase {
					return nil
				}
			}
		}
		if finished.Load() {
			return errors.New("bulk ended before mixed measurement began")
		}
		if err = relayClientDelay(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

func rrOwnedMixedPhase(parent context.Context, dir, phase, origin string, payload []byte, hash [32]byte, httpClient *http.Client, echo net.Conn, reader *bufio.Reader, rows *relayResearchRows) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	_ = echo.SetDeadline(time.Now().Add(180 * time.Second))
	stopPhase := rrCloseConnOnCancel(ctx, echo)
	defer stopPhase()
	bulkDone := make(chan error, 1)
	var finished atomic.Bool
	go func() {
		start := time.Now()
		method := http.MethodGet
		var body io.Reader
		if phase == "upload" {
			method = http.MethodPost
			body = bytes.NewReader(payload)
		}
		var wrote atomic.Int64
		trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(time.Since(start).Microseconds())
			}
		}}
		request, e := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, origin+"/bulk", body)
		if e == nil {
			request.Header.Set("X-Skirk-Research-Phase", phase)
			request.Header.Set("Accept-Encoding", "identity")
		}
		var n int64
		var first time.Duration
		if e == nil {
			var response *http.Response
			response, e = httpClient.Do(request)
			if e == nil {
				one := make([]byte, 1)
				_, e = io.ReadFull(response.Body, one)
				first = time.Since(start)
				if e == nil {
					if phase == "download" {
						h := sha256.New()
						_, _ = h.Write(one)
						var rest int64
						rest, e = io.Copy(h, io.LimitReader(response.Body, int64(len(payload))))
						n = rest + 1
						if n != int64(len(payload)) || !bytes.Equal(h.Sum(nil), hash[:]) {
							e = errRelayResearchConflict
						}
					} else {
						rest, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
						e = readErr
						var confirmation struct {
							Bytes  int64  `json:"bytes"`
							SHA256 string `json:"sha256"`
						}
						if json.Unmarshal(append(one, rest...), &confirmation) != nil || confirmation.Bytes != int64(len(payload)) || confirmation.SHA256 != hex.EncodeToString(hash[:]) {
							e = errRelayResearchConflict
						}
						n = confirmation.Bytes
					}
				}
				response.Body.Close()
				if response.StatusCode != 200 {
					e = errRelayResearchConflict
				}
			}
		}
		elapsed := time.Since(start)
		rowErr := rows.put(map[string]any{"kind": "bulk", "phase": phase, "bytes": n, "first_body_ms": rrMillis(first), "full_ms": rrMillis(elapsed), "mbps": float64(n) * 8 / elapsed.Seconds() / 1e6, "local_request_written_ms": float64(wrote.Load()) / 1000, "success": e == nil})
		finished.Store(true)
		bulkDone <- errors.Join(e, rowErr)
		if e != nil || rowErr != nil {
			cancel()
		}
	}()
	bulkReceived := false
	defer func() {
		cancel()
		if !bulkReceived {
			<-bulkDone
		}
	}()
	if err := rrWaitOwnedBulkStart(ctx, dir, phase, &finished); err != nil {
		return err
	}
	const count = 12
	sent := make([]time.Time, count)
	scheduled := make([]time.Time, count)
	var timesMu sync.Mutex
	writerDone := make(chan error, 1)
	start := time.Now()
	go func() {
		for i := 0; i < count; i++ {
			desired := start.Add(time.Duration(i) * 250 * time.Millisecond)
			if e := relayClientDelay(ctx, time.Until(desired)); e != nil {
				writerDone <- e
				cancel()
				return
			}
			timesMu.Lock()
			scheduled[i] = desired
			sent[i] = time.Now()
			timesMu.Unlock()
			if e := rrWriteOwnedEcho(ctx, echo, origin, rrOwnedMessage(phase, i)); e != nil {
				writerDone <- e
				cancel()
				return
			}
		}
		writerDone <- nil
	}()
	writerReceived := false
	defer func() {
		cancel()
		if !writerReceived {
			echo.Close()
			<-writerDone
		}
	}()
	overlapped := 0
	for i := 0; i < count; i++ {
		first, e := rrReadOwnedEcho(reader, rrOwnedMessage(phase, i))
		done := time.Now()
		timesMu.Lock()
		at, offer := sent[i], scheduled[i]
		timesMu.Unlock()
		during := !finished.Load()
		if during {
			overlapped++
		}
		rowErr := rows.put(map[string]any{"kind": "text", "phase": phase, "index": i, "bytes": 64, "request_ms": rrMillis(done.Sub(at)), "scheduled_ms": rrMillis(done.Sub(offer)), "first_body_ms": rrMillis(first.Sub(at)), "completed_during_bulk": during, "success": e == nil && !at.IsZero()})
		if e != nil || rowErr != nil || at.IsZero() {
			return errors.New("mixed persistent echo failed")
		}
	}
	writerErr := <-writerDone
	writerReceived = true
	bulkErr := <-bulkDone
	bulkReceived = true
	if overlapped == 0 {
		return errors.New("no text completed while bulk remained active")
	}
	if err := errors.Join(writerErr, bulkErr, ctx.Err()); err != nil {
		return err
	}
	// Stop the failure callback before normal phase cancellation so the same
	// verified persistent TLS stream remains available for the next phase.
	stopPhase()
	return nil
}
