package skirk

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func indexedTestName(client, class, sequence string) string {
	return fmt.Sprintf("muxv4/session/down/%s/run/epoch/%s/s0000000000000001/l00/%s.f1.b64", client, class, sequence)
}

func TestIndexedClassPropertyCreateOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, wantClass string
		enabled         bool
	}{
		{indexedTestName("client", "p0", "0000000000000001"), "", false},
		{indexedTestName("client", "p0", "0000000000000001"), "p0", true},
		{indexedTestName("p0", "p1", "0000000000000002"), "p1", true},
		{"control/session/object", "", true},
	} {
		t.Run(fmt.Sprintf("%s/%t", tc.name, tc.enabled), func(t *testing.T) {
			var metadata map[string]any
			requests := 0
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				part, err := multipart.NewReader(req.Body, params["boundary"]).NextPart()
				if err != nil {
					t.Fatal(err)
				}
				if err := json.NewDecoder(part).Decode(&metadata); err != nil {
					t.Fatal(err)
				}
				return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"file-id","name":%q,"size":"3"}`, tc.name)), nil
			})}}
			store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{IndexedClassProperties: tc.enabled})
			if _, err := store.PutObject(context.Background(), tc.name, []byte("abc")); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("create calls = %d, want one", requests)
			}
			properties, ok := metadata["properties"].(map[string]any)
			if tc.wantClass == "" {
				if ok {
					t.Fatalf("unexpected properties: %#v", properties)
				}
				return
			}
			if !ok || properties[driveIndexedClassProperty] != tc.wantClass || len(properties) != 1 {
				t.Fatalf("properties = %#v, want class %q", properties, tc.wantClass)
			}
		})
	}
}

func TestIndexedClassPropertyMetadataOnlyCreate(t *testing.T) {
	name := indexedTestName("client", "p0", "0000000000000001") + driveMetadataOnlyNameSuffix
	var properties map[string]string
	requests := 0
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Path != "/drive/v3/files" {
			t.Fatalf("metadata-only path = %q", req.URL.Path)
		}
		var metadata struct {
			Properties map[string]string `json:"properties"`
		}
		if err := json.NewDecoder(req.Body).Decode(&metadata); err != nil {
			t.Fatal(err)
		}
		properties = metadata.Properties
		return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"file-id","name":%q,"size":"0"}`, name)), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{IndexedClassProperties: true, MetadataOnlyPriority: true})
	if _, err := store.PutMetadataOnlyObject(context.Background(), name, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("metadata-only create calls = %d, want one", requests)
	}
	if len(properties) != 1 || properties[driveIndexedClassProperty] != "p0" {
		t.Fatalf("metadata-only properties = %#v", properties)
	}
}

func TestIndexedClassQueryUsesPropertyAndValidatesNames(t *testing.T) {
	prefix := "muxv4/session/down/p0/run/"
	good := indexedTestName("p0", "p0", "0000000000000001")
	wrongClass := indexedTestName("p0", "p1", "0000000000000002")
	wrongPrefix := indexedTestName("other", "p0", "0000000000000003")
	requests := 0
	var queries []url.Values
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		queries = append(queries, req.URL.Query())
		if requests == 1 {
			body, _ := json.Marshal(map[string]any{
				"nextPageToken": "next",
				"files": []map[string]string{
					{"id": "good", "name": good, "size": "64"},
					{"id": "wrong-class", "name": wrongClass, "size": "64"},
					{"id": "wrong-prefix", "name": wrongPrefix, "size": "64"},
				},
			})
			return stringResponse(http.StatusOK, string(body)), nil
		}
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{IndexedClassProperties: true, FolderID: "mailbox-folder"})
	first, err := store.ListFreshIndexedClassPageStatus(context.Background(), prefix, "p0", time.Time{}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Truncated || first.NextPageToken != "next" || first.Pages != 1 || len(first.Objects) != 1 || first.Objects[0].ID != "good" {
		t.Fatalf("first page = %+v", first)
	}
	wantClauses := []string{
		"'mailbox-folder' in parents",
		"name contains 'muxv4/session/down/p0/run/'",
		"properties has { key='skirkClass' and value='p0' }",
	}
	for _, clause := range wantClauses {
		if !strings.Contains(queries[0].Get("q"), clause) {
			t.Fatalf("query = %q, missing %q", queries[0].Get("q"), clause)
		}
	}
	if strings.Contains(queries[0].Get("q"), "name contains '/p0/'") {
		t.Fatalf("indexed query used undocumented interior name clause: %q", queries[0].Get("q"))
	}
	if queries[0].Get("pageSize") != "100" || queries[0].Get("orderBy") != "modifiedTime desc" {
		t.Fatalf("query options = %v", queries[0])
	}
	second, err := store.ListFreshIndexedClassPageStatus(context.Background(), prefix, "p0", time.Time{}, first.NextPageToken, 1)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || queries[1].Get("pageToken") != "next" || second.Truncated || len(second.Objects) != 0 {
		t.Fatalf("second page = %+v, queries = %v", second, queries)
	}
}

func TestIndexedClassQueryRequiresOptIn(t *testing.T) {
	store := NewDriveStore(nil, "test-token", DriveConfig{})
	if _, err := store.ListFreshIndexedClassPageStatus(context.Background(), "muxv4/session/down/", "p0", time.Time{}, "", 1); err == nil {
		t.Fatal("indexed query ran without opt-in")
	}
	store.indexedClassProperties = true
	for _, tc := range []struct{ prefix, class string }{{"control/", "p0"}, {"muxv4/session/down/", "unknown"}} {
		if _, err := store.ListFreshIndexedClassPageStatus(context.Background(), tc.prefix, tc.class, time.Time{}, "", 1); err == nil {
			t.Fatalf("accepted invalid query %q %q", tc.prefix, tc.class)
		}
	}
}

func TestIndexedClassOptInDoesNotSwitchLegacyDiscovery(t *testing.T) {
	var query string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query = req.URL.Query().Get("q")
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{IndexedClassProperties: true})
	if _, err := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/session/down/", "/p0/"}, time.Time{}, "", 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "name contains '/p0/'") || strings.Contains(query, "properties has") {
		t.Fatalf("legacy discovery changed by probe flag: %q", query)
	}
}

type indexedProbeMemoryStore struct{ *MemoryStore }

func (*indexedProbeMemoryStore) ListFreshIndexedClassPageStatus(context.Context, string, string, time.Time, string, int) (ObjectListInfo, error) {
	panic("production mux must not use unnegotiated indexed class query")
}

func TestIndexedClassCandidateKeepsLegacyMemoryStoreDiscovery(t *testing.T) {
	ctx := context.Background()
	store := &indexedProbeMemoryStore{MemoryStore: NewMemoryStore()}
	priority := indexedTestName("client", "p0", "0000000000000001")
	normal := indexedTestName("client", "p1", "0000000000000002")
	for _, name := range []string{priority, normal} {
		if err := store.Put(ctx, name, []byte("sealed")); err != nil {
			t.Fatal(err)
		}
	}
	mux, err := newDriveMux(&Tunnel{Data: store, ClientID: "client", RunID: "run"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := mux.listRecvMuxObjectsByClass(ctx, "muxv4/session/down/client/run/", "/p0/", &mux.priorityListMu, &mux.priorityListSince, &mux.priorityListPageToken, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("legacy fallback found %d objects, want both classes", len(listed))
	}
}
