package skirk

import (
	"bytes"
	"context"
	"errors"
	"time"
)

// A shadow record contains a real sealed mux batch, but the receiver only
// authenticates it. The ordinary immutable object remains authoritative for
// application delivery. This bounded probe is never an alternate transport.
type slotShadowRecord struct {
	name         string
	sealed       []byte
	deliveryDone chan error
}

func (m *driveMux) queueSlotShadow(batch muxPreparedUpload) {
	if m == nil || m.t == nil || !m.t.MetadataSlotV1 || !batch.priority ||
		len(batch.frames) == 0 || len(batch.sealed) > metadataSlotMaxDescription {
		return
	}
	streamID := batch.frames[0].StreamID
	for _, frame := range batch.frames {
		if frame.StreamID != streamID || (frame.Kind != muxFrameData && frame.Kind != muxFrameFIN) {
			return
		}
	}
	link := m.slotLink(muxSlotRunKey{clientID: batch.frames[0].ClientID, runID: batch.frames[0].RunID})
	if link == nil || link.delivery || !link.canSend() {
		return
	}
	link.mu.Lock()
	if len(link.mirrored) >= muxSlotShadowMaxRecords {
		link.mu.Unlock()
		return
	}
	if link.mirrored == nil {
		link.mirrored = make(map[string]struct{})
	}
	if _, seen := link.mirrored[batch.name]; seen {
		link.mu.Unlock()
		return
	}
	link.mirrored[batch.name] = struct{}{}
	link.mu.Unlock()
	select {
	case link.shadowQ <- slotShadowRecord{name: batch.name, sealed: bytes.Clone(batch.sealed)}:
		if m.t.Observe && m.t.Logger != nil {
			m.t.Logger.Printf("mux slot shadow queued role=%s record=%s authoritative=immutable", m.role, targetFingerprint(batch.name))
		}
	default:
		if m.t.Logger != nil {
			m.t.Logger.Printf("mux slot shadow dropped role=%s reason=queue_full authoritative=immutable", m.role)
		}
	}
}

