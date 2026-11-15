package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// shadowSlotMemoryStore keeps slot descriptions separate from immutable mux
// objects, as Drive does. Only the latter can reach the normal mux reader.
type shadowSlotMemoryStore struct {
	*MemoryStore
	mu          sync.Mutex
	slots       map[string]MetadataSlotInfo
	createdIDs  []string
	patchedIDs  []string
	deletedIDs  []string
	getCalls    int
	createError error
	createGate  <-chan struct{}
	createStart chan struct{}
	startOnce   sync.Once
}

func newShadowSlotMemoryStore() *shadowSlotMemoryStore {
	return &shadowSlotMemoryStore{MemoryStore: NewMemoryStore(), slots: make(map[string]MetadataSlotInfo)}
}

func (*shadowSlotMemoryStore) MetadataSlotEnabled() bool { return true }

func (s *shadowSlotMemoryStore) CreateMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	if s.createGate != nil {
		if s.createStart != nil {
			s.startOnce.Do(func() { close(s.createStart) })
		}
		select {
		case <-s.createGate:
		case <-ctx.Done():
			return MetadataSlotInfo{ID: id, Name: name}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createdIDs = append(s.createdIDs, id)
	if s.createError != nil {
		return MetadataSlotInfo{ID: id, Name: name}, s.createError
	}
	if !validMetadataSlotID(id) || !validMetadataSlotName(name) {
		return MetadataSlotInfo{}, errors.New("invalid slot identity")
	}
	if _, exists := s.slots[id]; exists {
		return MetadataSlotInfo{}, errors.New("slot ID already exists")
	}
	info := MetadataSlotInfo{ID: id, Name: name, Version: 1, Parents: []string{"mailbox"}}
	s.slots[id] = info
	return info, nil
}

func (s *shadowSlotMemoryStore) GetMetadataSlot(_ context.Context, id, expectedName string) (MetadataSlotInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	info, exists := s.slots[id]
	if !exists {
		return MetadataSlotInfo{}, &GoogleAPIError{Op: "slot get", Status: 404, Reason: "notFound"}
	}
	if !validMetadataSlotID(id) || !validMetadataSlotName(expectedName) || info.Name != expectedName {
		return MetadataSlotInfo{}, errors.New("slot name or ID mismatch")
	}
	return info, nil
}

func (s *shadowSlotMemoryStore) PatchMetadataSlot(_ context.Context, id, expectedName, description string) (MetadataSlotInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, exists := s.slots[id]
	if !exists || info.Name != expectedName || !validMetadataSlotDescription(description) {
		return MetadataSlotInfo{}, errors.New("slot PATCH identity or description invalid")
	}
	info.Description = description
	info.Version++
	s.slots[id] = info
	s.patchedIDs = append(s.patchedIDs, id)
	return info, nil
}

func (s *shadowSlotMemoryStore) DeleteID(ctx context.Context, id string) error {
	s.mu.Lock()
	s.deletedIDs = append(s.deletedIDs, id)
	_, slot := s.slots[id]
	if slot {
		delete(s.slots, id)
	}
	s.mu.Unlock()
	if slot {
		return nil
	}
	return s.MemoryStore.DeleteID(ctx, id)
}

type shadowLostCreateResponseStore struct {
	*shadowSlotMemoryStore
	readUsedFreshContext bool
}

func (s *shadowLostCreateResponseStore) CreateMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	info, err := s.shadowSlotMemoryStore.CreateMetadataSlot(ctx, id, name)
	if err != nil {
		return info, err
	}
	// Simulate a create that committed even though its canceled caller lost the
	// response. Exact-ID readback must not reuse that canceled context.
	return info, context.Canceled
}

func (s *shadowLostCreateResponseStore) GetMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	if err := ctx.Err(); err != nil {
		return MetadataSlotInfo{}, err
	}
	s.readUsedFreshContext = true
	return s.shadowSlotMemoryStore.GetMetadataSlot(ctx, id, name)
}

