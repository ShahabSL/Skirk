package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
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

func TestDriveStoreGenerateObjectIDsUsesAppDataSpace(t *testing.T) {
	var gotQuery url.Values
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		gotQuery = query
		return stringResponse(http.StatusOK, `{"ids":["id-1","id-2"]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	ids, err := store.GenerateObjectIDs(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "id-1" || ids[1] != "id-2" {
		t.Fatalf("ids = %#v, want id-1/id-2", ids)
	}
	if gotQuery.Get("count") != "2" || gotQuery.Get("space") != "appDataFolder" || gotQuery.Get("fields") != "ids" {
		t.Fatalf("query = %s, want count, appDataFolder space, and ids field", gotQuery.Encode())
	}
}

func TestDriveStorePutObjectWithIDIncludesGeneratedID(t *testing.T) {
	var metadata map[string]any
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || !strings.Contains(req.URL.RawQuery, "uploadType=multipart") {
			t.Fatalf("request = %s %s, want multipart upload", req.Method, req.URL.String())
		}
		mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		if mediaType != "multipart/related" {
			t.Fatalf("content type = %q, want multipart/related", mediaType)
		}
		reader := multipart.NewReader(req.Body, params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(part).Decode(&metadata); err != nil {
			t.Fatal(err)
		}
		return stringResponse(http.StatusOK, `{"id":"generated-id","name":"object","size":"4"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	info, err := store.PutObjectWithID(context.Background(), "generated-id", "object", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "generated-id" || info.Name != "object" || info.Size != 4 {
		t.Fatalf("info = %+v, want generated-id object size 4", info)
	}
	if metadata["id"] != "generated-id" || metadata["name"] != "object" || metadata["mimeType"] != "application/octet-stream" {
		t.Fatalf("metadata = %#v, want id/name/mimeType", metadata)
	}
	parents, ok := metadata["parents"].([]any)
	if !ok || len(parents) != 1 || parents[0] != "appDataFolder" {
		t.Fatalf("parents = %#v, want appDataFolder", metadata["parents"])
	}
}

func TestDriveStorePutObjectWithIDTreatsConflictAsIdempotent(t *testing.T) {
	var requests []string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
		if req.Method == http.MethodPost {
			return stringResponse(http.StatusConflict, `{"error":{"status":"ALREADY_EXISTS"}}`), nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/drive/v3/files/generated-id" {
			return stringResponse(http.StatusOK, `{"id":"generated-id","name":"object","size":"4","modifiedTime":"2026-05-14T00:00:00Z"}`), nil
		}
		return stringResponse(http.StatusNotFound, `{}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	info, err := store.PutObjectWithID(context.Background(), "generated-id", "object", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "generated-id" || info.Name != "object" || info.Size != 4 {
		t.Fatalf("info = %+v, want existing generated object", info)
	}
	if len(requests) != 2 || !strings.Contains(requests[1], "fields=id,name,size,modifiedTime") {
		t.Fatalf("requests = %#v, want conflict metadata lookup", requests)
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

func TestDriveStoreListFreshStatusReportsTruncatedPageLimit(t *testing.T) {
	requests := 0
	prefix := "muxv4/session/down/client/run/"
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		if requests == 1 && query.Get("pageToken") != "" {
			t.Fatalf("first pageToken = %q, want empty", query.Get("pageToken"))
		}
		if requests > 1 && query.Get("pageToken") == "" {
			t.Fatalf("request %d missing pageToken", requests)
		}
		body := fmt.Sprintf(`{"nextPageToken":"page-%d","files":[{"id":"id-%d","name":"%s%016x.f1.b1","size":"1","modifiedTime":"2026-05-12T23:40:01Z"}]}`, requests, requests, prefix, requests)
		return stringResponse(http.StatusOK, body), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	info, err := store.ListFreshStatus(context.Background(), prefix, time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if requests != driveListMaxPages {
		t.Fatalf("requests = %d, want page limit %d", requests, driveListMaxPages)
	}
	if !info.Truncated {
		t.Fatal("fresh list should report truncation when nextPageToken remains at page limit")
	}
	if len(info.Objects) != driveListMaxPages {
		t.Fatalf("objects = %d, want %d", len(info.Objects), driveListMaxPages)
	}
	if info.NextPageToken == "" {
		t.Fatal("fresh list should expose next page token when truncated")
	}
	if info.Pages != driveListMaxPages {
		t.Fatalf("pages = %d, want %d", info.Pages, driveListMaxPages)
	}
}

func TestDriveStoreListFreshStatusReportsIncompleteSearch(t *testing.T) {
	prefix := "muxv4/session/down/client/run/"
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(query.Get("fields"), "incompleteSearch") {
			t.Fatalf("fields = %q, want incompleteSearch", query.Get("fields"))
		}
		body := fmt.Sprintf(`{"incompleteSearch":true,"files":[{"id":"id-1","name":"%s%016x.f1.b1","size":"1","modifiedTime":"2026-05-12T23:40:01Z"}]}`, prefix, 1)
		return stringResponse(http.StatusOK, body), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	info, err := store.ListFreshStatus(context.Background(), prefix, time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Truncated {
		t.Fatal("fresh list should report truncation when Drive returns incompleteSearch")
	}
	if len(info.Objects) != 1 {
		t.Fatalf("objects = %d, want 1", len(info.Objects))
	}
	if !info.Incomplete {
		t.Fatal("fresh list should report incompleteSearch separately")
	}
}

func TestDriveStoreListFreshPageStatusUsesPageToken(t *testing.T) {
	prefix := "muxv4/session/down/client/run/"
	var gotPageToken string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		gotPageToken = query.Get("pageToken")
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	if _, err := store.ListFreshPageStatus(context.Background(), prefix, time.Date(2026, 5, 12, 23, 40, 0, 0, time.UTC), "next-page"); err != nil {
		t.Fatal(err)
	}
	if gotPageToken != "next-page" {
		t.Fatalf("pageToken = %q, want next-page", gotPageToken)
	}
}

func TestDriveStoreGetObjectRangeByIDValidatesContentRange(t *testing.T) {
	var gotRange string
	var gotEncoding string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotRange = req.Header.Get("Range")
		gotEncoding = req.Header.Get("Accept-Encoding")
		resp := stringResponse(http.StatusPartialContent, "hello")
		resp.Header.Set("Content-Range", "bytes 10-14/100")
		return resp, nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	data, info, err := store.GetObjectRangeByID(context.Background(), "file-id", 10, 14)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", string(data))
	}
	if info.Start != 10 || info.End != 14 || info.Total != 100 {
		t.Fatalf("range info = %+v, want 10-14/100", info)
	}
	if gotRange != "bytes=10-14" {
		t.Fatalf("Range = %q, want bytes=10-14", gotRange)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
}

func TestDriveStoreGetByIDUsesIdentityEncoding(t *testing.T) {
	var gotEncoding string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotEncoding = req.Header.Get("Accept-Encoding")
		return stringResponse(http.StatusOK, "hello"), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	data, err := store.GetByID(context.Background(), "file-id")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", string(data))
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
}

func TestDriveStoreGetUsesIdentityEncoding(t *testing.T) {
	var gotEncoding string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/drive/v3/files" {
			return stringResponse(http.StatusOK, `{"files":[{"id":"file-id","name":"object","size":"5","modifiedTime":"2026-05-14T00:00:00Z"}]}`), nil
		}
		gotEncoding = req.Header.Get("Accept-Encoding")
		return stringResponse(http.StatusOK, "hello"), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	data, err := store.Get(context.Background(), "object")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", string(data))
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
}

func TestDriveStoreGetObjectRangeByIDRejectsWrongContentRange(t *testing.T) {
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := stringResponse(http.StatusPartialContent, "hello")
		resp.Header.Set("Content-Range", "bytes 11-15/100")
		return resp, nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	_, _, err := store.GetObjectRangeByID(context.Background(), "file-id", 10, 14)
	if err == nil || !strings.Contains(err.Error(), "range mismatch") {
		t.Fatalf("error = %v, want range mismatch", err)
	}
}

func TestDriveStoreChangesStartPageToken(t *testing.T) {
	var gotPath string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path + "?" + req.URL.RawQuery
		return stringResponse(http.StatusOK, `{"startPageToken":"token-1"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	token, err := store.ChangesStartPageToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "token-1" {
		t.Fatalf("token = %q, want token-1", token)
	}
	if gotPath != "/drive/v3/changes/startPageToken?fields=startPageToken" {
		t.Fatalf("path = %q, want changes start token path", gotPath)
	}
}

func TestDriveStoreListChangesUsesAppDataAndFields(t *testing.T) {
	var gotQuery url.Values
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		gotQuery = query
		return stringResponse(http.StatusOK, `{
			"nextPageToken":"next-token",
			"changes":[
				{"id":"1","fileId":"file-1","time":"2026-05-13T12:00:00Z","file":{"id":"file-1","name":"mux/control","size":"42","modifiedTime":"2026-05-13T12:00:01Z"}},
				{"id":"2","fileId":"file-2","removed":true}
			]
		}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	info, err := store.ListChanges(context.Background(), "start-token", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("spaces") != "appDataFolder" {
		t.Fatalf("spaces = %q, want appDataFolder", gotQuery.Get("spaces"))
	}
	if gotQuery.Get("pageToken") != "start-token" || gotQuery.Get("includeRemoved") != "true" {
		t.Fatalf("query = %s, want page token and includeRemoved", gotQuery.Encode())
	}
	if !strings.Contains(gotQuery.Get("fields"), "newStartPageToken") || !strings.Contains(gotQuery.Get("fields"), "file(id,name,size,modifiedTime)") {
		t.Fatalf("fields = %q, want compact change fields", gotQuery.Get("fields"))
	}
	if strings.Contains(gotQuery.Get("fields"), "changes(id") {
		t.Fatalf("fields = %q, must not request unsupported top-level change id", gotQuery.Get("fields"))
	}
	if info.NextPageToken != "next-token" || len(info.Changes) != 2 {
		t.Fatalf("changes = %+v, want next token and two changes", info)
	}
	if info.Changes[0].FileID != "file-1" || info.Changes[0].Name != "mux/control" || info.Changes[0].Size != 42 || info.Changes[0].Updated != "2026-05-13T12:00:01Z" {
		t.Fatalf("first change = %+v, want file metadata", info.Changes[0])
	}
	if !info.Changes[1].Removed {
		t.Fatalf("second change = %+v, want removed", info.Changes[1])
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
	if driveQuotaUnits("changes") != 100 {
		t.Fatalf("changes quota units = %d, want 100", driveQuotaUnits("changes"))
	}
	if driveQuotaUnits("generate_ids") != 50 {
		t.Fatalf("generate_ids quota units = %d, want 50", driveQuotaUnits("generate_ids"))
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
