package skirk

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

type deliveryLostPatchResponseStore struct{ *shadowSlotMemoryStore }

func (s *deliveryLostPatchResponseStore) PatchMetadataSlot(ctx context.Context, id, name, description string) (MetadataSlotInfo, error) {
	info, err := s.shadowSlotMemoryStore.PatchMetadataSlot(ctx, id, name, description)
	if err != nil {
		return info, err
	}
	return info, context.DeadlineExceeded
}

func deliverySlotPair(t *testing.T, store *shadowSlotMemoryStore) (*driveMux, *driveMux, *muxSlotLink, *muxSlotLink) {
	t.Helper()
	client := shadowSlotTestMux(t, store, "client", true)
	exit := shadowSlotTestMux(t, store, "exit", true)
	client.t.MetadataSlotDeliveryV1 = true
	exit.t.MetadataSlotDeliveryV1 = true
	client.t.CleanupProcessed = true
	exit.t.CleanupProcessed = true
	client.ensureClientSlot(context.Background())
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}
	up := client.slotLink(key)
	if up == nil || !up.delivery || !up.setActive() {
		t.Fatal("delivery slot setup failed")
	}
	peer, err := newMetadataSlotPeer(client.t.Secret, client.t.SessionID, DirectionDown, key.clientID, key.runID, up.generation, metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	down := &muxSlotLink{
		key: key, generation: up.generation, upID: up.upID, downID: up.downID,
		upName: up.upName, downName: up.downName, peer: peer, active: true,
		delivery: true, shadowQ: make(chan slotShadowRecord, muxSlotShadowQueue), ackWake: make(chan struct{}, 1),
	}
	exit.slots = map[muxSlotRunKey]*muxSlotLink{key: down}
	return client, exit, up, down
}

func deliveryBatch(t *testing.T, client *driveMux, payload []byte) muxPreparedUpload {
	t.Helper()
	frame := muxFrame{Kind: muxFrameData, ClientID: client.t.ClientID, RunID: client.t.RunID, StreamID: 7, Seq: 1, Payload: payload, Priority: true}
	lane := client.lanes[client.frameLane(frame)]
	batch, err := lane.prepareUploadBatchV4(context.Background(), []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.priority || len(batch.sealed) > metadataSlotMaxDescription {
		t.Fatal("test batch is not slot eligible")
	}
	return batch
}

func TestMuxSlotDeliveryNegotiationRejectsShadowPeer(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client := shadowSlotTestMux(t, store, "client", true)
	client.t.MetadataSlotDeliveryV1 = true
	client.ensureClientSlot(context.Background())
	defer client.stopSlotLinks()
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}
	link := client.slotLink(key)
	if link == nil {
		t.Fatal("delivery client did not create a slot")
	}
	capability := shadowSlotOffer(t, client)
	decoded, err := decodeMuxSlotCapability(capability.Payload)
	if err != nil || !decoded.delivery {
		t.Fatalf("delivery marker missing: %+v %v", decoded, err)
	}
	shadowOnly := shadowSlotTestMux(t, store, "exit", true)
	shadowOnly.handleSlotCapability(context.Background(), capability)
	if shadowOnly.slotLink(key) != nil {
		t.Fatal("shadow-only peer accepted a delivery capability")
	}
}

