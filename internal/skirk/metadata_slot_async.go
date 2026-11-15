package skirk

import (
	"bytes"
	"context"
	"time"
)

const muxSlotAsyncMaxPending = 64
const muxSlotAsyncMaxPerStream = 4

// A handoff transfers upload ownership, not delivery success. The lane may
// continue preparing traffic, but its frame ACK remains pending until Drive
// publication (or the stronger peer processing ACK) actually succeeds.
func (l *muxLane) handoffSlotDelivery(ctx context.Context, batch muxPreparedUpload) bool {
	m := l.mux
	if !m.t.MetadataSlotDeliveryV1 || !batch.priority || len(batch.frames) == 0 ||
		len(batch.sealed) > metadataSlotMaxDescription || ctx.Err() != nil {
		return false
	}
	key := batch.frames[0].key()
	for _, frame := range batch.frames {
		if frame.key() != key || (frame.Kind != muxFrameData && frame.Kind != muxFrameFIN) {
			return false
		}
	}
	link := m.slotLink(muxSlotRunKey{clientID: key.ClientID, runID: key.RunID})
	if link == nil || !link.delivery || !link.canSend() {
		return false
	}
	m.slotAsyncMu.Lock()
	if m.slotAsyncCount >= muxSlotAsyncMaxPending || m.slotAsyncPending[key] >= muxSlotAsyncMaxPerStream {
		m.slotAsyncMu.Unlock()
		return false
	}
	if m.slotAsyncPending == nil {
		m.slotAsyncPending = make(map[muxStreamKey]int)
	}
	m.slotAsyncCount++
	m.slotAsyncPending[key]++
	m.slotAsyncMu.Unlock()
	done, published := make(chan error, 1), make(chan error, 1)
	record := slotShadowRecord{name: batch.name, sealed: bytes.Clone(batch.sealed), deliveryDone: done, publicationDone: published}
	// Serialize admission with writer shutdown: a stopped writer must not
	// drain its queue just before a new owner arrives behind it.
	link.mu.Lock()
	if !link.active || link.closed || link.sendClosed {
		link.mu.Unlock()
		m.releaseSlotHandoff(key)
		return false
	}
	select {
	case link.shadowQ <- record:
		link.mu.Unlock()
		if m.t.Observe && m.t.Logger != nil {
			m.t.Logger.Printf("mux slot async handoff role=%s record=%s state=memory_queued", m.role, targetFingerprint(batch.name))
		}
		go l.finishSlotHandoff(ctx, batch, link, done, published)
		return true
	default:
		link.mu.Unlock()
		m.releaseSlotHandoff(key)
		return false // Overflow uses immutable immediately, never the synchronous slot wait.
	}
}

func (m *driveMux) releaseSlotHandoff(key muxStreamKey) {
	m.slotAsyncMu.Lock()
	m.slotAsyncCount--
	if m.slotAsyncPending[key]--; m.slotAsyncPending[key] == 0 {
		delete(m.slotAsyncPending, key)
	}
	m.slotAsyncMu.Unlock()
}

func (l *muxLane) finishSlotHandoff(ctx context.Context, batch muxPreparedUpload, link *muxSlotLink, done, published <-chan error) {
	defer l.mux.releaseSlotHandoff(batch.frames[0].key())
	timer := time.NewTimer(muxSlotDeliveryACKWait)
	defer timer.Stop()
	publicationACK := false
	acceptProcessingACK := func(err error) bool {
		if err != nil {
			return false
		}
		if !publicationACK {
			completeMuxFrameAcks(batch.frames, nil)
			l.mux.t.markUpload()
		}
		if l.mux.t.Observe && l.mux.t.Logger != nil {
			l.mux.t.Logger.Printf("mux slot delivery sender_ack role=%s record=%s state=peer_processed async=true", l.mux.role, targetFingerprint(batch.name))
		}
		return true
	}
	processed := func() bool {
		select {
		case err := <-done:
			return acceptProcessingACK(err)
		default:
			return false
		}
	}
	waiting := true
	for waiting {
		select {
		case err := <-done:
			if acceptProcessingACK(err) {
				return
			}
			waiting = false
		case err := <-published:
			published = nil
			if err != nil {
				waiting = false
				break
			}
			publicationACK = true
			completeMuxFrameAcks(batch.frames, nil)
			l.mux.t.markUpload()
			l.mux.wakeReceiver()
			if l.mux.t.Observe && l.mux.t.Logger != nil {
				l.mux.t.Logger.Printf("mux slot async storage_ack role=%s record=%s state=drive_published", l.mux.role, targetFingerprint(batch.name))
			}
			// Publication confirms only that Drive accepted the ciphertext.
			// Keep ownership until processing ACK or identical immutable fallback.
		case <-timer.C:
			waiting = false
		case <-ctx.Done():
			l.failUploadBatch(ctx, batch.frames, ctx.Err(), 0)
			return
		}
	}
	if ctx.Err() != nil {
		l.failUploadBatch(ctx, batch.frames, ctx.Err(), 0)
		return
	}
	// Retry the identical prepared object, never hand it back through slot
	// routing. Existing stream sequence/digest claims dedupe a late slot copy.
	for attempt := 1; ; attempt++ {
		if processed() {
			return
		}
		if l.mux.t.Observe && l.mux.t.Logger != nil {
			l.mux.t.Logger.Printf("mux slot async fallback role=%s record=%s attempt=%d", l.mux.role, targetFingerprint(batch.name), attempt)
		}
		err := l.uploadPreparedImmutableBatchV4(ctx, batch)
		if err == nil {
			return
		}
		// A fallback response can fail after the peer has already processed
		// the slot. That positive ACK owns the outcome and prevents a reset.
		if processed() {
			return
		}
		if ctx.Err() != nil {
			l.failUploadBatch(ctx, batch.frames, ctx.Err(), attempt)
			return
		}
		if l.mux.reportTerminalTransportError(ctx, err) || attempt >= muxUploadMaxRetries {
			l.failUploadBatch(ctx, batch.frames, err, attempt)
			return
		}
		timer := time.NewTimer(muxRetryDelay(attempt))
		backoff := true
		for backoff {
			select {
			case ackErr := <-done:
				if acceptProcessingACK(ackErr) {
					timer.Stop()
					return
				}
			case <-ctx.Done():
				timer.Stop()
				if processed() {
					return
				}
				l.failUploadBatch(ctx, batch.frames, ctx.Err(), attempt)
				return
			case <-timer.C:
				backoff = false
			}
		}
	}
}
