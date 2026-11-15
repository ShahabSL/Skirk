package skirk

import (
	"bytes"
	"context"
	"errors"
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
	ch := make(chan ObjectInfo, 64)
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
