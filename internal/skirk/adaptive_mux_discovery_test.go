package skirk

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDriveAdaptiveMuxListUsesConfiguredPageSizeWithoutChangingLegacyList(t *testing.T) {
	prefix := "muxv4/session/down/client/run/"
	name := prefix + "epoch/p1/object"
	var pageSizes, pageTokens []string
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		pageSizes = append(pageSizes, query.Get("pageSize"))
		pageTokens = append(pageTokens, query.Get("pageToken"))
		if query.Get("pageSize") == "1000" && len(pageSizes) <= 4 {
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"nextPageToken":"page-%d","files":[{"id":"file-%d","name":%q,"size":"1"}]}`, len(pageSizes), len(pageSizes), name)), nil
		}
		return stringResponse(http.StatusOK, fmt.Sprintf(`{"files":[{"id":"file-last","name":%q,"size":"1"}]}`, name)), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{NormalListPageSize: 1000})
	first, err := store.ListFreshAdaptiveMuxPageStatus(context.Background(), prefix, time.Time{}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Truncated || first.NextPageToken != "page-1" || first.Pages != 1 || len(first.Objects) != 1 {
		t.Fatalf("first adaptive page = %+v", first)
	}
	second, err := store.ListFreshAdaptiveMuxPageStatus(context.Background(), prefix, time.Time{}, first.NextPageToken, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Truncated || second.NextPageToken != "page-4" || second.Pages != 3 || len(second.Objects) != 3 || pageTokens[1] != "page-1" {
		t.Fatalf("adaptive continuation = %+v, tokens=%v", second, pageTokens)
	}
	third, err := store.ListFreshAdaptiveMuxPageStatus(context.Background(), prefix, time.Time{}, second.NextPageToken, 1)
	if err != nil {
		t.Fatal(err)
	}
	if third.Truncated || len(third.Objects) != 1 || pageTokens[4] != "page-4" {
		t.Fatalf("final adaptive page = %+v, tokens=%v", third, pageTokens)
	}
	if _, err := store.ListFreshPageStatus(context.Background(), prefix, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if len(pageSizes) != 6 || pageSizes[0] != "1000" || pageSizes[1] != "1000" || pageSizes[2] != "1000" || pageSizes[3] != "1000" || pageSizes[4] != "1000" || pageSizes[5] != "100" {
		t.Fatalf("page sizes = %v, want adaptive 1000 and legacy 100", pageSizes)
	}
}

func TestDriveMuxRetryMetadataChecksMailboxMembership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    DriveConfig
		body      string
		inMailbox bool
		wantErr   bool
	}{
		{name: "folder", config: DriveConfig{FolderID: "mailbox-folder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","parents":["mailbox-folder"]}`, inMailbox: true},
		{name: "moved", config: DriveConfig{FolderID: "mailbox-folder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","parents":["other-folder"]}`},
		{name: "empty-parents", config: DriveConfig{FolderID: "mailbox-folder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","parents":[]}`},
		{name: "missing-parents", config: DriveConfig{FolderID: "mailbox-folder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1"}`, wantErr: true},
		{name: "trashed", config: DriveConfig{FolderID: "mailbox-folder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","parents":["mailbox-folder"],"trashed":true}`},
		{name: "appdata", config: DriveConfig{Space: "appDataFolder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","spaces":["appDataFolder"]}`, inMailbox: true},
		{name: "appdata-moved", config: DriveConfig{Space: "appDataFolder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","spaces":["drive"]}`},
		{name: "appdata-empty-spaces", config: DriveConfig{Space: "appDataFolder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1","spaces":[]}`},
		{name: "appdata-missing-spaces", config: DriveConfig{Space: "appDataFolder"}, body: `{"id":"file-id","name":"muxv4/session/down/p0/object","size":"1"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/drive/v3/files/file-id" || !strings.Contains(req.URL.Query().Get("fields"), "spaces") {
					t.Fatalf("metadata request = %s", req.URL.Redacted())
				}
				return stringResponse(http.StatusOK, tc.body), nil
			})}}
			store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), tc.config)
			info, inMailbox, err := store.GetMuxObjectMetadataByID(context.Background(), "file-id")
			if tc.wantErr {
				if err == nil || inMailbox {
					t.Fatalf("omitted mailbox field gave info=%+v inMailbox=%t err=%v", info, inMailbox, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.ID != "file-id" || inMailbox != tc.inMailbox {
				t.Fatalf("info=%+v inMailbox=%t, want %t", info, inMailbox, tc.inMailbox)
			}
		})
	}
}

