package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDriveInlinePriorityPreservesMediaAndReadsDiscovery(t *testing.T) {
	payload := []byte("sealed-test-object")
	name := "muxv4/test/p0/object"
	description := ""
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			reader := multipart.NewReader(req.Body, params["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err = json.NewDecoder(part).Decode(&metadata); err != nil {
				t.Fatal(err)
			}
			description, _ = metadata["description"].(string)
			part, err = reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			media, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(media, payload) {
				t.Fatal("inline prototype changed backwards-compatible media body")
			}
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"test-file","name":%q,"size":%q}`, name, strconv.Itoa(len(payload)))), nil
		}
		if !strings.Contains(req.URL.Query().Get("fields"), "description") {
			t.Fatal("inline reader omitted description fields")
		}
		body, _ := json.Marshal(map[string]any{"files": []map[string]any{{"id": "test-file", "name": name, "size": strconv.Itoa(len(payload)), "description": description}}})
		return stringResponse(http.StatusOK, string(body)), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{InlineSmallObjects: true})
	if _, err := store.PutObject(context.Background(), name, payload); err != nil {
		t.Fatal(err)
	}
	if description != driveInlineMarker+base64.StdEncoding.EncodeToString(payload) {
		t.Fatal("missing sealed metadata copy")
	}
	listed, err := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/", "/p0/"}, time.Time{}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Objects) != 1 || !bytes.Equal(listed.Objects[0].InlineData, payload) {
		t.Fatalf("inline discovery = %+v", listed)
	}
}

func TestDriveInlineDataBoundsAndOptIn(t *testing.T) {
	store := NewDriveStore(nil, "test-token", DriveConfig{InlineSmallObjects: true})
	valid := driveInlineMarker + base64.StdEncoding.EncodeToString([]byte("abc"))
	for _, sample := range []struct {
		name, description string
		size              int64
	}{
		{"muxv4/test/p1/object", valid, 3},
		{"unrelated/p0/object", valid, 3},
		{"muxv4/test/p0/object", valid, 4},
		{"muxv4/test/p0/object", driveInlineMarker + "bad!", 3},
		{"muxv4/test/p0/object", driveInlineMarker + strings.Repeat("A", 20000), 3},
		{"muxv4/test/p0/object", valid, driveInlineMaxBytes + 1},
	} {
		if got := store.decodeInlineData(sample.name, sample.description, sample.size); got != nil {
			t.Fatal("accepted invalid inline metadata")
		}
	}
	store.inlineSmallObjects = false
	if got := store.decodeInlineData("muxv4/test/p0/object", valid, 3); got != nil {
		t.Fatal("inline metadata enabled by default")
	}
}

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
	if snapshot := store.QuotaSnapshot(); snapshot.Calls != 2 || snapshot.Units != 100 || snapshot.Errors != 1 {
		t.Fatalf("quota after 401 refresh = %+v, want both upload attempts and one failed attempt", snapshot)
	}
}

func TestDriveQuotaCountsRecoveredRetries(t *testing.T) {
	stats := newDriveQuotaStats(time.Minute)
	stats.Record("list", http.StatusOK, 10, time.Second, nil, "", 3)
	got := stats.Snapshot()
	if got.Calls != 3 || got.Units != 300 || got.Errors != 2 || got.LastErrorReason != "retry_recovered" {
		t.Fatalf("recovered retry accounting = %+v", got)
	}
}

func TestDriveQuotaProjectHeaderStaysOnDriveRequests(t *testing.T) {
	tokenHeader := make(chan string, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHeader <- r.Header.Get("X-Goog-User-Project")
		_, _ = w.Write([]byte(`{"access_token":"access-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenServer.Close()

	var driveHeader string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		driveHeader = req.Header.Get("X-Goog-User-Project")
		return stringResponse(http.StatusOK, `{}`), nil
	})}}
	tokenSource := NewAccessTokenSource(AuthConfig{
		ClientID: "client-id", RefreshToken: "refresh-token", TokenURL: tokenServer.URL,
	}, RouteConfig{Mode: "direct"})
	store := NewDriveStoreWithTokenSource(httpClient, tokenSource, DriveConfig{QuotaProject: "flowing-precept-514715-p1"})
	if _, err := store.request(context.Background(), http.MethodGet, "/drive/v3/files", map[string]string{"x-goog-user-project": "different-project"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-tokenHeader; got != "" {
		t.Fatalf("OAuth token request unexpectedly carried quota project %q", got)
	}
	if driveHeader != "flowing-precept-514715-p1" {
		t.Fatalf("Drive quota project header = %q", driveHeader)
	}

	defaultStore := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "access-token"}, RouteConfig{Mode: "direct"}), DriveConfig{})
	if _, err := defaultStore.request(context.Background(), http.MethodGet, "/drive/v3/files", nil, nil); err != nil {
		t.Fatal(err)
	}
	if driveHeader != "" {
		t.Fatalf("default Drive request unexpectedly carried quota project %q", driveHeader)
	}
}

