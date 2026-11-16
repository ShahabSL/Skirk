package skirk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

type fsHybridStoreConfig struct {
	Secret                                                                 string
	Session, Epoch                                                         [16]byte
	Direction                                                              byte
	ClientID, RunID                                                        string
	MaxSnapshotBytes                                                       int
	FallbackAfter, OperationTimeout, ACKDelay, Duration, DiscoveryInterval time.Duration
	PhaseTrace                                                             *fsHybridPhaseTrace
	InlinePerStream                                                        int
	SelectiveReceipts                                                      bool
}

func (c fsHybridStoreConfig) normalized() (fsHybridStoreConfig, error) {
	if c.MaxSnapshotBytes == 0 {
		c.MaxSnapshotBytes = fsHybridWireMax
	}
	if c.InlinePerStream == 0 {
		c.InlinePerStream = 4
	}
	if c.MaxSnapshotBytes < 1024 || c.MaxSnapshotBytes > fsHybridWireMax || c.InlinePerStream != 4 && c.InlinePerStream != 8 {
		return c, errFSHybridBounds
	}
	if c.FallbackAfter == 0 {
		c.FallbackAfter = 2 * time.Second
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = 5 * time.Second
	}
	if c.ACKDelay == 0 {
		c.ACKDelay = 10 * time.Millisecond
	}
	if c.Duration == 0 {
		c.Duration = 5 * time.Minute
	}
	if c.DiscoveryInterval == 0 {
		c.DiscoveryInterval = 100 * time.Millisecond
	}
	if c.FallbackAfter < time.Millisecond || c.FallbackAfter > 30*time.Second || c.OperationTimeout < time.Millisecond || c.OperationTimeout > 15*time.Second || c.ACKDelay < time.Millisecond || c.ACKDelay > 100*time.Millisecond || c.Duration < time.Second || c.Duration > 5*time.Minute || c.DiscoveryInterval < time.Millisecond || c.DiscoveryInterval > 5*time.Second {
		return c, errFSHybridBounds
	}
	return c, nil
}

type fsHybridPriorityStore struct {
	ctx               context.Context
	cancel            context.CancelFunc
	base              BlobStore
	carrier           fsPriorityCarrier
	cfg               fsHybridStoreConfig
	mu                sync.Mutex
	state             *fsHybridState
	closed            bool
	joined            bool
	fatal             error
	retired           bool
	retireErr         error
	dirty             bool
	ackDue            time.Time
	own               fsPriorityDocument
	ownPublished      bool
	ready             chan struct{}
	readyClosed       bool
	wake              chan struct{}
	fallbackWake      chan struct{}
	fallbackJobs      chan *fsHybridOutbound
	discoverWake      chan struct{}
	prefetchJobs      chan ObjectInfo
	prefetchAdmission chan struct{}
	prefetchPending   map[string]ObjectInfo
	space             chan struct{}
	bindingCancel     context.CancelFunc
	bindingStop       func() bool
	wg                sync.WaitGroup
	closeOnce         sync.Once
	closeErr          error
	counters          map[string]int64
	receive           func(ObjectInfo) bool
	cleanup           func(ObjectInfo)
	cleanupQueued     map[string]bool
	deletedPhysical   map[string]bool
}

