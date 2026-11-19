package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type driveV2PriorityFakeDoc struct {
	name     string
	wire     []byte
	revision uint64
}
type driveV2PriorityFakeCall struct {
	Method, ID, IfMatch string
	Body                []byte
}
type driveV2PriorityFake struct {
	mu                                        sync.Mutex
	docs                                      map[string]*driveV2PriorityFakeDoc
	calls                                     []driveV2PriorityFakeCall
	expectedURLHost                           string
	honor, constantTag, omitTag               bool
	nextWriteBefore, nextWriteAfter, nextRead error
	nextReadStatus                            int
	nextMetadata                              func(map[string]string)
	nextHeaderETag                            string
	faultWriteID, faultReadID, frozenReadID   string
}

func newDriveV2PriorityFake() *driveV2PriorityFake {
	return &driveV2PriorityFake{honor: true, expectedURLHost: "www.google.com", docs: map[string]*driveV2PriorityFakeDoc{
		"public-v2-up-owned-file":   {name: "skirk-v2-priority-0123456789abcdef0123456789abcdef-up", revision: 1},
		"public-v2-down-owned-file": {name: "skirk-v2-priority-0123456789abcdef0123456789abcdef-down", revision: 1},
	}}
}
func (f *driveV2PriorityFake) tag(d *driveV2PriorityFakeDoc) string {
	revision := d.revision
	if f.constantTag {
		revision = 1
	}
	return fmt.Sprintf("\"public-v2-fence-%d\"", revision)
}
func (f *driveV2PriorityFake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Host != f.expectedURLHost || r.Host != "www.googleapis.com" || r.Header.Get("Authorization") != "Bearer public-v2-fixture-token" || r.URL.Query().Get("fields") != "id,title,description,version,etag" || len(r.URL.Query()) != 1 {
		return nil, errFSHybridIntegrity
	}
	id := strings.TrimPrefix(r.URL.Path, "/drive/v2/files/")
	d := f.docs[id]
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	f.calls = append(f.calls, driveV2PriorityFakeCall{r.Method, id, r.Header.Get("If-Match"), bytes.Clone(body)})
	response := func(status int, raw []byte) *http.Response {
		return &http.Response{StatusCode: status, Proto: "HTTP/2.0", ProtoMajor: 2, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}
	}
	if d == nil {
		return response(404, []byte(`{"error":{"code":404}}`)), nil
	}
	if r.Method == http.MethodGet {
		if f.nextRead != nil && (f.faultReadID == "" || f.faultReadID == id) {
			err := f.nextRead
			f.nextRead = nil
			return nil, err
		}
		if f.nextReadStatus != 0 {
			status := f.nextReadStatus
			f.nextReadStatus = 0
			return response(status, []byte(`{"error":{"code":403}}`)), nil
		}
		if r.Header.Get("If-None-Match") == f.tag(d) {
			return response(304, nil), nil
		}
		if f.frozenReadID == id && r.Header.Get("If-None-Match") != "" {
			return response(304, nil), nil
		}
	} else if r.Method == http.MethodPatch {
		if f.nextWriteBefore != nil && (f.faultWriteID == "" || f.faultWriteID == id) {
			err := f.nextWriteBefore
			f.nextWriteBefore = nil
			return nil, err
		}
		if f.honor && r.Header.Get("If-Match") != f.tag(d) {
			return response(412, []byte(`{"error":{"code":412}}`)), nil
		}
		var value map[string]string
		if json.Unmarshal(body, &value) != nil || len(value) != 1 {
			return response(400, nil), nil
		}
		wire, err := base64.RawURLEncoding.Strict().DecodeString(value["description"])
		if err != nil {
			return response(400, nil), nil
		}
		d.wire = bytes.Clone(wire)
		d.revision++
		if f.nextWriteAfter != nil && (f.faultWriteID == "" || f.faultWriteID == id) {
			err := f.nextWriteAfter
			f.nextWriteAfter = nil
			return nil, err
		}
	} else {
		return response(405, nil), nil
	}
	metadata := map[string]string{"id": id, "title": d.name, "description": base64.RawURLEncoding.EncodeToString(d.wire), "version": fmt.Sprint(d.revision)}
	if !f.omitTag {
		metadata["etag"] = f.tag(d)
	}
	if f.nextMetadata != nil {
		f.nextMetadata(metadata)
		f.nextMetadata = nil
	}
	raw, _ := json.Marshal(metadata)
	result := response(200, raw)
	if f.nextHeaderETag != "" {
		result.Header.Set("ETag", f.nextHeaderETag)
		f.nextHeaderETag = ""
	}
	return result, nil
}