func TestDriveRejectsInvalidQuotaProjectWithoutFallback(t *testing.T) {
	var requests int
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return stringResponse(http.StatusOK, `{}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "access-token"}, RouteConfig{Mode: "direct"}), DriveConfig{QuotaProject: "invalid_project"})
	if _, err := store.request(context.Background(), http.MethodGet, "/drive/v3/files", nil, nil); err == nil {
		t.Fatal("expected invalid quota project error")
	}
	if requests != 0 {
		t.Fatalf("Drive requests = %d; invalid quota project must not fall back", requests)
	}
}

func TestDriveStoreDoesNotHTTPRetryRateLimit(t *testing.T) {
	var requests int32
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		return stringResponse(http.StatusForbidden, `{"error":{"errors":[{"reason":"rateLimitExceeded"}],"message":"Rate Limit Exceeded"}}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})

	_, err := store.PutObject(context.Background(), "object", []byte("data"))
	if err == nil {
		t.Fatal("expected Drive rate-limit error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one Drive HTTP attempt before returning quota error", requests)
	}
}

func TestDriveStoreRateLimitBackoffDelaysNextRequest(t *testing.T) {
	var requests int32
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&requests, 1) == 1 {
			return stringResponse(http.StatusForbidden, `{"error":{"code":403,"message":"Rate Limit Exceeded","status":"RATE_LIMIT_EXCEEDED"}}`), nil
		}
		return stringResponse(http.StatusOK, `{"id":"file-id","name":"object","size":"4"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	store.backoff.base = 30 * time.Millisecond
	store.backoff.max = 30 * time.Millisecond
	store.backoff.jitter = 0

	_, err := store.PutObject(context.Background(), "object", []byte("data"))
	if err == nil {
		t.Fatal("expected first request to trip Drive quota backoff")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.PutObject(ctx, "object", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 25*time.Millisecond {
		t.Fatalf("second request waited %s, want quota backoff delay", elapsed)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want one failed request and one delayed success", requests)
	}
}

func TestDriveStoreQuotaBackoffSnapshotReportsCooldown(t *testing.T) {
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return stringResponse(http.StatusForbidden, `{"error":{"code":403,"message":"Rate Limit Exceeded","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	store.backoff.base = time.Minute
	store.backoff.max = time.Minute
	store.backoff.jitter = 0

	_, err := store.PutObject(context.Background(), "object", []byte("data"))
	if err == nil {
		t.Fatal("expected Drive rate-limit error")
	}
	snapshot := store.QuotaBackoffSnapshot()
	if !snapshot.Active || snapshot.Reason != "RATE_LIMIT_EXCEEDED" || snapshot.Op != "upload" || snapshot.Failures != 1 || snapshot.WaitSeconds <= 0 {
		t.Fatalf("backoff snapshot = %+v, want active upload cooldown", snapshot)
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

func TestDriveStoreEnsureFolderUsesExistingVisibleFolder(t *testing.T) {
	requests := 0
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files" {
			t.Fatalf("request = %s %s, want Drive files lookup", req.Method, req.URL.String())
		}
		query, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		q := query.Get("q")
		for _, want := range []string{
			"trashed = false",
			"mimeType = 'application/vnd.google-apps.folder'",
			"name = 'skirk-mailbox-test'",
		} {
			if !strings.Contains(q, want) {
				t.Fatalf("folder lookup query = %q, missing %q", q, want)
			}
		}
		return stringResponse(http.StatusOK, `{"files":[{"id":"folder-id","name":"skirk-mailbox-test"}]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{})

	info, err := store.EnsureFolder(context.Background(), "skirk-mailbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "folder-id" || info.Name != "skirk-mailbox-test" {
		t.Fatalf("info = %+v, want existing folder", info)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want lookup only", requests)
	}
}

func TestDriveStoreEnsureFolderCreatesVisibleFolder(t *testing.T) {
	requests := 0
	var metadata map[string]string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files" {
				t.Fatalf("request = %s %s, want Drive files lookup", req.Method, req.URL.String())
			}
			return stringResponse(http.StatusOK, `{"files":[]}`), nil
		case 2:
			if req.Method != http.MethodPost || req.URL.Path != "/drive/v3/files" {
				t.Fatalf("request = %s %s, want Drive folder create", req.Method, req.URL.String())
			}
			if got := req.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("content type = %q, want JSON", got)
			}
			if err := json.NewDecoder(req.Body).Decode(&metadata); err != nil {
				t.Fatal(err)
			}
			return stringResponse(http.StatusOK, `{"id":"created-folder","name":"skirk-mailbox-test"}`), nil
		default:
			t.Fatalf("unexpected request %d: %s %s", requests, req.Method, req.URL.String())
			return nil, nil
		}
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{})

	info, err := store.EnsureFolder(context.Background(), "skirk-mailbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "created-folder" || info.Name != "skirk-mailbox-test" {
		t.Fatalf("info = %+v, want created folder", info)
	}
	if metadata["name"] != "skirk-mailbox-test" || metadata["mimeType"] != "application/vnd.google-apps.folder" {
		t.Fatalf("metadata = %#v, want Drive folder metadata", metadata)
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
	digest := sha256.Sum256([]byte("data"))
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
		if req.Method == http.MethodPost {
			return stringResponse(http.StatusConflict, `{"error":{"status":"ALREADY_EXISTS"}}`), nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/drive/v3/files/generated-id" {
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"generated-id","name":"object","size":"4","sha256Checksum":%q}`, hex.EncodeToString(digest[:]))), nil
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
	if len(requests) != 2 || !strings.Contains(requests[1], "sha256Checksum") {
		t.Fatalf("requests = %#v, want exact conflict checksum lookup", requests)
	}
}

func TestDriveUploadHedgeOnlyEligibleNormalGeneratedID(t *testing.T) {
	for _, tc := range []struct {
		name, fileID string
		size         int
		delayMS      int
		fast         bool
	}{
		{"muxv4/session/p1/normal", "generated-id", driveUploadHedgeMinBytes, 3000, true},
		{"muxv4/session/p1/normal", "generated-id", driveUploadHedgeMinBytes, 0, false},
		{"muxv4/session/p0/urgent", "generated-id", driveUploadHedgeMinBytes, 3000, false},
		{"muxv4/session/p1/small", "generated-id", driveUploadHedgeMinBytes - 1, 3000, false},
		{"muxv4/session/p1/idless", "", driveUploadHedgeMinBytes, 3000, false},
	} {
		t.Run(tc.name+"/delay="+strconv.Itoa(tc.delayMS), func(t *testing.T) {
			var hedgeCalls atomic.Int32
			primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if !tc.fast {
					select {
					case <-time.After(30 * time.Millisecond):
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"generated-id","name":%q,"size":%q}`, tc.name, strconv.Itoa(tc.size))), nil
			})}}
			store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: tc.delayMS})
			store.uploadHedgeDelay = 5 * time.Millisecond
			if tc.delayMS == 0 {
				store.uploadHedgeDelay = 0
			}
			store.newHedgeHTTPClient = func() *GoogleHTTPClient {
				hedgeCalls.Add(1)
				return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"generated-id","name":%q,"size":%q}`, tc.name, strconv.Itoa(tc.size))), nil
				})}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			if tc.fileID == "" {
				_, err = store.PutObject(ctx, tc.name, make([]byte, tc.size))
			} else {
				_, err = store.PutObjectWithID(ctx, tc.fileID, tc.name, make([]byte, tc.size))
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := hedgeCalls.Load(); got != 0 {
				t.Fatalf("extra hedge calls = %d, want zero", got)
			}
			if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 1 {
				t.Fatalf("quota calls = %+v, want one upload", snapshot.Ops["upload"])
			}
		})
	}
}

