package skirk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fsHybridEventually(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("counter/ownership condition did not complete")
		case <-ticker.C:
		}
	}
}

func fsHybridMemoryStores(t *testing.T) (*fsHybridPriorityStore, *fsHybridPriorityStore, *fsHybridMemoryCloud, *MemoryStore) {
	t.Helper()
	cloud := newFSHybridMemoryCloud()
	base := NewMemoryStore()
	cfg := fsHybridStoreConfig{Secret: "public-firestore-fixture", Session: [16]byte{1}, Epoch: [16]byte{2}, Direction: DirectionUp, ClientID: "client", RunID: "run", FallbackAfter: time.Second, ACKDelay: time.Millisecond, DiscoveryInterval: time.Millisecond, OperationTimeout: time.Second, Duration: 20 * time.Second}
	client, err := newFSHybridPriorityStore(context.Background(), base, cloud.carrier(0), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Direction = DirectionDown
	exit, err := newFSHybridPriorityStore(context.Background(), base, cloud.carrier(1), cfg)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := exit.Close(); err != nil {
			t.Error(err)
		}
		for _, store := range []*fsHybridPriorityStore{client, exit} {
			targets, err := store.CleanupTargets()
			if err != nil {
				t.Error(err)
				continue
			}
			for _, target := range targets {
				if err := base.DeleteID(context.Background(), target.ID); err != nil {
					t.Error(err)
				}
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = client.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err = exit.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return client, exit, cloud, base
}

func fsHybridStoreRecord(t *testing.T, s *fsHybridPriorityStore, seq uint64, stream uint64) fsHybridRecord {
	t.Helper()
	return fsHybridTestRecord(t, s.state.outgoing, seq, 0, muxFrame{Kind: muxFrameData, StreamID: stream, Seq: 1, Payload: []byte("public message")})
}

func fsHybridReceiveChannel(s *fsHybridPriorityStore) <-chan ObjectInfo {
	ch := make(chan ObjectInfo, 128)
	s.mu.Lock()
	s.receive = func(info ObjectInfo) bool {
		select {
		case ch <- info:
			return true
		default:
			return false
		}
	}
	s.mu.Unlock()
	return ch
}

func TestFSHybridStorePrimaryReceiptReleasesRetentionWithoutDeferredDeletion(t *testing.T) {
	client, exit, _, base := fsHybridMemoryStores(t)
	seen := fsHybridReceiveChannel(exit)
	r := fsHybridStoreRecord(t, client, 1, 7)
	info, err := client.PutObject(context.Background(), r.Name, r.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(info.ID), []byte("firestore-priority:")) {
		t.Fatal("primary was not Firestore")
	}
	var received ObjectInfo
	select {
	case received = <-seen:
	case <-time.After(time.Second):
		t.Fatal("notification did not deliver")
	}
	got, err := exit.GetByID(context.Background(), received.ID)
	if err != nil || !bytes.Equal(got, r.Sealed) {
		t.Fatal("primary cache failed", err)
	}
	if client.Snapshot()["peer_processed_prefix"] != 0 {
		t.Fatal("watch/storage delivery fabricated a processing ACK")
	}
	if err = exit.NoteMuxProcessed(received.Name, received.ID); err != nil {
		t.Fatal(err)
	}
	fsHybridEventually(t, func() bool { return client.Snapshot()["peer_processed_prefix"] == 1 })
	if client.Snapshot()["send_payload_bytes"] != 0 || exit.Snapshot()["receive_payload_bytes"] != 0 {
		t.Fatal("processed payload retained")
	}
	objects, _ := base.List(context.Background(), "")
	if len(objects) != 0 {
		t.Fatal("healthy primary made Drive fallback objects")
	}
	client.mu.Lock()
	dirtyClient := client.dirty
	client.mu.Unlock()
	exit.mu.Lock()
	dirtyExit := exit.dirty
	exit.mu.Unlock()
	if dirtyClient || dirtyExit {
		t.Fatal("processing ACK scheduled ACK-of-ACK work")
	}
	if err = exit.DeleteID(context.Background(), received.ID); err != nil {
		t.Fatal(err)
	}
	if exit.Snapshot()["processed_receipts"] != 1 {
		t.Fatal("deletion changed receipt count")
	}
}

func TestFSHybridStoreLostNotificationFallsBackAndLateSnapshotDoesNotRedispatch(t *testing.T) {
	client, exit, cloud, _ := fsHybridMemoryStores(t)
	seen := fsHybridReceiveChannel(exit)
	cloud.setDrop(0, true)
	r := fsHybridStoreRecord(t, client, 1, 7)
	if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err != nil {
		t.Fatal(err)
	}
	client.forceFallback(r.Name)
	fsHybridEventually(t, func() bool { return client.Snapshot()["fallback_published"] == 1 })
	prefix := muxDirPrefix(client.cfg.Session, DirectionUp, "client", "run") + "p0/"
	if _, err := exit.List(context.Background(), prefix); err != nil {
		t.Fatal(err)
	}
	var received ObjectInfo
	select {
	case received = <-seen:
	case <-time.After(time.Second):
		t.Fatal("owned Drive discovery did not dispatch fallback")
	}
	if err := exit.NoteMuxProcessed(received.Name, received.ID); err != nil {
		t.Fatal(err)
	}
	if exit.Snapshot()["processed_prefix"] != 0 {
		t.Fatal("physical-first processing invented the unknown Firestore sequence")
	}
	cloud.deliver(0)
	fsHybridEventually(t, func() bool { return client.Snapshot()["peer_processed_prefix"] == 1 })
	if len(seen) != 0 || exit.Snapshot()["processed_receipts"] != 1 {
		t.Fatal("delayed snapshot dispatched or receipted the application twice")
	}
}

func TestFSHybridStoreAmbiguousWriteExactReadbackAndIdenticalConditionalRetry(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed_lost_response", false: "uncommitted_exact_retry"}[committed], func(t *testing.T) {
			client, _, cloud, _ := fsHybridMemoryStores(t)
			next := cloud.nextWrite(0)
			cloud.setWriteFault(0, next, fsHybridMemoryFault{Commit: committed, Err: context.DeadlineExceeded})
			r := fsHybridStoreRecord(t, client, 1, 7)
			if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err != nil {
				t.Fatal(err)
			}
			writes, _, _ := cloud.counts()
			if committed {
				if writes[0] != next || client.Snapshot()["exact_readbacks"] != 1 {
					t.Fatal("committed lost response was not proven exactly")
				}
			} else {
				if writes[0] != next+1 {
					t.Fatal("uncommitted write did not take exactly one conditional retry")
				}
				var pair []fsHybridMemoryWrite
				for _, row := range cloud.writeLog() {
					if row.Side == 0 && row.Call >= next {
						pair = append(pair, row)
					}
				}
				if len(pair) != 2 || pair[0].Expected != pair[1].Expected || pair[0].Digest != pair[1].Digest {
					t.Fatal("ambiguous retry changed ciphertext or updateTime fence")
				}
			}
		})
	}
}