func driveV2PriorityFixture(t *testing.T, backend *driveV2PriorityFake, up bool, configure ...func(*driveV2PriorityConfig)) *driveV2PriorityCarrier {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("owned research carrier requires POSIX private-directory permission proof")
	}
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "carrier")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	own, peer := "public-v2-up-owned-file", "public-v2-down-owned-file"
	if !up {
		own, peer = peer, own
	}
	cfg := driveV2PriorityConfig{PrivateDir: dir, OwnID: own, PeerID: peer, OwnName: backend.docs[own].name, PeerName: backend.docs[peer].name, Route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, Token: func(context.Context) (string, error) { return "public-v2-fixture-token", nil }, BeforeDispatch: func(context.Context, string, string, int) error { return nil }, PollInterval: 20 * time.Millisecond, IdleInterval: 40 * time.Millisecond, ActiveHold: 40 * time.Millisecond, Duration: 15 * time.Second}
	for _, apply := range configure {
		apply(&cfg)
	}
	c, err := newDriveV2PriorityCarrier(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.getClient.client.Transport = backend
	c.patchClient.client.Transport = backend
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestDriveV2PriorityCarrierFeatureDetectionAndExactConditionalWrites(t *testing.T) {
	b := newDriveV2PriorityFake()
	c := driveV2PriorityFixture(t, b, true)
	if _, err := c.ReadOwn(context.Background()); !errors.Is(err, errDriveV2CASUnsupported) {
		t.Fatal("unverified carrier admitted application", err)
	}
	if err := c.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().ProbePassed != 1 || c.Snapshot().Visible != 7 {
		t.Fatal("probe did not verify exact feature phases", c.Snapshot())
	}
	a, err := c.ReadOwn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire := []byte("synthetic opaque authenticated snapshot fixture")
	next, err := c.WriteOwn(context.Background(), a.fence(), wire)
	if err != nil || !bytes.Equal(next.Ciphertext, wire) || next.sameFence(a) {
		t.Fatal("conditional mutation failed", err)
	}
	if _, err = c.WriteOwn(context.Background(), a.fence(), []byte("stale overwrite")); !fsHybridPrecondition(err) {
		t.Fatal("stale write was not fenced", err)
	}
	r, err := c.ReadOwn(context.Background())
	if err != nil || !r.sameFence(next) || !bytes.Equal(r.Ciphertext, wire) {
		t.Fatal("stale request changed current data", err)
	}
	if err = c.ProbeOwnCAS(context.Background()); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("feature probe reused live application document")
	}
}

func TestDriveV2PriorityCarrierRespectsUserSelectedDirectRoute(t *testing.T) {
	b := newDriveV2PriorityFake()
	b.expectedURLHost = "www.googleapis.com"
	c := driveV2PriorityFixture(t, b, true, func(cfg *driveV2PriorityConfig) {
		cfg.Route = RouteConfig{Mode: "direct"}
	})
	if err := c.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	previous, err := c.ReadOwn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := c.WriteOwn(context.Background(), previous.fence(), []byte("authenticated direct-route fixture"))
	if err != nil || updated.sameFence(previous) {
		t.Fatal("direct-route CAS mutation failed", err)
	}
	if _, err := c.ReadPeer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.ProbePassed != 1 || s.Visible != 10 || s.Errors != 0 {
		t.Fatal("direct route did not preserve feature probe and exact reads", s)
	}
}

func TestDriveV2PriorityCarrierRejectsUnselectedRouteFallback(t *testing.T) {
	b := newDriveV2PriorityFake()
	for _, route := range []RouteConfig{
		{Mode: "direct", GoogleIP: "203.0.113.1"},
		{Mode: "google_front_pinned"},
		{Mode: "google_front", GoogleIP: "203.0.113.1"},
		{Mode: "direct", Proxy: "http://127.0.0.1:8080"},
	} {
		t.Run(fmt.Sprintf("%s-%s-%s", route.Mode, route.GoogleIP, route.Proxy), func(t *testing.T) {
			c := driveV2PriorityFixture(t, b, true)
			cfg := c.cfg
			cfg.Route = route
			if _, err := cfg.normalized(); err == nil {
				t.Fatal("unselected route was admitted")
			}
		})
	}
}