func TestDriveSeparateUploadPoolRoutesOnlyLargeNormalGeneratedID(t *testing.T) {
	var ordinaryUploads, bulkUploads, controlCalls atomic.Int32
	var capturedMu sync.Mutex
	var captured []capturedDriveUpload
	respond := func(req *http.Request, counter *atomic.Int32) (*http.Response, error) {
		if req.Method == http.MethodGet {
			controlCalls.Add(1)
			return stringResponse(http.StatusOK, `{"files":[]}`), nil
		}
		upload, err := captureDriveUpload(req)
		if err != nil {
			return nil, err
		}
		counter.Add(1)
		capturedMu.Lock()
		captured = append(captured, upload)
		capturedMu.Unlock()
		id := upload.id
		if id == "" {
			id = "created-id"
		}
		return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, id, upload.name, strconv.Itoa(len(upload.data)))), nil
	}
	route := RouteConfig{Mode: "direct", TimeoutSeconds: 240}
	ordinary := &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return respond(req, &ordinaryUploads)
	})}}
	store := NewDriveStoreWithTokenSource(ordinary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, route), DriveConfig{QuotaProject: "flowing-precept-514715-p1", SeparateUploadPool: true})
	if store.normalUploadHTTP == nil || store.normalUploadHTTP.route != route {
		t.Fatal("separate normal upload pool omitted or changed the configured route")
	}
	store.normalUploadHTTP = &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return respond(req, &bulkUploads)
	})}}
	ctx := context.Background()
	if _, err := store.PutObjectWithID(ctx, "normal-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObjectWithID(ctx, "priority-id", "muxv4/session/p0/priority", make([]byte, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObjectWithID(ctx, "small-id", "muxv4/session/p1/small", make([]byte, driveUploadHedgeMinBytes-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObject(ctx, "muxv4/session/p1/idless", make([]byte, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "muxv4/session/"); err != nil {
		t.Fatal(err)
	}
	if ordinaryUploads.Load() != 3 || bulkUploads.Load() != 1 || controlCalls.Load() != 1 {
		t.Fatalf("pool routing ordinary=%d bulk=%d control=%d", ordinaryUploads.Load(), bulkUploads.Load(), controlCalls.Load())
	}
	capturedMu.Lock()
	defer capturedMu.Unlock()
	for _, upload := range captured {
		if upload.authorization != "Bearer token" || upload.quotaProject != "flowing-precept-514715-p1" {
			t.Fatalf("separate pool lost auth or quota project: auth=%q quota=%q", upload.authorization, upload.quotaProject)
		}
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 4 || snapshot.Ops["upload"].Units != 200 || snapshot.Ops["list"].Calls != 1 {
		t.Fatalf("shared quota accounting = %+v", snapshot.Ops)
	}
}