func TestFSHybridStoreReadbackFailuresCannotBeLaunderedByAnotherWrite(t *testing.T) {
	for _, failure := range []error{&GoogleAPIError{Op: "public_fixture", Status: 403, Reason: "PERMISSION_DENIED"}, errFSHybridIntegrity, errFSHybridBounds, errors.Join(context.DeadlineExceeded, errFSHybridIntegrity)} {
		t.Run(errorSummary(failure), func(t *testing.T) {
			client, _, cloud, _ := fsHybridMemoryStores(t)
			next := cloud.nextWrite(0)
			read := cloud.nextOwnRead(0)
			cloud.setWriteFault(0, next, fsHybridMemoryFault{Err: context.DeadlineExceeded})
			cloud.setOwnReadFault(0, read, failure)
			r := fsHybridStoreRecord(t, client, 1, 7)
			if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err == nil {
				t.Fatal("fatal readback was hidden")
			}
			fsHybridEventually(t, func() bool { return client.Error() != nil })
			writes, _, _ := cloud.counts()
			if writes[0] != next {
				t.Fatal("another write erased the readback failure")
			}
		})
	}
}

func TestFSHybridStoreRejectsStateErasureAfterAuthenticatedPeerSnapshot(t *testing.T) {
	client, _, _, _ := fsHybridMemoryStores(t)
	client.mu.Lock()
	revision, credit := client.state.peerRevision, client.state.peerCreditSeq
	client.mu.Unlock()
	if err := client.ingest(fsPriorityDocument{UpdateTime: fsHybridMemoryTimestamp(999)}); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("authenticated state erased by empty document", err)
	}
	client.mu.Lock()
	changed := client.state.peerRevision != revision || client.state.peerCreditSeq != credit
	client.mu.Unlock()
	if changed {
		t.Fatal("empty document changed protocol state")
	}
}