type adaptiveListCall struct {
	contains  []string
	since     time.Time
	pageToken string
	maxPages  int
	pageSize  int
}

type adaptiveDiscoveryStore struct {
	*MemoryStore
	objects                   []ObjectInfo // Newest first, as returned by Drive files.list.
	calls                     []adaptiveListCall
	incomplete                bool
	incompleteToken           bool
	priorityIncompleteToken   bool
	emptyContinuation         bool
	rejectPageTokenOnce       bool
	broadOmitsPriority        bool
	priorityListOmitsPriority bool
	priorityHook              func()
	metadataCalls             []string
	metadataError             bool
}

func (s *adaptiveDiscoveryStore) ListFreshContainsPageStatus(_ context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if containsString(contains, "/p0/") && s.priorityHook != nil {
		hook := s.priorityHook
		s.priorityHook = nil
		hook()
	}
	return s.listFreshPageStatus(contains, since, pageToken, maxPages, 100)
}

func (s *adaptiveDiscoveryStore) ListFreshAdaptiveMuxPageStatus(_ context.Context, prefix string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	return s.listFreshPageStatus([]string{prefix}, since, pageToken, maxPages, 1000)
}

func (s *adaptiveDiscoveryStore) listFreshPageStatus(contains []string, since time.Time, pageToken string, maxPages, pageSize int) (ObjectListInfo, error) {
	s.calls = append(s.calls, adaptiveListCall{contains: append([]string(nil), contains...), since: since, pageToken: pageToken, maxPages: maxPages, pageSize: pageSize})
	if maxPages <= 0 {
		return ObjectListInfo{}, fmt.Errorf("unbounded adaptive list")
	}
	if s.priorityListOmitsPriority && containsString(contains, "/p0/") {
		return ObjectListInfo{Pages: 1}, nil
	}
	if s.rejectPageTokenOnce && pageToken != "" && len(contains) == 1 {
		s.rejectPageTokenOnce = false
		return ObjectListInfo{}, fmt.Errorf("invalid pageToken")
	}
	if s.emptyContinuation && pageToken != "" && len(contains) == 1 {
		return ObjectListInfo{Pages: 1}, nil
	}
	filtered := make([]ObjectInfo, 0, len(s.objects))
	for _, object := range s.objects {
		if s.broadOmitsPriority && len(contains) == 1 && strings.Contains(object.Name, "/p0/") {
			continue
		}
		if updated := parseObjectUpdated(object); !updated.IsZero() && updated.Before(since) {
			continue
		}
		matches := true
		for _, needle := range contains {
			if !strings.Contains(object.Name, needle) {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, object)
		}
	}
	if s.incomplete && len(contains) == 1 {
		result := ObjectListInfo{Truncated: true, Incomplete: true, Pages: 1}
		if s.incompleteToken {
			result.NextPageToken = "incomplete-next"
		}
		return result, nil
	}
	if s.priorityIncompleteToken && containsString(contains, "/p0/") {
		return ObjectListInfo{Objects: filtered, Truncated: true, Incomplete: true, NextPageToken: "priority-incomplete-next", Pages: 1}, nil
	}
	offset := 0
	if pageToken != "" {
		var err error
		offset, err = strconv.Atoi(strings.TrimPrefix(pageToken, "offset:"))
		if err != nil {
			return ObjectListInfo{}, err
		}
	}
	if offset > len(filtered) {
		return ObjectListInfo{}, fmt.Errorf("page offset %d past %d results", offset, len(filtered))
	}
	end := offset + maxPages*pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	result := ObjectListInfo{Objects: filtered[offset:end], Pages: (end - offset + pageSize - 1) / pageSize}
	if end < len(filtered) {
		result.Truncated = true
		result.NextPageToken = fmt.Sprintf("offset:%d", end)
	}
	return result, nil
}