// A delivery candidate uses the already sealed mux identity. If no
// authenticated processing ACK arrives quickly, the caller uploads that exact
// same batch through the ordinary immutable path.
func (m *driveMux) trySlotDelivery(ctx context.Context, batch muxPreparedUpload) bool {
	if m == nil || m.t == nil || !m.t.MetadataSlotDeliveryV1 || !batch.priority ||
		len(batch.frames) == 0 || len(batch.sealed) > metadataSlotMaxDescription {
		return false
	}
	streamID := batch.frames[0].StreamID
	for _, frame := range batch.frames {
		if frame.StreamID != streamID || (frame.Kind != muxFrameData && frame.Kind != muxFrameFIN) {
			return false
		}
	}
	link := m.slotLink(muxSlotRunKey{clientID: batch.frames[0].ClientID, runID: batch.frames[0].RunID})
	if link == nil || !link.delivery || !link.canSend() {
		return false
	}
	done := make(chan error, 1)
	record := slotShadowRecord{name: batch.name, sealed: bytes.Clone(batch.sealed), deliveryDone: done}
	select {
	case link.shadowQ <- record:
	default:
		return false
	}
	timer := time.NewTimer(muxSlotDeliveryACKWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err == nil
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (m *driveMux) runSlotShadow(ctx context.Context, link *muxSlotLink) {
	shadowCtx, cancel := context.WithTimeout(ctx, muxSlotShadowLifetime)
	defer cancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		m.runSlotShadowWriter(shadowCtx, link)
	}()
	if m.t.Logger != nil {
		if link.delivery {
			m.t.Logger.Printf("mux slot delivery active role=%s max_duration=%s max_patches=%d max_gets=%d fallback=immutable", m.role, muxSlotShadowLifetime, muxSlotShadowMaxPatches, muxSlotShadowMaxGets)
		} else {
			m.t.Logger.Printf("mux slot shadow active role=%s max_duration=%s max_patches=%d max_gets=%d authoritative=immutable", m.role, muxSlotShadowLifetime, muxSlotShadowMaxPatches, muxSlotShadowMaxGets)
		}
	}
	m.runSlotShadowReader(shadowCtx, link)
	cancel()
	<-writerDone
	m.closeSlotLink(link)
}

func (m *driveMux) runSlotShadowWriter(ctx context.Context, link *muxSlotLink) {
	for {
		select {
		case <-ctx.Done():
			return
		case record := <-link.shadowQ:
			if !link.canSend() {
				if record.deliveryDone != nil {
					record.deliveryDone <- errMetadataSlotClosed
				}
				return
			}
			enqueued, err := link.peer.enqueue(record.name, record.sealed)
			if err != nil {
				if record.deliveryDone != nil {
					record.deliveryDone <- err
				}
				if m.t.Logger != nil {
					m.t.Logger.Printf("mux slot shadow dropped role=%s reason=queue_or_size record=%s authoritative=immutable", m.role, targetFingerprint(record.name))
				}
				continue
			}
			if record.deliveryDone != nil {
				link.mu.Lock()
				if link.deliveryWaiters == nil {
					link.deliveryWaiters = make(map[uint64]chan error)
				}
				link.deliveryWaiters[enqueued.SlotSeq] = record.deliveryDone
				link.mu.Unlock()
			}
			if err := m.patchSlotShadow(ctx, link); err != nil {
				m.logSlotShadowError(err)
				link.failDeliveryWaiters(err)
				return
			}
		case <-link.ackWake:
			link.mu.Lock()
			lastAck := link.lastAck
			link.mu.Unlock()
			if link.peer.acked() <= lastAck {
				continue
			}
			if err := m.patchSlotShadow(ctx, link); err != nil {
				m.logSlotShadowError(err)
				link.failDeliveryWaiters(err)
				return
			}
		}
	}
}

func (m *driveMux) patchSlotShadow(ctx context.Context, link *muxSlotLink) error {
	store, ok := m.slotStore()
	if !ok {
		return errors.New("metadata slot store disabled")
	}
	link.sendMu.Lock()
	defer link.sendMu.Unlock()
	if !link.canSend() {
		return errMetadataSlotClosed
	}
	link.mu.Lock()
	if link.patches >= muxSlotShadowMaxPatches {
		link.mu.Unlock()
		return errors.New("metadata slot shadow PATCH budget exhausted")
	}
	last := link.lastPatch
	link.mu.Unlock()
	if delay := muxSlotMinPatchInterval - time.Since(last); !last.IsZero() && delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	ackAtPrepare := link.peer.acked()
	patch, err := link.peer.prepare()
	if err != nil {
		return err
	}
	link.mu.Lock()
	link.lastPatch = time.Now()
	link.patches++
	link.mu.Unlock()
	id, name := link.upID, link.upName
	if m.sendDir == DirectionDown {
		id, name = link.downID, link.downName
	}
	opCtx, stop := context.WithTimeout(ctx, muxDriveAttemptTimeout(len(patch.description), true, m.t.RouteProxy != ""))
	_, err = store.PatchMetadataSlot(opCtx, id, name, patch.description)
	stop()
	if err == nil && link.peer.published(patch) == nil {
		link.mu.Lock()
		link.lastAck = ackAtPrepare
		link.mu.Unlock()
		if m.t.Observe && m.t.Logger != nil {
			m.t.Logger.Printf("mux slot shadow patch role=%s revision=%d bytes=%d authoritative=immutable", m.role, patch.revision, len(patch.description))
		}
		return nil
	}
	// The one-shot PATCH has returned, but its response might have been lost.
	// Exact readback is required before any subsequent PATCH by this writer.
	if link.peer.ambiguous(patch) == nil {
		readCtx, cancel := context.WithTimeout(ctx, muxDriveAttemptTimeout(0, true, m.t.RouteProxy != ""))
		readback, readErr := store.GetMetadataSlot(readCtx, id, name)
		cancel()
		if readErr == nil && link.peer.resolveAmbiguous(patch, readback.Description) == nil {
			link.mu.Lock()
			link.lastAck = ackAtPrepare
			link.mu.Unlock()
			return nil
		}
	}
	link.peer.fallbackAndClose() // Delivery callers retain the same sealed immutable fallback.
	link.mu.Lock()
	link.sendClosed = true
	link.mu.Unlock()
	return errors.New("metadata slot shadow PATCH unresolved; immutable path continues")
}

func (m *driveMux) runSlotShadowReader(ctx context.Context, link *muxSlotLink) {
	store, ok := m.slotStore()
	if !ok {
		return
	}
	id, name := link.downID, link.downName
	if m.recvDir == DirectionUp {
		id, name = link.upID, link.upName
	}
	ticker := time.NewTicker(muxSlotMinPatchInterval)
	defer ticker.Stop()
	var lastDescription string
	consecutiveErrors := 0
	for {
		if !link.isActive() {
			return
		}
		link.mu.Lock()
		if link.gets >= muxSlotShadowMaxGets {
			link.mu.Unlock()
			return
		}
		link.gets++
		link.mu.Unlock()
		readCtx, cancel := context.WithTimeout(ctx, muxDriveAttemptTimeout(0, true, m.t.RouteProxy != ""))
		info, err := store.GetMetadataSlot(readCtx, id, name)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveErrors++
			if consecutiveErrors >= 3 {
				m.logSlotShadowError(err)
				return
			}
		} else {
			consecutiveErrors = 0
			// Drive version is only a hint. A changed encrypted description
			// must be checked even if a cache returns an unexpected version.
			if info.Description != "" && info.Description != lastDescription {
				records, receiveErr := link.peer.receive(info.Description)
				if receiveErr != nil && !errors.Is(receiveErr, errMetadataSlotReplay) {
					m.logSlotShadowError(receiveErr)
					return
				}
				lastDescription = info.Description
				if link.delivery {
					link.completeDeliveryACKs()
					m.queueSlotDeliveryHead(ctx, link)
				} else {
					for _, record := range records {
						if err := m.validateSlotShadow(record, link); err != nil {
							m.logSlotShadowError(err)
							return
						}
						if err := link.peer.markProcessed(record.SlotSeq, record.MuxName); err != nil {
							m.logSlotShadowError(err)
							return
						}
						if m.t.Observe && m.t.Logger != nil {
							m.t.Logger.Printf("mux slot shadow observed role=%s slot_seq=%d record=%s authoritative=immutable", m.role, record.SlotSeq, targetFingerprint(record.MuxName))
						}
					}
					if len(records) > 0 {
						select {
						case link.ackWake <- struct{}{}:
						default:
						}
					}
				}
			}
		}
		if link.delivery {
			m.queueSlotDeliveryHead(ctx, link)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *driveMux) validateSlotShadow(record metadataSlotRecord, link *muxSlotLink) error {
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: record.MuxName})
	if !ok || !meta.Priority || meta.ClientID != link.key.clientID || meta.RunID != link.key.runID || len(meta.StreamIDs) != 1 {
		return errors.New("shadow record mux name invalid")
	}
	key, err := DeriveMuxLaneKeyV4(m.t.Secret, m.t.SessionID, m.recvDir, meta.ClientID, meta.RunID, meta.Lane)
	if err != nil {
		return err
	}
	envelope, raw, err := OpenEnvelope(key, record.MuxSealed)
	if err != nil {
		return err
	}
	if envelope.Direction != m.recvDir || envelope.Sequence != meta.Seq || envelope.SessionID != m.t.SessionID {
		return errors.New("shadow mux envelope identity mismatch")
	}
	frames, err := decodeMuxBatch(raw)
	if err != nil || len(frames) == 0 {
		return errors.New("shadow mux frame batch invalid")
	}
	streamID := frames[0].StreamID
	for _, frame := range frames {
		if frame.StreamID != streamID || (frame.Kind != muxFrameData && frame.Kind != muxFrameFIN) {
			return errors.New("shadow mux batch has unsupported frame kind")
		}
	}
	if meta.StreamID != streamID {
		return errors.New("shadow mux stream identity mismatch")
	}
	return nil
}

