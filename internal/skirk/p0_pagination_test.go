package skirk

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestPriorityContinuationChecksForNewOpensWithinItsPageBudget(t *testing.T) {
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	const prefix = "muxv5/session/up/"
	store := &adaptiveDiscoveryStore{MemoryStore: NewMemoryStore()}
	for i := 999; i >= 0; i-- {
		store.objects = append(store.objects, ObjectInfo{
			Name:    fmt.Sprintf("%sclient/run/epoch/p0/object-%d", prefix, i),
			ID:      fmt.Sprintf("object-%d", i),
			Updated: start.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano),
		})
	}
	mux := &driveMux{t: &Tunnel{Data: store}, startedAt: start}
	seen := make(map[string]bool)
	list := func() []ObjectInfo {
		t.Helper()
		infos, err := mux.listRecvMuxObjectsByClass(t.Context(), prefix, "/p0/", &mux.priorityListMu, &mux.priorityListSince, &mux.priorityListPageToken, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range infos {
			seen[info.ID] = true
		}
		return infos
	}
	list()
	if token := mux.priorityListPageToken; token == "" || !mux.hasPriorityListPartial() {
		t.Fatal("fixture did not produce a partial priority scan")
	}
	var arrivals []ObjectInfo
	for i := 119; i >= 0; i-- {
		arrivals = append(arrivals, ObjectInfo{
			Name:    fmt.Sprintf("%snew-client/run/epoch/p0/fresh-open-%d", prefix, i),
			ID:      fmt.Sprintf("fresh-open-%d", i),
			Updated: start.Add(time.Duration(1001+i) * time.Second).Format(time.RFC3339Nano),
		})
	}
	store.objects = append(arrivals, store.objects...)
	if infos := list(); !slices.ContainsFunc(infos, func(info ObjectInfo) bool { return info.ID == arrivals[0].ID }) {
		t.Fatal("a new stream open was missing while the previous scan continued")
	}
	if got := store.calls; len(got) != 3 || got[1].pageToken == "" || got[1].maxPages != 3 || got[2].pageToken != "" || got[2].maxPages != 1 {
		t.Fatalf("the continuation did not reserve one of four pages for a fresh scan: %+v", got)
	}
	for attempt := 0; mux.priorityListPageToken != "" && attempt < 8; attempt++ {
		list()
	}
	if mux.priorityListPageToken != "" || mux.hasPriorityListPartial() {
		t.Fatal("older priority continuation did not finish")
	}
	// The fresh page was smaller than the burst. A subsequent scan must
	// replay the rest of the new opens instead of advancing past them.
	list()
	if len(seen) != len(store.objects) {
		t.Fatalf("priority scan did not finish all objects: %d of %d", len(seen), len(store.objects))
	}
}