func (s *adaptiveDiscoveryStore) GetMuxObjectMetadataByID(_ context.Context, fileID string) (ObjectInfo, bool, error) {
	s.metadataCalls = append(s.metadataCalls, fileID)
	if s.metadataError {
		return ObjectInfo{}, false, fmt.Errorf("temporary metadata failure")
	}
	for _, object := range s.objects {
		if object.ID == fileID {
			return object, true, nil
		}
	}
	return ObjectInfo{}, false, &GoogleAPIError{Status: http.StatusNotFound, Reason: "notFound"}
}

func adaptiveTestMux(t *testing.T, store *adaptiveDiscoveryStore) *driveMux {
	t.Helper()
	cfg := &Config{SessionID: "00112233445566778899aabbccddeeff", Client: ClientConfig{ID: "client-test", RunID: "run-test"}, Tunnel: TunnelConfig{Transport: "muxv4"}}
	cfg.Drive.AdaptiveMuxDiscovery = true
	tunnel, err := NewTunnel(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !tunnel.AdaptiveMuxDiscovery || tunnel.CombinedMuxDiscovery || tunnel.MetadataSlotV1 {
		t.Fatal("adaptive discovery changed unrelated transport capabilities")
	}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.active.Store(1)
	return mux
}

func TestAdaptiveMuxDiscoverySparseUsesOneLegacyList(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	legacy := ObjectInfo{
		ID:      "old-peer-file-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: time.Now().UTC().Format(time.RFC3339Nano),
	}
	store.objects = []ObjectInfo{legacy}
	if !mux.pollMuxObjects(context.Background()) || !mux.isKnown(legacy.Name) || len(mux.recvUrgent) != 1 {
		t.Fatal("adaptive discovery did not receive an untagged old-peer p0 object")
	}
	if len(store.calls) != 1 || len(store.calls[0].contains) != 1 || store.calls[0].maxPages != 1 || store.calls[0].pageSize != 1000 {
		t.Fatalf("sparse calls = %#v, want one bounded legacy prefix query", store.calls)
	}
	if mux.hasListFreshPartial() || mux.hasListFreshPageToken() {
		t.Fatal("complete sparse list retained partial state")
	}
}

func TestAdaptiveMuxDiscoveryLatePriorityBehindEightThousandNormalObjects(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	newer := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	older := mux.startedAt.Add(time.Minute).Format(time.RFC3339Nano)
	for i := 0; i < 8001; i++ {
		store.objects = append(store.objects, ObjectInfo{
			ID:      fmt.Sprintf("normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+1), []uint64{uint64(i + 1)}, 0, uint64(i+1), 1, 1, 1, 1, false),
			Updated: newer,
		})
	}
	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("first broad page budget did not report continued work")
	}
	if len(store.calls) != 3 || store.calls[0].pageToken != "" || store.calls[0].maxPages != 1 || !containsString(store.calls[1].contains, "/p0/") || store.calls[2].maxPages != 3 || store.calls[2].pageToken != "offset:1000" {
		t.Fatalf("first partial scan calls = %#v, want broad then priority fallback", store.calls)
	}
	if store.calls[0].maxPages+store.calls[1].maxPages+store.calls[2].maxPages > 8 {
		t.Fatalf("discovery page request budget exceeded: %#v", store.calls)
	}
	if got := mux.listFreshSince(); !got.Equal(mux.startedAt) {
		t.Fatalf("combined cursor advanced during truncated scan: %s", got)
	}
	priority := ObjectInfo{
		ID:      "late-priority-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: older,
	}
	store.objects = append(store.objects, priority) // Older than every p1 and absent from first poll.
	if !mux.pollMuxObjects(context.Background()) || !mux.isKnown(priority.Name) || len(mux.recvUrgent) != 1 {
		t.Fatal("late p0 was not found by the separate priority query")
	}
	if len(store.calls) != 6 || store.calls[3].pageToken != "offset:4000" || !containsString(store.calls[4].contains, "/p0/") || store.calls[5].pageToken != "offset:5000" {
		t.Fatalf("second partial scan calls = %#v, want continued broad cursor and p0 fallback", store.calls)
	}
	for polls := 2; mux.hasListFreshPageToken() && polls < 20; polls++ {
		mux.pollMuxObjects(context.Background())
	}
	if mux.hasListFreshPageToken() || mux.hasListFreshPartial() {
		t.Fatalf("combined pagination did not complete after %d calls", len(store.calls))
	}
	if got := mux.listFreshSince(); !got.After(mux.startedAt) {
		t.Fatalf("combined cursor did not advance after complete scan: %s", got)
	}
	if len(mux.recvUrgent) != 1 {
		t.Fatalf("priority was queued %d times, want one", len(mux.recvUrgent))
	}
	for _, call := range store.calls {
		if call.maxPages > muxAdaptiveCombinedListPages {
			t.Fatalf("unbounded list call: %#v", call)
		}
	}
}

func TestAdaptiveMuxDiscoveryIncompleteSearchFallsBackWithoutCursorAdvance(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), incomplete: true}
	mux := adaptiveTestMux(t, store)
	priority := ObjectInfo{
		ID:      "incomplete-priority-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: time.Now().UTC().Format(time.RFC3339Nano),
	}
	store.objects = []ObjectInfo{priority}
	if !mux.pollMuxObjects(context.Background()) || !mux.isKnown(priority.Name) {
		t.Fatal("incomplete broad search did not fall back to p0")
	}
	if len(store.calls) != 2 || !containsString(store.calls[1].contains, "/p0/") || mux.hasListFreshPageToken() || !mux.hasListFreshPartial() {
		t.Fatalf("incomplete fallback calls/state = %#v, token=%t partial=%t", store.calls, mux.hasListFreshPageToken(), mux.hasListFreshPartial())
	}
	if got := mux.listFreshSince(); !got.Equal(mux.startedAt) {
		t.Fatalf("incomplete broad search advanced cursor: %s", got)
	}
}

