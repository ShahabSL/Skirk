package skirk

import (
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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFSHybridNativeSDKActiveExhaustionStillRemovesAndJoins(t *testing.T) {
	path := os.Getenv("SKIRK_WEBCHANNEL_BLOB_PATH")
	if path == "" {
		t.Skip("pinned official SDK fixture required")
	}
	blob, e := os.ReadFile(path)
	if e != nil || fsHybridBlobDigest(blob) != firestoreWebChannelSHA {
		t.Fatal("invalid SDK fixture")
	}
	c := fsHybridCarrierFixture(t)
	c.cfg.MaxWatchRequests = 16
	if e = os.WriteFile(filepath.Join(c.cfg.PrivateDir, "webchannel_blob_es2018.js"), blob, 0600); e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	initial, removed := false, false
	sequence := 2
	frame := func(value any) []byte {
		raw, _ := json.Marshal(value)
		return []byte(fmt.Sprintf("%d\n%s", len(raw), raw))
	}
	response := func(body []byte) *http.Response {
		r := fsHybridHTTP(200, body)
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("X-HTTP-Session-Id", "bounded-session")
		return r
	}
	c.client.client.Transport = fsHybridRoundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host != "www.google.com" || r.Host != "firestore.googleapis.com" || r.URL.Path != firestoreListenPath {
			return nil, errFSHybridIntegrity
		}
		q := r.URL.Query()
		if q.Get("TYPE") == "terminate" {
			return response(nil), nil
		}
		if q.Get("SID") == "" {
			return response(frame([]any{[]any{0, []any{"c", "bounded-session", "", 8, "fixture", 5000}}})), nil
		}
		if r.Method == http.MethodPost {
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
			decoded, _ := url.QueryUnescape(string(body))
			if strings.Contains(decoded, "removeTarget") {
				removed = true
			}
			return response(frame([]any{1, sequence + 1, 0})), nil
		}
		if !initial {
			initial = true
			doc := map[string]any{"name": c.cfg.PeerDocument, "updateTime": fsHybridPublicTime, "fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString([]byte("public"))}}}
			return response(frame([]any{[]any{1, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": "CURRENT", "targetIds": []int{2}}}}}, []any{2, []any{map[string]any{"documentChange": map[string]any{"document": doc}}}}})), nil
		}
		sequence++
		kind := "NO_CHANGE"
		ids := []int{}
		if removed {
			kind = "REMOVE"
			ids = []int{2}
		}
		return response(frame([]any{[]any{sequence, []any{map[string]any{"targetChange": map[string]any{"targetChangeType": kind, "targetIds": ids}}}}})), nil
	})
	done := make(chan error, 1)
	go func() { done <- c.WatchPeer(context.Background(), func(fsPriorityDocument) error { return nil }) }()
	select {
	case e = <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("active-bound native observer didnotjoin")
	}
	if !errors.Is(e, errFSHybridBounds) {
		t.Fatal("original active-bound failure notretained", e)
	}
	if e = c.Close(); e == nil {
		t.Fatal("failed workload was relabeledclean")
	}
	state := c.WatchSnapshot()
	if !c.WritersJoined() || state["child_joined"] != true || state["control_joined"] != true || state["target_remove_acknowledged"] != true || state["stopped"] != true || state["error"] != "REQUEST_BOUND" || state["pending_requests"] != float64(0) || state["pending_events"] != float64(0) || state["pending_sdk_timers"] != float64(0) {
		t.Fatal("failure cleanup lacked exact ownership/REMOVE proof", state)
	}
	if state["active_request_count"] != float64(8) || state["cleanup_request_count"].(float64) < 1 || state["request_count"].(float64) > 16 || c.Snapshot()["cleanup_calls"] == 0 {
		t.Fatal("native headroom wasnotbounded", state, c.Snapshot())
	}
	// A preserved workload error must remain an error while concrete joined
	// ownership still permits the outer fixture's exact provider cleanup.
	run := &fsHybridLiveRun{cancel: func() {}, carriers: []*fsHybridGoogleCarrier{c}}
	if err := run.stop(); err == nil || !run.joined {
		t.Fatal("failed-but-joined observer blocked exact cleanup or lost failure", err, run.joined)
	}
}