func TestDriveUploadHedgeSkipsDuringSharedQuotaBackoff(t *testing.T) {
	var hedgeCalls atomic.Int32
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		hedgeCalls.Add(1)
		return nil
	}
	store.backoff.mu.Lock()
	store.backoff.until = time.Now().Add(250 * time.Millisecond)
	store.backoff.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	if hedgeCalls.Load() != 0 {
		t.Fatalf("launched %d hedges during quota backoff", hedgeCalls.Load())
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 1 {
		t.Fatalf("quota calls = %+v, want one upload after backoff", snapshot.Ops["upload"])
	}
}

func TestDriveUploadHedgeUsesSeparatePrimaryPool(t *testing.T) {
	var ordinaryUploads, bulkUploads, hedgeUploads atomic.Int32
	bulkCanceled := make(chan struct{})
	route := RouteConfig{Mode: "direct", TimeoutSeconds: 240}
	ordinary := &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		ordinaryUploads.Add(1)
		return nil, fmt.Errorf("ordinary pool received normal upload")
	})}}
	store := NewDriveStoreWithTokenSource(ordinary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, route), DriveConfig{SeparateUploadPool: true, UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.normalUploadHTTP = &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		bulkUploads.Add(1)
		<-req.Context().Done()
		close(bulkCanceled)
		return nil, req.Context().Err()
	})}}
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hedgeUploads.Add(1)
			if req.Header.Get("Authorization") != "Bearer token" {
				return nil, fmt.Errorf("hedge lost bearer token")
			}
			return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
		})}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bulkCanceled:
	default:
		t.Fatal("returned before canceling separate-pool primary")
	}
	if ordinaryUploads.Load() != 0 || bulkUploads.Load() != 1 || hedgeUploads.Load() != 1 {
		t.Fatalf("pool calls ordinary=%d bulk=%d hedge=%d", ordinaryUploads.Load(), bulkUploads.Load(), hedgeUploads.Load())
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 {
		t.Fatalf("shared quota calls = %+v, want both pools", snapshot.Ops["upload"])
	}
}