func TestAdaptiveMuxDiscoveryIncompleteSearchWithTokenRestartsBothCursors(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), incomplete: true, incompleteToken: true, priorityIncompleteToken: true}
	mux := adaptiveTestMux(t, store)
	store.objects = []ObjectInfo{{
		ID:      "priority-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: time.Now().UTC().Format(time.RFC3339Nano),
	}}
	for i := 0; i < 2; i++ {
		mux.pollMuxObjects(context.Background())
	}
	if len(store.calls) != 4 || store.calls[0].pageToken != "" || store.calls[1].pageToken != "" || store.calls[2].pageToken != "" || store.calls[3].pageToken != "" {
		t.Fatalf("incomplete lists followed unsafe page tokens: %#v", store.calls)
	}
	if mux.hasListFreshPageToken() || mux.hasListFreshPageTokenFor(&mux.priorityListMu, &mux.priorityListPageToken) || !mux.hasListFreshPartial() || !mux.hasPriorityListPartial() {
		t.Fatal("incomplete list state lost a retry or retained an unsafe token")
	}
	if got := mux.listFreshSince(); !got.Equal(mux.startedAt) {
		t.Fatalf("incomplete broad list advanced cursor: %s", got)
	}
}

func TestAdaptiveMuxDiscoveryFinishesPriorityCursorAfterBroadScanCompletes(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	newer := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	older := mux.startedAt.Add(time.Minute).Format(time.RFC3339Nano)
	store.objects = append(store.objects, ObjectInfo{
		ID:      "normal-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", 1, []uint64{1}, 0, 1, 1, 1, 1, 1, false),
		Updated: newer,
	})
	for i := 0; i < 401; i++ {
		store.objects = append(store.objects, ObjectInfo{
			ID:      fmt.Sprintf("priority-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", uint64(i+1), nil, 0, uint64(i+1), 1, 0, 0, 16, true),
			Updated: older,
		})
	}
	filler := make([]ObjectInfo, 0, 4000)
	for i := 0; i < 4000; i++ {
		filler = append(filler, ObjectInfo{
			ID:      fmt.Sprintf("filler-normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+2), []uint64{uint64(i + 2)}, 0, uint64(i+2), 1, 1, 1, 1, false),
			Updated: newer,
		})
	}
	store.objects = append(filler, store.objects...)
	if !mux.pollMuxObjects(context.Background()) || !mux.hasListFreshPageToken() || !mux.hasPriorityListPartial() {
		t.Fatal("first dense poll did not retain both cursors")
	}
	if len(store.calls) != 3 || store.calls[0].maxPages != 1 || !containsString(store.calls[1].contains, "/p0/") || store.calls[2].maxPages != 3 || store.calls[0].maxPages+store.calls[1].maxPages+store.calls[2].maxPages != 8 {
		t.Fatalf("dense poll exceeded 1+4+3 page budget or misordered fallback: %#v", store.calls)
	}
	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("second poll did not complete both cursors")
	}
	if mux.hasListFreshPageToken() || mux.hasPriorityListPartial() || mux.hasListFreshPageTokenFor(&mux.priorityListMu, &mux.priorityListPageToken) {
		t.Fatal("priority page token was abandoned when broad list became complete")
	}
	if len(store.calls) != 5 || store.calls[3].pageToken != "offset:4000" || store.calls[4].pageToken != "offset:400" {
		t.Fatalf("cursor continuation calls = %#v", store.calls)
	}
	if len(mux.recvUrgent) != 401 {
		t.Fatalf("priority queue contains %d objects, want 401 unique objects", len(mux.recvUrgent))
	}
}