func TestFSHybridStoreCloseJoinsUnresponsiveConditionalWriteAndCannotRecreateDeletedDoc(t *testing.T) {
	client, _, cloud, _ := fsHybridMemoryStores(t)
	gate := make(chan struct{})
	next := cloud.nextWrite(0)
	cloud.setWriteFault(0, next, fsHybridMemoryFault{Commit: true, Gate: gate, IgnoreContext: true})
	r := fsHybridStoreRecord(t, client, 1, 7)
	put := make(chan error, 1)
	go func() { _, err := client.PutObject(context.Background(), r.Name, r.Sealed); put <- err }()
	fsHybridEventually(t, func() bool { writes, _, _ := cloud.counts(); return writes[0] == next })
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	fsHybridEventually(t, func() bool { client.mu.Lock(); defer client.mu.Unlock(); return client.closed })
	if _, err := client.CleanupTargets(); !errors.Is(err, errFSHybridBusy) {
		t.Fatal("unjoined writer exposed a cleanup proof", err)
	}
	select {
	case <-closed:
		t.Fatal("Close detached the outstanding conditional write")
	default:
	}
	cloud.deleteDocument(0)
	close(gate)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join released write")
	}
	select {
	case err := <-put:
		if err == nil {
			t.Fatal("canceled publication became success")
		}
	case <-time.After(time.Second):
		t.Fatal("caller did not join")
	}
	cloud.mu.Lock()
	exists := cloud.exists[0]
	cloud.mu.Unlock()
	if exists {
		t.Fatal("late conditional update recreated the deleted document")
	}
	for _, key := range []string{"pending_records", "pending_prefetch", "queued_prefetch", "queued_fallback", "send_payload_bytes", "receive_payload_bytes"} {
		if client.Snapshot()[key] != 0 {
			t.Fatalf("closed owner retained %s", key)
		}
	}
	if client.Snapshot()["joined"] != 1 || client.Snapshot()["outbound_claims"] != 1 {
		t.Fatal("join discarded the finite-epoch replay claim")
	}
	before := client.Snapshot()["processed_receipts"]
	if err := client.NoteMuxProcessed(r.Name, "late-unrecognized-id"); err != nil || client.Snapshot()["processed_receipts"] != before {
		t.Fatal("late callback invented processing ownership", err)
	}
}

func TestFSHybridStoreRejectsUnprocessedDeleteAndLatePriorityList(t *testing.T) {
	client, exit, _, _ := fsHybridMemoryStores(t)
	seen := fsHybridReceiveChannel(exit)
	r := fsHybridStoreRecord(t, client, 1, 7)
	if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err != nil {
		t.Fatal(err)
	}
	var received ObjectInfo
	select {
	case received = <-seen:
	case <-time.After(time.Second):
		t.Fatal("missing primary")
	}
	if err := exit.DeleteID(context.Background(), received.ID); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("unprocessed deletion was accepted", err)
	}
	if exit.Snapshot()["processed_receipts"] != 0 || exit.Snapshot()["receive_payload_bytes"] == 0 {
		t.Fatal("failed cleanup manufactured ACK or evicted unprocessed bytes")
	}
	if err := exit.Close(); err != nil {
		t.Fatal(err)
	}
	prefix := muxDirPrefix(exit.cfg.Session, DirectionUp, "client", "run") + "p0/"
	queries := []func() error{
		func() error { _, e := exit.ListFreshStatus(context.Background(), prefix, time.Time{}); return e },
		func() error {
			_, e := exit.ListFreshPageStatus(context.Background(), prefix, time.Time{}, "")
			return e
		},
		func() error {
			_, e := exit.ListFreshAdaptiveMuxPageStatus(context.Background(), prefix, time.Time{}, "", 1)
			return e
		},
		func() error {
			_, e := exit.ListFreshContainsPageStatus(context.Background(), []string{prefix}, time.Time{}, "", 1)
			return e
		},
	}
	for _, query := range queries {
		if !errors.Is(query(), errFSHybridClosed) {
			t.Fatal("late priority query admitted work")
		}
	}
}

func TestFSHybridStoreReconnectReadsCumulativeStateAndHardStopsAuthorization(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport_reconnect", true: "permission_hard_stop"}[terminal], func(t *testing.T) {
			client, _, cloud, _ := fsHybridMemoryStores(t)
			_, _, before := cloud.counts()
			failure := error(io.EOF)
			if terminal {
				failure = &GoogleAPIError{Op: "public_fixture_watch", Status: 403, Reason: "PERMISSION_DENIED"}
			}
			cloud.failWatch(0, failure)
			if terminal {
				fsHybridEventually(t, func() bool { return client.Error() != nil })
				if client.Snapshot()["watch_reconnects"] != 0 {
					t.Fatal("authorization failure was retried")
				}
			} else {
				fsHybridEventually(t, func() bool {
					_, _, after := cloud.counts()
					return after[0] > before[0] && client.Snapshot()["watch_reconnects"] == 1
				})
				if client.Error() != nil {
					t.Fatal(client.Error())
				}
			}
		})
	}
}