func TestMuxSlotDeliverySlotFirstAndLateImmutableID(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		exit.stopSlotLinks()
		client.stopSlotLinks()
	}()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("slot first"))
	go client.runSlotShadow(ctx, up)
	go exit.runSlotShadow(ctx, down)
	go exit.runReceiveWorker(ctx, true)
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, batch.name); err == nil {
		t.Fatal("ACKed slot delivery also uploaded an immutable object")
	}
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("delivered %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slot bytes did not reach mux stream")
	}
	if len(exit.cleanupQueue) != 0 {
		t.Fatal("virtual slot record entered cleanup")
	}
	// Evict the mux discovery set before the physical fallback appears. The
	// independent slot identity cache must still claim and verify it.
	exit.seenMu.Lock()
	for i := 0; i < 200000; i++ {
		exit.seen[fmt.Sprintf("filler-%d", i)] = struct{}{}
	}
	exit.seenMu.Unlock()
	exit.markSeen("evict-seen")
	if !exit.isKnown(batch.name) {
		t.Fatal("slot identity disappeared with mux discovery eviction")
	}
	late, err := store.PutObjectWithID(ctx, "late-physical-id", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(late)
	if !ok || !exit.isKnownMuxObject(meta) {
		t.Fatal("late immutable object was not deduplicated")
	}
	select {
	case task := <-exit.cleanupQueue:
		if task.id != "late-physical-id" || task.name != batch.name {
			t.Fatalf("wrong exact cleanup target: %+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("late immutable ID did not enter exact cleanup")
	}
	if stream.expectedRecvSeq() != 2 || len(stream.inbound) != 0 {
		t.Fatal("late immutable object caused duplicate stream delivery")
	}
}

func TestMuxSlotDeliveryTimeoutFallsBackToSameSealedObject(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); client.stopSlotLinks() }()
	batch := deliveryBatch(t, client, []byte("fallback identity"))
	go client.runSlotShadowWriter(ctx, up)
	started := time.Now()
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < muxSlotDeliveryACKWait {
		t.Fatal("sender skipped fallback before any processing ACK")
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("fallback changed sealed mux identity: %v", err)
	}
}

func TestMuxSlotDeliveryImmutableFirstAdvancesStagedACK(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("immutable first"))
	info, err := store.PutObjectWithID(context.Background(), "physical-first-id", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(info)
	if !ok || !exit.claimQueuedObject(meta) {
		t.Fatal("physical object was not claimed")
	}
	if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetMetadataSlot(context.Background(), up.upID, up.upName)
	if err != nil {
		t.Fatal(err)
	}
	records, err := down.peer.receive(snapshot.Description)
	if err != nil || len(records) != 1 {
		t.Fatalf("slot record was not staged: %d %v", len(records), err)
	}
	exit.queueSlotDeliveryHead(context.Background(), down)
	if down.peer.acked() != 0 {
		t.Fatal("queued physical claim prematurely ACKed slot")
	}
	if err := exit.processMuxObject(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	if down.peer.acked() != 1 {
		t.Fatal("successful immutable delivery did not ACK staged slot")
	}
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("delivered %q", got)
		}
	default:
		t.Fatal("physical object was not delivered")
	}
}

func TestMuxSlotDeliveryPhysicalArrivesWhileVirtualQueued(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("queued virtual"))
	if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetMetadataSlot(context.Background(), up.upID, up.upName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.peer.receive(snapshot.Description); err != nil {
		t.Fatal(err)
	}
	exit.queueSlotDeliveryHead(context.Background(), down)
	physical, err := store.PutObjectWithID(context.Background(), "physical-during-queue", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(physical)
	if !ok || !exit.isKnownMuxObject(meta) {
		t.Fatal("physical object did not join virtual claim")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { exit.runReceiveWorker(ctx, true); close(done) }()
	deadline := time.After(2 * time.Second)
	for down.peer.acked() != 1 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("virtual worker did not process")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case task := <-exit.cleanupQueue:
		if task.id != "physical-during-queue" || task.name != batch.name {
			t.Fatalf("wrong physical cleanup: %+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("queued physical object was not verified and cleaned")
	}
	cancel()
	<-done
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("delivered %q", got)
		}
	default:
		t.Fatal("virtual mux delivery was lost")
	}
}

func TestMuxSlotDeliveryLostPatchResponseUsesReadback(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, base)
	store := &deliveryLostPatchResponseStore{shadowSlotMemoryStore: base}
	client.t.Data = store
	exit.t.Data = store
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		exit.stopSlotLinks()
		client.stopSlotLinks()
	}()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("ambiguous patch"))
	go client.runSlotShadow(ctx, up)
	go exit.runSlotShadow(ctx, down)
	go exit.runReceiveWorker(ctx, true)
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Get(ctx, batch.name); err == nil {
		t.Fatal("resolved PATCH caused an immutable upload")
	}
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("delivered %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readback-resolved PATCH did not deliver")
	}
}

