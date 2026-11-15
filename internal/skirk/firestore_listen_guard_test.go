package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func firestoreListenFake(t *testing.T) (*firestoreListenDispatch, *atomic.Int64) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "journal.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	var calls atomic.Int64
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Hostname() != "www.google.com" || r.Host != "firestore.googleapis.com" || r.Header.Get("Authorization") != "Bearer synthetic-owner-token" {
			t.Error("logical authority, front or owner credential changed")
		}
		response := stringResponse(200, "[]")
		response.Proto = "HTTP/2.0"
		return response, nil
	}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
	return &firestoreListenDispatch{client: client, token: "synthetic-owner-token", journal: &firestoreListenJournal{file: file}}, &calls
}

func TestFirestoreListenBridgeEventsProgressWhenForwardSlotsAreFull(t *testing.T) {
	dispatch, _ := firestoreListenFake(t)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	dispatch.client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		response := stringResponse(200, "[]")
		response.Proto = "HTTP/2.0"
		return response, nil
	})
	database := "projects/skirk-p/databases/skirk-front-probe-abcdef012345"
	auth := strings.Repeat("a", 64)
	bridge := newFirestoreListenBridge(context.Background(), dispatch, database, auth)
	done := make(chan error, 4)
	defer func() {
		close(release)
		for i := 0; i < 4; i++ {
			<-done
		}
		bridge.close()
	}()
	body, _ := json.Marshal(map[string]any{"method": "GET", "path": firestoreListenPath + "?database=" + url.QueryEscape(database), "headers": map[string]string{}, "body_base64": ""})
	for i := 0; i < 4; i++ {
		go func() {
			request, _ := http.NewRequest(http.MethodPost, bridge.server.URL+"/request", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+auth)
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("forward requests did not reach controlled transport")
		}
	}
	for i := 0; i < 8; i++ {
		event, _ := json.Marshal(firestoreListenEvent{Kind: "target", Change: "NO_CHANGE"})
		request, _ := http.NewRequest(http.MethodPost, bridge.server.URL+"/event", bytes.NewReader(event))
		request.Header.Set("Authorization", "Bearer "+auth)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 204 {
			t.Fatal("forward pressure starved the local event acknowledgement", response.StatusCode)
		}
	}
}

func TestFirestoreListenBridgeRetainsLocalFailurePhaseWithoutSecrets(t *testing.T) {
	dispatch, calls := firestoreListenFake(t)
	database := "projects/skirk-p/databases/skirk-front-probe-abcdef012345"
	auth := strings.Repeat("a", 64)
	bridge := newFirestoreListenBridge(context.Background(), dispatch, database, auth)
	for _, input := range []struct {
		path   string
		body   string
		status int
	}{
		{"/event", `{"kind":"error","code":"synthetic-owner-token"}`, 400},
		{"/request", `{"method":"GET","path":"` + firestoreListenPath + `?database=` + url.QueryEscape(database) + `&opaque=synthetic-owner-token"}`, 403},
		{"/unowned-synthetic-owner-token", "{}", 404},
	} {
		request, _ := http.NewRequest(http.MethodPost, bridge.server.URL+input.path, strings.NewReader(input.body))
		request.Header.Set("Authorization", "Bearer "+auth)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != input.status {
			t.Fatal("unexpected bounded rejection", response.StatusCode)
		}
	}
	bridge.close()
	data, err := os.ReadFile(dispatch.journal.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || bytes.Contains(data, []byte(dispatch.token)) || bytes.Contains(data, []byte(auth)) || bytes.Contains(data, []byte("unowned-synthetic")) {
		t.Fatal("local diagnostics forwarded a rejected request or retained sensitive input")
	}
	for _, wanted := range []string{`"phase":"/event"`, `"status":400`, `"reason":"event_bound"`, `"phase":"/request"`, `"reason":"url_credentials"`, `"phase":"other"`, `"status":404`} {
		if !bytes.Contains(data, []byte(wanted)) {
			t.Fatal("local failure diagnosis missing", wanted)
		}
	}
}

func TestFirestoreListenPathRejectsForeignTargetsAndURLCredentials(t *testing.T) {
	database := "projects/skirk-p/databases/skirk-front-probe-abcdef012345"
	valid := firestoreListenPath + "?database=" + url.QueryEscape(database) + "&VER=8&RID=1"
	if !firestoreListenBridgePath(valid, database) {
		t.Fatal("official logical listener path rejected")
	}
	for _, bad := range []string{"https://unowned.invalid" + valid, "/v1/" + database + "/documents:listen?database=" + url.QueryEscape(database), firestoreListenPath + "?database=projects/another/databases/default", valid + "&$httpHeaders=x", valid + "&headers=Authorization%3ABearer%20secret", valid + "#fragment"} {
		if firestoreListenBridgePath(bad, database) {
			t.Fatal("foreign or credential-bearing path accepted")
		}
	}
}

func TestFirestoreListenBridgeRequiresSecretBeforeForwardAndPreservesFront(t *testing.T) {
	dispatch, calls := firestoreListenFake(t)
	database := "projects/skirk-p/databases/skirk-front-probe-abcdef012345"
	auth := strings.Repeat("a", 64)
	bridge := newFirestoreListenBridge(context.Background(), dispatch, database, auth)
	defer bridge.close()
	value := map[string]any{"method": "POST", "path": firestoreListenPath + "?database=" + url.QueryEscape(database), "headers": map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Bearer untrusted"}, "body_base64": base64.StdEncoding.EncodeToString([]byte("count=1"))}
	body, _ := json.Marshal(value)
	request, _ := http.NewRequest(http.MethodPost, bridge.server.URL+"/request", bytes.NewReader(body))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 0 {
		t.Fatal("missing bridge secret reached Google transport")
	}
	request, _ = http.NewRequest(http.MethodPost, bridge.server.URL+"/request", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+auth)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("authorized fake logical request failed")
	}
	value["path"] = "https://unowned.invalid/"
	body, _ = json.Marshal(value)
	request, _ = http.NewRequest(http.MethodPost, bridge.server.URL+"/request", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+auth)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 1 {
		t.Fatal("bridge escaped logical target whitelist")
	}
}

func TestFirestoreListenDispatchReservesCleanupAndCountsBeforePhysicalCall(t *testing.T) {
	dispatch, calls := firestoreListenFake(t)
	for i := 0; i < 36; i++ {
		if _, err := dispatch.call(context.Background(), "listen", http.MethodGet, firestoreListenPath, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dispatch.call(context.Background(), "listen", http.MethodGet, firestoreListenPath, nil, nil, false); err == nil || calls.Load() != 36 {
		t.Fatal("active probe used cleanup headroom")
	}
	for i := 0; i < 4; i++ {
		if _, err := dispatch.call(context.Background(), "cleanup", http.MethodDelete, "/v1/owned", nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dispatch.call(context.Background(), "cleanup", http.MethodDelete, "/v1/owned", nil, nil, true); err == nil || calls.Load() != 40 {
		t.Fatal("total dispatch cap exceeded")
	}
	if dispatch.snapshot()["http_attempts_upper"] != 320 {
		t.Fatal("physical retry upper accounting incorrect")
	}
	dispatch, calls = firestoreListenFake(t)
	if _, err := dispatch.call(context.Background(), "write", http.MethodPatch, "/v1/owned", nil, make([]byte, 2<<20), false); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.call(context.Background(), "write", http.MethodPatch, "/v1/owned", nil, []byte{1}, false); err == nil || calls.Load() != 1 {
		t.Fatal("encoded retry body upper exceeded")
	}
}
