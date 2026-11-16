package skirk

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestMuxSeenLimitKeepsRecentProcessedIdentities(t *testing.T) {
	m := &driveMux{
		seen:      map[string]struct{}{},
		processed: map[string]struct{}{},
	}
	for i := 0; i < 199_998; i++ {
		m.markSeen(fmt.Sprintf("old-%06d", i))
	}
	first := muxObjectMeta{Name: "recent-processed-a", ID: "id-a"}
	second := muxObjectMeta{Name: "recent-processed-b", ID: "id-b"}
	m.markSeenProcessed(first)
	m.markSeenProcessed(second)
	m.markSeen("newest")
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	for _, name := range []string{first.Name, second.Name, "newest"} {
		if _, ok := m.seen[name]; !ok {
			t.Errorf("recent seen identity %q lost at limit", name)
		}
	}
	for _, name := range []string{first.Name, second.Name} {
		if _, ok := m.processed[name]; !ok {
			t.Errorf("recent processed identity %q lost at limit", name)
		}
	}
	if len(m.seen) > 200_000 {
		t.Errorf("seen cache grew beyond cap: %d", len(m.seen))
	}
}

func TestMuxClosedLimitKeepsUnexpiredTombstones(t *testing.T) {
	now := time.Now()
	keep := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 1}
	m := &driveMux{closed: map[muxStreamKey]time.Time{keep: now.Add(time.Minute)}, streams: map[muxStreamKey]*muxStream{}}
	for i := 2; i <= 100_002; i++ {
		m.closed[muxStreamKey{ClientID: "client", RunID: "run", StreamID: uint64(i)}] = now.Add(time.Minute)
	}
	newKey := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 100_003}
	m.rememberClosedStreamLocked(newKey, now)
	if len(m.closed) > muxClosedStreamLimit {
		t.Fatalf("closed tombstones exceeded hard cap: %d", len(m.closed))
	}
	if got := m.closed[newKey]; !got.Equal(now.Add(muxClosedStreamTTL)) {
		t.Fatalf("new tombstone missing: %v", got)
	}
	if !m.isClosedStream(keep) {
		t.Fatal("unexpired tombstone lost during saturated close cache")
	}
	if m.claimExitOpen(keep) {
		t.Fatal("closed stream reopened after unrelated tombstone pressure")
	}
	fresh := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 900_000}
	if m.claimExitOpen(fresh) {
		t.Fatal("new OPEN admitted while evicted live tombstones need a fence")
	}
	if !m.isClosedStream(fresh) {
		t.Fatal("overflow fence did not reject pending frames for unknown streams")
	}
	m.streams[fresh] = &muxStream{}
	if m.isClosedStream(fresh) {
		t.Fatal("overflow fence damaged an already active stream")
	}
	delete(m.streams, fresh)
	m.closedOverflowUntil = time.Now().Add(-time.Second)
	if !m.claimExitOpen(fresh) {
		t.Fatal("fresh OPEN stayed blocked after overflow fence expired")
	}
}

func TestMuxClosedPruneRemovesOnlyExpiredEntries(t *testing.T) {
	now := time.Now()
	keep := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 1}
	m := &driveMux{closed: map[muxStreamKey]time.Time{keep: now.Add(time.Minute)}}
	for i := 2; i <= 100_002; i++ {
		m.closed[muxStreamKey{ClientID: "client", RunID: "run", StreamID: uint64(i)}] = now.Add(-time.Second)
	}
	newKey := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 100_003}
	m.rememberClosedStreamLocked(newKey, now)
	if len(m.closed) != 2 || m.closed[keep].IsZero() || m.closed[newKey].IsZero() {
		t.Fatalf("expired-only prune left %d tombstones", len(m.closed))
	}
}

func TestMuxStale404DistinguishesProcessedDuplicateFromMissingUniqueObject(t *testing.T) {
	key := muxStreamKey{ClientID: "client", RunID: "run", StreamID: 7}
	m := &driveMux{
		t:               &Tunnel{},
		streams:         map[muxStreamKey]*muxStream{},
		closed:          map[muxStreamKey]time.Time{},
		seen:            map[string]struct{}{},
		processed:       map[string]struct{}{},
		recvNormalFlows: map[muxStreamKey][]muxObjectMeta{},
	}
	stream := m.registerStream(key.StreamID, key.ClientID, key.RunID, nil)
	t.Cleanup(func() { stream.close() })
	processed := muxObjectMeta{Name: "already-delivered", ID: "deleted-id", ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID}
	m.markSeenProcessed(processed)
	m.dropStaleMuxObject(context.Background(), processed, errors.New("synthetic Drive 404"))
	select {
	case <-stream.done:
		t.Fatal("stale duplicate 404 closed an established stream")
	default:
	}
	if m.isClosedStream(key) {
		t.Fatal("stale duplicate 404 installed a closed-stream tombstone")
	}
	missing := muxObjectMeta{Name: "unique-unprocessed", ID: "missing-id", ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID}
	m.dropStaleMuxObject(context.Background(), missing, errors.New("synthetic Drive 404"))
	select {
	case <-stream.done:
	default:
		t.Fatal("unique unprocessed 404 left stream hung")
	}
	if !m.isClosedStream(key) {
		t.Fatal("missing unique object did not install tombstone")
	}
}