type shadowAmbiguousCreateStore struct {
	*shadowSlotMemoryStore
	hideFirstRead bool
}

func (s *shadowAmbiguousCreateStore) CreateMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	info, err := s.shadowSlotMemoryStore.CreateMetadataSlot(ctx, id, name)
	if err != nil {
		return info, err
	}
	return info, context.DeadlineExceeded
}

func (s *shadowAmbiguousCreateStore) GetMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	if s.hideFirstRead {
		s.hideFirstRead = false
		return MetadataSlotInfo{}, &GoogleAPIError{Op: "slot get", Status: 404, Reason: "notFound"}
	}
	return s.shadowSlotMemoryStore.GetMetadataSlot(ctx, id, name)
}

func shadowSlotTestMux(t *testing.T, store *shadowSlotMemoryStore, role string, enabled bool) *driveMux {
	t.Helper()
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	sendDir, recvDir := DirectionUp, DirectionDown
	if role == "exit" {
		sendDir, recvDir = DirectionDown, DirectionUp
	}
	mux, err := newDriveMux(&Tunnel{
		Data: store, Secret: "slot-integration-test-secret", SessionID: sid,
		ClientID: "client.one", RunID: "run.two", MetadataSlotV1: enabled,
		Concurrency: 4, PollInterval: time.Second,
	}, role, sendDir, recvDir)
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func shadowSlotOffer(t *testing.T, client *driveMux) muxFrame {
	t.Helper()
	open := muxFrame{
		Kind: muxFrameOpen, ClientID: client.t.ClientID, RunID: client.t.RunID,
		StreamID: 7, Payload: encodeMuxOpenPayload("example.com:80", nil),
	}
	frames := client.injectSlotOffer([]muxFrame{open})
	if len(frames) != 2 || frames[1].Kind != muxFrameMetadataCapability {
		t.Fatalf("slot offer missing from OPEN batch: %+v", frames)
	}
	return frames[1]
}

func TestMuxSlotShadowDisabledOrOldPeerKeepsImmutableOpen(t *testing.T) {
	ctx := context.Background()
	store := newShadowSlotMemoryStore()
	disabled := shadowSlotTestMux(t, store, "client", false)
	disabled.ensureClientSlot(ctx)
	if len(store.createdIDs) != 0 || disabled.slotLink(muxSlotRunKey{clientID: disabled.t.ClientID, runID: disabled.t.RunID}) != nil {
		t.Fatal("default-off client created a slot")
	}

	client := shadowSlotTestMux(t, store, "client", true)
	client.ensureClientSlot(ctx)
	capability := shadowSlotOffer(t, client)
	legacyExit := shadowSlotTestMux(t, store, "exit", false)
	legacyExit.handleSlotCapability(ctx, capability)
	if legacyExit.slotLink(muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}) != nil {
		t.Fatal("legacy exit accepted a slot offer")
	}
	open := muxFrame{Kind: muxFrameOpen, ClientID: client.t.ClientID, RunID: client.t.RunID, StreamID: 7, Payload: encodeMuxOpenPayload("example.com:80", nil)}
	batch, err := client.lanes[client.frameLane(open)].prepareUploadBatchV4(ctx, []muxFrame{open})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.priority || batch.metadataOnly || !strings.Contains(batch.name, "/p0/") || len(batch.frames) != 2 {
		t.Fatalf("OPEN did not retain immutable p0 bootstrap: %+v", batch)
	}
	if err := client.lanes[client.frameLane(open)].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("legacy OPEN immutable object missing: %v", err)
	}
	store.mu.Lock()
	patches := len(store.patchedIDs)
	store.mu.Unlock()
	if patches != 0 {
		t.Fatalf("unconfirmed bootstrap PATCHed %d slots", patches)
	}
	client.stopSlotLinks()
}