func newFSHybridPriorityStore(parent context.Context, base BlobStore, carrier fsPriorityCarrier, cfg fsHybridStoreConfig) (*fsHybridPriorityStore, error) {
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	var err error
	cfg, err = cfg.normalized()
	if err != nil || base == nil || carrier == nil {
		return nil, errFSHybridBounds
	}
	if _, ok := base.(ObjectIDStore); !ok {
		return nil, errFSHybridBounds
	}
	if _, ok := base.(ObjectIDReserveStore); !ok {
		return nil, errFSHybridBounds
	}
	if _, ok := base.(ObjectPutIDStore); !ok {
		return nil, errFSHybridBounds
	}
	out, err := newFSHybridCodec(cfg.Secret, cfg.Session, cfg.Epoch, cfg.Direction, cfg.ClientID, cfg.RunID)
	if err != nil {
		return nil, err
	}
	recv := DirectionUp
	if cfg.Direction == DirectionUp {
		recv = DirectionDown
	}
	in, err := newFSHybridCodec(cfg.Secret, cfg.Session, cfg.Epoch, recv, cfg.ClientID, cfg.RunID)
	if err != nil {
		return nil, err
	}
	out.wireMax, in.wireMax = cfg.MaxSnapshotBytes, cfg.MaxSnapshotBytes
	if cfg.SelectiveReceipts {
		if cfg.MaxSnapshotBytes != driveV2PriorityCipherMax {
			return nil, errFSHybridBounds
		}
		if _, ok := carrier.(*driveV2PriorityCarrier); !ok {
			return nil, errFSHybridBounds
		}
		out.selective, in.selective = true, true
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	s := &fsHybridPriorityStore{ctx: ctx, cancel: cancel, base: base, carrier: carrier, cfg: cfg, state: newFSHybridState(out, in), ready: make(chan struct{}), wake: make(chan struct{}, 1), fallbackWake: make(chan struct{}, 1), fallbackJobs: make(chan *fsHybridOutbound, fsHybridQueueMax), counters: make(map[string]int64), cleanupQueued: make(map[string]bool), deletedPhysical: make(map[string]bool), discoverWake: make(chan struct{}, 1), prefetchJobs: make(chan ObjectInfo, 64), prefetchAdmission: make(chan struct{}, 66), prefetchPending: make(map[string]ObjectInfo), space: make(chan struct{})}
	s.state.outPerStreamMax = cfg.InlinePerStream
	readCtx, stop := context.WithTimeout(ctx, cfg.OperationTimeout)
	s.own, err = carrier.ReadOwn(readCtx)
	stop()
	if err != nil || !s.own.valid() || len(s.own.Ciphertext) != 0 {
		cancel()
		_ = carrier.Close()
		return nil, errors.Join(err, errFSHybridIntegrity)
	}
	// All workers are owned before the store becomes available to callers.
	s.wg.Add(8)
	go func() { defer s.wg.Done(); s.runWriter() }()
	go func() { defer s.wg.Done(); s.runWatch() }()
	go func() { defer s.wg.Done(); s.runFallbackScheduler() }()
	for i := 0; i < 2; i++ {
		go func() { defer s.wg.Done(); s.runFallbackWorker() }()
	}
	go func() { defer s.wg.Done(); s.runPriorityDiscovery() }()
	for i := 0; i < 2; i++ {
		go func() { defer s.wg.Done(); s.runPrefetchWorker() }()
	}
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	s.signal(s.wake)
	return s, nil
}

func (s *fsHybridPriorityStore) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (s *fsHybridPriorityStore) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.fatal == nil {
		s.fatal = err
	}
	s.mu.Unlock()
	s.cancel()
	s.signal(s.wake)
	s.signal(s.fallbackWake)
}
func (s *fsHybridPriorityStore) Error() error { s.mu.Lock(); defer s.mu.Unlock(); return s.fatal }

// The outer fixture must stop the whole mux/workload when this finite epoch
// ends, not just its discovery callbacks. The adapter does not own unrelated
// caller-created mux goroutines or silently rotate their RunID.
func (s *fsHybridPriorityStore) Done() <-chan struct{} { return s.ctx.Done() }
func (s *fsHybridPriorityStore) updateReadyLocked() {
	if !s.readyClosed && s.ownPublished && s.state.peerRevision > 0 && s.state.peerCreditSeq > 0 {
		close(s.ready)
		s.readyClosed = true
	}
}

func (s *fsHybridPriorityStore) scheduleACKLocked() {
	if !s.dirty {
		s.dirty = true
		s.ackDue = time.Now().Add(s.cfg.ACKDelay)
	}
	// Do not move an existing deadline: a continuous receipt stream must not
	// postpone the ACK forever, and urgent outbound DATA already sets zero.
}
func (s *fsHybridPriorityStore) WaitReady(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errors.Join(s.ctx.Err(), s.Error())
	}
}

// begin joins every base-store operation with close. Late callbacks cannot add
// work after the closed flag fences the WaitGroup's admission.
func (s *fsHybridPriorityStore) begin(caller context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || caller.Err() != nil {
		s.mu.Unlock()
		return nil, nil, errFSHybridClosed
	}
	s.wg.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(caller)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel(); s.wg.Done() }, nil
}

