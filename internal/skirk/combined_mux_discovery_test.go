package skirk

import (
	"context"
	"testing"
	"time"
)

type discoverySelectionStore struct {
	*MemoryStore
	combined int
	priority int
	normal   int
	legacy   ObjectInfo
}

func (s *discoverySelectionStore) ListFreshPageStatus(context.Context, string, time.Time, string) (ObjectListInfo, error) {
	s.combined++
	return ObjectListInfo{Objects: []ObjectInfo{s.legacy}}, nil
}

func (s *discoverySelectionStore) ListFreshContainsPageStatus(_ context.Context, contains []string, _ time.Time, _ string, _ int) (ObjectListInfo, error) {
	if containsString(contains, "/p0/") {
		s.priority++
		return ObjectListInfo{Objects: []ObjectInfo{s.legacy}}, nil
	}
	if containsString(contains, "/p1/") {
		s.normal++
	}
	return ObjectListInfo{}, nil
}

func TestCombinedMuxDiscoverySelectsOnlyListStrategy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		combined     bool
		wantCombined int
		wantPriority int
		wantNormal   int
	}{
		{name: "default-class-split", combined: false, wantPriority: 1, wantNormal: 1},
		{name: "combined-legacy-prefix", combined: true, wantCombined: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &discoverySelectionStore{MemoryStore: NewMemoryStore()}
			cfg := &Config{SessionID: "00112233445566778899aabbccddeeff", Client: ClientConfig{ID: "client-test", RunID: "run-test"}}
			cfg.Drive.CombinedMuxDiscovery = tc.combined
			tunnel, err := NewTunnel(store, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tunnel.CombinedMuxDiscovery != tc.combined || tunnel.MetadataSlotV1 || tunnel.MetadataOnlyPriority {
				t.Fatal("discovery switch changed transport capabilities")
			}
			store.legacy = ObjectInfo{
				ID:   "old-peer-file-id",
				Name: muxObjectNameWithStreamIDs(tunnel.SessionID, DirectionDown, tunnel.ClientID, tunnel.RunID, "old-epoch", 1, nil, 0, 1, 1, 0, 0, 16, true),
			}
			mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
			if err != nil {
				t.Fatal(err)
			}
			mux.active.Store(1)
			if !mux.pollMuxObjectsWithNormal(context.Background(), true) || !mux.isKnown(store.legacy.Name) {
				t.Fatal("existing mux object was not queued by selected list strategy")
			}
			if store.combined != tc.wantCombined || store.priority != tc.wantPriority || store.normal != tc.wantNormal {
				t.Fatalf("list calls combined=%d priority=%d normal=%d", store.combined, store.priority, store.normal)
			}
		})
	}
}
