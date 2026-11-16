package skirk

import (
	"context"
	"testing"
	"time"
)

// This pair uses the actual route-aware GoogleHTTPClient against a controlled
// projected-v2 backend. The shared TLS/SOCKS test still carries every byte
// through mux and MemoryStore bulk objects; there is no Google performance claim.
func driveV2PriorityStores(t *testing.T, traces ...*fsHybridPhaseTrace) (*fsHybridPriorityStore, *fsHybridPriorityStore, *driveV2PriorityFake, *driveV2PriorityCarrier, *driveV2PriorityCarrier, *MemoryStore) {
	return driveV2PriorityStoresWindow(t, 4, traces...)
}

func driveV2PriorityStoresWindow(t *testing.T, window int, traces ...*fsHybridPhaseTrace) (*fsHybridPriorityStore, *fsHybridPriorityStore, *driveV2PriorityFake, *driveV2PriorityCarrier, *driveV2PriorityCarrier, *MemoryStore) {
	t.Helper()
	if len(traces) > 1 {
		t.Fatal("one shared trace at most")
	}
	backend := newDriveV2PriorityFake()
	// The accelerated 20ms fake-poll correctness fixture covers 40 fresh TLS
	// streams plus two files. Its explicitly selected component upper differs
	// from the unchanged 100k default; neither is a live campaign allowance.
	selectBound := func(cfg *driveV2PriorityConfig) { cfg.MaxQuotaUnitsUpper = 150000 }
	up := driveV2PriorityFixture(t, backend, true, selectBound)
	down := driveV2PriorityFixture(t, backend, false, selectBound)
	// Finish both fresh-file feature probes before either queue owner starts
	// publishing authenticated bootstrap snapshots.
	if err := up.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := down.ProbeOwnCAS(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := NewMemoryStore()
	cfg := fsHybridStoreConfig{Secret: "public-drive-v2-fixture", Session: [16]byte{1}, Epoch: [16]byte{2}, Direction: DirectionUp, ClientID: "client", RunID: "run", FallbackAfter: time.Second, ACKDelay: time.Millisecond, DiscoveryInterval: time.Millisecond, OperationTimeout: time.Second, Duration: 20 * time.Second, MaxSnapshotBytes: driveV2PriorityCipherMax, InlinePerStream: window}
	if len(traces) == 1 {
		cfg.PhaseTrace = traces[0]
	}
	client, err := newFSHybridPriorityStore(context.Background(), base, up, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg.Direction = DirectionDown
	exit, err := newFSHybridPriorityStore(context.Background(), base, down, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := exit.Close(); err != nil {
			t.Error(err)
		}
	})
	ready, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err = client.WaitReady(ready); err != nil {
		t.Fatal("client bootstrap", err)
	}
	if err = exit.WaitReady(ready); err != nil {
		t.Fatal("exit bootstrap", err)
	}
	return client, exit, backend, up, down, base
}

func TestDriveV2PriorityRealTLS16MiBEachDirectionMixedTextAndLostResponse(t *testing.T) {
	trace, err := newFSHybridPhaseTrace(1024)
	if err != nil {
		t.Fatal(err)
	}
	client, exit, backend, up, down, base := driveV2PriorityStores(t, trace)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("V2 owned local diagnostic: up=%+v down=%+v client=%v exit=%v", up.Snapshot(), down.Snapshot(), client.Snapshot(), exit.Snapshot())
		}
	})
	f := fsHybridFullMuxFixtureWithStores(t, client, exit, base, true)
	before := func() {
		backend.mu.Lock()
		// Commit one exact prepared snapshot but lose its HTTP response. Also
		// withhold conditional peer visibility until the same identity's
		// immutable Drive fallback has reached the actual TLS destination.
		backend.faultWriteID = up.cfg.OwnID
		backend.nextWriteAfter = context.DeadlineExceeded
		backend.frozenReadID = up.cfg.OwnID
		backend.mu.Unlock()
	}
	afterGET := func() {
		backend.mu.Lock()
		backend.frozenReadID = ""
		backend.faultReadID = up.cfg.OwnID
		backend.nextRead = context.DeadlineExceeded
		backend.mu.Unlock()
		select {
		case down.wake <- struct{}{}:
		default:
		}
		fsHybridEventually(t, func() bool { return exit.Snapshot()["watch_reconnects"] == 1 })
	}
	afterStop := func() {
		events := trace.snapshot()
		if events.Limit != 1024 || len(events.Events) == 0 || len(events.Events) > events.Limit || events.Dropped < 0 {
			t.Fatal("priority phase trace bounded coverage", events.Dropped, len(events.Events))
		}
		seen := make(map[string]int)
		byClaim := make(map[string]map[string]bool)
		streamTagged := false
		for _, event := range events.Events {
			seen[event.Phase]++
			key := string([]byte{event.Direction}) + event.Claim
			if byClaim[key] == nil {
				byClaim[key] = make(map[string]bool)
			}
			byClaim[key][event.Phase] = true
			if event.Phase == "queued_inline" && event.StreamCount > 0 && len(event.StreamIDs) > 0 && len(event.StreamIDs) <= 4 {
				streamTagged = true
			}
			if len(event.Claim) != 16 || event.Sequence > fsHybridClaimsMax || event.ElapsedMS < 0 {
				t.Fatal("invalid sanitized phase event", event)
			}
		}
		for _, phase := range []string{"queued_inline", "patch_started", "patch_completed", "peer_metadata_observed", "receiver_processed", "sender_processing_ack", "fallback_started", "fallback_completed", "peer_physical_observed"} {
			if seen[phase] == 0 {
				t.Fatal("missing real fake-route phase", phase, seen)
			}
		}
		completeClaim := false
		for _, phases := range byClaim {
			if phases["queued_inline"] && phases["patch_started"] && phases["patch_completed"] && phases["peer_metadata_observed"] && phases["receiver_processed"] && phases["sender_processing_ack"] {
				completeClaim = true
				break
			}
		}
		if !completeClaim || !streamTagged {
			t.Fatal("no correlated inline claim or bounded stream identity", completeClaim, streamTagged)
		}
		if client.Snapshot()["exact_readbacks"] < 1 || client.Snapshot()["fallback_published"] < 1 || exit.Snapshot()["watch_reconnects"] != 1 {
			t.Fatal("v2 workload did not traverse declared lost-response/fallback/reconnect faults", client.Snapshot(), exit.Snapshot())
		}
		for _, carrier := range []*driveV2PriorityCarrier{up, down} {
			s := carrier.Snapshot()
			// One deliberately lost response per carrier has no HTTP result:
			// UP's committed write and DOWN's failed peer poll. Retain both
			// errors, and distinguish them from ordinary owned shutdown cancel.
			if s.Joined != 1 || s.ProbePassed != 1 || s.Errors != 1 || s.HTTP2 > s.Visible || s.HTTP2+s.Errors+s.Canceled < s.Visible || s.GuardDenied != 0 || s.QuotaUnitsUpper > carrier.cfg.MaxQuotaUnitsUpper {
				t.Fatal("v2 ownership/guard evidence", s)
			}
		}
	}
	runFSHybridTLSMixedWorkload(t, f, before, afterGET, afterStop)
}
