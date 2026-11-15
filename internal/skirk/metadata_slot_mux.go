package skirk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	muxSlotCapabilityMagic     = "SKSL"
	muxSlotDeliveryMagic       = "SKSD"
	muxSlotCapabilityVersion   = byte(1)
	muxSlotCapabilityOffer     = byte(1)
	muxSlotCapabilityAccept    = byte(2)
	muxSlotCapabilityMaxID     = 128
	muxSlotMaxActiveRuns       = 16
	muxSlotSetupTimeout        = 10 * time.Second
	muxSlotMinPatchInterval    = 250 * time.Millisecond
	muxSlotShadowLifetime      = 2 * time.Minute
	muxSlotAcceptTimeout       = 45 * time.Second
	muxSlotShadowMaxPatches    = 64
	muxSlotShadowMaxGets       = 320
	muxSlotShadowMaxRecords    = 64
	muxSlotShadowQueue         = 16
	muxSlotDeliveryACKWait     = 1500 * time.Millisecond
	muxSlotDigestRetention     = 30 * time.Minute
	muxSlotDigestMax           = 65536
	muxSlotDeferredPhysicalMax = 8
)

type muxSlotRunKey struct {
	clientID string
	runID    string
}

type muxSlotCleanupTarget struct{ id, name string }

type muxSlotLink struct {
	key             muxSlotRunKey
	generation      [16]byte
	upID            string
	downID          string
	upName          string
	downName        string
	peer            *metadataSlotPeer
	delivery        bool
	created         bool // Only the client that created the files deletes them at stop.
	mu              sync.Mutex
	active          bool
	closed          bool
	sendClosed      bool
	lastPatch       time.Time
	lastAck         uint64
	patches         int
	gets            int
	mirrored        map[string]struct{}
	shadowQ         chan slotShadowRecord
	ackWake         chan struct{}
	deliveryWaiters map[uint64]chan error
	pollOnce        sync.Once
	closeOnce       sync.Once
	sendMu          sync.Mutex
}

func (s *muxSlotLink) setActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active = true
	return true
}

func (s *muxSlotLink) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active && !s.closed
}

func (s *muxSlotLink) canOffer() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.active && !s.closed
}

func (s *muxSlotLink) canSend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active && !s.closed && !s.sendClosed
}

func (s *muxSlotLink) completeDeliveryACKs() {
	acked := s.peer.outboundAcked()
	s.mu.Lock()
	for seq, done := range s.deliveryWaiters {
		if seq <= acked {
			delete(s.deliveryWaiters, seq)
			done <- nil
		}
	}
	s.mu.Unlock()
}

func (s *muxSlotLink) failDeliveryWaiters(err error) {
	s.mu.Lock()
	for seq, done := range s.deliveryWaiters {
		delete(s.deliveryWaiters, seq)
		done <- err
	}
	s.mu.Unlock()
}

func (m *driveMux) slotStore() (MetadataSlotStore, bool) {
	if m == nil || m.t == nil || !m.t.MetadataSlotV1 {
		return nil, false
	}
	store, ok := m.t.Data.(MetadataSlotStore)
	return store, ok && store.MetadataSlotEnabled()
}

func (m *driveMux) slotLink(key muxSlotRunKey) *muxSlotLink {
	m.slotsMu.Lock()
	defer m.slotsMu.Unlock()
	return m.slots[key]
}