func (s *fsHybridPriorityStore) ingest(doc fsPriorityDocument) error {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return errFSHybridClosed
	}
	if !doc.valid() || doc.FenceKind != s.own.FenceKind {
		s.mu.Unlock()
		return errFSHybridIntegrity
	}
	if len(doc.Ciphertext) == 0 {
		// Only the explicitly fresh, precreated bootstrap document may be
		// empty. It grants no credit/readiness and can carry no application.
		authenticated := s.state.peerRevision > 0
		s.mu.Unlock()
		if authenticated {
			return errFSHybridIntegrity
		}
		return nil
	}
	ackedBefore := s.state.acked
	ready, advanced, err := s.state.receive(doc.Ciphertext, time.Now())
	if errors.Is(err, errFSHybridReplay) {
		s.counters["watch_replays"]++
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.counters["watch_snapshots"]++
	if trace := s.cfg.PhaseTrace; trace != nil {
		for _, entry := range ready {
			if entry.sequence != 0 && !entry.trace.metadataObserved {
				trace.record(s.state.incoming.direction, entry.info.Name, entry.sequence, "peer_metadata_observed")
				entry.trace.metadataObserved = true
			}
		}
		if s.state.acked > ackedBefore {
			for _, claim := range s.state.outNames {
				if claim.record.Sequence > ackedBefore && claim.record.Sequence <= s.state.acked && !claim.trace.acked {
					trace.record(s.cfg.Direction, claim.name, claim.record.Sequence, "sender_processing_ack")
					claim.trace.acked = true
				}
			}
		}
	}
	if advanced {
		s.scheduleACKLocked()
	}
	s.updateReadyLocked()
	s.mu.Unlock()
	for _, entry := range ready {
		s.dispatch(entry)
	}
	if advanced {
		s.signal(s.wake)
	}
	s.signal(s.fallbackWake)
	return nil
}

func (s *fsHybridPriorityStore) dispatch(entry *fsHybridInbound) {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || entry.done || entry.queued || s.receive == nil {
		s.mu.Unlock()
		return
	}
	entry.queued = true
	callback, info := s.receive, entry.info
	s.mu.Unlock()
	if !callback(info) {
		s.mu.Lock()
		entry.queued = false
		s.mu.Unlock()
	}
}

func (s *fsHybridPriorityStore) runWatch() {
	for attempts := 0; attempts < 3 && s.ctx.Err() == nil; attempts++ {
		// A reconnect starts from current cumulative state, never event count.
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.OperationTimeout)
		doc, err := s.carrier.ReadPeer(ctx)
		cancel()
		if err == nil {
			err = s.ingest(doc)
		}
		if err != nil && !fsHybridTransient(err) {
			s.fail(err)
			return
		}
		if s.ctx.Err() != nil {
			return
		}
		err = s.carrier.WatchPeer(s.ctx, s.ingest)
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errFSHybridClosed
		}
		if !fsHybridTransient(err) {
			s.fail(err)
			return
		}
		s.mu.Lock()
		s.counters["watch_reconnects"]++
		s.mu.Unlock()
	}
	if s.ctx.Err() == nil {
		s.retireWriter(errors.New("Firestore watch reconnect bound"))
	}
}

func (s *fsHybridPriorityStore) retireWriter(err error) {
	s.mu.Lock()
	if !s.retired {
		s.retired = true
		s.retireErr = err
		s.counters["writer_retirements"]++
	}
	for _, r := range s.state.outNames {
		if !r.processed && !r.fbDone {
			r.deadline = time.Time{}
		}
	}
	s.mu.Unlock()
	s.signal(s.fallbackWake)
}

func fsHybridDocumentAfter(doc, previous fsPriorityDocument, wire []byte) bool {
	if !doc.valid() || !previous.valid() || doc.FenceKind != previous.FenceKind || !bytes.Equal(doc.Ciphertext, wire) {
		return false
	}
	if doc.FenceKind == fsPriorityDriveV2Fence {
		return doc.OpaqueFence != previous.OpaqueFence
	}
	newTime, e1 := time.Parse(time.RFC3339Nano, doc.UpdateTime)
	oldTime, e2 := time.Parse(time.RFC3339Nano, previous.UpdateTime)
	return e1 == nil && e2 == nil && newTime.After(oldTime)
}