func (m *driveMux) queueSlotDeliveryHead(ctx context.Context, link *muxSlotLink) {
	if link == nil || !link.delivery || !link.isActive() {
		return
	}
	record, ok := link.peer.stagedHead()
	if !ok {
		return
	}
	if err := m.validateSlotShadow(record, link); err != nil {
		m.logSlotShadowError(err)
		go m.closeSlotLink(link)
		return
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: record.MuxName})
	if !ok {
		return
	}
	stream := m.streamByKey(meta.key())
	if stream == nil || !stream.ready.Load() || m.isClosedStream(meta.key()) {
		// No slot ACK for DATA/FIN that only exists in the pending-OPEN
		// memory queue. The sender will publish its immutable fallback.
		return
	}
	meta.SlotSealed = record.MuxSealed
	meta.SlotSeq = record.SlotSeq
	meta.SlotLink = link
	// An immutable object can already own this name. Its successful receive
	// will call noteFallbackProcessed and advance the staged record.
	m.enqueueMuxObject(ctx, meta)
}

func (m *driveMux) noteSlotFallbackProcessed(meta muxObjectMeta, sealed []byte, frames []muxFrame) {
	if m == nil || m.t == nil || !m.t.MetadataSlotDeliveryV1 || !meta.Priority || len(frames) == 0 {
		return
	}
	streamID := frames[0].StreamID
	for _, frame := range frames {
		if frame.StreamID != streamID || (frame.Kind != muxFrameData && frame.Kind != muxFrameFIN) {
			return
		}
	}
	link := m.slotLink(muxSlotRunKey{clientID: meta.ClientID, runID: meta.RunID})
	if link == nil || !link.delivery {
		return
	}
	if err := link.peer.noteFallbackProcessed(meta.Name, sealed); err != nil {
		// A malformed or conflicting slot identity cannot revoke successful
		// immutable delivery. Close only the experimental slot generation.
		m.logSlotShadowError(err)
		go m.closeSlotLink(link)
		return
	}
	select {
	case link.ackWake <- struct{}{}:
	default:
	}
}