func TestMuxSlotDeliveryOverflowUsesImmutableBatch(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); client.stopSlotLinks() }()
	batch := deliveryBatch(t, client, bytes.Repeat([]byte("x"), 7000))
	go client.runSlotShadowWriter(ctx, up)
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("overflow did not preserve immutable bytes: %v", err)
	}
	if up.peer.outboundAcked() != 0 {
		t.Fatal("overflow fabricated an ACK")
	}
}

func TestMuxSlotDeliveryInvalidSlotLeavesImmutableClaimFree(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	batch := deliveryBatch(t, client, []byte("invalid record"))
	bad := bytes.Clone(batch.sealed)
	bad[0] ^= 1
	if _, err := up.peer.enqueue(batch.name, bad); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetMetadataSlot(context.Background(), up.upID, up.upName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.peer.receive(snapshot.Description); err != nil {
		t.Fatal(err)
	}
	exit.queueSlotDeliveryHead(context.Background(), down)
	if exit.isKnown(batch.name) {
		t.Fatal("invalid slot record claimed immutable name")
	}
	physical, err := store.PutObjectWithID(context.Background(), "valid-fallback-id", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(physical)
	if !ok || !exit.claimQueuedObject(meta) {
		t.Fatal("valid immutable fallback could not claim after invalid slot")
	}
}

func TestMuxSlotDeliveryQueuedPhysicalSurvivesVirtualFailureAndCursor(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, _, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("recover despite cursor"))
	physical, err := store.PutObjectWithID(context.Background(), "cursor-skipped-physical", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	physicalMeta, ok := parseMuxObjectInfo(physical)
	if !ok {
		t.Fatal("physical mux name invalid")
	}
	virtual, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name})
	if !ok {
		t.Fatal("virtual mux name invalid")
	}
	virtual.SlotLink, virtual.SlotSeq = down, 1
	virtual.SlotSealed = bytes.Clone(batch.sealed)
	virtual.SlotSealed[0] ^= 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !exit.enqueueMuxObject(ctx, virtual) || !exit.isKnownMuxObject(physicalMeta) {
		t.Fatal("physical copy was not captured behind virtual claim")
	}
	exit.priorityListMu.Lock()
	exit.priorityListSince = time.Now().Add(time.Hour)
	exit.priorityListMu.Unlock()
	done := make(chan struct{})
	go func() { exit.runReceiveWorker(ctx, true); close(done) }()
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("requeued physical delivered %q", got)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("cursor-skipped physical was not requeued after virtual failure")
	}
	select {
	case task := <-exit.cleanupQueue:
		if task.id != physical.ID {
			t.Fatalf("wrong cleanup ID: %+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("requeued physical ID was not cleaned")
	}
	cancel()
	<-done
}

func TestMuxSlotDeliveryNeverReadyOpenFallsBackWithoutACK(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		exit.stopSlotLinks()
		client.stopSlotLinks()
	}()
	batch := deliveryBatch(t, client, []byte("wait for open"))
	go client.runSlotShadow(ctx, up)
	go exit.runSlotShadow(ctx, down)
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("never-ready OPEN lost exact immutable fallback: %v", err)
	}
	if down.peer.acked() != 0 || exit.isKnown(batch.name) {
		t.Fatal("slot ACKed or claimed DATA before destination OPEN was ready")
	}
}

func TestMuxSlotDeliveryQueuedDuplicatePhysicalIDsGetExactCleanup(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("two physical IDs"))
	if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetMetadataSlot(context.Background(), up.upID, up.upName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.peer.receive(snapshot.Description); err != nil {
		t.Fatal(err)
	}
	exit.queueSlotDeliveryHead(context.Background(), down)
	if _, err := store.PutObjectWithID(context.Background(), "duplicate-a", batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	store.MemoryStore.mu.Lock()
	store.MemoryStore.ids["duplicate-b"] = batch.name
	store.MemoryStore.mu.Unlock()
	for _, id := range []string{"duplicate-a", "duplicate-b"} {
		meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, ID: id})
		if !ok || !exit.isKnownMuxObject(meta) {
			t.Fatalf("physical %s was not retained", id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { exit.runReceiveWorker(ctx, true); close(done) }()
	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case task := <-exit.cleanupQueue:
			if task.name != batch.name || (task.id != "duplicate-a" && task.id != "duplicate-b") {
				t.Fatalf("unexpected cleanup target: %+v", task)
			}
			got[task.id] = true
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("only cleaned physical IDs %v", got)
		}
	}
	cancel()
	<-done
}

