package skirk

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type asyncBlockedPatchStore struct {
	*shadowSlotMemoryStore
	entered chan struct{}
	release chan struct{}
}

type asyncUncommittedPatchStore struct{ *shadowSlotMemoryStore }

type asyncFailingImmutableStore struct {
	*shadowSlotMemoryStore
	calls       atomic.Int64
	beforeError func()
}

func (s *asyncFailingImmutableStore) PutObject(context.Context, string, []byte) (ObjectInfo, error) {
	s.calls.Add(1)
	if s.beforeError != nil {
		s.beforeError()
	}
	return ObjectInfo{}, errors.New("injected immutable fallback failure")
}

func (s *asyncUncommittedPatchStore) PatchMetadataSlot(context.Context, string, string, string) (MetadataSlotInfo, error) {
	return MetadataSlotInfo{}, context.DeadlineExceeded
}

func (s *asyncBlockedPatchStore) PatchMetadataSlot(ctx context.Context, id, name, description string) (MetadataSlotInfo, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return s.shadowSlotMemoryStore.PatchMetadataSlot(ctx, id, name, description)
	case <-ctx.Done():
		return MetadataSlotInfo{}, ctx.Err()
	}
}

func asyncAckBatch(t *testing.T, client *driveMux, seq uint64) muxPreparedUpload {
	t.Helper()
	frame := muxFrame{Kind: muxFrameData, ClientID: client.t.ClientID, RunID: client.t.RunID, StreamID: 7, Seq: seq, Payload: []byte("async exact payload"), Priority: true, Ack: newMuxFrameAck()}
	batch, err := client.lanes[client.frameLane(frame)].prepareUploadBatchV4(context.Background(), []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func waitAsyncIdle(t *testing.T, mux *driveMux) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mux.slotAsyncMu.Lock()
		count, streams := mux.slotAsyncCount, len(mux.slotAsyncPending)
		mux.slotAsyncMu.Unlock()
		if count == 0 && streams == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("async ownership was not released")
}

func TestSlotAsyncQueueDoesNotACKBeforePublicationOrBlockLane(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, base)
	client.t.MetadataSlotAsyncV1 = true
	blocked := &asyncBlockedPatchStore{shadowSlotMemoryStore: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
	client.t.Data = blocked
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	batch := asyncAckBatch(t, client, 1)
	start := time.Now()
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("handoff blocked the lane")
	}
	select {
	case err := <-batch.frames[0].Ack.ch:
		t.Fatalf("memory queue acknowledged: %v", err)
	default:
	}
	go client.runSlotShadowWriter(ctx, up)
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("PATCH did not start")
	}
	select {
	case err := <-batch.frames[0].Ack.ch:
		t.Fatalf("unpublished PATCH acknowledged: %v", err)
	default:
	}
	close(blocked.release)
	select {
	case err := <-batch.frames[0].Ack.ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication did not ACK")
	}
	// No receiver exists: processing ACK cannot arrive. Ownership must publish
	// the exact immutable fallback after the bounded processing wait.
	waitAsyncIdle(t, client)
	stored, err := base.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("fallback identity changed: %v", err)
	}
}

func TestSlotAsyncPerStreamOverflowUsesImmediateImmutable(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, _, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	for i := 0; i < muxSlotAsyncMaxPerStream; i++ {
		batch := asyncAckBatch(t, client, uint64(i+1))
		if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
			t.Fatal("early capacity rejection")
		}
	}
	batch := asyncAckBatch(t, client, 5)
	start := time.Now()
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("capacity fallback waited for processing ACK")
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatal("capacity fallback was not immutable")
	}
}

func TestSlotAsyncCancellationFailsACKAndReleasesOwnership(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, _, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	batch := asyncAckBatch(t, client, 1)
	if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
		t.Fatal("handoff failed")
	}
	cancel()
	select {
	case err := <-batch.frames[0].Ack.ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel ACK=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel ACK absent")
	}
	waitAsyncIdle(t, client)
	client.stopSlotLinks()
}

func TestSlotAsyncAmbiguousPATCHReadbackConfirmsPublication(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	client.t.Data = &deliveryLostPatchResponseStore{store}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	go client.runSlotShadowWriter(ctx, up)
	batch := asyncAckBatch(t, client, 1)
	if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
		t.Fatal("handoff failed")
	}
	select {
	case err := <-batch.frames[0].Ack.ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("exact PATCH readback not acknowledged")
	}
}

func TestSlotAsyncBatchesQueuedRecordsBeforePATCH(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	var batches []muxPreparedUpload
	for i := 1; i <= 3; i++ {
		b := asyncAckBatch(t, client, uint64(i))
		batches = append(batches, b)
		if !client.lanes[client.frameLane(b.frames[0])].handoffSlotDelivery(ctx, b) {
			t.Fatal("handoff failed")
		}
	}
	go client.runSlotShadowWriter(ctx, up)
	for _, b := range batches {
		select {
		case err := <-b.frames[0].Ack.ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("batched publication ACK missing")
		}
	}
	info, err := store.GetMetadataSlot(ctx, up.upID, up.upName)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := newMetadataSlotCodec(client.t.Secret, client.t.SessionID, DirectionUp, client.t.ClientID, client.t.RunID, up.generation, metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := codec.open(info.Description)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.records) != 3 {
		t.Fatalf("first cumulative PATCH has %d records,want3", len(snapshot.records))
	}
}