func fsHybridTransient(err error) bool {
	if err == nil || IsTerminalOAuthTokenError(err) || errors.Is(err, errFSHybridIntegrity) || errors.Is(err, errFSHybridBounds) || errors.Is(err, errFSHybridClosed) {
		return false
	}
	var api *GoogleAPIError
	if errors.As(err, &api) {
		return false
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if _, joined := cause.(interface{ Unwrap() []error }); joined {
			return false
		}
	}
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) || errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func fsHybridPrecondition(err error) bool {
	if err == nil || IsTerminalOAuthTokenError(err) || errors.Is(err, errFSHybridIntegrity) || errors.Is(err, errFSHybridBounds) || errors.Is(err, errFSHybridClosed) {
		return false
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if _, joined := cause.(interface{ Unwrap() []error }); joined {
			return false
		}
	}
	var api *GoogleAPIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Status == 412 || (api.Status == 400 || api.Status == 409) && strings.EqualFold(api.Reason, "FAILED_PRECONDITION")
}

func (s *fsHybridPriorityStore) writePrepared(previous fsPriorityDocument, wire []byte) (fsPriorityDocument, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.OperationTimeout)
		doc, err := s.carrier.WriteOwn(ctx, previous.fence(), wire)
		cancel()
		if err == nil {
			if !fsHybridDocumentAfter(doc, previous, wire) {
				return fsPriorityDocument{}, errFSHybridIntegrity
			}
			return doc, nil
		}
		if s.ctx.Err() != nil {
			return fsPriorityDocument{}, s.ctx.Err()
		}
		if !fsHybridPrecondition(err) && !fsHybridTransient(err) {
			return fsPriorityDocument{}, err
		}
		lastErr = err
		s.mu.Lock()
		s.counters["ambiguous_writes"]++
		s.mu.Unlock()
		readCtx, stop := context.WithTimeout(s.ctx, s.cfg.OperationTimeout)
		readback, readErr := s.carrier.ReadOwn(readCtx)
		stop()
		if s.ctx.Err() != nil {
			return fsPriorityDocument{}, s.ctx.Err()
		}
		if readErr != nil && !fsHybridTransient(readErr) {
			return fsPriorityDocument{}, readErr
		}
		if readErr == nil {
			if fsHybridDocumentAfter(readback, previous, wire) {
				s.mu.Lock()
				s.counters["exact_readbacks"]++
				s.mu.Unlock()
				return readback, nil
			}
			if !readback.sameFence(previous) || !bytes.Equal(readback.Ciphertext, previous.Ciphertext) {
				return fsPriorityDocument{}, errFSHybridIntegrity
			}
		}
		// The only retry is byte-identical under the SAME updateTime fence.
		// Even a failed read cannot authorize a newer prepared snapshot.
	}
	return fsPriorityDocument{}, lastErr
}

func (s *fsHybridPriorityStore) runWriter() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if s.closed || s.retired || !s.dirty {
				s.mu.Unlock()
				break
			}
			delay := time.Until(s.ackDue)
			if delay > 0 {
				s.mu.Unlock()
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-s.ctx.Done():
					timer.Stop()
					return
				}
				continue
			}
			s.dirty = false
			wire, high, err := s.state.prepare()
			previous := s.own
			if err == nil {
				if trace := s.cfg.PhaseTrace; trace != nil {
					for _, claim := range s.state.outQueue {
						if claim.record.Sequence <= high && !claim.trace.patchStart {
							trace.record(s.cfg.Direction, claim.name, claim.record.Sequence, "patch_started")
							claim.trace.patchStart = true
						}
					}
				}
			}
			s.mu.Unlock()
			if err != nil {
				s.fail(err)
				return
			}
			doc, err := s.writePrepared(previous, wire)
			if err != nil {
				if s.ctx.Err() != nil {
					return
				}
				if !fsHybridTransient(err) && !fsHybridPrecondition(err) {
					s.fail(err)
					return
				}
				s.retireWriter(err)
				return
			}
			s.mu.Lock()
			if s.closed || s.ctx.Err() != nil {
				s.counters["late_write_completions"]++
				s.mu.Unlock()
				return
			}
			s.own = doc
			s.ownPublished = true
			if trace := s.cfg.PhaseTrace; trace != nil {
				for _, claim := range s.state.outNames {
					if claim.record.Sequence != 0 && claim.record.Sequence <= high && !claim.trace.patchDone {
						trace.record(s.cfg.Direction, claim.name, claim.record.Sequence, "patch_completed")
						claim.trace.patchDone = true
					}
				}
			}
			s.state.published(high)
			s.counters["snapshot_writes"]++
			s.updateReadyLocked()
			s.mu.Unlock()
		}
	}
}