func TestDriveUploadHedgeSecondaryWinsSameObjectAndSharesQuota(t *testing.T) {
	const name = "muxv4/session/p1/normal"
	const id = "generated-id"
	data := bytes.Repeat([]byte("z"), driveUploadHedgeMinBytes)
	primaryCanceled := make(chan struct{})
	var capturedMu sync.Mutex
	var captured []capturedDriveUpload
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upload, err := captureDriveUpload(req)
		if err != nil {
			return nil, err
		}
		capturedMu.Lock()
		captured = append(captured, upload)
		capturedMu.Unlock()
		<-req.Context().Done()
		close(primaryCanceled)
		return nil, req.Context().Err()
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{QuotaProject: "flowing-precept-514715-p1", UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			upload, err := captureDriveUpload(req)
			if err != nil {
				return nil, err
			}
			capturedMu.Lock()
			captured = append(captured, upload)
			capturedMu.Unlock()
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, id, name, strconv.Itoa(len(data)))), nil
		})}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := store.PutObjectWithID(ctx, id, name, data)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != id || info.Name != name || info.Size != int64(len(data)) {
		t.Fatalf("upload info = %+v", info)
	}
	select {
	case <-primaryCanceled:
	default:
		t.Fatal("returned before canceled primary finished")
	}
	if len(store.uploadHedgeSlots) != 0 {
		t.Fatal("hedge slot not released")
	}
	capturedMu.Lock()
	defer capturedMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("captured %d uploads, want two", len(captured))
	}
	for _, upload := range captured {
		if upload.id != id || upload.name != name || !bytes.Equal(upload.data, data) || upload.authorization != "Bearer token" || upload.quotaProject != "flowing-precept-514715-p1" {
			t.Fatalf("hedge changed ID, name, media, or auth: id=%q name=%q bytes=%d auth=%q quota=%q", upload.id, upload.name, len(upload.data), upload.authorization, upload.quotaProject)
		}
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 || snapshot.Ops["upload"].Units != 100 {
		t.Fatalf("shared quota = %+v, want two uploads/100 units", snapshot.Ops["upload"])
	}
}

func TestDriveUploadHedgeWaitsAfterPrimaryFailure(t *testing.T) {
	for _, secondaryStatus := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(strconv.Itoa(secondaryStatus), func(t *testing.T) {
			hedgeStarted := make(chan struct{})
			primaryFailed := make(chan struct{})
			primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-hedgeStarted
				close(primaryFailed)
				return stringResponse(http.StatusBadRequest, `{"error":"primary failure"}`), nil
			})}}
			store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
			store.uploadHedgeDelay = 5 * time.Millisecond
			store.newHedgeHTTPClient = func() *GoogleHTTPClient {
				return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					close(hedgeStarted)
					<-primaryFailed
					if secondaryStatus == http.StatusOK {
						return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
					}
					return stringResponse(http.StatusBadRequest, `{"error":"secondary failure"}`), nil
				})}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			info, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes))
			if secondaryStatus == http.StatusOK {
				if err != nil || info.ID != "generated-id" {
					t.Fatalf("secondary success = %+v, %v", info, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "primary failure") {
				t.Fatalf("both failed, got info=%+v err=%v; want primary failure", info, err)
			}
			if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 {
				t.Fatalf("quota calls = %+v, want both legs", snapshot.Ops["upload"])
			}
		})
	}
}

func TestDriveUploadHedgeWaitsAfterSecondaryFailure(t *testing.T) {
	secondaryReturned := make(chan struct{})
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-secondaryReturned
		// Keep the primary pending after the secondary has returned its error.
		time.Sleep(20 * time.Millisecond)
		return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(secondaryReturned)
			return stringResponse(http.StatusBadRequest, `{"error":"secondary failure"}`), nil
		})}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes))
	if err != nil || info.ID != "generated-id" {
		t.Fatalf("primary success after secondary failure = %+v, %v", info, err)
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 {
		t.Fatalf("quota calls = %+v, want both legs", snapshot.Ops["upload"])
	}
}

