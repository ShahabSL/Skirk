package skirk

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

type adaptiveListCall struct {
	contains  []string
	since     time.Time
	pageToken string
	maxPages  int
}

type adaptiveDiscoveryStore struct {
	*MemoryStore
	objects    []ObjectInfo // Newest first, as returned by Drive files.list.
	calls      []adaptiveListCall
	incomplete bool
}

func (s *adaptiveDiscoveryStore) ListFreshContainsPageStatus(_ context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	s.calls = append(s.calls, adaptiveListCall{append([]string(nil), contains...), since, pageToken, maxPages})
	if maxPages <= 0 {
		return ObjectListInfo{}, fmt.Errorf("unbounded adaptive list")
	}
	filtered := make([]ObjectInfo, 0, len(s.objects))
	for _, object := range s.objects {
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
		return ObjectListInfo{Truncated: true, Incomplete: true, Pages: 1}, nil
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
	end := offset + maxPages*100 // Production's broad and priority lists use pageSize=100.
	if end > len(filtered) {
		end = len(filtered)
	}
	result := ObjectListInfo{Objects: filtered[offset:end], Pages: (end - offset + 99) / 100}
	if end < len(filtered) {
		result.Truncated = true
		result.NextPageToken = fmt.Sprintf("offset:%d", end)
	}
	return result, nil
}

func adaptiveTestMux(t *testing.T, store *adaptiveDiscoveryStore) *driveMux {
	t.Helper()
	cfg := &Config{SessionID: "00112233445566778899aabbccddeeff", Client: ClientConfig{ID: "client-test", RunID: "run-test"}}
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
	if len(store.calls) != 1 || len(store.calls[0].contains) != 1 || store.calls[0].maxPages != muxAdaptiveCombinedListPages {
		t.Fatalf("sparse calls = %#v, want one bounded legacy prefix query", store.calls)
	}
	if mux.hasListFreshPartial() || mux.hasListFreshPageToken() {
		t.Fatal("complete sparse list retained partial state")
	}
}

func TestAdaptiveMuxDiscoveryLatePriorityBehindFourThousandNormalObjects(t *testing.T) {
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
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
	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("first broad page budget did not report continued work")
	}
	if len(store.calls) != 2 || store.calls[0].pageToken != "" || store.calls[0].maxPages != 4 || !containsString(store.calls[1].contains, "/p0/") {
		t.Fatalf("first partial scan calls = %#v, want broad then priority fallback", store.calls)
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
	if len(store.calls) != 4 || store.calls[2].pageToken != "offset:400" || !containsString(store.calls[3].contains, "/p0/") {
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
	if !mux.pollMuxObjects(context.Background()) || !mux.hasListFreshPageToken() || !mux.hasPriorityListPartial() {
		t.Fatal("first dense poll did not retain both cursors")
	}
	if !mux.pollMuxObjects(context.Background()) {
		t.Fatal("second poll did not complete both cursors")
	}
	if mux.hasListFreshPageToken() || mux.hasPriorityListPartial() || mux.hasListFreshPageTokenFor(&mux.priorityListMu, &mux.priorityListPageToken) {
		t.Fatal("priority page token was abandoned when broad list became complete")
	}
	if len(store.calls) != 4 || store.calls[2].pageToken != "offset:400" || store.calls[3].pageToken != "offset:400" {
		t.Fatalf("cursor continuation calls = %#v", store.calls)
	}
	if len(mux.recvUrgent) != 401 {
		t.Fatalf("priority queue contains %d objects, want 401 unique objects", len(mux.recvUrgent))
	}
}