func TestMuxSlotShadowSetupAndExactIDCleanup(t *testing.T) {
	ctx := context.Background()
	store := newShadowSlotMemoryStore()
	client := shadowSlotTestMux(t, store, "client", true)
	client.ensureClientSlot(ctx)
	client.ensureClientSlot(ctx)
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}
	link := client.slotLink(key)
	if link == nil || link.upID == link.downID || link.upName == link.downName || len(store.createdIDs) != 2 {
		t.Fatalf("slot pair setup failed: link=%+v creates=%v", link, store.createdIDs)
	}
	if !strings.HasPrefix(link.upName, "slotv1/") || !strings.HasPrefix(link.downName, "slotv1/") ||
		!strings.Contains(link.upName, "/client.one/run.two") {
		t.Fatalf("slot names not scoped to safe dotted run: %q %q", link.upName, link.downName)
	}
	// A legacy exit never accepts the offer. Close that inactive pair directly
	// instead of waiting for the production acceptance timeout.
	client.closeSlotLink(link)
	client.closeSlotLink(link)
	client.ensureClientSlot(ctx)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.slots) != 0 || len(store.createdIDs) != 2 || len(store.deletedIDs) != 2 || store.getCalls != 4 {
		t.Fatalf("inactive exact ID cleanup or one-attempt latch failed: slots=%d created=%v deleted=%v gets=%d", len(store.slots), store.createdIDs, store.deletedIDs, store.getCalls)
	}
	if store.deletedIDs[0] != link.upID || store.deletedIDs[1] != link.downID {
		t.Fatalf("cleanup deleted unexpected IDs: %v", store.deletedIDs)
	}
}

func TestMuxSlotShadowSlowFailedPrewarmDoesNotBlockOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := make(chan struct{})
	store := newShadowSlotMemoryStore()
	store.createGate = gate
	store.createStart = make(chan struct{})
	store.createError = errors.New("simulated slot create failure")
	client := shadowSlotTestMux(t, store, "client", true)
	started := time.Now()
	client.start(ctx)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slot prewarm delayed mux startup by %s", elapsed)
	}
	select {
	case <-store.createStart:
	case <-time.After(time.Second):
		t.Fatal("slot prewarm did not start")
	}
	open := muxFrame{Kind: muxFrameOpen, ClientID: client.t.ClientID, RunID: client.t.RunID, StreamID: 7, Payload: encodeMuxOpenPayload("example.com:80", nil)}
	started = time.Now()
	batch, err := client.lanes[client.frameLane(open)].prepareUploadBatchV4(ctx, []muxFrame{open})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("ordinary OPEN preparation waited %s on slot setup", elapsed)
	}
	if !batch.priority || batch.metadataOnly || len(batch.frames) != 1 || !strings.Contains(batch.name, "/p0/") {
		t.Fatalf("OPEN did not fall back to immutable p0 during setup: %+v", batch)
	}
	close(gate)
	deadline := time.After(time.Second)
	for {
		store.mu.Lock()
		cleaned := len(store.deletedIDs)
		store.mu.Unlock()
		if cleaned > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("failed prewarm did not clean its generated ID")
		case <-time.After(time.Millisecond):
		}
	}
	if link := client.slotLink(muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}); link != nil {
		t.Fatal("failed prewarm installed a slot")
	}
	client.stopSlotLinks()
}