// Real core polling invokes priority and normal discovery sequentially. The
// independently owned page tail may wait for two blocked media readers while
// the actual core method must still discover a normal object.
type fsHybridPagedBase struct {
	*MemoryStore
	gate        <-chan struct{}
	started     atomic.Int64
	normalLists atomic.Int64
	mu          sync.Mutex
	pages       map[string][]ObjectInfo
	tokens      []string
}

func (b *fsHybridPagedBase) GetByID(ctx context.Context, id string) ([]byte, error) {
	b.started.Add(1)
	select {
	case <-b.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.MemoryStore.GetByID(ctx, id)
}
func (b *fsHybridPagedBase) ListFreshContainsPageStatus(ctx context.Context, contains []string, _ time.Time, token string, _ int) (ObjectListInfo, error) {
	if fsHybridPriorityQuery(contains) {
		b.mu.Lock()
		b.tokens = append(b.tokens, token)
		objects := append([]ObjectInfo(nil), b.pages[token]...)
		b.mu.Unlock()
		next := ""
		if token == "" {
			next = "second"
		}
		return ObjectListInfo{Objects: objects, NextPageToken: next, Truncated: next != "", Pages: 1}, nil
	}
	b.normalLists.Add(1)
	objects, err := b.MemoryStore.List(ctx, contains[0])
	var selected []ObjectInfo
	for _, info := range objects {
		if fsHybridContains(info.Name, contains) {
			selected = append(selected, info)
		}
	}
	return ObjectListInfo{Objects: selected, Pages: 1}, err
}
func TestFSHybridStorePageTailRetainedWithoutBlockingActualNormalMuxPoll(t *testing.T) {
	client, exit, _, base := fsHybridMemoryStores(t)
	gate := make(chan struct{})
	paged := &fsHybridPagedBase{MemoryStore: base, gate: gate, pages: make(map[string][]ObjectInfo)}
	// Both owned stores are idle here, before discovery or publication begins.
	exit.mu.Lock()
	exit.base = paged
	exit.mu.Unlock()
	seen := fsHybridReceiveChannel(exit)
	for i := 0; i < 81; i++ {
		r := fsHybridStoreRecord(t, client, uint64(i+1), uint64(i+1))
		ids, _ := base.GenerateObjectIDs(context.Background(), 1)
		info, err := base.PutObjectWithID(context.Background(), ids[0], r.Name, r.Sealed)
		if err != nil {
			t.Fatal(err)
		}
		page := ""
		if i >= 80 {
			page = "second"
		}
		paged.pages[page] = append(paged.pages[page], info)
	}
	prefix := muxDirPrefix(client.cfg.Session, DirectionUp, "client", "run")
	if _, err := exit.List(context.Background(), prefix+"p0/"); err != nil {
		t.Fatal(err)
	}
	fsHybridEventually(t, func() bool { return paged.started.Load() == 2 && exit.Snapshot()["pending_prefetch"] == 66 })
	m, err := newDriveMux(&Tunnel{Data: exit, Secret: client.cfg.Secret, SessionID: client.cfg.Session, ClientID: "client", RunID: "run", Concurrency: 4, PollInterval: time.Millisecond, Logger: log.New(io.Discard, "", 0)}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.closeAll()
	done := make(chan struct{})
	go func() { m.pollMuxObjectsWithNormal(context.Background(), true); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("priority page pressure blocked the actual sequential normal poll")
	}
	if paged.normalLists.Load() == 0 {
		t.Fatal("normal discovery was not executed")
	}
	paged.mu.Lock()
	for _, token := range paged.tokens {
		if token == "second" {
			t.Fatal("backend token advanced past unowned page tail")
		}
	}
	paged.mu.Unlock()
	close(gate)
	ids := make(map[string]bool)
	for len(ids) < 81 {
		select {
		case info := <-seen:
			if ids[info.ID] {
				t.Fatal("page tail dispatched twice")
			}
			ids[info.ID] = true
			if err := exit.NoteMuxProcessed(info.Name, info.ID); err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of81 physical objects dispatched", len(ids))
		}
	}
	if exit.Error() != nil {
		t.Fatal(exit.Error())
	}
	paged.mu.Lock()
	advanced := false
	for _, token := range paged.tokens {
		advanced = advanced || token == "second"
	}
	paged.mu.Unlock()
	if !advanced {
		t.Fatal("retained tail never advanced to next backend page")
	}
}
