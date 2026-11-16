package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hybridTestPair(t *testing.T) (*hybridStore, *hybridStore, string, []byte) {
	t.Helper()
	sid, _ := ParseSessionID("00112233445566778899aabbccddeeff")
	client, e := newHybridStore(NewMemoryStore(), "hybrid-test", sid, DirectionUp, "client-a", "run-a")
	if e != nil {
		t.Fatal(e)
	}
	exit, e := newHybridStore(client.base, "hybrid-test", sid, DirectionDown, "client-a", "run-a")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := encodeMuxBatch([]muxFrame{{Kind: muxFrameData, StreamID: 7, Seq: 1, Payload: bytes.Repeat([]byte{33}, 12000)}})
	if e != nil {
		t.Fatal(e)
	}
	key, _ := DeriveMuxLaneKeyV4("hybrid-test", sid, DirectionUp, "client-a", "run-a", 0)
	sealed, e := Seal(key, sid, DirectionUp, 1, raw, false)
	if e != nil {
		t.Fatal(e)
	}
	name := muxObjectNameWithStreamIDs(sid, DirectionUp, "client-a", "run-a", "epoch", 7, []uint64{7}, 0, 1, 1, 1, 1, len(raw), true)
	return client, exit, name, sealed
}
func hybridTestMessage(t *testing.T, s *hybridStore, name string, sealed []byte, ack string, at time.Time) hybridMessage {
	t.Helper()
	outer, e := s.seal(name, sealed)
	if e != nil {
		t.Fatal(e)
	}
	var m hybridMessage
	m.AckID = ack
	m.Message.Data = base64.StdEncoding.EncodeToString(outer)
	m.Message.PublishTime = at.UTC().Format(time.RFC3339Nano)
	return m
}
func TestHybridAuthenticationBoundsAndOriginalAge(t *testing.T) {
	client, exit, name, sealed := hybridTestPair(t)
	outer, e := client.seal(name, sealed)
	if e != nil {
		t.Fatal(e)
	}
	got, b, e := exit.open(outer)
	if e != nil || got != name || !bytes.Equal(b, sealed) {
		t.Fatal("valid outer/inner not recovered", e)
	}
	for _, mutate := range []func([]byte) []byte{func(b []byte) []byte { return b[:5] }, func(b []byte) []byte { b[len(b)-1] ^= 1; return b }, func(b []byte) []byte { return make([]byte, hybridSealedMax+8193) }} {
		if _, _, e = exit.open(mutate(bytes.Clone(outer))); e == nil {
			t.Fatal("invalid outer accepted")
		}
	}
	if _, _, e = client.open(outer); e == nil {
		t.Fatal("wrong direction accepted")
	}
	other, _ := newHybridStore(NewMemoryStore(), "hybrid-test", [16]byte{1}, DirectionDown, "client-a", "run-a")
	if _, _, e = other.open(outer); e == nil {
		t.Fatal("wrong session accepted")
	}
	if _, e = client.seal(name, make([]byte, hybridSealedMax+1)); e == nil {
		t.Fatal("oversized priority accepted")
	}
	bad := bytes.Clone(sealed)
	bad[len(bad)-1] ^= 1
	outer, e = client.seal(name, bad)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = exit.open(outer); e == nil {
		t.Fatal("inner tamper accepted")
	}
	var queued, acks atomic.Int64
	exit.receive = func(ObjectInfo) bool { queued.Add(1); return true }
	exit.ack = func(_ context.Context, _, _ string, _ any) (*HTTPResult, error) {
		acks.Add(1)
		return &HTTPResult{Status: 200}, nil
	}
	m := hybridTestMessage(t, client, name, sealed, "stale", exit.startedAt.Add(-time.Second))
	if e = exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if queued.Load() != 0 || acks.Load() != 1 {
		t.Fatal("stale object replayed or not discarded")
	}
	// Same payload republished later cannot turn a tombstoned stale OPEN into fresh data.
	m.AckID = "republished"
	m.Message.PublishTime = time.Now().UTC().Format(time.RFC3339Nano)
	if e = exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if queued.Load() != 0 || acks.Load() != 2 {
		t.Fatal("redelivery reset stale age")
	}
}
func TestHybridACKAfterDeleteAndRedeliveryMerge(t *testing.T) {
	client, exit, name, sealed := hybridTestPair(t)
	var mu sync.Mutex
	var acked []string
	fail := true
	exit.receive = func(ObjectInfo) bool { return true }
	exit.ack = func(_ context.Context, _, _ string, body any) (*HTTPResult, error) {
		ids := body.(map[string]any)["ackIds"].([]string)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("fictional ACK failure")
		}
		acked = append(acked, ids...)
		return &HTTPResult{Status: 200}, nil
	}
	m := hybridTestMessage(t, client, name, sealed, "first", time.Now())
	if e := exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	m.AckID = "second"
	if e := exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if len(acked) != 0 {
		t.Fatal("ACK sent before mux processing/deferred Delete")
	}
	id := hybridID(name, sealed)
	got, e := exit.GetByID(context.Background(), id)
	if e != nil || !bytes.Equal(got, sealed) || len(got) <= 8192 {
		t.Fatal("large priority cache failed", e)
	}
	if e = exit.DeleteID(context.Background(), id); e == nil {
		t.Fatal("ACK failure hidden")
	}
	fail = false
	if e = exit.DeleteID(context.Background(), id); e != nil {
		t.Fatal(e)
	}
	if len(acked) != 2 || exit.cacheBytes != 0 {
		t.Fatal("ACK IDs lost across retry")
	}
	m.AckID = "third"
	if e = exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if len(acked) != 3 || exit.delivered.Load() != 1 || exit.duplicates.Load() != 2 {
		t.Fatal("tombstone duplicate handling failed")
	}
}
func TestHybridParallelPullCancellationAndRetryAdmission(t *testing.T) {
	client, exit, name, sealed := hybridTestPair(t)
	message := hybridTestMessage(t, client, name, sealed, "ack", time.Now())
	raw, _ := json.Marshal(map[string]any{"receivedMessages": []hybridMessage{message}})
	var active, peak, served, attempts atomic.Int64
	ready := make(chan struct{}, 4)
	exit.pull = func(ctx context.Context, _, _ string, _ any) (*HTTPResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		ready <- struct{}{}
		if served.Add(1) == 1 {
			return &HTTPResult{Status: 200, Body: raw}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	exit.publish = func(context.Context, string, string, any) (*HTTPResult, error) {
		return &HTTPResult{Status: 200, Body: []byte(`{"messageIds":["id"]}`)}, nil
	}
	exit.ack = exit.publish
	exit.receive = func(ObjectInfo) bool { return attempts.Add(1) >= 2 }
	if e := exit.start(context.Background(), 2); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(time.Second):
			t.Fatal("pull pool did not start")
		}
	}
	deadline := time.Now().Add(time.Second)
	for exit.delivered.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	exit.stop()
	if exit.error() != nil || peak.Load() > 2 || active.Load() != 0 || exit.delivered.Load() != 1 {
		t.Fatal("pool/retry/cancellation invariant", exit.error(), peak.Load(), active.Load(), exit.delivered.Load())
	}
}
func TestHybridGlobalBudgetConcurrentAndBoundedHTTP(t *testing.T) {
	budget := &hybridBudget{MaxCalls: 42 * hybridH2AttemptUpper, MaxBytes: hybridCleanupByteReserve + 100}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if budget.reserve(1, 0, false) == nil {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 || budget.snapshot()["calls"] != 10 {
		t.Fatal("concurrent guard exceeded")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "pubsub.googleapis.com" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("wrong API request")
		}
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", hybridResponseMax+1)), hybridResponseMax+1))
	}))
	defer server.Close()
	transport := &hybridRewriteTransport{target: server.URL, base: http.DefaultTransport}
	api := &hybridREST{client: &GoogleHTTPClient{client: &http.Client{Transport: transport}}, token: "fictional", budget: &hybridBudget{MaxCalls: 100 * hybridH2AttemptUpper, MaxBytes: 8 << 20}}
	if _, e := api.call(context.Background(), "POST", "/v1/test", map[string]int{"maxMessages": 8}); e == nil {
		t.Fatal("oversized response accepted")
	}
}

type hybridRewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (t *hybridRewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return t.base.RoundTrip(clone)
}

func TestHybridClaimRetainsOriginalAgeAndExpiredBrokerRecord(t *testing.T) {
	client, exit, name, sealed := hybridTestPair(t)
	var opens, acks atomic.Int64
	exit.receive = func(ObjectInfo) bool { opens.Add(1); return true }
	exit.ack = func(context.Context, string, string, any) (*HTTPResult, error) {
		acks.Add(1)
		return &HTTPResult{Status: 200}, nil
	}
	at := time.Now()
	m := hybridTestMessage(t, client, name, sealed, "original", at)
	if e := exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	id := hybridID(name, sealed)
	if e := exit.DeleteID(context.Background(), id); e != nil {
		t.Fatal(e)
	}
	// Advance beyond mux's two-minute closed-stream lifetime while the
	// original broker timestamp remains inside its ten-minute retention.
	exit.now = func() time.Time { return at.Add(muxClosedStreamTTL + time.Second) }
	m.AckID = "after-mux-closed-ttl"
	// Even a republished copy carrying the current broker time cannot replace
	// the tombstone or repeat the already processed OPEN identity.
	m.Message.PublishTime = exit.now().UTC().Format(time.RFC3339Nano)
	if e := exit.ingest(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if opens.Load() != 1 || acks.Load() != 2 {
		t.Fatal("tombstone expired with mux closed lifetime")
	}
	otherName := strings.Replace(name, "/epoch/", "/older/", 1)
	old := hybridTestMessage(t, client, otherName, sealed, "beyond-retention", exit.now().Add(-601*time.Second-driveClockTolerance))
	exit.startedAt = time.Now().Add(-time.Hour)
	if e := exit.ingest(context.Background(), old); e != nil {
		t.Fatal(e)
	}
	if opens.Load() != 1 || acks.Load() != 3 {
		t.Fatal("expired broker record processed")
	}
}

type hybridNormalMemory struct {
	*MemoryStore
	normal atomic.Int64
}

func (s *hybridNormalMemory) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if strings.Contains(name, "/p0/") {
		return ObjectInfo{}, errors.New("priority leaked into bulk store")
	}
	s.normal.Add(1)
	return s.MemoryStore.PutObject(ctx, name, data)
}
func (s *hybridNormalMemory) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if strings.Contains(name, "/p0/") {
		return ObjectInfo{}, errors.New("priority ID leaked into bulk store")
	}
	s.normal.Add(1)
	return s.MemoryStore.PutObjectWithID(ctx, id, name, data)
}
func TestHybridFullSOCKSMixedBytesAndDuplicateCarrier(t *testing.T) {
	sid, _ := NewSessionID()
	cfg := &Config{Secret: "hybrid-full-local", SessionID: hex.EncodeToString(sid[:]), Client: ClientConfig{ID: "client", RunID: "run"}, Tunnel: TunnelConfig{Transport: "muxv4", PollIntervalMS: 10, ChunkSize: 256 << 10, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
	cfg.ApplyDefaults()
	base := &hybridNormalMemory{MemoryStore: NewMemoryStore()}
	client, _ := newHybridStore(base, cfg.Secret, sid, DirectionUp, cfg.Client.ID, cfg.Client.RunID)
	exit, _ := newHybridStore(base, cfg.Secret, sid, DirectionDown, cfg.Client.ID, cfg.Client.RunID)
	up, down := make(chan hybridMessage, 256), make(chan hybridMessage, 256)
	var sequence atomic.Int64
	var ackCount atomic.Int64
	bind := func(store *hybridStore, send, recv chan hybridMessage) {
		store.publish = func(ctx context.Context, _, _ string, body any) (*HTTPResult, error) {
			raw, _ := json.Marshal(body)
			var v struct {
				Messages []struct {
					Data string `json:"data"`
				} `json:"messages"`
			}
			if json.Unmarshal(raw, &v) != nil || len(v.Messages) != 1 {
				return nil, errors.New("invalid local publish")
			}
			n := sequence.Add(1)
			var m hybridMessage
			m.AckID = fmt.Sprintf("ack-%d-a", n)
			m.Message.Data = v.Messages[0].Data
			m.Message.ID = fmt.Sprint(n)
			m.Message.PublishTime = time.Now().UTC().Format(time.RFC3339Nano)
			select {
			case send <- m:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			m.AckID = fmt.Sprintf("ack-%d-b", n)
			select {
			case send <- m:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &HTTPResult{Status: 200, Body: []byte(`{"messageIds":["fictional"]}`)}, nil
		}
		store.pull = func(ctx context.Context, _, _ string, _ any) (*HTTPResult, error) {
			select {
			case m := <-recv:
				b, _ := json.Marshal(map[string]any{"receivedMessages": []hybridMessage{m}})
				return &HTTPResult{Status: 200, Body: b}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		store.ack = func(context.Context, string, string, any) (*HTTPResult, error) {
			ackCount.Add(1)
			return &HTTPResult{Status: 200, Body: []byte(`{}`)}, nil
		}
	}
	bind(client, up, down)
	bind(exit, down, up)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	rows, e := hybridSOCKSMeasurements(ctx, cfg, client, exit, client, exit, 4<<20, nil, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 23 {
		t.Fatalf("mixed scenario rows=%d want23", len(rows))
	}
	for _, row := range rows {
		if !row.OK {
			t.Fatalf("mixed row failed %+v", row)
		}
	}
	if base.normal.Load() == 0 || client.published.Load() == 0 || exit.published.Load() == 0 || client.duplicates.Load()+exit.duplicates.Load() == 0 {
		t.Fatal("full hybrid did not exercise both directions, bulk and redelivery")
	}
}

func TestHybridClockGuardRejectsBeforeSubscriptionCreate(t *testing.T) {
	for _, skew := range []time.Duration{-2 * time.Minute, 2 * time.Minute} {
		calls := 0
		cloud := hybridCloudResources{control: func(_ context.Context, method, path string, _ any) (*HTTPResult, error) {
			calls++
			now := time.Now()
			return &HTTPResult{Status: 200, Header: http.Header{"Date": []string{now.Add(skew).UTC().Format(http.TimeFormat)}}, RequestStarted: now, ResponseReceived: now.Add(time.Millisecond)}, nil
		}}
		e := cloud.create(context.Background(), "skirk-local-clock")
		if !errors.Is(e, ErrDriveClockSkew) || calls != 1 {
			t.Fatal("subscriber started before clock validation", e, calls)
		}
	}
}
func TestHybridPhysicalDriveGuardAndStopJoin(t *testing.T) {
	began, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	g := &hybridDriveGate{budget: &hybridBudget{MaxCalls: 100 * hybridH2AttemptUpper, MaxBytes: hybridDriveCleanupUnitsReserve + 1600}, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(began)
		<-release
		return stringResponse(200, "fictional"), nil
	})}
	request, _ := http.NewRequest("GET", "https://www.google.com/drive/v3/files?alt=media", nil)
	done := make(chan struct{})
	go func() {
		r, _ := g.RoundTrip(request)
		if r != nil {
			r.Body.Close()
		}
		close(done)
	}()
	<-began
	stopped := make(chan struct{})
	go func() { g.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned before in-flight HTTP finished")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	<-done
	<-stopped
	if _, e := g.RoundTrip(request); !errors.Is(e, context.Canceled) {
		t.Fatal("late request escaped stopped gate")
	}
	if calls.Load() != 1 {
		t.Fatal("late HTTP reached Google transport")
	}
	// Exactly one 200-unit media reservation fits before the 10k cleanup reserve.
	guard := &hybridBudget{MaxCalls: 100 * hybridH2AttemptUpper, MaxBytes: hybridDriveCleanupUnitsReserve + 1600}
	if guard.reserveDrive(200, false) != nil || guard.reserveDrive(5, false) == nil || guard.reserveDrive(50, true) != nil {
		t.Fatal("Drive cleanup reservation not protected")
	}
}

func TestHybridDriveStopWaitsForResponseBody(t *testing.T) {
	g := &hybridDriveGate{budget: &hybridBudget{MaxCalls: 100 * hybridH2AttemptUpper, MaxBytes: hybridDriveCleanupUnitsReserve + 1600}, base: roundTripFunc(func(r *http.Request) (*http.Response, error) { return stringResponse(200, "fictional"), nil })}
	req, _ := http.NewRequest("GET", "https://www.google.com/drive/v3/files/own?alt=media", nil)
	response, e := g.RoundTrip(req)
	if e != nil {
		t.Fatal(e)
	}
	stopped := make(chan struct{})
	go func() { g.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("gate stopped while response body still live")
	case <-time.After(10 * time.Millisecond):
	}
	response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("body closure did not release gate")
	}
}

func TestHybridCleanupReservesSurviveActiveExhaustion(t *testing.T) {
	budget := &hybridBudget{MaxCalls: 40 * hybridH2AttemptUpper, MaxBytes: hybridCleanupByteReserve + 200}
	if e := budget.reserve(0, 200, false); e != nil {
		t.Fatal(e)
	}
	if budget.reserve(1, 0, false) == nil {
		t.Fatal("active response-byte reservation exceeded")
	}
	budget.finishResponse(200, 200)
	if budget.reserve(1, 0, false) == nil {
		t.Fatal("active bytes consumed cleanup reserve")
	}
	for i := 0; i < 12; i++ {
		if e := budget.reserve(0, hybridCleanupResponseMax+1, true); e != nil {
			t.Fatal("exact delete/get cleanup blocked", e)
		}
		budget.finishResponse(hybridCleanupResponseMax+1, 128)
	}
	calls := &hybridBudget{MaxCalls: 34 * hybridH2AttemptUpper, MaxBytes: 8 << 20}
	for i := 0; i < 2; i++ {
		if e := calls.reserve(0, 0, false); e != nil {
			t.Fatal(e)
		}
	}
	if calls.reserve(0, 0, false) == nil {
		t.Fatal("active calls consumed cleanup reserve")
	}
	for i := 0; i < 12; i++ {
		if calls.reserve(0, 0, true) != nil {
			t.Fatal("cloud cleanup call reserve lost")
		}
	}
	drive := &hybridBudget{MaxCalls: 34 * hybridH2AttemptUpper, MaxBytes: 5 * hybridDriveCleanupUnitsReserve}
	if drive.reserveDrive(5, false) != nil || drive.reserveDrive(5, false) != nil || drive.reserveDrive(5, false) == nil || drive.reserveDrive(50, true) != nil || drive.reserveDrive(5, true) != nil {
		t.Fatal("Drive delete/get call reserve lost")
	}
}

type hybridRejectBase struct {
	*MemoryStore
	called atomic.Int64
}

func (s *hybridRejectBase) Get(context.Context, string) ([]byte, error) {
	s.called.Add(1)
	return nil, errors.New("base must not see missing p0")
}
func (s *hybridRejectBase) Delete(context.Context, string) error {
	s.called.Add(1)
	return errors.New("base must not see missing p0")
}
func TestHybridMissingPriorityNameNeverFallsBackAndACKBytesBounded(t *testing.T) {
	client, exit, name, sealed := hybridTestPair(t)
	base := &hybridRejectBase{MemoryStore: NewMemoryStore()}
	exit.base = base
	if _, e := exit.Get(context.Background(), name); e == nil {
		t.Fatal("missing p0 Get accepted")
	}
	if e := exit.Delete(context.Background(), name); e == nil {
		t.Fatal("missing p0 Delete accepted")
	}
	if base.called.Load() != 0 {
		t.Fatal("missing p0 reached Drive")
	}
	exit.ackBytes = hybridAckBytesMax
	exit.receive = func(ObjectInfo) bool { t.Fatal("ACK-overflow record enqueued"); return false }
	m := hybridTestMessage(t, client, name, sealed, "over-budget", time.Now())
	if e := exit.ingest(context.Background(), m); e == nil {
		t.Fatal("aggregate retained ACK-ID byte bound exceeded")
	}
}
func TestHybridActualOPENDoesNotRedialAfterMuxClosedExpiry(t *testing.T) {
	client, exit, _, _ := hybridTestPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	initial := []byte("one destination-side effect")
	raw, e := encodeMuxBatch([]muxFrame{{Kind: muxFrameOpen, StreamID: 99, Payload: encodeMuxOpenPayload("example.test:443", initial)}})
	if e != nil {
		t.Fatal(e)
	}
	key, _ := DeriveMuxLaneKeyV4(exit.secret, exit.sid, DirectionUp, exit.clientID, exit.runID, 0)
	sealed, e := Seal(key, exit.sid, DirectionUp, 42, raw, false)
	if e != nil {
		t.Fatal(e)
	}
	name := muxObjectNameWithStreamIDs(exit.sid, DirectionUp, exit.clientID, exit.runID, "open-epoch", 99, []uint64{99}, 0, 42, 1, 0, 0, len(raw), true)
	tunnel := &Tunnel{Data: exit, Secret: exit.secret, SessionID: exit.sid, ClientID: exit.clientID, RunID: exit.runID, Logger: log.New(io.Discard, "", 0)}
	mux, e := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if e != nil {
		t.Fatal(e)
	}
	defer mux.closeAll()
	var dials, writes, acks atomic.Int64
	written := make(chan struct{}, 2)
	var observers sync.WaitGroup
	mux.dialExit = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		local, remote := net.Pipe()
		observers.Add(1)
		go func() {
			defer observers.Done()
			defer remote.Close()
			got := make([]byte, len(initial))
			if _, e := io.ReadFull(remote, got); e == nil && bytes.Equal(got, initial) {
				writes.Add(1)
				written <- struct{}{}
			}
			<-ctx.Done()
		}()
		return local, nil
	}
	exit.receive = func(info ObjectInfo) bool {
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			t.Fatal("invalid OPEN metadata")
		}
		if e := mux.processMuxObject(ctx, meta); e != nil {
			t.Error(e)
			return false
		}
		return true
	}
	exit.ack = func(context.Context, string, string, any) (*HTTPResult, error) {
		acks.Add(1)
		return &HTTPResult{Status: 200}, nil
	}
	at := time.Now()
	m := hybridTestMessage(t, client, name, sealed, "open-first", at)
	if e = exit.ingest(ctx, m); e != nil {
		t.Fatal(e)
	}
	select {
	case <-written:
	case <-ctx.Done():
		t.Fatal("initial destination bytes missing")
	}
	if acks.Load() != 0 {
		t.Fatal("OPEN ACK before mux processing/deferred Delete")
	}
	if e = exit.DeleteID(ctx, hybridID(name, sealed)); e != nil {
		t.Fatal(e)
	}
	streamKey := muxStreamKey{ClientID: exit.clientID, RunID: exit.runID, StreamID: 99}
	stream := mux.streamByKey(streamKey)
	if stream == nil {
		t.Fatal("real destination stream missing")
	}
	stream.close()
	mux.streamsMu.Lock()
	mux.closed[streamKey] = time.Now().Add(-time.Second)
	mux.streamsMu.Unlock()
	if mux.isClosedStream(streamKey) {
		t.Fatal("could not expire mux closed claim")
	}
	exit.now = func() time.Time { return at.Add(muxClosedStreamTTL + time.Second) }
	m.AckID = "open-redelivery-after-closed-ttl"
	if e = exit.ingest(ctx, m); e != nil {
		t.Fatal(e)
	}
	if dials.Load() != 1 || writes.Load() != 1 || acks.Load() != 2 {
		t.Fatal("duplicate OPEN redialed/repeated initial bytes or lost ACK", dials.Load(), writes.Load(), acks.Load())
	}
	cancel()
	mux.closeAll()
	observers.Wait()
}

func TestHybridProgressJournalConcurrentRowsAndFailureCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := &hybridProgress{file: file, cancel: cancel}
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func(i int) { defer writers.Done(); progress.write(map[string]any{"phase": "sample", "sequence": i}) }(i)
	}
	writers.Wait()
	if e = progress.error(); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 8 {
		t.Fatal("journal rows lost", len(lines))
	}
	for _, line := range lines {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil || row["phase"] != "sample" {
			t.Fatal("journal row interleaved")
		}
	}
	file.Close()
	progress.write(map[string]any{"phase": "failure"})
	if progress.error() == nil || ctx.Err() == nil {
		t.Fatal("journal failure did not cancel bounded active work")
	}
}

func TestHybridGuardDenialCancelsBeforeTransportDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := &hybridBudget{MaxCalls: 32, MaxBytes: hybridDriveCleanupUnitsReserve + 1600}
	var dispatched atomic.Int64
	gate := &hybridDriveGate{budget: budget, onGuard: cancel, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return stringResponse(200, "unexpected"), nil
	})}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.google.com/drive/v3/files", nil)
	if _, e := gate.RoundTrip(req); e == nil || ctx.Err() == nil || dispatched.Load() != 0 || budget.snapshot()["local_guard_denials"] != 1 {
		t.Fatal("Drive guard did not abort before transport dispatch")
	}
	api := &hybridREST{budget: &hybridBudget{MaxCalls: 32, MaxBytes: 8 << 20}, onGuard: cancel}
	if _, e := api.call(context.Background(), "POST", "/v1/test", nil); e == nil {
		t.Fatal("PubSub guard did not stop before transport")
	}
}
func TestHybridBoundedLogAndBodyProgress(t *testing.T) {
	var output bytes.Buffer
	bounded := &hybridBoundedLog{writer: &output, limit: 5}
	if n, e := bounded.Write([]byte("12345678")); e != nil || n != 8 {
		t.Fatal("bounded log writer contract")
	}
	bounded.Write([]byte("abc"))
	if output.String() != "12345" || bounded.stats()["dropped_bytes"] != 6 {
		t.Fatal("private log exceeded bound")
	}
	var progress []int64
	reader := &hybridBodyProgress{base: bytes.NewReader(make([]byte, 2<<20)), next: 1 << 20, emit: func(n int64) { progress = append(progress, n) }}
	if n, e := io.Copy(io.Discard, reader); e != nil || n != 2<<20 || len(progress) != 2 {
		t.Fatal("body progress did not preserve exact stream", n, e, len(progress))
	}
}

func TestHybridWorstCaseH2ReservationsPrecedeEveryDispatch(t *testing.T) {
	budget := &hybridBudget{MaxCalls: 34 * hybridH2AttemptUpper, MaxBytes: hybridCleanupByteReserve + 80}
	if e := budget.reserve(10, 0, false); e != nil {
		t.Fatal(e)
	}
	if budget.reserve(1, 0, false) == nil {
		t.Fatal("opaque body replay upper bound not reserved before dispatch")
	}
	if budget.snapshot()["request_body_bytes"] != 10 || budget.snapshot()["request_body_bytes_upper_bound"] != 80 || budget.snapshot()["attempt_reservations_upper"] != 8 {
		t.Fatal("visible bytes and upper bound conflated")
	}
	driver := &hybridBudget{MaxCalls: 34 * hybridH2AttemptUpper, MaxBytes: hybridDriveCleanupUnitsReserve + 800}
	if driver.reserveDrive(100, false) != nil || driver.reserveDrive(5, false) == nil || driver.driveSnapshot()["reserved_units_upper"] != 800 {
		t.Fatal("bodyless GET internal retry quota not reserved")
	}
	if driver.reserveDrive(50, true) != nil {
		t.Fatal("worst-case reservation consumed cleanup")
	}
}