func TestAdaptiveMuxDiscoveryEmptyContinuationAdvancesFromFirstPage(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), emptyContinuation: true}
	mux := adaptiveTestMux(t, store)
	updated := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	for i := 0; i < 1001; i++ {
		store.objects = append(store.objects, ObjectInfo{
			ID:      fmt.Sprintf("normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+1), []uint64{uint64(i + 1)}, 0, uint64(i+1), 1, 1, 1, 1, false),
			Updated: updated,
		})
	}
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != 3 || store.calls[2].pageToken != "offset:1000" || mux.hasListFreshPageToken() || mux.hasListFreshPartial() {
		t.Fatalf("empty continuation state/calls = %#v, token=%t partial=%t", store.calls, mux.hasListFreshPageToken(), mux.hasListFreshPartial())
	}
	if got := mux.listFreshSince(); !got.After(mux.startedAt) {
		t.Fatalf("empty final page lost first-page cursor advancement: %s", got)
	}
}

func TestAdaptiveMuxDiscoveryFirstPageBackpressureClearsContinuation(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	updated := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	for i := 0; i < 1001; i++ {
		store.objects = append(store.objects, ObjectInfo{
			ID:      fmt.Sprintf("normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+1), []uint64{uint64(i + 1)}, 0, uint64(i+1), 1, 1, 1, 1, false),
			Updated: updated,
		})
	}
	mux.recvNormalQueuedTotal = muxNormalReceiveGlobalBytes
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != 2 || store.calls[0].maxPages != 1 || !containsString(store.calls[1].contains, "/p0/") {
		t.Fatalf("backpressured first page should skip broad continuation: %#v", store.calls)
	}
	if mux.hasListFreshPageToken() || !mux.listFreshSince().Equal(mux.startedAt) {
		t.Fatal("backpressured first page advanced or retained stale cursor")
	}
}

func TestAdaptiveMuxDiscoveryRejectedContinuationTokenRestartsWithoutSkipping(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), rejectPageTokenOnce: true}
	mux := adaptiveTestMux(t, store)
	newer := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	older := mux.startedAt.Add(time.Minute).Format(time.RFC3339Nano)
	for i := 0; i < 4001; i++ {
		store.objects = append(store.objects, ObjectInfo{
			ID:      fmt.Sprintf("normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+1), []uint64{uint64(i + 1)}, 0, uint64(i+1), 1, 1, 1, 1, false),
			Updated: newer,
		})
	}
	priority := ObjectInfo{
		ID:      "priority-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: older,
	}
	store.objects = append(store.objects, priority)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != 4 || store.calls[0].pageToken != "" || !containsString(store.calls[1].contains, "/p0/") || store.calls[2].pageToken != "offset:1000" || store.calls[3].pageToken != "" {
		t.Fatalf("rejected continuation did not retry from same cursor: %#v", store.calls)
	}
	if !mux.isKnown(priority.Name) || !mux.listFreshSince().Equal(mux.startedAt) {
		t.Fatal("rejected token skipped p0 or advanced partial cursor")
	}
	if _, token := mux.listFreshCursor(); token != "offset:3000" {
		t.Fatalf("restarted continuation token = %q, want offset:3000", token)
	}
	mux.pollMuxObjects(context.Background())
	if mux.hasListFreshPageToken() || mux.hasListFreshPartial() || !mux.isKnown(store.objects[4000].Name) {
		t.Fatal("rejected-token recovery did not finish broad scan")
	}
}

