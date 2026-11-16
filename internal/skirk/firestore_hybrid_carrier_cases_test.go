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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fsHybridPublicTime = "2026-11-15T22:30:01.123456Z"

func fsHybridCarrierFixture(t *testing.T) *fsHybridGoogleCarrier {
	t.Helper()
	if runtime.GOOS == "windows" {
		// This owned research fixture enforces POSIX group/other permissions.
		// Go's Chmod on Windows controls only the read-only attribute.
		t.Skip("POSIX private-file permission fixture")
	}
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "carrier")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	database := "projects/skirk-p/databases/skirk-hybrid-probe-abcdef012345"
	c, err := newFSHybridGoogleCarrier(context.Background(), fsHybridGoogleCarrierConfig{PrivateDir: dir, Database: database, OwnDocument: database + "/documents/owned/up", PeerDocument: database + "/documents/owned/down", OwnerToken: "owner-public-fixture-token", Route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.10"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type fsHybridRoundTrip func(*http.Request) (*http.Response, error)

func (f fsHybridRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fsHybridHTTP(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Proto: "HTTP/2.0", ProtoMajor: 2, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}
func fsHybridDocJSON(name string, wire []byte) []byte {
	raw, _ := json.Marshal(map[string]any{"name": name, "fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(wire)}}, "createTime": fsHybridPublicTime, "updateTime": fsHybridPublicTime})
	return raw
}

func TestFSHybridCarrierCASPreservesTimestampCipherAndPinnedFront(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	wire := []byte("public sealed bytes")
	calls := 0
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "www.google.com" || r.Host != "firestore.googleapis.com" || r.Method != http.MethodPatch || r.URL.Path != "/v1/"+c.cfg.OwnDocument {
			t.Fatal("route or owned path changed")
		}
		if r.Header.Get("Authorization") != "Bearer "+c.cfg.OwnerToken || strings.Contains(r.URL.String(), c.cfg.OwnerToken) {
			t.Fatal("private token placement changed")
		}
		q := r.URL.Query()
		if q.Get("currentDocument.updateTime") != fsHybridPublicTime || q.Get("updateMask.fieldPaths") != "sealed" || len(q) != 2 {
			t.Fatal("CAS precondition changed")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]map[string]map[string]string
		if json.Unmarshal(body, &value) != nil || value["fields"]["sealed"]["bytesValue"] != base64.StdEncoding.EncodeToString(wire) {
			t.Fatal("sealed body changed")
		}
		return fsHybridHTTP(200, fsHybridDocJSON(c.cfg.OwnDocument, wire)), nil
	})
	got, err := c.WriteOwn(context.Background(), fsHybridPublicTime, wire)
	if err != nil || !bytes.Equal(got.Ciphertext, wire) || got.UpdateTime != fsHybridPublicTime || calls != 1 {
		t.Fatal("conditional write failed", err)
	}
	if c.Snapshot()["attempts_upper"] != 8 || c.Snapshot()["rest_writes"] != 1 {
		t.Fatal("physical replay upper not reserved")
	}
}

func TestFSHybridCarrier412RetainsStatusWithoutServerBodyOrToken(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		return fsHybridHTTP(412, []byte(`{"error":{"message":"`+c.cfg.OwnerToken+`"}}`)), nil
	})
	_, err := c.WriteOwn(context.Background(), fsHybridPublicTime, []byte("public"))
	var api *GoogleAPIError
	if !errors.As(err, &api) || api.Status != 412 || api.Body != "" || strings.Contains(err.Error(), c.cfg.OwnerToken) {
		t.Fatal("412 or credential privacy lost")
	}
	if c.Snapshot()["api_errors"] != 1 {
		t.Fatal("real API failure disappeared")
	}
}

func TestFSHybridCarrierRetainsOnlyControlledRPCReason(t *testing.T) {
	for _, which := range []struct{ reason, want string }{{"FAILED_PRECONDITION", "FAILED_PRECONDITION"}, {"owner-public-fixture-token", http.StatusText(400)}} {
		raw, _ := json.Marshal(map[string]any{"error": map[string]string{"status": which.reason, "message": "owner-public-fixture-token"}})
		err := fsHybridResponseError(&HTTPResult{Status: 400, Body: raw})
		var api *GoogleAPIError
		if !errors.As(err, &api) || api.Reason != which.want || strings.Contains(err.Error(), "owner-public-fixture-token") {
			t.Fatal("untrusted error detail retained or typed reason lost")
		}
	}
}

func TestFSHybridCarrierReadRequiresExactIdentityFieldTypeAndTimestamp(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	valid := fsHybridDocJSON(c.cfg.OwnDocument, []byte("public"))
	for _, which := range []string{"foreign", "missing-field", "wrong-field-type", "nanosecond-time", "oversized", "extra-field"} {
		t.Run(which, func(t *testing.T) {
			raw := bytes.Clone(valid)
			switch which {
			case "foreign":
				raw = bytes.Replace(raw, []byte(c.cfg.OwnDocument), []byte(c.cfg.PeerDocument), 1)
			case "missing-field":
				raw = bytes.Replace(raw, []byte(`"bytesValue":"cHVibGlj"`), []byte(`"other":"cHVibGlj"`), 1)
			case "wrong-field-type":
				raw = bytes.Replace(raw, []byte(`"bytesValue":"cHVibGlj"`), []byte(`"bytesValue":7`), 1)
			case "nanosecond-time":
				raw = bytes.ReplaceAll(raw, []byte(fsHybridPublicTime), []byte("2026-11-15T22:30:01.123456789Z"))
			case "oversized":
				raw = fsHybridDocJSON(c.cfg.OwnDocument, make([]byte, fsHybridWireMax+1))
			case "extra-field":
				raw = bytes.Replace(raw, []byte(`"fields":{`), []byte(`"fields":{"foreign":{"bytesValue":""},`), 1)
			}
			if _, err := fsHybridDecodeDocument(raw, c.cfg.OwnDocument); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
}

func TestFSHybridCarrierNoRetryAfterAmbiguousWrite(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	calls := 0
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) { calls++; return nil, context.DeadlineExceeded })
	_, err := c.WriteOwn(context.Background(), fsHybridPublicTime, []byte("public"))
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || strings.Contains(err.Error(), c.cfg.OwnDocument) {
		t.Fatal("carrier replayed or exposed request identity")
	}
}

func TestFSHybridCarrierAdmissionReservesCleanupCallsAttemptsAndBytes(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	c.cfg.MaxCalls = 32
	c.cfg.MaxAttemptsUpper = 256
	c.cfg.MaxEncodedBytesUpper = 1 << 20
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) { return fsHybridHTTP(200, []byte("{}")), nil })
	for range 16 {
		if _, err := c.call(context.Background(), "read", http.MethodGet, "/v1/"+c.cfg.OwnDocument, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.call(context.Background(), "read", http.MethodGet, "/v1/"+c.cfg.OwnDocument, nil, nil, false); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("foreground consumed reserved cleanup")
	}
	for range 16 {
		if _, err := c.call(context.Background(), "listen", http.MethodGet, firestoreListenPath, nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.call(context.Background(), "listen", http.MethodGet, firestoreListenPath, nil, nil, true); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("cleanup exceeded total bound")
	}
	s := c.Snapshot()
	if s["visible_calls"] != 32 || s["attempts_upper"] != 256 || s["cleanup_calls"] != 16 {
		t.Fatal("conservative accounting changed")
	}
}

func TestFSHybridCarrierRejectsConcurrentWritersWithoutQueue(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return fsHybridHTTP(200, fsHybridDocJSON(c.cfg.OwnDocument, []byte("public"))), nil
	})
	done := make(chan error, 1)
	go func() { _, err := c.WriteOwn(context.Background(), fsHybridPublicTime, []byte("public")); done <- err }()
	<-entered
	if _, err := c.WriteOwn(context.Background(), fsHybridPublicTime, []byte("other")); !errors.Is(err, errFSHybridBusy) {
		t.Fatal("second writer admitted")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFSHybridWatchWhitelistRejectsForeignDocURLTokenAndDuplicateDatabase(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	path := firestoreListenPath + "?database=" + url.QueryEscape(c.cfg.Database)
	if !c.watchPath(path) {
		t.Fatal("owned path rejected")
	}
	for _, bad := range []string{path + "&database=" + url.QueryEscape(c.cfg.Database+"-foreign"), path + "&%24httpHeaders=public", path + "&headers=" + url.QueryEscape("Bearer "+c.cfg.OwnerToken), "https://firestore.googleapis.com" + path, "/v1/" + c.cfg.OwnDocument} {
		if c.watchPath(bad) {
			t.Fatal("foreign or credential URL admitted")
		}
	}
	packet := map[string]any{"database": c.cfg.Database, "addTarget": map[string]any{"targetId": 2, "documents": map[string]any{"documents": []string{c.cfg.PeerDocument}}}}
	raw, _ := json.Marshal(packet)
	form := url.Values{"count": []string{"1"}, "ofs": []string{"0"}, "req0___data__": []string{string(raw)}}
	if !c.watchPayload(http.MethodPost, []byte(form.Encode())) {
		t.Fatal("owned target rejected")
	}
	raw = bytes.Replace(raw, []byte(c.cfg.PeerDocument), []byte(c.cfg.OwnDocument), 1)
	form.Set("req0___data__", string(raw))
	if c.watchPayload(http.MethodPost, []byte(form.Encode())) {
		t.Fatal("wrong listener document admitted")
	}
}

func TestFSHybridCarrierCloseCancelsOwnedRESTAndRejectsNewWork(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	entered := make(chan struct{})
	done := make(chan error, 1)
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	go func() { _, err := c.ReadOwn(context.Background()); done <- err }()
	<-entered
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("active request not canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("owned request did not join")
	}
	if _, err := c.ReadPeer(context.Background()); !errors.Is(err, errFSHybridClosed) {
		t.Fatal("closed carrier admitted work")
	}
}

func TestFSHybridCarrierBudgetConcurrentAdmissionIsAtomic(t *testing.T) {
	c := fsHybridCarrierFixture(t)
	c.cfg.MaxCalls = 32
	c.cfg.MaxAttemptsUpper = 256
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) { return fsHybridHTTP(200, []byte("{}")), nil })
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.call(context.Background(), "read", http.MethodGet, "/v1/"+c.cfg.OwnDocument, nil, nil, false)
		}()
	}
	wg.Wait()
	s := c.Snapshot()
	if s["visible_calls"] > 16 || s["attempts_upper"] != s["visible_calls"]*8 {
		t.Fatal("concurrent guard overshot foreground reservation")
	}
}

