package skirk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDriveStoreAppDataQuery(t *testing.T) {
	store := NewDriveStore(nil, "token", DriveConfig{Space: "appDataFolder"})
	if !store.isAppData() {
		t.Fatal("expected appDataFolder mode")
	}
	query := store.query("control/session/", false)
	if strings.Contains(query, "in parents") {
		t.Fatalf("appDataFolder query should not include a visible folder parent: %s", query)
	}
	if !strings.Contains(query, "name contains 'control/session/'") {
		t.Fatalf("query did not include name prefix: %s", query)
	}
}

func TestDriveStoreLegacyAppDataFolderID(t *testing.T) {
	store := NewDriveStore(nil, "token", DriveConfig{FolderID: "appDataFolder"})
	if !store.isAppData() {
		t.Fatal("expected legacy appDataFolder folder_id to enable appDataFolder mode")
	}
}

func TestDriveStoreRefreshesTokenAfterUnauthorized(t *testing.T) {
	var tokenCount int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := "token-" + strconv.Itoa(int(atomic.AddInt32(&tokenCount, 1)))
		_, _ = w.Write([]byte(`{"access_token":"` + token + `","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenServer.Close()

	source := NewAccessTokenSource(AuthConfig{
		ClientID:     "client-id",
		RefreshToken: "refresh-token",
		TokenURL:     tokenServer.URL,
	}, RouteConfig{Mode: "direct"})

	var mu sync.Mutex
	authHeaders := []string{}
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		attempt := len(authHeaders)
		mu.Unlock()
		if attempt == 1 {
			return stringResponse(http.StatusUnauthorized, `{"error":{"status":"UNAUTHENTICATED"}}`), nil
		}
		return stringResponse(http.StatusOK, `{"id":"file-id","name":"object","size":"4"}`), nil
	})}}

	store := NewDriveStoreWithTokenSource(httpClient, source, DriveConfig{Space: "appDataFolder"})
	if _, err := store.PutObject(context.Background(), "object", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("request attempts = %d, want 2", len(authHeaders))
	}
	if authHeaders[0] != "Bearer token-1" || authHeaders[1] != "Bearer token-2" {
		t.Fatalf("auth headers = %#v, want refreshed token on retry", authHeaders)
	}
}

func TestDriveStoreCreatesFastControlMarkersAsMetadataOnly(t *testing.T) {
	var gotPath string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.EscapedPath()
		if req.URL.RawQuery != "" {
			gotPath += "?" + req.URL.RawQuery
		}
		if req.Header.Get("Content-Type") != "application/json; charset=UTF-8" {
			t.Fatalf("content-type = %q, want metadata JSON", req.Header.Get("Content-Type"))
		}
		return stringResponse(http.StatusOK, `{"id":"file-id","name":"control/session/up/conn/0000000000000000.OPEN.dGFyZ2V0","size":"0"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	_, err := store.PutObject(context.Background(), "control/session/up/conn/0000000000000000.OPEN.dGFyZ2V0", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPath, "/drive/v3/files?") {
		t.Fatalf("path = %q, want metadata files.create endpoint", gotPath)
	}
	if strings.Contains(gotPath, "/upload/") {
		t.Fatalf("path = %q, should not use media upload endpoint", gotPath)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stringResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