func setupAdaptiveFailedPriorityPoll(t *testing.T) (*driveMux, *adaptiveDiscoveryStore, ObjectInfo) {
	t.Helper()
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), broadOmitsPriority: true}
	mux := adaptiveTestMux(t, store)
	newer := mux.startedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	older := mux.startedAt.Add(time.Minute).Format(time.RFC3339Nano)
	for i := 0; i < 1001; i++ {
		object := ObjectInfo{
			ID:      fmt.Sprintf("normal-id-%d", i),
			Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", uint64(i+1), []uint64{uint64(i + 1)}, 0, uint64(i+1), 1, 1, 1, 1, false),
			Updated: newer,
		}
		store.objects = append(store.objects, object)
		mux.markSeen(object.Name) // Already processed, but still waiting for Drive cleanup.
	}
	priority := ObjectInfo{
		ID:      "failed-priority-id",
		Name:    muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
		Updated: older,
	}
	store.objects = append(store.objects, priority)
	mux.recvUrgent = make(chan muxObjectMeta, 1)
	mux.recvUrgent <- muxObjectMeta{Name: "occupied"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.priorityHook = cancel
	mux.pollMuxObjects(ctx)
	if mux.hasListFreshPartial() || mux.hasPriorityListPartial() || mux.hasListFreshPageToken() {
		t.Fatal("first poll did not finish both lists")
	}
	if !mux.hasPriorityRetryPending() || mux.isKnown(priority.Name) {
		t.Fatal("failed p0 enqueue did not retain a retry")
	}
	if len(store.metadataCalls) != 0 {
		t.Fatal("retry created during a poll was verified in that same poll")
	}
	if got := mux.listFreshSince(); !got.After(timeMustParse(t, older)) {
		t.Fatalf("broad cursor did not advance past failed older p0: %s", got)
	}
	<-mux.recvUrgent // Make room for the retry in the next poll.
	return mux, store, priority
}