// A failed opt-in setup never prevents a normal muxv4 OPEN. One attempt per
// Tunnel/RunID bounds quota even if the first setup fails or the slot expires.
func (m *driveMux) ensureClientSlot(ctx context.Context) {
	if m.role != "client" {
		return
	}
	store, ok := m.slotStore()
	if !ok || !metadataSlotLabelValid(m.t.ClientID) || !metadataSlotLabelValid(m.t.RunID) {
		return
	}
	if !m.t.slotAttempted.CompareAndSwap(false, true) {
		return
	}
	key := muxSlotRunKey{clientID: m.t.ClientID, runID: m.t.RunID}
	upName := muxSlotName(m.t.SessionID, DirectionUp, key.clientID, key.runID)
	downName := muxSlotName(m.t.SessionID, DirectionDown, key.clientID, key.runID)
	if !validMetadataSlotName(upName) || !validMetadataSlotName(downName) {
		return
	}
	ids, ok := m.t.Data.(ObjectIDReserveStore)
	if !ok {
		return
	}
	deleter, ok := m.t.Data.(ObjectIDStore)
	if !ok {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, muxSlotSetupTimeout)
	defer cancel()
	generated, err := ids.GenerateObjectIDs(opCtx, 2)
	if err != nil || len(generated) != 2 || generated[0] == generated[1] {
		m.logSlotSetupError(err)
		return
	}
	// Generated IDs remain safe cleanup targets even if a create response was
	// lost. A later stale-slot sweep covers an indeterminate late commit.
	created := make([]muxSlotCleanupTarget, 0, 2)
	cleanup := func(ambiguous bool) {
		targets := append([]muxSlotCleanupTarget(nil), created...)
		confirmed := m.deleteMuxSlotTargets(store, deleter, targets)
		if ambiguous || !confirmed {
			// A canceled create can still commit after its response is lost.
			// Repeat exact-ID deletion later; the opt-in janitor handles a
			// process crash before these bounded recovery checks finish.
			go m.recoverMuxSlotTargets(store, deleter, targets, ambiguous)
		}
	}
	created = append(created, muxSlotCleanupTarget{generated[0], upName})
	up, err := createOrReadMuxSlot(opCtx, store, generated[0], upName)
	if err != nil {
		cleanup(true)
		m.logSlotSetupError(err)
		return
	}
	created = append(created, muxSlotCleanupTarget{generated[1], downName})
	down, err := createOrReadMuxSlot(opCtx, store, generated[1], downName)
	if err != nil {
		cleanup(true)
		m.logSlotSetupError(err)
		return
	}
	generation, err := newMetadataSlotGeneration()
	if err != nil {
		cleanup(false)
		m.logSlotSetupError(err)
		return
	}
	peer, err := newMetadataSlotPeer(m.t.Secret, m.t.SessionID, DirectionUp, key.clientID, key.runID, generation, metadataSlotLimits{})
	if err != nil {
		cleanup(false)
		m.logSlotSetupError(err)
		return
	}
	link := &muxSlotLink{key: key, generation: generation, upID: up.ID, downID: down.ID, upName: upName, downName: downName, peer: peer, delivery: m.t.MetadataSlotDeliveryV1, created: true, shadowQ: make(chan slotShadowRecord, muxSlotShadowQueue), ackWake: make(chan struct{}, 1)}
	m.slotsMu.Lock()
	if m.slotStopped {
		m.slotsMu.Unlock()
		cleanup(false)
		return
	}
	if m.slots == nil {
		m.slots = make(map[muxSlotRunKey]*muxSlotLink)
	}
	m.slots[key] = link
	m.slotsMu.Unlock()
	if m.t.Observe && m.t.Logger != nil {
		m.t.Logger.Printf("mux slot shadow prewarmed role=client authoritative=immutable")
	}
	go m.watchSlotAcceptance(ctx, link)
}

