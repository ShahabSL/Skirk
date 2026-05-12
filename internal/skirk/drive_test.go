package skirk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestDriveStoreListUsesDocumentedPageSize(t *testing.T) {
	var gotQuery string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotQuery = req.URL.RawQuery
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	if _, err := store.List(context.Background(), "control/session/"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "pageSize=100") {
		t.Fatalf("query = %q, want documented pageSize=100", gotQuery)
	}
}

func TestDriveStoreListFreshStopsAtOlderObjects(t *testing.T) {
	since := time.Date(2026, 5, 11, 15, 0, 0, 0, time.UTC)
	pages := 0
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		pages++
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		if query.Get("pageToken") == "" {
			return stringResponse(http.StatusOK, `{
				"nextPageToken":"next",
				"files":[
					{"id":"fresh","name":"muxv3/session/down/l00/0000000000000001.f1.b32","size":"32","modifiedTime":"2026-05-11T15:00:01Z"},
					{"id":"old","name":"muxv3/session/down/l00/0000000000000000.f1.b32","size":"32","modifiedTime":"2026-05-11T14:59:59Z"}
				]
			}`), nil
		}
		return stringResponse(http.StatusOK, `{"files":[{"id":"too-old","name":"muxv3/session/down/l00/ffffffffffffffff.f1.b32","size":"32","modifiedTime":"2026-05-11T14:00:00Z"}]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	infos, err := store.ListFresh(context.Background(), "muxv3/session/down/", since)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Fatalf("pages = %d, want 1", pages)
	}
	if len(infos) != 1 || infos[0].ID != "fresh" {
		t.Fatalf("infos = %#v, want only fresh object", infos)
	}
}

func TestDriveQuotaStatsReportsEstimatedUnits(t *testing.T) {
	stats := newDriveQuotaStats(time.Minute)
	stats.since = time.Now().Add(-time.Second)
	stats.Record("upload", http.StatusOK, 10, nil)
	stats.since = time.Now().Add(-time.Minute)
	report, ok := stats.Record("download", http.StatusTooManyRequests, 20, nil)
	if !ok {
		t.Fatal("expected report")
	}
	if report.Calls != 2 || report.Units != 250 || report.Errors != 1 || report.ResponseBytes != 30 {
		t.Fatalf("report = %+v, want 2 calls, 250 units, 1 error, 30 bytes", report)
	}
	if report.Ops["upload"].Units != 50 || report.Ops["download"].Units != 200 {
		t.Fatalf("ops = %#v, want upload=50 and download=200 units", report.Ops)
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