func TestMuxSlotShadowLostCreateResponseUsesFreshReadback(t *testing.T) {
	store := &shadowLostCreateResponseStore{shadowSlotMemoryStore: newShadowSlotMemoryStore()}
	client := shadowSlotTestMux(t, store.shadowSlotMemoryStore, "client", true)
	name := muxSlotName(client.t.SessionID, DirectionUp, client.t.ClientID, client.t.RunID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	info, err := createOrReadMuxSlot(ctx, store, "mem-id-lost-response", name)
	if err != nil || info.ID != "mem-id-lost-response" || info.Name != name || !store.readUsedFreshContext {
		t.Fatalf("committed create was not recovered by fresh exact-ID readback: info=%+v fresh=%t err=%v", info, store.readUsedFreshContext, err)
	}
}

func TestMuxSlotShadowAmbiguousCreateUsesExactIDCleanup(t *testing.T) {
	store := &shadowAmbiguousCreateStore{shadowSlotMemoryStore: newShadowSlotMemoryStore(), hideFirstRead: true}
	client := shadowSlotTestMux(t, store.shadowSlotMemoryStore, "client", true)
	name := muxSlotName(client.t.SessionID, DirectionUp, client.t.ClientID, client.t.RunID)
	id := "mem-id-timeout"
	if _, err := createOrReadMuxSlot(context.Background(), store, id, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ambiguous create returned %v, want deadline error", err)
	}
	store.mu.Lock()
	_, stillPresent := store.slots[id]
	store.mu.Unlock()
	if !stillPresent {
		t.Fatal("fake timeout did not leave a committed slot for exact-ID cleanup")
	}
	if !client.deleteMuxSlotTargets(store, store, []muxSlotCleanupTarget{{id: id, name: name}}) {
		t.Fatal("ambiguous create cleanup did not confirm two exact-ID 404 reads")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.slots) != 0 || len(store.deletedIDs) != 1 || store.deletedIDs[0] != id {
		t.Fatalf("ambiguous create cleanup touched wrong IDs: slots=%d deleted=%v", len(store.slots), store.deletedIDs)
	}
}

func TestMuxSlotShadowAcceptWaitsForImmutableReplyUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newShadowSlotMemoryStore()
	client := shadowSlotTestMux(t, store, "client", true)
	exit := shadowSlotTestMux(t, store, "exit", true)
	defer func() {
		cancel()
		exit.stopSlotLinks()
		client.stopSlotLinks()
	}()
	client.ensureClientSlot(ctx)
	offer := shadowSlotOffer(t, client)
	exit.handleSlotCapability(ctx, offer)
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}
	exitLink := exit.slotLink(key)
	if exitLink == nil || exitLink.isActive() {
		t.Fatal("exit activated slot before sending an immutable acceptance")
	}
	var accept muxFrame
	select {
	case accept = <-exit.lanes[exit.frameLane(offer)].urgent:
	case <-time.After(time.Second):
		t.Fatal("exit did not enqueue an immutable acceptance frame")
	}
	decoded, err := decodeMuxSlotCapability(accept.Payload)
	if err != nil || decoded.kind != muxSlotCapabilityAccept || decoded.generation != exitLink.generation {
		t.Fatalf("invalid acceptance frame: %+v err=%v", decoded, err)
	}
	if exitLink.isActive() {
		t.Fatal("exit activated slot before acceptance upload completed")
	}
	accept.Ack.complete(nil)
	deadline := time.After(time.Second)
	for !exitLink.isActive() {
		select {
		case <-deadline:
			t.Fatal("exit did not activate after immutable acceptance upload")
		case <-time.After(time.Millisecond):
		}
	}
	client.handleSlotCapability(ctx, accept)
	if link := client.slotLink(key); link == nil || !link.isActive() {
		t.Fatal("client did not activate after authentic acceptance")
	}
}

func TestMuxSlotShadowRejectsMalformedOfferAndWrongKnownID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newShadowSlotMemoryStore()
	client := shadowSlotTestMux(t, store, "client", true)
	client.ensureClientSlot(ctx)
	valid := shadowSlotOffer(t, client)
	exit := shadowSlotTestMux(t, store, "exit", true)
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}

	badID := valid
	badID.Payload = bytes.Clone(valid.Payload)
	badID.Payload[26] = ' '
	exit.handleSlotCapability(ctx, badID)
	if exit.slotLink(key) != nil {
		t.Fatal("malformed slot ID was accepted")
	}

	link := client.slotLink(key)
	store.mu.Lock()
	wrong := store.slots[link.upID]
	wrong.Name = fmt.Sprintf("slotv1/%s/up/other/run", SessionString(client.t.SessionID))
	store.slots[link.upID] = wrong
	store.mu.Unlock()
	exit.handleSlotCapability(ctx, valid)
	if exit.slotLink(key) != nil {
		t.Fatal("known ID with wrong slot name was accepted")
	}
	client.stopSlotLinks()
}