func TestSlotAsyncUnresolvedPATCHFallsBackWithoutStorageACK(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	client.t.Data = &asyncUncommittedPatchStore{store}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	batch := asyncAckBatch(t, client, 1)
	go client.runSlotShadowWriter(ctx, up)
	if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
		t.Fatal("handoff failed")
	}
	select {
	case err := <-batch.frames[0].Ack.ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("immutable fallback ACK missing")
	}
	waitAsyncIdle(t, client)
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatal("unresolved publication falsely ACKed without immutable fallback")
	}
}

func TestSlotAsyncGlobalCapacityRejectsHandoff(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, _, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	defer client.stopSlotLinks()
	client.slotAsyncCount = muxSlotAsyncMaxPending
	batch := asyncAckBatch(t, client, 1)
	if client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(context.Background(), batch) {
		t.Fatal("global ownership bound bypassed")
	}
	if client.slotAsyncCount != muxSlotAsyncMaxPending || len(client.slotAsyncPending) != 0 {
		t.Fatal("rejected handoff mutated ownership")
	}
}

func TestSlotPrimaryWaitsAfterPublicationAndFallsBackOnWriterFailure(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotPrimaryV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
	go client.runSlotShadowWriter(ctx, up)
	batch := asyncAckBatch(t, client, 1)
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-batch.frames[0].Ack.ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication ACK missing")
	}
	time.Sleep(muxSlotDeliveryACKWait + 100*time.Millisecond)
	if _, err := store.Get(ctx, batch.name); err == nil {
		t.Fatal("primary publication duplicated into immutable on processing deadline")
	}
	client.slotAsyncMu.Lock()
	pending := client.slotAsyncCount
	client.slotAsyncMu.Unlock()
	if pending != 1 {
		t.Fatalf("unacknowledged primary record lost ownership: %d", pending)
	}
	client.closeSlotLink(up)
	waitAsyncIdle(t, client)
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("writer failure did not preserve exact fallback: %v", err)
	}
}

func TestSlotWriterBudgetExitStopsHandoffsAndWakesQueuedOwners(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	defer client.stopSlotLinks()
	up.patches = muxSlotShadowMaxPatches
	var owners []slotShadowRecord
	for i := 1; i <= 2; i++ {
		batch := asyncAckBatch(t, client, uint64(i))
		r := slotShadowRecord{name: batch.name, sealed: batch.sealed, deliveryDone: make(chan error, 1), publicationDone: make(chan error, 1)}
		owners = append(owners, r)
		up.shadowQ <- r
	}
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); client.runSlotShadowWriter(context.Background(), up) }()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer budget did not stop writer")
	}
	if up.canSend() {
		t.Fatal("stopped writer remains sendable while reader active")
	}
	if !up.isActive() {
		t.Fatal("test did not isolate live reader from stopped writer")
	}
	for _, r := range owners {
		select {
		case err := <-r.deliveryDone:
			if err == nil {
				t.Fatal("stopped writer ACKed processing")
			}
		default:
			t.Fatal("queued owner not woken")
		}
	}
	batch := asyncAckBatch(t, client, 3)
	if client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(context.Background(), batch) {
		t.Fatal("handoff accepted after writer exit")
	}
}

func TestSlotAsyncLateProcessingACKPreventsResetAfterFallbackError(t *testing.T) {
	for _, duringBackoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-error-return", true: "during-backoff"}[duringBackoff], func(t *testing.T) {
			store := newShadowSlotMemoryStore()
			client, _, up, _ := deliverySlotPair(t, store)
			client.t.MetadataSlotAsyncV1 = true
			failed := &asyncFailingImmutableStore{shadowSlotMemoryStore: store}
			client.t.Data = failed
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); waitAsyncIdle(t, client); client.stopSlotLinks() }()
			failed.beforeError = func() {
				up.mu.Lock()
				done := up.deliveryWaiters[1]
				up.mu.Unlock()
				if done == nil {
					t.Error("fallback began without processing waiter")
					return
				}
				if duringBackoff {
					go func() { time.Sleep(10 * time.Millisecond); done <- nil }()
				} else {
					done <- nil
				}
			}
			go client.runSlotShadowWriter(ctx, up)
			batch := asyncAckBatch(t, client, 1)
			if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
				t.Fatal("handoff failed")
			}
			waitAsyncIdle(t, client)
			if got := failed.calls.Load(); got != 1 {
				t.Fatalf("immutable fallback retried after slot ACK: %d", got)
			}
			if client.isClosedStream(batch.frames[0].key()) {
				t.Fatal("healthy slot-delivered stream reset after fallback error")
			}
			select {
			case err := <-batch.frames[0].Ack.ch:
				if err != nil {
					t.Fatalf("publication ACK changed to error: %v", err)
				}
			default:
				t.Fatal("upload ACK missing")
			}
		})
	}
}

func TestSlotAsyncSlotFirstLateImmutableIsDeduplicated(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); waitAsyncIdle(t, client); exit.stopSlotLinks(); client.stopSlotLinks() }()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	go client.runSlotShadow(ctx, up)
	go exit.runSlotShadow(ctx, down)
	go exit.runReceiveWorker(ctx, true)
	batch := asyncAckBatch(t, client, 1)
	if !client.lanes[client.frameLane(batch.frames[0])].handoffSlotDelivery(ctx, batch) {
		t.Fatal("handoff failed")
	}
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatal("wrong bytes")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slot did not deliver")
	}
	waitAsyncIdle(t, client)
	late, err := store.PutObjectWithID(ctx, "async-late-id", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(late)
	if !ok || !exit.isKnownMuxObject(meta) {
		t.Fatal("late physical not deduplicated")
	}
	if len(stream.inbound) != 0 || stream.expectedRecvSeq() != 2 {
		t.Fatal("late copy duplicated stream data")
	}
}
