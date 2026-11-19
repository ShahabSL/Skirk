package skirk

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSlidingNormalListDrainsDenseBacklogWithoutSkippingNewHead(t *testing.T) {
	startedAt := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	prefix := "muxv4/session/down/client/run/"
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	makeObject := func(i int, updated time.Time) ObjectInfo {
		return ObjectInfo{
			Name:    fmt.Sprintf("%sepoch/p1/s%016d/l00/%016d.f1.b65536", prefix, i, i),
			ID:      fmt.Sprintf("object-%d", i),
			Updated: updated.Format(time.RFC3339Nano),
		}
	}
	for i := 449; i >= 0; i-- {
		store.objects = append(store.objects, makeObject(i, startedAt.Add(time.Duration(i+1)*time.Second)))
	}
	mux := &driveMux{t: &Tunnel{Data: store, PollInterval: 250 * time.Millisecond}, startedAt: startedAt, listSince: startedAt}
	list := func() []ObjectInfo {
		t.Helper()
		infos, err := mux.listRecvMuxObjectsByClass(context.Background(), prefix, "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
		if err != nil {
			t.Fatal(err)
		}
		return infos
	}
	seen := make(map[string]bool)
	for _, info := range list() {
		seen[info.ID] = true
	}
	if !mux.hasListFreshPageToken() {
		t.Fatal("first four-page scan lost its continuation")
	}
	firstScanSince := mux.normalListScanSince
	if firstScanSince.IsZero() || !mux.listFreshSince().Equal(startedAt) {
		t.Fatalf("scan watermark=%s cursor=%s", firstScanSince, mux.listFreshSince())
	}
	// Add more than the one-page head budget. The unobserved 50 must still
	// appear after the older continuation completes.
	newObjects := make([]ObjectInfo, 0, 150)
	for i := 599; i >= 450; i-- {
		newObjects = append(newObjects, makeObject(i, startedAt.Add(time.Duration(i+1)*time.Second)))
	}
	store.objects = append(newObjects, store.objects...)
	for _, info := range list() {
		seen[info.ID] = true
	}
	if mux.hasListFreshPageToken() {
		t.Fatal("older continuation did not complete")
	}
	if got := mux.listFreshSince(); !got.Equal(firstScanSince) {
		t.Fatalf("cursor advanced to a new head item and could skip its second page: got %s, want %s", got, firstScanSince)
	}
	for _, info := range list() {
		seen[info.ID] = true
	}
	if len(seen) != 600 {
		var missing []string
		for i := range 600 {
			id := fmt.Sprintf("object-%d", i)
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		t.Fatalf("saw %d/600 objects; missing %s", len(seen), strings.Join(missing, ", "))
	}
	if len(store.calls) != 4 || store.calls[0].maxPages != 4 || store.calls[0].pageToken != "" || store.calls[1].maxPages != 3 || store.calls[1].pageToken == "" || store.calls[2].maxPages != 1 || store.calls[2].pageToken != "" || store.calls[3].maxPages != 4 || store.calls[3].pageToken != "" {
		t.Fatalf("bounded head/continuation calls = %+v", store.calls)
	}
}

func TestSlidingNormalListContinuesAfterLastStreamGoesIdle(t *testing.T) {
	mux := &driveMux{t: &Tunnel{PollInterval: 250 * time.Millisecond}, listPageToken: "next", normalListLastPoll: time.Now()}
	if mux.shouldPollNormalMuxObjects(time.Now()) {
		t.Fatal("idle continuation ignored normal poll interval")
	}
	if !mux.shouldPollNormalMuxObjects(mux.normalListLastPoll.Add(mux.normalActivePollInterval())) {
		t.Fatal("idle continuation stranded a saved normal page token")
	}
}

func TestSlidingNormalListProgressesWithRepeatedNewerInserts(t *testing.T) {
	startedAt := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	prefix := "muxv4/session/down/client/run/"
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	object := func(i int) ObjectInfo {
		return ObjectInfo{
			Name:    fmt.Sprintf("%sepoch/p1/s%016d/l00/%016d.f1.b65536", prefix, i, i),
			ID:      fmt.Sprintf("object-%d", i),
			Updated: startedAt.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano),
		}
	}
	for i := 999; i >= 0; i-- {
		store.objects = append(store.objects, object(i))
	}
	mux := &driveMux{t: &Tunnel{Data: store}, startedAt: startedAt, listSince: startedAt}
	seen := make(map[string]bool)
	list := func() {
		t.Helper()
		infos, err := mux.listRecvMuxObjectsByClass(context.Background(), prefix, "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range infos {
			seen[info.ID] = true
		}
	}
	list()
	nextID := 1000
	for mux.hasListFreshPageToken() && nextID < 2000 {
		newObjects := make([]ObjectInfo, 0, 120)
		for i := nextID + 119; i >= nextID; i-- {
			newObjects = append(newObjects, object(i))
		}
		store.objects = append(newObjects, store.objects...)
		nextID += 120
		list()
	}
	if mux.hasListFreshPageToken() {
		t.Fatal("older-page cursor did not progress through repeated newer inserts")
	}
	for i := range 1000 {
		if !seen[fmt.Sprintf("object-%d", i)] {
			t.Fatalf("older object %d was starved", i)
		}
	}
	for attempt := 0; attempt < 10 && len(seen) < nextID; attempt++ {
		list()
	}
	if len(seen) != nextID {
		t.Fatalf("saw %d/%d objects after head replay", len(seen), nextID)
	}
	for i := 0; i < len(store.calls)-1; i++ {
		call := store.calls[i]
		if call.pageToken != "" && call.maxPages != muxFreshClassListPages-1 {
			t.Fatalf("continuation call exceeded its three-page budget: %+v", call)
		}
	}
}

type rejectedNormalPaginationStore struct {
	*adaptiveDiscoveryStore
	rejectOnce bool
}

func (s *rejectedNormalPaginationStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if pageToken != "" && s.rejectOnce {
		s.rejectOnce = false
		return ObjectListInfo{}, fmt.Errorf("invalid pageToken")
	}
	return s.adaptiveDiscoveryStore.ListFreshContainsPageStatus(ctx, contains, since, pageToken, maxPages)
}

func TestSlidingNormalListRestartsAfterRejectedPageToken(t *testing.T) {
	startedAt := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	prefix := "muxv4/session/down/client/run/"
	base := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	for i := 449; i >= 0; i-- {
		base.objects = append(base.objects, ObjectInfo{
			Name:    fmt.Sprintf("%sepoch/p1/object-%d", prefix, i),
			ID:      fmt.Sprintf("object-%d", i),
			Updated: startedAt.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano),
		})
	}
	store := &rejectedNormalPaginationStore{adaptiveDiscoveryStore: base, rejectOnce: true}
	mux := &driveMux{t: &Tunnel{Data: store}, startedAt: startedAt, listSince: startedAt}
	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		infos, err := mux.listRecvMuxObjectsByClass(context.Background(), prefix, "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range infos {
			seen[info.ID] = true
		}
	}
	if len(seen) != 450 || mux.hasListFreshPageToken() || store.rejectOnce {
		t.Fatalf("rejected token recovery saw %d/450, token=%q, rejectOnce=%t", len(seen), mux.listPageToken, store.rejectOnce)
	}
	if len(base.calls) != 4 || base.calls[0].maxPages != 4 || base.calls[1].maxPages != 4 || base.calls[2].maxPages != 3 || base.calls[3].maxPages != 1 {
		t.Fatalf("rejected-token fallback calls = %+v", base.calls)
	}
}
