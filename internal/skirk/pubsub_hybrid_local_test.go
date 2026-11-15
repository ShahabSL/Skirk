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
	"net/http"
	"net/http/httptest"
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
	budget := &hybridBudget{MaxCalls: 42, MaxBytes: 100}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if budget.reserve(1, false) == nil {
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
	api := &hybridREST{client: &GoogleHTTPClient{client: &http.Client{Transport: transport}}, token: "fictional", budget: &hybridBudget{MaxCalls: 100, MaxBytes: 8 << 20}}
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

func TestHybridTombstoneSurvivesMuxClosedLifetimeAndExpiredBrokerRecord(t *testing.T) {
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
	cfg := &Config{Secret: "hybrid-full-local", SessionID: hex.EncodeToString(sid[:]), Client: ClientConfig{ID: "client", RunID: "run"}, Tunnel: TunnelConfig{PollIntervalMS: 10, ChunkSize: 256 << 10, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
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
	rows, e := hybridSOCKSMeasurements(ctx, cfg, client, exit, client, exit, 4<<20)
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
	g := &hybridDriveGate{budget: &hybridBudget{MaxCalls: 100, MaxBytes: 10200}, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
	guard := &hybridBudget{MaxCalls: 100, MaxBytes: 10200}
	if guard.reserveDrive(200, false) != nil || guard.reserveDrive(5, false) == nil || guard.reserveDrive(50, true) != nil {
		t.Fatal("Drive cleanup reservation not protected")
	}
}

func TestHybridDriveStopWaitsForResponseBody(t *testing.T) {
	g := &hybridDriveGate{budget: &hybridBudget{MaxCalls: 100, MaxBytes: 10200}, base: roundTripFunc(func(r *http.Request) (*http.Response, error) { return stringResponse(200, "fictional"), nil })}
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