func TestDriveUploadHedgeConflictLookup404WaitsForOtherLeg(t *testing.T) {
	hedgeStarted := make(chan struct{})
	lookupFailed := make(chan struct{})
	var lookupOnce sync.Once
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			lookupOnce.Do(func() { close(lookupFailed) })
			return stringResponse(http.StatusNotFound, `{}`), nil
		}
		<-hedgeStarted
		return stringResponse(http.StatusConflict, `{"error":"already exists"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(hedgeStarted)
			<-lookupFailed
			return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
		})}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes))
	if err != nil || info.ID != "generated-id" {
		t.Fatalf("409/404 race masked secondary success: info=%+v err=%v", info, err)
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 || snapshot.Ops["upload_commit_probe"].Calls != 1 {
		t.Fatalf("quota calls = %+v, want two uploads and one canceled-leg checksum probe", snapshot.Ops)
	}
}

func TestDriveUploadHedgeCallerCancellationJoinsBothLegs(t *testing.T) {
	primaryDone := make(chan struct{})
	secondaryStarted := make(chan struct{})
	secondaryDone := make(chan struct{})
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		close(primaryDone)
		return nil, req.Context().Err()
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(secondaryStarted)
			<-req.Context().Done()
			close(secondaryDone)
			return nil, req.Context().Err()
		})}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := store.PutObjectWithID(ctx, "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes))
		finished <- err
	}()
	select {
	case <-secondaryStarted:
	case <-ctx.Done():
		t.Fatal("secondary did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled upload succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled upload did not finish")
	}
	select {
	case <-primaryDone:
	default:
		t.Fatal("returned before primary finished")
	}
	select {
	case <-secondaryDone:
	default:
		t.Fatal("returned before secondary finished")
	}
	if len(store.uploadHedgeSlots) != 0 {
		t.Fatal("hedge slot leaked after cancellation")
	}
	if snapshot := store.QuotaSnapshot(); snapshot.Ops["upload"].Calls != 2 {
		t.Fatalf("quota calls = %+v, want two canceled HTTP attempts", snapshot.Ops["upload"])
	}
}

func TestDriveUploadHedgeSkipsWhenAllExtraSlotsBusy(t *testing.T) {
	primaryStarted := make(chan struct{})
	releasePrimary := make(chan struct{})
	primary := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(primaryStarted)
		<-releasePrimary
		return stringResponse(http.StatusOK, `{"id":"generated-id","name":"muxv4/session/p1/normal","size":"131072"}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{UploadHedgeDelayMS: 1000})
	store.uploadHedgeDelay = 5 * time.Millisecond
	for i := 0; i < driveUploadHedgeMaxConcurrent; i++ {
		store.uploadHedgeSlots <- struct{}{}
	}
	var hedgeCalls atomic.Int32
	store.newHedgeHTTPClient = func() *GoogleHTTPClient {
		hedgeCalls.Add(1)
		return nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := store.PutObjectWithID(context.Background(), "generated-id", "muxv4/session/p1/normal", make([]byte, driveUploadHedgeMinBytes))
		finished <- err
	}()
	<-primaryStarted
	time.Sleep(30 * time.Millisecond)
	close(releasePrimary)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if hedgeCalls.Load() != 0 || len(store.uploadHedgeSlots) != driveUploadHedgeMaxConcurrent {
		t.Fatalf("busy hedge slots were not respected: calls=%d slots=%d", hedgeCalls.Load(), len(store.uploadHedgeSlots))
	}
}

type capturedDriveUpload struct {
	id, name, authorization, quotaProject string
	data                                  []byte
}

func captureDriveUpload(req *http.Request) (capturedDriveUpload, error) {
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" {
		return capturedDriveUpload{}, fmt.Errorf("upload media type %q: %v", mediaType, err)
	}
	reader := multipart.NewReader(req.Body, params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		return capturedDriveUpload{}, err
	}
	var metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(part).Decode(&metadata); err != nil {
		return capturedDriveUpload{}, err
	}
	part, err = reader.NextPart()
	if err != nil {
		return capturedDriveUpload{}, err
	}
	media, err := io.ReadAll(part)
	if err != nil {
		return capturedDriveUpload{}, err
	}
	return capturedDriveUpload{id: metadata.ID, name: metadata.Name, data: media, authorization: req.Header.Get("Authorization"), quotaProject: req.Header.Get("X-Goog-User-Project")}, nil
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