func TestDriveV2PriorityCarrierFeatureDetectionFailsClosed(t *testing.T) {
	for _, mode := range []string{"ignored", "constant", "missing", "nonempty", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			b := newDriveV2PriorityFake()
			switch mode {
			case "ignored":
				b.honor = false
			case "constant":
				b.constantTag = true
			case "missing":
				b.omitTag = true
			case "nonempty":
				b.docs["public-v2-up-owned-file"].wire = []byte("not fresh")
			case "forbidden":
				b.nextReadStatus = 403
			}
			c := driveV2PriorityFixture(t, b, true)
			if err := c.ProbeOwnCAS(context.Background()); err == nil {
				t.Fatal("unsafe feature was accepted")
			}
			if c.Snapshot().ProbePassed != 0 {
				t.Fatal("failed probe granted readiness")
			}
			if _, err := c.WriteOwn(context.Background(), "\"public\"", nil); !errors.Is(err, errDriveV2CASUnsupported) {
				t.Fatal("failed feature admitted write", err)
			}
		})
	}
}

func TestDriveV2PriorityCarrierFixedPathsMethodHeaderAndQuotaGuard(t *testing.T) {
	b := newDriveV2PriorityFake()
	c := driveV2PriorityFixture(t, b, true)
	for _, r := range []struct {
		method, id string
		headers    map[string]string
	}{
		{http.MethodDelete, c.cfg.OwnID, nil}, {http.MethodPut, c.cfg.OwnID, nil}, {http.MethodPatch, c.cfg.PeerID, nil}, {http.MethodGet, "root", nil}, {http.MethodGet, c.cfg.OwnID, map[string]string{"Authorization": "Bearer foreign"}},
	} {
		if _, err := c.request(context.Background(), r.method, r.id, r.headers, nil); err == nil {
			t.Fatal("forbidden request admitted")
		}
	}
	if c.Snapshot().Reserved != 0 || len(b.calls) != 0 {
		t.Fatal("forbidden request reached budget/backend")
	}
	c.cfg.BeforeDispatch = func(context.Context, string, string, int) error { return errFSHybridBounds }
	if err := c.ProbeOwnCAS(context.Background()); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("global guard did not stop carrier", err)
	}
	if s := c.Snapshot(); s.Reserved != 1 || s.Visible != 0 || s.GuardDenied != 1 || len(b.calls) != 0 {
		t.Fatal("reservation was reported as actual HTTP", s)
	}
}

func TestDriveV2PriorityCarrierPollingCoalescesAndCancels(t *testing.T) {
	b := newDriveV2PriorityFake()
	c := driveV2PriorityFixture(t, b, true)
	if err := c.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan fsPriorityDocument, 8)
	done := make(chan error, 1)
	go func() { done <- c.WatchPeer(ctx, func(d fsPriorityDocument) error { seen <- d; return nil }) }()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("no initial peer snapshot")
	}
	fsHybridEventually(t, func() bool { return c.Snapshot().NotModified >= 2 && c.Snapshot().PollIdle > 0 })
	b.mu.Lock()
	peer := b.docs[c.cfg.PeerID]
	peer.wire = []byte("coalesced final state")
	peer.revision += 3
	b.mu.Unlock()
	select {
	case d := <-seen:
		if !bytes.Equal(d.Ciphertext, []byte("coalesced final state")) {
			t.Fatal("wrong latest state")
		}
	case <-time.After(time.Second):
		t.Fatal("coalesced change not seen")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll owner did not join")
	}
	if err := c.Close(); err != nil || c.Snapshot().Joined != 1 {
		t.Fatal("carrier did not close", err)
	}
	if _, err := c.ReadPeer(context.Background()); !errors.Is(err, errFSHybridClosed) {
		t.Fatal("late request after Close", err)
	}
}

func TestDriveV2PriorityCarrierLostWriteUsesExactOpaqueReadback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprint(commit), func(t *testing.T) {
			b := newDriveV2PriorityFake()
			c := driveV2PriorityFixture(t, b, true)
			if err := c.ProbeOwnCAS(context.Background()); err != nil {
				t.Fatal(err)
			}
			previous, err := c.ReadOwn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			before := len(b.calls)
			if commit {
				b.nextWriteAfter = context.DeadlineExceeded
			} else {
				b.nextWriteBefore = context.DeadlineExceeded
			}
			b.mu.Unlock()
			store := &fsHybridPriorityStore{ctx: context.Background(), carrier: c, cfg: fsHybridStoreConfig{OperationTimeout: time.Second}, counters: make(map[string]int64)}
			wire := []byte("prepared-once-opaque-snapshot")
			doc, err := store.writePrepared(previous, wire)
			if err != nil || !bytes.Equal(doc.Ciphertext, wire) || doc.sameFence(previous) {
				t.Fatal("opaque lost-write recovery failed", err)
			}
			b.mu.Lock()
			calls := append([]driveV2PriorityFakeCall(nil), b.calls[before:]...)
			b.mu.Unlock()
			var writes []driveV2PriorityFakeCall
			for _, call := range calls {
				if call.Method == http.MethodPatch {
					writes = append(writes, call)
				}
			}
			want := 2
			if commit {
				want = 1
			}
			if len(writes) != want {
				t.Fatal("unexpected mutation count", len(writes))
			}
			for _, w := range writes {
				if w.IfMatch != previous.fence() || !bytes.Equal(w.Body, writes[0].Body) {
					t.Fatal("retry changed prepared body or fence")
				}
			}
		})
	}
}