func (s *fsHybridPriorityStore) runFallbackScheduler() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.fallbackWake:
		}
		s.mu.Lock()
		now := time.Now()
		for _, r := range s.state.outNames {
			if r.processed || r.fbDone || r.fbQueued || r.fbAttempts >= 2 || now.Before(r.deadline) {
				continue
			}
			select {
			case s.fallbackJobs <- r:
				r.fbQueued = true
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *fsHybridPriorityStore) runFallbackWorker() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case r := <-s.fallbackJobs:
			s.mu.Lock()
			if r.processed || r.fbDone || s.closed || s.ctx.Err() != nil {
				s.mu.Unlock()
				continue
			}
			r.fbAttempts++
			body := bytes.Clone(r.sealed)
			id := r.physicalID
			s.counters["fallback_attempts"]++
			s.cfg.PhaseTrace.record(s.cfg.Direction, r.name, r.record.Sequence, "fallback_started")
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(s.ctx, s.cfg.OperationTimeout)
			var err error
			if id == "" {
				ids, e := s.base.(ObjectIDReserveStore).GenerateObjectIDs(ctx, 1)
				err = e
				if err == nil && (len(ids) != 1 || len(ids[0]) > 128 || !isSafeObjectSegment(ids[0])) {
					err = errFSHybridIntegrity
				}
				if err == nil {
					id = ids[0]
					s.mu.Lock()
					r.physicalID = id
					s.mu.Unlock()
				}
			}
			var info ObjectInfo
			if err == nil {
				info, err = s.base.(ObjectPutIDStore).PutObjectWithID(ctx, id, r.name, body)
			}
			cancel()
			s.mu.Lock()
			r.fbQueued = false
			r.fbErr = err
			if err == nil {
				r.fbDone = true
				fsHybridMarkPublished(r, info)
				s.counters["fallback_published"]++
				s.cfg.PhaseTrace.record(s.cfg.Direction, r.name, r.record.Sequence, "fallback_completed")
				if r.record.Sequence == 0 && len(r.sealed) > 0 {
					s.state.outRetained -= len(r.sealed)
					r.sealed = nil
				}
			} else {
				s.counters["fallback_errors"]++
				s.cfg.PhaseTrace.record(s.cfg.Direction, r.name, r.record.Sequence, "fallback_error")
				r.deadline = time.Now().Add(50 * time.Millisecond)
			}
			// A late processing ACK wins over an immutable fallback failure.
			// Never replace that successful ownership with a terminal error.
			if err != nil && r.fbAttempts >= 2 && !r.processed && !r.pubClosed {
				close(r.pubDone)
				r.pubClosed = true
			}
			s.mu.Unlock()
			if err != nil && !fsHybridTransient(err) {
				s.fail(err)
				return
			}
		}
	}
}

func (s *fsHybridPriorityStore) forceFallback(name string) {
	s.mu.Lock()
	if r := s.state.outNames[name]; r != nil {
		r.deadline = time.Time{}
	}
	s.mu.Unlock()
	s.signal(s.fallbackWake)
}

// Called only by the actual mux success path. No network, decoding, retries or
// allocation proportional to traffic: identity and capacity were reserved at
// discovery. Close fences late callbacks without inventing an ACK.
func (s *fsHybridPriorityStore) NoteMuxProcessed(name, id string) error {
	if !strings.Contains(name, "/p0/") {
		return nil
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil
	}
	entry := s.state.rxIDs[id]
	if entry == nil || entry.info.Name != name {
		s.mu.Unlock()
		s.fail(errFSHybridIntegrity)
		return errFSHybridIntegrity
	}
	wasDone := entry.done
	_, _, advanced, err := s.state.markProcessed(id)
	if err == nil {
		if !wasDone {
			s.counters["processed_receipts"]++
			if !entry.trace.processed {
				s.cfg.PhaseTrace.record(s.state.incoming.direction, entry.info.Name, entry.sequence, "receiver_processed")
				entry.trace.processed = true
			}
		} else {
			s.counters["duplicate_processing_receipts"]++
		}
		if advanced {
			s.scheduleACKLocked()
		}
		if !wasDone {
			// A single processing receipt may free enough cache for both
			// prefetch owners. Wake the generation, not just one waiter.
			close(s.space)
			s.space = make(chan struct{})
		}
	}
	s.mu.Unlock()
	if err != nil {
		s.fail(err)
		return err
	}
	if advanced {
		s.signal(s.wake)
	}
	return nil
}