func TestNormalMuxListUsesIndependentPageSize(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured int
		contains   []string
		want       string
	}{
		{"normal-default", 0, []string{"muxv4/session/down/client/run/", "/p1/"}, "1000"},
		{"normal-500", 500, []string{"muxv4/session/down/client/run/", "/p1/"}, "500"},
		{"normal-100", 100, []string{"muxv4/session/down/client/run/", "/p1/"}, "100"},
		{"priority-stays-100", 0, []string{"muxv4/session/down/client/run/", "/p0/"}, "100"},
		{"targeted-p1-stays-100", 0, []string{"muxv4/session/down/client/run/", "client/run", "stream", "/p1/"}, "100"},
		{"unrelated-p1-stays-100", 0, []string{"control/session/", "/p1/"}, "100"},
		{"invalid-direct-constructor-fails-safe", 2000, []string{"muxv4/session/down/client/run/", "/p1/"}, "100"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got url.Values
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.URL.Query()
				return stringResponse(http.StatusOK, `{"files":[]}`), nil
			})}}
			store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{NormalListPageSize: test.configured})
			if _, err := store.ListFreshContainsPageStatus(context.Background(), test.contains, time.Time{}, "", 4); err != nil {
				t.Fatal(err)
			}
			if got.Get("pageSize") != test.want {
				t.Fatalf("pageSize = %q, want %q", got.Get("pageSize"), test.want)
			}
			if got.Get("orderBy") != "modifiedTime desc" || !strings.Contains(got.Get("q"), "name contains '") {
				t.Fatalf("query options changed: %v", got)
			}
		})
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