func (m *driveMux) watchSlotAcceptance(ctx context.Context, link *muxSlotLink) {
	timer := time.NewTimer(muxSlotAcceptTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	if !link.isActive() {
		m.closeSlotLink(link)
	}
}

// A generated-ID create can succeed despite a lost response. Read the exact
// ID and validate its name and parent before advertising it in OPEN.
func createOrReadMuxSlot(ctx context.Context, store MetadataSlotStore, generatedID, name string) (MetadataSlotInfo, error) {
	info, err := store.CreateMetadataSlot(ctx, generatedID, name)
	if err == nil {
		return info, nil
	}
	readCtx, cancel := context.WithTimeout(context.Background(), muxSlotSetupTimeout)
	defer cancel()
	readback, readErr := store.GetMetadataSlot(readCtx, generatedID, name)
	if readErr == nil {
		return readback, nil
	}
	return MetadataSlotInfo{}, fmt.Errorf("metadata slot create and exact-ID readback failed: %w", errors.Join(err, readErr))
}

func (m *driveMux) logSlotSetupError(err error) {
	if m.t != nil && m.t.Logger != nil && err != nil {
		m.t.Logger.Printf("mux slot setup skipped role=%s reason=%s", m.role, errorSummary(err))
	}
}

func (m *driveMux) injectSlotOffer(frames []muxFrame) []muxFrame {
	if m == nil || m.role != "client" || len(frames) == 0 || !m.t.MetadataSlotV1 {
		return frames
	}
	key := muxSlotRunKey{clientID: frames[0].ClientID, runID: frames[0].RunID}
	link := m.slotLink(key)
	if link == nil || !link.canOffer() {
		return frames
	}
	for _, frame := range frames {
		if frame.Kind != muxFrameOpen || frame.ClientID != key.clientID || frame.RunID != key.runID {
			continue
		}
		payload, err := encodeMuxSlotCapability(muxSlotCapability{kind: muxSlotCapabilityOffer, delivery: link.delivery, generation: link.generation, limit: metadataSlotMaxDescription, upID: link.upID, downID: link.downID})
		encodedBytes := muxBatchHeaderSize + muxFrameHeaderSize + len(payload)
		for _, existing := range frames {
			encodedBytes += encodedMuxFrameSize(existing)
		}
		if err != nil || len(frames) >= muxMaxFrames || encodedBytes > muxMaxBatch {
			return frames
		}
		if m.t.Observe && m.t.Logger != nil {
			m.t.Logger.Printf("mux slot shadow offer role=client authoritative=immutable")
		}
		return append(frames, muxFrame{Kind: muxFrameMetadataCapability, ClientID: key.clientID, RunID: key.runID, StreamID: frame.StreamID, Payload: payload, Priority: true})
	}
	return frames
}

func (m *driveMux) handleSlotCapability(ctx context.Context, frame muxFrame) {
	if len(frame.Payload) < 4 || (string(frame.Payload[:4]) != muxSlotCapabilityMagic && string(frame.Payload[:4]) != muxSlotDeliveryMagic) {
		return
	}
	capability, err := decodeMuxSlotCapability(frame.Payload)
	if err != nil || frame.StreamID == 0 {
		return
	}
	if capability.delivery && !m.t.MetadataSlotDeliveryV1 {
		return
	}
	store, ok := m.slotStore()
	if !ok {
		return
	}
	key := muxSlotRunKey{clientID: frame.ClientID, runID: frame.RunID}
	if capability.kind == muxSlotCapabilityAccept && m.role == "client" {
		link := m.slotLink(key)
		if link != nil && capability.generation == link.generation && capability.limit == metadataSlotMaxDescription && capability.delivery == link.delivery && link.setActive() {
			if m.t.Observe && m.t.Logger != nil {
				m.t.Logger.Printf("mux slot shadow acceptance role=client authoritative=immutable")
			}
			m.startSlotPoll(ctx, link)
		}
		return
	}
	if capability.kind != muxSlotCapabilityOffer || m.role != "exit" ||
		!metadataSlotLabelValid(key.clientID) || !metadataSlotLabelValid(key.runID) ||
		capability.limit != metadataSlotMaxDescription {
		return
	}
	upName := muxSlotName(m.t.SessionID, DirectionUp, key.clientID, key.runID)
	downName := muxSlotName(m.t.SessionID, DirectionDown, key.clientID, key.runID)
	if !validMetadataSlotName(upName) || !validMetadataSlotName(downName) {
		return
	}
	m.slotsMu.Lock()
	if m.slotStopped {
		m.slotsMu.Unlock()
		return
	}
	if prior := m.slots[key]; prior != nil {
		m.slotsMu.Unlock()
		return
	}
	if _, pending := m.slotPending[key]; pending || len(m.slots)+len(m.slotPending) >= muxSlotMaxActiveRuns {
		m.slotsMu.Unlock()
		return
	}
	if m.slotPending == nil {
		m.slotPending = make(map[muxSlotRunKey]struct{})
	}
	m.slotPending[key] = struct{}{}
	m.slotsMu.Unlock()
	defer func() {
		m.slotsMu.Lock()
		delete(m.slotPending, key)
		m.slotsMu.Unlock()
	}()
	opCtx, cancel := context.WithTimeout(ctx, muxSlotSetupTimeout)
	defer cancel()
	if _, err := store.GetMetadataSlot(opCtx, capability.upID, upName); err != nil {
		m.logSlotSetupError(err)
		return
	}
	if _, err := store.GetMetadataSlot(opCtx, capability.downID, downName); err != nil {
		m.logSlotSetupError(err)
		return
	}
	peer, err := newMetadataSlotPeer(m.t.Secret, m.t.SessionID, DirectionDown, key.clientID, key.runID, capability.generation, metadataSlotLimits{DescriptionBytes: int(capability.limit)})
	if err != nil {
		m.logSlotSetupError(err)
		return
	}
	link := &muxSlotLink{key: key, generation: capability.generation, upID: capability.upID, downID: capability.downID, upName: upName, downName: downName, peer: peer, delivery: capability.delivery, shadowQ: make(chan slotShadowRecord, muxSlotShadowQueue), ackWake: make(chan struct{}, 1)}
	m.slotsMu.Lock()
	if m.slotStopped {
		m.slotsMu.Unlock()
		return
	}
	if m.slots == nil {
		m.slots = make(map[muxSlotRunKey]*muxSlotLink)
	}
	if m.slots[key] != nil || len(m.slots) >= muxSlotMaxActiveRuns {
		m.slotsMu.Unlock()
		return
	}
	m.slots[key] = link
	m.slotsMu.Unlock()
	go m.sendSlotAccept(ctx, link, frame.StreamID)
}

func (m *driveMux) sendSlotAccept(ctx context.Context, link *muxSlotLink, streamID uint64) {
	accepted := false
	defer func() {
		if !accepted {
			m.closeSlotLink(link)
		}
	}()
	payload, err := encodeMuxSlotCapability(muxSlotCapability{kind: muxSlotCapabilityAccept, delivery: link.delivery, generation: link.generation, limit: metadataSlotMaxDescription})
	if err != nil {
		return
	}
	acceptCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ack := newMuxFrameAck()
	frame := muxFrame{Kind: muxFrameMetadataCapability, ClientID: link.key.clientID, RunID: link.key.runID, StreamID: streamID, Payload: payload, Priority: true, Ack: ack}
	if err := m.sendFrame(acceptCtx, frame); err != nil {
		return
	}
	select {
	case err := <-ack.ch:
		if err == nil && link.setActive() {
			accepted = true
			if m.t.Observe && m.t.Logger != nil {
				m.t.Logger.Printf("mux slot shadow acceptance role=exit authoritative=immutable")
			}
			m.startSlotPoll(ctx, link)
		}
	case <-acceptCtx.Done():
	}
}

func (m *driveMux) startSlotPoll(ctx context.Context, link *muxSlotLink) {
	link.pollOnce.Do(func() { go m.runSlotShadow(ctx, link) })
}

// This payload is carried as a muxFrameMetadataCapability beside an OPEN or
// first reply. Older muxv4 peers ignore its length and still process the OPEN.
type muxSlotCapability struct {
	kind       byte
	delivery   bool
	generation [16]byte
	limit      uint16
	upID       string
	downID     string
}

func muxSlotName(sid [16]byte, direction byte, clientID, runID string) string {
	return fmt.Sprintf("slotv1/%s/%s/%s/%s", SessionString(sid), directionName(direction), clientID, runID)
}

func encodeMuxSlotCapability(value muxSlotCapability) ([]byte, error) {
	if value.kind != muxSlotCapabilityOffer && value.kind != muxSlotCapabilityAccept ||
		value.generation == [16]byte{} || value.limit < 128 || value.limit > metadataSlotMaxDescription {
		return nil, errors.New("invalid mux slot capability")
	}
	if value.kind == muxSlotCapabilityOffer {
		if len(value.upID) > muxSlotCapabilityMaxID || len(value.downID) > muxSlotCapabilityMaxID ||
			!validMetadataSlotID(value.upID) || !validMetadataSlotID(value.downID) || value.upID == value.downID {
			return nil, errors.New("invalid mux slot file IDs")
		}
	} else if value.upID != "" || value.downID != "" {
		return nil, errors.New("mux slot acceptance cannot carry file IDs")
	}
	payload := make([]byte, 0, 27+len(value.upID)+len(value.downID))
	if value.delivery {
		payload = append(payload, muxSlotDeliveryMagic...)
	} else {
		payload = append(payload, muxSlotCapabilityMagic...)
	}
	payload = append(payload, muxSlotCapabilityVersion, value.kind)
	payload = append(payload, value.generation[:]...)
	payload = binary.BigEndian.AppendUint16(payload, value.limit)
	if value.kind == muxSlotCapabilityOffer {
		payload = append(payload, byte(len(value.upID)), byte(len(value.downID)))
		payload = append(payload, value.upID...)
		payload = append(payload, value.downID...)
	}
	return payload, nil
}

func decodeMuxSlotCapability(payload []byte) (muxSlotCapability, error) {
	if len(payload) < 24 || (string(payload[:4]) != muxSlotCapabilityMagic && string(payload[:4]) != muxSlotDeliveryMagic) || payload[4] != muxSlotCapabilityVersion {
		return muxSlotCapability{}, errors.New("mux slot capability marker invalid")
	}
	value := muxSlotCapability{kind: payload[5], delivery: string(payload[:4]) == muxSlotDeliveryMagic, limit: binary.BigEndian.Uint16(payload[22:24])}
	copy(value.generation[:], payload[6:22])
	if value.kind == muxSlotCapabilityOffer {
		if len(payload) < 26 {
			return muxSlotCapability{}, errors.New("mux slot offer truncated")
		}
		upLen, downLen := int(payload[24]), int(payload[25])
		if upLen == 0 || downLen == 0 || len(payload) != 26+upLen+downLen {
			return muxSlotCapability{}, errors.New("mux slot offer IDs truncated")
		}
		value.upID = string(payload[26 : 26+upLen])
		value.downID = string(payload[26+upLen:])
	} else if value.kind != muxSlotCapabilityAccept || len(payload) != 24 {
		return muxSlotCapability{}, errors.New("mux slot capability kind or length invalid")
	}
	_, err := encodeMuxSlotCapability(value)
	if err != nil {
		return muxSlotCapability{}, err
	}
	return value, nil
}