func timeMustParse(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestAdaptiveMuxDiscoveryRetriesFailedPriorityAfterBroadCompletes(t *testing.T) {
	mux, store, priority := setupAdaptiveFailedPriorityPoll(t)
	store.objects = []ObjectInfo{priority}
	store.priorityListOmitsPriority = true // Simulate a transient search omission.
	before := len(store.calls)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != before+2 || !containsString(store.calls[before+1].contains, "/p0/") {
		t.Fatalf("sparse poll skipped pending p0 retry: %#v", store.calls[before:])
	}
	if len(store.metadataCalls) != 1 || store.metadataCalls[0] != priority.ID || !mux.isKnown(priority.Name) || len(mux.recvUrgent) != 1 || mux.hasPriorityRetryPending() {
		t.Fatalf("exact-ID recovery failed: metadata=%d queued=%d pending=%t", len(store.metadataCalls), len(mux.recvUrgent), mux.hasPriorityRetryPending())
	}
}

func TestAdaptiveMuxDiscoveryClearsRetryOnlyAfterConfirmedDeletion(t *testing.T) {
	mux, store, _ := setupAdaptiveFailedPriorityPoll(t)
	store.objects = nil // Exact metadata GET now returns 404.
	before := len(store.calls)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != before+2 || len(store.metadataCalls) != 1 || !mux.hasPriorityRetryPending() {
		t.Fatalf("one 404 cleared a retry prematurely: calls=%#v metadata=%d", store.calls[before:], len(store.metadataCalls))
	}
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 1 || !mux.hasPriorityRetryPending() {
		t.Fatal("second deletion check ignored cooldown or dropped retry")
	}
	expireAdaptiveRetryProbeCooldown(mux)
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 2 || mux.hasPriorityRetryPending() {
		t.Fatalf("two separated 404s did not clear retry: metadata=%d", len(store.metadataCalls))
	}
	before = len(store.calls)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != before+1 || len(mux.recvUrgent) != 0 {
		t.Fatalf("deleted priority caused unbounded sparse fallback: %#v", store.calls[before:])
	}
}

func expireAdaptiveRetryProbeCooldown(mux *driveMux) {
	mux.priorityListMu.Lock()
	for key, state := range mux.priorityRetryObjects {
		state.lastProbe = time.Now().Add(-muxPriorityRetryProbeInterval)
		mux.priorityRetryObjects[key] = state
	}
	mux.priorityListMu.Unlock()
}

func TestAdaptiveMuxDiscoveryWorkerRewindTracksOnlyPriority(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	priorityInfo := ObjectInfo{ID: "priority-id", Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true)}
	priority, ok := parseMuxObjectInfo(priorityInfo)
	if !ok {
		t.Fatal("test priority name did not parse")
	}
	normalInfo := ObjectInfo{ID: "normal-id", Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "normal-epoch", 2, []uint64{2}, 0, 2, 1, 1, 1, 1, false)}
	normal, ok := parseMuxObjectInfo(normalInfo)
	if !ok {
		t.Fatal("test normal name did not parse")
	}
	mux.rewindReceiveListForMeta(normal)
	if mux.hasPriorityRetryPending() {
		t.Fatal("p1 worker rewind enabled p0 fallback")
	}
	mux.rewindReceiveListForMeta(priority)
	if !mux.hasPriorityRetryPending() {
		t.Fatal("p0 worker rewind did not enable p0 fallback")
	}
	if !mux.enqueueMuxObject(context.Background(), priority) || mux.hasPriorityRetryPending() {
		t.Fatal("successful p0 enqueue did not retire worker retry")
	}
}

func TestAdaptiveMuxDiscoveryExactRetryVerificationIsBoundedPerPoll(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	for i := 0; i < muxPriorityRetryProbeLimit+1; i++ {
		info := ObjectInfo{
			ID:   fmt.Sprintf("deleted-id-%d", i),
			Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", uint64(i+1), nil, 0, uint64(i+1), 1, 0, 0, 16, true),
		}
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			t.Fatal("test priority name did not parse")
		}
		mux.rewindReceiveListForMeta(meta)
	}
	if !mux.hasPriorityRetryPending() {
		t.Fatal("priority rewinds did not register pending retries")
	}
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != muxPriorityRetryProbeLimit || !mux.hasPriorityRetryPending() {
		t.Fatalf("first poll made %d exact metadata requests; pending=%t", len(store.metadataCalls), mux.hasPriorityRetryPending())
	}
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != muxPriorityRetryProbeLimit+1 || !mux.hasPriorityRetryPending() {
		t.Fatalf("second poll made %d exact metadata requests; pending=%t", len(store.metadataCalls), mux.hasPriorityRetryPending())
	}
	expireAdaptiveRetryProbeCooldown(mux)
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 2*muxPriorityRetryProbeLimit+1 || !mux.hasPriorityRetryPending() {
		t.Fatalf("third poll made %d exact metadata requests; pending=%t", len(store.metadataCalls), mux.hasPriorityRetryPending())
	}
	expireAdaptiveRetryProbeCooldown(mux)
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 2*(muxPriorityRetryProbeLimit+1) || mux.hasPriorityRetryPending() {
		t.Fatalf("fourth poll made %d exact metadata requests; pending=%t", len(store.metadataCalls), mux.hasPriorityRetryPending())
	}
	before := len(store.calls)
	mux.pollMuxObjects(context.Background())
	if len(store.calls) != before+1 {
		t.Fatalf("cleared retries kept p0 query active: %#v", store.calls[before:])
	}
}