func TestDriveStoreCleanupAllDeletesEveryMailboxObject(t *testing.T) {
	var deleted []string
	var queries []string
	var mu sync.Mutex
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			query, err := url.ParseQuery(req.URL.RawQuery)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			queries = append(queries, query.Get("q"))
			mu.Unlock()
			return stringResponse(http.StatusOK, `{
				"files":[
					{"id":"mux-id","name":"muxv4/abc/down/old","size":"10","modifiedTime":"2026-05-11T10:00:00Z"},
					{"id":"bench-id","name":"bench-drive/sample.bin","size":"20","modifiedTime":"2026-05-11T10:01:00Z"},
					{"id":"setup-id","name":"setup/session/marker.json","size":"30","modifiedTime":"2026-05-11T10:02:00Z"}
				]
			}`), nil
		case http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(req.URL.Path, "/drive/v3/files/"))
			mu.Unlock()
			return stringResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected request method %s", req.Method)
		}
		return nil, nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	result, err := store.Cleanup(context.Background(), DriveCleanupOptions{
		All:               true,
		OlderThan:         0,
		Now:               time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		DeleteConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Prefix != "<all>" || result.Scanned != 3 || result.Matched != 3 || result.Deleted != 3 || result.Failed != 0 || result.MatchedSize != 60 {
		t.Fatalf("cleanup all result = %+v, want every object deleted", result)
	}
	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "bench-id,mux-id,setup-id" {
		t.Fatalf("deleted = %#v, want all object ids", deleted)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "trashed = false") || strings.Contains(queries[0], "name contains") {
		t.Fatalf("cleanup all query = %#v, want all non-trashed objects", queries)
	}
}

func TestDriveStoreCleanupReportsTruncatedList(t *testing.T) {
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return stringResponse(http.StatusOK, `{
			"nextPageToken":"next",
			"files":[{"id":"old-id","name":"muxv4/abc/down/old","size":"10","modifiedTime":"2026-05-11T10:00:00Z"}]
		}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	result, err := store.Cleanup(context.Background(), DriveCleanupOptions{
		Prefix:    "muxv4/",
		OlderThan: time.Hour,
		Now:       time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		DryRun:    true,
		MaxPages:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.Pages != 1 || result.Matched != 1 {
		t.Fatalf("cleanup result = %+v, want truncated single-page match", result)
	}
}

func TestDriveStoreCleanupHonorsMaxDeletes(t *testing.T) {
	var deleted []string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			return stringResponse(http.StatusOK, `{
				"files":[
					{"id":"old-1","name":"muxv4/abc/down/old-1","size":"10","modifiedTime":"2026-05-11T10:00:00Z"},
					{"id":"old-2","name":"muxv4/abc/down/old-2","size":"20","modifiedTime":"2026-05-11T10:01:00Z"},
					{"id":"old-3","name":"muxv4/abc/down/old-3","size":"30","modifiedTime":"2026-05-11T10:02:00Z"}
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
		Prefix:            "muxv4/",
		OlderThan:         time.Hour,
		Now:               time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		DeleteConcurrency: 1,
		MaxDeletes:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 || !result.Truncated {
		t.Fatalf("cleanup result = %+v, want two deletes and truncated=true", result)
	}
	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "old-1,old-2" {
		t.Fatalf("deleted = %#v, want first two candidates only", deleted)
	}
}

func TestDriveQuotaStatsReportsEstimatedUnits(t *testing.T) {
	stats := newDriveQuotaStats(time.Minute)
	stats.since = time.Now().Add(-time.Second)
	stats.Record("upload", http.StatusOK, 10, 100*time.Millisecond, nil, "")
	stats.since = time.Now().Add(-time.Minute)
	report, ok := stats.Record("download", http.StatusTooManyRequests, 20, 250*time.Millisecond, nil, "rateLimitExceeded")
	if !ok {
		t.Fatal("expected report")
	}
	if report.Calls != 2 || report.Units != 250 || report.Errors != 1 || report.ResponseBytes != 30 {
		t.Fatalf("report = %+v, want 2 calls, 250 units, 1 error, 30 bytes", report)
	}
	if report.LastErrorReason != "rateLimitExceeded" {
		t.Fatalf("last error reason = %q, want rateLimitExceeded", report.LastErrorReason)
	}
	if report.Ops["upload"].Units != 50 || report.Ops["download"].Units != 200 {
		t.Fatalf("ops = %#v, want upload=50 and download=200 units", report.Ops)
	}
	if driveQuotaUnits("changes") != 100 {
		t.Fatalf("changes quota units = %d, want 100", driveQuotaUnits("changes"))
	}
	if driveQuotaUnits("generate_ids") != 5 {
		t.Fatalf("generate_ids quota units = %d, want 5", driveQuotaUnits("generate_ids"))
	}
	if driveQuotaUnits("patch") != 50 || driveQuotaUnits("update") != 50 {
		t.Fatal("Drive file edits must count 50 quota units")
	}
	snapshot := stats.Snapshot()
	if snapshot.Calls != 2 || snapshot.Units != 250 || snapshot.Errors != 1 || snapshot.ResponseBytes != 30 {
		t.Fatalf("snapshot = %+v, want lifetime totals after window reset", snapshot)
	}
	if snapshot.LastErrorReason != "rateLimitExceeded" {
		t.Fatalf("snapshot last error reason = %q, want rateLimitExceeded", snapshot.LastErrorReason)
	}
	if snapshot.Ops["download"].P50DurationMS != 250 || snapshot.Ops["upload"].P95DurationMS != 100 {
		t.Fatalf("snapshot ops = %+v, want duration percentiles", snapshot.Ops)
	}
}

func TestDriveQuotaSnapshotDeltaOmitsIdleOps(t *testing.T) {
	before := DriveQuotaSnapshot{
		Calls: 1,
		Units: 100,
		Ops: map[string]DriveQuotaOpSnapshot{
			"list": {Calls: 1, Units: 100},
		},
	}
	after := DriveQuotaSnapshot{
		Calls: 2,
		Units: 200,
		Ops: map[string]DriveQuotaOpSnapshot{
			"list":     {Calls: 2, Units: 200},
			"download": {Calls: 0, Units: 0},
		},
	}
	delta := after.Delta(before)
	if _, ok := delta.Ops["download"]; ok {
		t.Fatalf("delta included idle download op: %+v", delta.Ops["download"])
	}
	if got := delta.Ops["list"].Calls; got != 1 {
		t.Fatalf("list delta calls = %d, want 1", got)
	}
}

func TestDriveQuotaSnapshotDeltaCarriesErrorReasonFromOps(t *testing.T) {
	before := DriveQuotaSnapshot{
		Calls:  1,
		Errors: 0,
		Ops: map[string]DriveQuotaOpSnapshot{
			"upload": {Calls: 1, Errors: 0},
		},
	}
	after := DriveQuotaSnapshot{
		Calls:  2,
		Errors: 1,
		Ops: map[string]DriveQuotaOpSnapshot{
			"upload": {Calls: 2, Errors: 1, LastErrorReason: "storageQuotaExceeded"},
		},
	}
	delta := after.Delta(before)
	if delta.LastErrorReason != "storageQuotaExceeded" {
		t.Fatalf("delta last error reason = %q, want storageQuotaExceeded", delta.LastErrorReason)
	}
	if delta.Ops["upload"].LastErrorReason != "storageQuotaExceeded" {
		t.Fatalf("op last error reason = %q, want storageQuotaExceeded", delta.Ops["upload"].LastErrorReason)
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
	if snapshot := store.quota.Snapshot(); snapshot.Calls != 1 || snapshot.Units != 200 || snapshot.Errors != 0 {
		t.Fatalf("quota snapshot = %+v, want attempted units recorded without cancellation error noise", snapshot)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stringResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Date":         []string{time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}