// A physical fallback discovered after virtual delivery is never blindly
// deleted by name. Verify its exact sealed identity, then delete its exact ID.
func (m *driveMux) verifySlotDuplicateAsync(meta muxObjectMeta, digest [32]byte) {
	if meta.ID == "" {
		return
	}
	m.seenMu.Lock()
	if m.slotVerifySlots == nil {
		m.slotVerifySlots = make(chan struct{}, 2)
	}
	slots := m.slotVerifySlots
	m.seenMu.Unlock()
	select {
	case slots <- struct{}{}:
		go func() {
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(context.Background(), muxDriveAttemptTimeout(0, true, m.t.RouteProxy != ""))
			defer cancel()
			var sealed []byte
			var err error
			if meta.MetadataOnly {
				sealed, err = m.downloadMuxObject(ctx, meta)
			} else {
				// A matching inline description is not proof that the media
				// bytes at this exact Drive ID have the same identity.
				sealed, err = m.downloadMuxObjectOnce(ctx, meta)
			}
			if err != nil {
				m.forgetSlotDuplicateID(meta.Name, meta.ID)
				return
			}
			actual := metadataSlotRecordDigest(metadataSlotRecord{MuxName: meta.Name, MuxSealed: sealed})
			if actual != digest {
				m.logSlotShadowError(errors.New("physical fallback sealed identity differs from processed slot record"))
				for _, key := range meta.streamKeys() {
					m.terminalCloseStreamKey(ctx, key, "mux_slot_identity_conflict", []byte("mux_slot_identity_conflict"), true)
				}
			}
			m.enqueueCleanup(cleanupTask{name: meta.Name, id: meta.ID})
		}()
	default:
		// Keep the object discoverable on a later poll when verification is
		// saturated. This avoids an unbounded goroutine queue.
		m.forgetSlotDuplicateID(meta.Name, meta.ID)
	}
}

func (m *driveMux) forgetSlotDuplicateID(name, id string) {
	m.seenMu.Lock()
	if ids := m.knownIDs[name]; ids != nil {
		delete(ids, id)
		if len(ids) == 0 {
			delete(m.knownIDs, name)
		}
	}
	m.seenMu.Unlock()
}