func TestMuxSlotDeliveryDeferredPhysicalStorageIsBounded(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, _, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	batch := deliveryBatch(t, client, []byte("bounded capture"))
	virtual, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name})
	if !ok {
		t.Fatal("virtual mux name invalid")
	}
	virtual.SlotLink, virtual.SlotSeq, virtual.SlotSealed = down, 1, batch.sealed
	if !exit.enqueueMuxObject(context.Background(), virtual) {
		t.Fatal("virtual record was not queued")
	}
	for i := 0; i < muxSlotDeferredPhysicalMax+1; i++ {
		meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, ID: fmt.Sprintf("physical-%02d", i), Updated: time.Now().Format(time.RFC3339Nano)})
		if !ok || !exit.isKnownMuxObject(meta) {
			t.Fatal("queued physical metadata was not suppressed")
		}
	}
	exit.seenMu.Lock()
	count := len(exit.deferredPhysical[batch.name])
	_, overflow := exit.deferredOverflow[batch.name]
	exit.seenMu.Unlock()
	if count != muxSlotDeferredPhysicalMax || !overflow {
		t.Fatalf("deferred physical retention count=%d overflow=%t", count, overflow)
	}
}

func TestMuxSlotDeliveryFailedVirtualTriesNextPhysicalID(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, _, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("second physical survives"))
	valid, err := store.PutObjectWithID(context.Background(), "b-valid", batch.name, batch.sealed)
	if err != nil {
		t.Fatal(err)
	}
	virtual, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name})
	if !ok {
		t.Fatal("virtual mux name invalid")
	}
	virtual.SlotLink, virtual.SlotSeq = down, 1
	virtual.SlotSealed = bytes.Clone(batch.sealed)
	virtual.SlotSealed[0] ^= 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !exit.enqueueMuxObject(ctx, virtual) {
		t.Fatal("virtual record was not queued")
	}
	stale, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, ID: "a-stale"})
	if !ok || !exit.isKnownMuxObject(stale) || !exit.isKnownMuxObject(mustParseMuxMeta(t, valid)) {
		t.Fatal("physical candidates were not deferred")
	}
	done := make(chan struct{})
	go func() { exit.runReceiveWorker(ctx, true); close(done) }()
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatalf("delivered %q", got)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("valid second physical ID was not tried")
	}
	cancel()
	<-done
}

func mustParseMuxMeta(t *testing.T, info ObjectInfo) muxObjectMeta {
	t.Helper()
	meta, ok := parseMuxObjectInfo(info)
	if !ok {
		t.Fatalf("invalid mux object name %q", info.Name)
	}
	return meta
}

func TestMuxSlotDeliveryLatePhysicalVerifiesMediaDespiteMatchingInline(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, _, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	batch := deliveryBatch(t, client, []byte("media identity"))
	virtual := mustParseMuxMeta(t, ObjectInfo{Name: batch.name})
	virtual.SlotLink, virtual.SlotSeq, virtual.SlotSealed = down, 1, batch.sealed
	exit.markSeenProcessed(virtual)
	wrongMedia := bytes.Clone(batch.sealed)
	wrongMedia[0] ^= 1
	if _, err := store.PutObjectWithID(context.Background(), "wrong-media-id", batch.name, wrongMedia); err != nil {
		t.Fatal(err)
	}
	physical := mustParseMuxMeta(t, ObjectInfo{Name: batch.name, ID: "wrong-media-id", InlineData: batch.sealed})
	if !exit.isKnownMuxObject(physical) {
		t.Fatal("processed slot identity was not known")
	}
	select {
	case <-stream.done:
	case <-time.After(2 * time.Second):
		t.Fatal("matching inline description hid different physical media")
	}
}
