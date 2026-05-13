package skirk

import (
	"bytes"
	"context"
	"io"
	"log"
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
		t.Fatal("expected appDataFolder folder_id to enable appDataFolder mode")
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

func TestDriveStoreListFreshFiltersByModifiedTime(t *testing.T) {
	var gotQuery url.Values
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		gotQuery = query
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	since := time.Date(2026, 5, 12, 23, 40, 1, 123000000, time.UTC)
	if _, err := store.ListFresh(context.Background(), "muxv4/session/down/client/run/", since); err != nil {
		t.Fatal(err)
	}
	query := gotQuery.Get("q")
	if !strings.Contains(query, "name contains 'muxv4/session/down/client/run/'") {
		t.Fatalf("q = %q, want name prefix filter", query)
	}
	if !strings.Contains(query, "modifiedTime >= '2026-05-12T23:40:01.123Z'") {
		t.Fatalf("q = %q, want modifiedTime lower bound", query)
	}
	if gotQuery.Get("orderBy") != "modifiedTime desc" {
		t.Fatalf("orderBy = %q, want modifiedTime desc", gotQuery.Get("orderBy"))
	}
}

func TestDriveStoreCleanupDeletesExpiredMuxObjects(t *testing.T) {
	var deleted []string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			query, err := url.ParseQuery(req.URL.RawQuery)
			if err != nil {
				t.Fatal(err)
			}
			if query.Get("orderBy") != "modifiedTime asc" {
				t.Fatalf("orderBy = %q, want modifiedTime asc", query.Get("orderBy"))
			}
			if query.Get("spaces") != "appDataFolder" {
				t.Fatalf("spaces = %q, want appDataFolder", query.Get("spaces"))
			}
			return stringResponse(http.StatusOK, `{
				"nextPageToken":"should-not-be-read",
				"files":[
					{"id":"old-id","name":"muxv4/abc/up/old","size":"10","modifiedTime":"2026-05-11T10:00:00Z"},
					{"id":"other-id","name":"other/abc/up/old","size":"20","modifiedTime":"2026-05-11T10:00:00Z"},
					{"id":"new-id","name":"muxv4/abc/up/new","size":"30","modifiedTime":"2026-05-11T12:30:00Z"}
				]
			}`), nil
		case http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(req.URL.Path, "/drive/v3/files/"))
			return stringResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected request method %s", req.Method)
		}
		return nil, nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	result, err := store.Cleanup(context.Background(), DriveCleanupOptions{
		Prefix:            "muxv4/abc/",
		OlderThan:         time.Hour,
		Now:               time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		DeleteConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 3 || result.Matched != 1 || result.Deleted != 1 || result.Failed != 0 || result.MatchedSize != 10 {
		t.Fatalf("cleanup result = %+v, want scanned=3 matched=1 deleted=1 size=10", result)
	}
	if len(deleted) != 1 || deleted[0] != "old-id" {
		t.Fatalf("deleted = %#v, want old-id only", deleted)
	}
}

func TestDriveStoreCleanupDryRunDoesNotDelete(t *testing.T) {
	deletes := 0
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deletes++
			return stringResponse(http.StatusNoContent, ""), nil
		}
		return stringResponse(http.StatusOK, `{
			"files":[{"id":"old-id","name":"muxv4/abc/down/old","size":"10","modifiedTime":"2026-05-11T10:00:00Z"}]
		}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	result, err := store.Cleanup(context.Background(), DriveCleanupOptions{
		Prefix:    "muxv4/abc/",
		OlderThan: time.Hour,
		Now:       time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		DryRun:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 1 || result.Deleted != 0 || deletes != 0 {
		t.Fatalf("result = %+v deletes=%d, want dry-run match without delete", result, deletes)
	}
}

func TestDriveQuotaStatsReportsEstimatedUnits(t *testing.T) {
	stats := newDriveQuotaStats(time.Minute)
	stats.since = time.Now().Add(-time.Second)
	stats.Record("upload", http.StatusOK, 10, 100*time.Millisecond, nil)
	stats.since = time.Now().Add(-time.Minute)
	report, ok := stats.Record("download", http.StatusTooManyRequests, 20, 250*time.Millisecond, nil)
	if !ok {
		t.Fatal("expected report")
	}
	if report.Calls != 2 || report.Units != 250 || report.Errors != 1 || report.ResponseBytes != 30 {
		t.Fatalf("report = %+v, want 2 calls, 250 units, 1 error, 30 bytes", report)
	}
	if report.Ops["upload"].Units != 50 || report.Ops["download"].Units != 200 {
		t.Fatalf("ops = %#v, want upload=50 and download=200 units", report.Ops)
	}
	snapshot := stats.Snapshot()
	if snapshot.Calls != 2 || snapshot.Units != 250 || snapshot.Errors != 1 || snapshot.ResponseBytes != 30 {
		t.Fatalf("snapshot = %+v, want lifetime totals after window reset", snapshot)
	}
	if snapshot.Ops["download"].P50DurationMS != 250 || snapshot.Ops["upload"].P95DurationMS != 100 {
		t.Fatalf("snapshot ops = %+v, want duration percentiles", snapshot.Ops)
	}
}

func TestDriveStoreSuppressesCanceledRequestObservabilityNoise(t *testing.T) {
	var logs bytes.Buffer
	store := &DriveStore{
		quota:  newDriveQuotaStats(time.Minute),
		Logger: log.New(&logs, "", 0),
	}
	store.logDriveRequest("download", 1, 0, nil, 250*time.Millisecond, context.Canceled)
	if logs.Len() != 0 {
		t.Fatalf("canceled request log = %q, want no log noise", logs.String())
	}
	if snapshot := store.quota.Snapshot(); snapshot.Calls != 0 || snapshot.Errors != 0 {
		t.Fatalf("quota snapshot = %+v, want canceled hedge attempt omitted", snapshot)
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