func TestDriveV2PriorityCarrierMalformedIdentityFenceAndPayloadStayFatal(t *testing.T) {
	for _, mode := range []string{"id", "title", "header_conflict", "oversized", "wrong_written_ciphertext", "unchanged_write_tag"} {
		t.Run(mode, func(t *testing.T) {
			b := newDriveV2PriorityFake()
			c := driveV2PriorityFixture(t, b, true)
			if err := c.ProbeOwnCAS(context.Background()); err != nil {
				t.Fatal(err)
			}
			previous, err := c.ReadOwn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			switch mode {
			case "id":
				b.nextMetadata = func(m map[string]string) { m["id"] = "foreign-public-fixture" }
			case "title":
				b.nextMetadata = func(m map[string]string) { m["title"] = "foreign-public-fixture" }
			case "header_conflict":
				b.nextHeaderETag = "\"different-fixture-tag\""
			case "oversized":
				b.nextMetadata = func(m map[string]string) {
					m["description"] = base64.RawURLEncoding.EncodeToString(make([]byte, driveV2PriorityCipherMax+1))
				}
			case "wrong_written_ciphertext":
				b.nextMetadata = func(m map[string]string) {
					m["description"] = base64.RawURLEncoding.EncodeToString([]byte("wrong response"))
				}
			case "unchanged_write_tag":
				b.nextMetadata = func(m map[string]string) { m["etag"] = previous.fence() }
			}
			b.mu.Unlock()
			if mode == "wrong_written_ciphertext" || mode == "unchanged_write_tag" {
				_, err = c.WriteOwn(context.Background(), previous.fence(), []byte("prepared correct ciphertext"))
			} else {
				_, err = c.ReadOwn(context.Background())
			}
			if !errors.Is(err, errFSHybridIntegrity) {
				t.Fatal("invalid metadata was not an integrity failure", err)
			}
		})
	}
}

func TestDriveV2PriorityCarrierMissingDocumentAndForbiddenReadbackCannotRetryMutation(t *testing.T) {
	b := newDriveV2PriorityFake()
	c := driveV2PriorityFixture(t, b, true)
	if err := c.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	previous, err := c.ReadOwn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	before := len(b.calls)
	b.nextWriteBefore = context.DeadlineExceeded
	b.nextReadStatus = 403
	b.mu.Unlock()
	store := &fsHybridPriorityStore{ctx: context.Background(), carrier: c, cfg: fsHybridStoreConfig{OperationTimeout: time.Second}, counters: make(map[string]int64)}
	if _, err = store.writePrepared(previous, []byte("prepared")); err == nil {
		t.Fatal("forbidden readback became retry permission")
	}
	b.mu.Lock()
	writes := 0
	for _, call := range b.calls[before:] {
		if call.Method == http.MethodPatch {
			writes++
		}
	}
	delete(b.docs, c.cfg.OwnID)
	b.mu.Unlock()
	if writes != 1 {
		t.Fatal("authorization failure caused another mutation", writes)
	}
	if _, err = c.WriteOwn(context.Background(), previous.fence(), []byte("must not recreate")); err == nil {
		t.Fatal("missing document recreated")
	}
	b.mu.Lock()
	_, exists := b.docs[c.cfg.OwnID]
	b.mu.Unlock()
	if exists {
		t.Fatal("conditional PATCH recreated a missing file")
	}
}

func TestDriveV2PriorityCarrierCloseJoinsUnresponsiveTokenAndRejectsLateSuccess(t *testing.T) {
	b := newDriveV2PriorityFake()
	c := driveV2PriorityFixture(t, b, true)
	if err := c.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	previous, err := c.ReadOwn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	c.cfg.Token = func(context.Context) (string, error) {
		close(started)
		<-release
		return "public-v2-fixture-token", nil
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := c.WriteOwn(context.Background(), previous.fence(), []byte("pending"))
		writeDone <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close detached token owner")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("late token returned write success")
		}
	case <-time.After(time.Second):
		t.Fatal("write did not join")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join")
	}
	if c.Snapshot().Joined != 1 {
		t.Fatal("join proof missing")
	}
}