func TestAdaptiveMuxDiscoveryRetryEligibilityDoesNotDependOnClockResolution(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	info := ObjectInfo{
		ID:   "deleted-priority-id",
		Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
	}
	meta, ok := parseMuxObjectInfo(info)
	if !ok {
		t.Fatal("test priority name did not parse")
	}
	mux.rewindReceiveListForMeta(meta)
	mux.priorityListMu.Lock()
	for key, state := range mux.priorityRetryObjects {
		state.addedAt = time.Now().Add(time.Hour) // Simulate a clock that cannot order the two polls.
		mux.priorityRetryObjects[key] = state
	}
	mux.priorityListMu.Unlock()
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 1 || store.metadataCalls[0] != info.ID {
		t.Fatalf("retry predating this poll was not verified: %v", store.metadataCalls)
	}
}

func TestAdaptiveMuxDiscoveryExactRetryVerificationIsFair(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore(), metadataError: true}
	mux := adaptiveTestMux(t, store)
	for i := 0; i < muxPriorityRetryProbeLimit+1; i++ {
		info := ObjectInfo{
			ID:   fmt.Sprintf("pending-id-%d", i),
			Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", uint64(i+1), nil, 0, uint64(i+1), 1, 0, 0, 16, true),
		}
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			t.Fatal("test priority name did not parse")
		}
		mux.rewindReceiveListForMeta(meta)
	}
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != muxPriorityRetryProbeLimit {
		t.Fatalf("first poll made %d exact metadata requests", len(store.metadataCalls))
	}
	first := make(map[string]struct{}, muxPriorityRetryProbeLimit)
	for _, id := range store.metadataCalls {
		first[id] = struct{}{}
	}
	mux.priorityListMu.Lock()
	for key, state := range mux.priorityRetryObjects {
		if !state.lastProbe.IsZero() {
			state.lastProbe = time.Now().Add(-muxPriorityRetryProbeInterval)
			mux.priorityRetryObjects[key] = state
		}
	}
	mux.priorityListMu.Unlock()
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 2*muxPriorityRetryProbeLimit {
		t.Fatalf("second poll made %d total exact metadata requests", len(store.metadataCalls))
	}
	if _, repeated := first[store.metadataCalls[muxPriorityRetryProbeLimit]]; repeated {
		t.Fatalf("oldest unprobed ID was starved by repeat metadata checks: %v", store.metadataCalls)
	}
}

func TestAdaptiveMuxDiscoveryRetryTracksDriveIDBehindKnownName(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	mux := adaptiveTestMux(t, store)
	info := ObjectInfo{ID: "alternate-id", Name: muxObjectNameWithStreamIDs(mux.t.SessionID, DirectionDown, mux.t.ClientID, mux.t.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true)}
	meta, ok := parseMuxObjectInfo(info)
	if !ok {
		t.Fatal("test priority name did not parse")
	}
	mux.markSeen(info.Name)
	mux.seenMu.Lock()
	mux.knownIDs[info.Name] = map[string]struct{}{"primary-id": {}}
	mux.seenMu.Unlock()
	mux.rewindReceiveListForMeta(meta)
	if !mux.hasPriorityRetryPending() {
		t.Fatal("known primary name cleared retry for a distinct Drive ID")
	}
	mux.isKnownMuxObject(meta) // Records the alternate ID for cleanup/verification.
	if mux.hasPriorityRetryPending() {
		t.Fatal("recognized alternate Drive ID did not retire retry")
	}
}

func TestAdaptiveMuxDiscoveryTransientMetadataErrorKeepsRetryWithCooldown(t *testing.T) {
	mux, store, priority := setupAdaptiveFailedPriorityPoll(t)
	store.objects = []ObjectInfo{priority}
	store.priorityListOmitsPriority = true
	store.metadataError = true
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 1 || !mux.hasPriorityRetryPending() {
		t.Fatal("transient metadata error discarded retry")
	}
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 1 || !mux.hasPriorityRetryPending() {
		t.Fatal("exact metadata GET ignored cooldown or discarded retry")
	}
	expireAdaptiveRetryProbeCooldown(mux)
	store.metadataError = false
	mux.pollMuxObjects(context.Background())
	if len(store.metadataCalls) != 2 || !mux.isKnown(priority.Name) || mux.hasPriorityRetryPending() {
		t.Fatal("priority retry did not recover after metadata probe cooldown")
	}
}