func TestMuxSlotShadowAuthenticatesButDoesNotDeliver(t *testing.T) {
	ctx := context.Background()
	store := newShadowSlotMemoryStore()
	client := shadowSlotTestMux(t, store, "client", true)
	client.ensureClientSlot(ctx)
	key := muxSlotRunKey{clientID: client.t.ClientID, runID: client.t.RunID}
	up := client.slotLink(key)
	if up == nil || !up.setActive() {
		t.Fatal("client slot did not activate")
	}
	exit := shadowSlotTestMux(t, store, "exit", true)
	downPeer, err := newMetadataSlotPeer(client.t.Secret, client.t.SessionID, DirectionDown, key.clientID, key.runID, up.generation, metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	down := &muxSlotLink{
		key: key, generation: up.generation, upID: up.upID, downID: up.downID,
		upName: up.upName, downName: up.downName, peer: downPeer,
		active: true, shadowQ: make(chan slotShadowRecord, muxSlotShadowQueue), ackWake: make(chan struct{}, 1),
	}
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, key.clientID, key.runID, local)
	defer stream.close()
	frame := muxFrame{Kind: muxFrameData, ClientID: key.clientID, RunID: key.runID, StreamID: 7, Seq: 1, Payload: []byte("delivered only by immutable mux"), Priority: true}
	batch, err := client.lanes[client.frameLane(frame)].prepareUploadBatchV4(ctx, []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.lanes[client.frameLane(frame)].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	var mirror slotShadowRecord
	select {
	case mirror = <-up.shadowQ:
	case <-time.After(time.Second):
		t.Fatal("priority DATA was not queued for shadow")
	}
	if mirror.name != batch.name || !bytes.Equal(mirror.sealed, batch.sealed) {
		t.Fatal("shadow changed the immutable mux identity")
	}
	if _, err := up.peer.enqueue(mirror.name, mirror.sealed); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(ctx, up); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { exit.runSlotShadowReader(readCtx, down); close(done) }()
	deadline := time.After(2 * time.Second)
	for down.peer.acked() != 1 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("shadow reader did not authenticate and ACK the mirror")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if stream.expectedRecvSeq() != 1 || len(stream.inbound) != 0 {
		t.Fatal("shadow record delivered application data")
	}
	if len(client.cleanupQueue) != 0 || len(exit.cleanupQueue) != 0 {
		t.Fatal("virtual shadow record entered Drive object cleanup")
	}
	if err := exit.patchSlotShadow(ctx, down); err != nil {
		t.Fatal(err)
	}
	ackInfo, err := store.GetMetadataSlot(ctx, down.downID, down.downName)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := up.peer.receive(ackInfo.Description); err != nil || len(records) != 0 {
		t.Fatalf("reverse shadow ACK invalid: records=%d err=%v", len(records), err)
	}
	up.peer.mu.Lock()
	pending, acked := len(up.peer.outbound.pending), up.peer.outbound.acked
	up.peer.mu.Unlock()
	if pending != 0 || acked != 1 {
		t.Fatalf("authenticated shadow ACK did not trim sender queue: pending=%d acked=%d", pending, acked)
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name})
	if !ok {
		t.Fatal("prepared immutable mux name is invalid")
	}
	if err := exit.processMuxObject(ctx, meta); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-stream.inbound:
		if !bytes.Equal(received, frame.Payload) {
			t.Fatalf("immutable mux delivered %q, want %q", received, frame.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("immutable mux object did not deliver the data")
	}
	client.stopSlotLinks()
}