func TestFSHybridWatchOfficialSDKRealLoopbackJoins(t *testing.T) {
	blobPath := os.Getenv("SKIRK_WEBCHANNEL_BLOB_PATH")
	if blobPath == "" {
		t.Skip("pinned official blob fixture required")
	}
	blob, err := os.ReadFile(blobPath)
	if err != nil || fsHybridBlobDigest(blob) != firestoreWebChannelSHA {
		t.Fatal("invalid pinned official blob fixture")
	}
	c := fsHybridCarrierFixture(t)
	if err = os.WriteFile(filepath.Join(c.cfg.PrivateDir, "webchannel_blob_es2018.js"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	removed := make(chan struct{})
	var removeOnce, initialOnce sync.Once
	frame := func(v any) []byte { raw, _ := json.Marshal(v); return []byte(fmt.Sprintf("%d\n%s", len(raw), raw)) }
	response := func(body []byte) *http.Response {
		r := fsHybridHTTP(200, body)
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("X-HTTP-Session-Id", "hybrid-go-session")
		return r
	}
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.google.com" || r.Host != "firestore.googleapis.com" || r.URL.Path != firestoreListenPath || r.URL.Query().Get("database") != c.cfg.Database {
			t.Error("SDK escaped exact owned fronted listener")
			return nil, errFSHybridIntegrity
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		q := r.URL.Query()
		if q.Get("TYPE") == "terminate" {
			return response(nil), nil
		}
		if q.Get("SID") == "" {
			return response(frame([]any{[]any{0, []any{"c", "hybrid-go-session", "", 8, "fixture", 5000}}})), nil
		}
		if r.Method == http.MethodPost {
			decoded, _ := url.QueryUnescape(string(body))
			if strings.Contains(decoded, "removeTarget") {
				removeOnce.Do(func() { close(removed) })
			}
			return response(frame([]any{1, 3, 0})), nil
		}
		first := false
		initialOnce.Do(func() { first = true })
		if first {
			document := map[string]any{"name": c.cfg.PeerDocument, "updateTime": fsHybridPublicTime, "fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString([]byte("public"))}}}
			return response(frame([]any{[]any{1, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": "CURRENT", "targetIds": []int{2}}}}}, []any{2, []any{map[string]any{"documentChange": map[string]any{"document": document}}}}})), nil
		}
		select {
		case <-removed:
			return response(frame([]any{[]any{3, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": "REMOVE", "targetIds": []int{2}}}}}})), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	got := make(chan fsPriorityDocument, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.WatchPeer(context.Background(), func(d fsPriorityDocument) error {
			select {
			case got <- d:
			default:
			}
			return nil
		})
	}()
	select {
	case document := <-got:
		if !bytes.Equal(document.Ciphertext, []byte("public")) || document.UpdateTime != fsHybridPublicTime {
			t.Fatal("exact callback identity lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("official SDK delivered no exact state")
	}
	if err = c.Close(); err != nil {
		t.Fatal("owned SDK close did not prove clean join", err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch owner did not join")
	}
	state := c.WatchSnapshot()
	if state["stopped"] != true || state["target_remove_acknowledged"] != true || state["pending_requests"] != float64(0) || state["pending_events"] != float64(0) || state["pending_sdk_timers"] != float64(0) {
		t.Fatal("incomplete owned observer proof")
	}
}

func fsHybridReconnectBackend(c *fsHybridGoogleCarrier) fsHybridRoundTrip {
	type session struct {
		initial bool
		removed chan struct{}
		once    sync.Once
	}
	var mu sync.Mutex
	sessions := map[string]*session{}
	next := 0
	frame := func(v any) []byte { raw, _ := json.Marshal(v); return []byte(fmt.Sprintf("%d\n%s", len(raw), raw)) }
	response := func(body []byte, sid string) *http.Response {
		r := fsHybridHTTP(200, body)
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("X-HTTP-Session-Id", sid)
		return r
	}
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.google.com" || r.Host != "firestore.googleapis.com" {
			return nil, errFSHybridIntegrity
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/"+c.cfg.PeerDocument {
			return fsHybridHTTP(200, fsHybridDocJSON(c.cfg.PeerDocument, []byte("public"))), nil
		}
		if r.URL.Path != firestoreListenPath || r.URL.Query().Get("database") != c.cfg.Database {
			return nil, errFSHybridIntegrity
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		q := r.URL.Query()
		sid := q.Get("SID")
		if q.Get("TYPE") == "terminate" {
			return response(nil, sid), nil
		}
		mu.Lock()
		if sid == "" {
			next++
			sid = fmt.Sprintf("hybrid-session-%d", next)
			sessions[sid] = &session{removed: make(chan struct{})}
			mu.Unlock()
			return response(frame([]any{[]any{0, []any{"c", sid, "", 8, "fixture", 5000}}}), sid), nil
		}
		current := sessions[sid]
		if current == nil {
			mu.Unlock()
			return nil, errFSHybridIntegrity
		}
		if r.Method == http.MethodPost {
			decoded, _ := url.QueryUnescape(string(body))
			if strings.Contains(decoded, "removeTarget") {
				current.once.Do(func() { close(current.removed) })
			}
			mu.Unlock()
			return response(frame([]any{1, 3, 0}), sid), nil
		}
		initial := !current.initial
		current.initial = true
		mu.Unlock()
		if initial {
			document := map[string]any{"name": c.cfg.PeerDocument, "updateTime": fsHybridPublicTime, "fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString([]byte("public"))}}}
			return response(frame([]any{[]any{1, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": "CURRENT", "targetIds": []int{2}}}}}, []any{2, []any{map[string]any{"documentChange": map[string]any{"document": document}}}}}), sid), nil
		}
		select {
		case <-current.removed:
			return response(frame([]any{[]any{3, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": "REMOVE", "targetIds": []int{2}}}}}}), sid), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
}

func TestFSHybridWatchThreeSequentialOwnersJoinWithoutResettingSharedBudgets(t *testing.T) {
	path := os.Getenv("SKIRK_WEBCHANNEL_BLOB_PATH")
	if path == "" {
		t.Skip("pinned official blob fixture required")
	}
	blob, err := os.ReadFile(path)
	if err != nil || fsHybridBlobDigest(blob) != firestoreWebChannelSHA {
		t.Fatal("invalid SDK fixture")
	}
	c := fsHybridCarrierFixture(t)
	if err = os.WriteFile(filepath.Join(c.cfg.PrivateDir, "webchannel_blob_es2018.js"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	c.client.client.Transport = fsHybridReconnectBackend(c)
	fixtureErr := errors.New("public deliberate watch owner retry")
	previousCalls := int64(0)
	for attempt := 1; attempt <= 3; attempt++ {
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		go func() {
			done <- c.WatchPeer(context.Background(), func(d fsPriorityDocument) error {
				if !d.valid() {
					return errFSHybridIntegrity
				}
				close(entered)
				<-release
				return fixtureErr
			})
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("retry received no state")
		}
		if err = c.WatchPeer(context.Background(), func(fsPriorityDocument) error { return nil }); !errors.Is(err, errFSHybridBusy) {
			releaseOnce.Do(func() { close(release) })
			t.Fatal("concurrent owner admitted")
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case err = <-done:
			if !errors.Is(err, fixtureErr) {
				t.Fatal("original callback failure lost", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("previous SDK owner did not join")
		}
		state := c.WatchSnapshot()
		if state["attempts"] != attempt || state["stopped"] != true || state["target_remove_acknowledged"] != true || state["pending_requests"] != float64(0) {
			t.Fatal("reconnect started without prior shutdown proof")
		}
		dir := filepath.Join(c.cfg.PrivateDir, fmt.Sprintf("watch-%02d", attempt))
		if _, err = os.Stat(filepath.Join(dir, "hybrid-watch-result.json")); err != nil {
			t.Fatal("attempt evidence missing")
		}
		if _, err = os.Stat(filepath.Join(dir, "hybrid-watch-config.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private attempt config retained")
		}
		if _, err = c.ReadPeer(context.Background()); err != nil {
			t.Fatal("attempt retirement closed REST recovery", err)
		}
		calls := c.Snapshot()["visible_calls"]
		if calls <= previousCalls {
			t.Fatal("shared budgets reset or attempt made no calls")
		}
		previousCalls = calls
	}
	if err = c.WatchPeer(context.Background(), func(fsPriorityDocument) error { return nil }); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("fourth observer process admitted")
	}
	if c.WatchSnapshot()["attempt_failures"] != int64(3) {
		t.Fatal("deliberate failed attempts disappeared")
	}
}

func TestFSHybridCarrierRejectsPrivateDirectoryReuseAndSharedLifetimeExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private-file permission fixture")
	}
	c := fsHybridCarrierFixture(t)
	if _, err := newFSHybridGoogleCarrier(context.Background(), c.cfg); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("research allocation reused")
	}
	c.deadline = time.Now().Add(-time.Second)
	if _, err := c.ReadPeer(context.Background()); !errors.Is(err, errFSHybridClosed) {
		t.Fatal("REST exceeded shared session lifetime")
	}
	if err := c.WatchPeer(context.Background(), func(fsPriorityDocument) error { return nil }); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("watch exceeded shared lifetime")
	}
}

func TestFSHybridWatchStoreStyleCancelThenCloseHasCleanJoinedProof(t *testing.T) {
	path := os.Getenv("SKIRK_WEBCHANNEL_BLOB_PATH")
	if path == "" {
		t.Skip("pinned official SDK fixture required")
	}
	blob, err := os.ReadFile(path)
	if err != nil || fsHybridBlobDigest(blob) != firestoreWebChannelSHA {
		t.Fatal("invalid SDK fixture")
	}
	c := fsHybridCarrierFixture(t)
	if err = os.WriteFile(filepath.Join(c.cfg.PrivateDir, "webchannel_blob_es2018.js"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	c.client.client.Transport = fsHybridReconnectBackend(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		done <- c.WatchPeer(ctx, func(fsPriorityDocument) error {
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no connected SDK state")
	}
	cancel()
	if err = c.Close(); err != nil {
		t.Fatal("normal Store-style cancellation became a cleanup failure", err)
	}
	select {
	case err = <-done:
		if err != nil && err != context.Canceled {
			t.Fatal("unexpected watch result", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not join")
	}
	state := c.WatchSnapshot()
	if state["stopped"] != true || state["target_remove_acknowledged"] != true || state["attempt_failures"] != int64(0) {
		t.Fatal("clean canceled watch lost proof or hid a failure")
	}
}

func TestFSHybridWatchCloseNeverWaivesJoinedOrCallbackCancellation(t *testing.T) {
	for _, which := range []string{"joined-api", "wrapped-cancel", "callback-cancel"} {
		t.Run(which, func(t *testing.T) {
			c := fsHybridCarrierFixture(t)
			c.watchProofClean = true
			c.watchOwnerCanceled = true
			switch which {
			case "joined-api":
				c.watchErr = errors.Join(context.Canceled, &GoogleAPIError{Op: "public", Status: 403, Reason: "PERMISSION_DENIED"})
			case "wrapped-cancel":
				c.watchErr = fmt.Errorf("public wrapper: %w", context.Canceled)
			case "callback-cancel":
				c.watchErr = context.Canceled
				c.watchOwnerCanceled = false
			}
			if err := c.Close(); err == nil {
				t.Fatal("non-owned or mixed error disappeared")
			}
		})
	}
}