func (m *driveMux) logSlotShadowError(err error) {
	if err != nil && m.t != nil && m.t.Logger != nil {
		m.t.Logger.Printf("mux slot shadow stopped role=%s reason=%s authoritative=immutable", m.role, errorSummary(err))
	}
}

func (m *driveMux) stopSlotLinks() {
	m.slotsMu.Lock()
	m.slotStopped = true
	setupDone := m.slotSetupDone
	m.slotsMu.Unlock()
	// A canceled prewarm may still own generated IDs. Give it time to
	// perform exact-ID cleanup before a short-lived client process exits.
	if setupDone != nil {
		timer := time.NewTimer(3 * muxSlotSetupTimeout)
		select {
		case <-setupDone:
		case <-timer.C:
			m.logSlotShadowError(errors.New("metadata slot setup cleanup exceeded shutdown wait"))
		}
		timer.Stop()
	}
	m.slotsMu.Lock()
	links := make([]*muxSlotLink, 0, len(m.slots))
	for _, link := range m.slots {
		links = append(links, link)
	}
	m.slotsMu.Unlock()
	for _, link := range links {
		m.closeSlotLink(link)
	}
}

func (m *driveMux) closeSlotLink(link *muxSlotLink) {
	if link == nil {
		return
	}
	link.closeOnce.Do(func() {
		link.failDeliveryWaiters(errMetadataSlotClosed)
		// Wait for the one writer's last PATCH before deleting by exact ID.
		link.sendMu.Lock()
		link.mu.Lock()
		link.closed = true
		link.active = false
		link.mu.Unlock()
		link.sendMu.Unlock()
		m.slotsMu.Lock()
		if m.slots[link.key] == link {
			delete(m.slots, link.key)
		}
		m.slotsMu.Unlock()
		if !link.created {
			return
		}
		deleter, ok := m.t.Data.(ObjectIDStore)
		if !ok {
			return
		}
		store, ok := m.slotStore()
		if !ok {
			return
		}
		targets := []muxSlotCleanupTarget{{link.upID, link.upName}, {link.downID, link.downName}}
		if !m.deleteMuxSlotTargets(store, deleter, targets) {
			go m.recoverMuxSlotTargets(store, deleter, targets, false)
		}
	})
}

// Only the exact generated IDs are ever deletion targets. Two 404 readbacks
// reduce the chance that a transiently hidden slot is mistaken for cleanup.
func (m *driveMux) deleteMuxSlotTargets(store MetadataSlotStore, deleter ObjectIDStore, targets []muxSlotCleanupTarget) bool {
	ctx, cancel := context.WithTimeout(context.Background(), muxSlotSetupTimeout)
	defer cancel()
	allConfirmed := true
	for _, item := range targets {
		confirmed := 0
		if err := deleter.DeleteID(ctx, item.id); err != nil && !isDriveNotFound(err) {
			m.logSlotShadowError(err)
		}
		for i := 0; i < 2; i++ {
			_, err := store.GetMetadataSlot(ctx, item.id, item.name)
			if isDriveNotFound(err) {
				confirmed++
				continue
			}
			if err == nil {
				if deleteErr := deleter.DeleteID(ctx, item.id); deleteErr != nil && !isDriveNotFound(deleteErr) {
					m.logSlotShadowError(deleteErr)
				}
			} else {
				m.logSlotShadowError(err)
			}
		}
		if confirmed != 2 {
			allConfirmed = false
			m.logSlotShadowError(errors.New("metadata slot exact-ID cleanup did not confirm two 404 reads"))
		}
	}
	return allConfirmed
}

func (m *driveMux) recoverMuxSlotTargets(store MetadataSlotStore, deleter ObjectIDStore, targets []muxSlotCleanupTarget, ambiguousCreate bool) {
	for _, delay := range [...]time.Duration{2 * time.Second, 10 * time.Second, 45 * time.Second} {
		timer := time.NewTimer(delay)
		<-timer.C
		if m.deleteMuxSlotTargets(store, deleter, targets) && !ambiguousCreate {
			return
		}
	}
}
