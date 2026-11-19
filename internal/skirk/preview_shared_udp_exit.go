package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pilotSharedMaxAssociations  = 16
	pilotSharedRetirementWindow = 256
	pilotSharedReapInterval     = time.Second
	pilotSharedSocketRetry      = time.Second
	pilotSharedSocketAttempts   = 3
	pilotSharedProxyPending     = pilotMaxBatch
	pilotSharedProxyPackets     = pilotQueuePackets
	pilotSharedProxyBytes       = pilotFairQueueBytes
)

var (
	errSharedUDPRetirementLimit = errors.New("shared UDP association retirement limit reached")
	errSharedUDPSetupExhausted  = errors.New("shared UDP origin setup attempts exhausted")
	errSharedUDPResetQueueFull  = errors.New("shared UDP association reset queue full")
)

type pilotSharedIDRange struct{ first, last uint32 }

// Keep exact close ranges for this lease. Arrival order over Drive is
// independent of ID allocation and ordered control, so gaps remain eligible.
type pilotSharedRetiredIDs []pilotSharedIDRange

func (r pilotSharedRetiredIDs) search(id uint32) int {
	return sort.Search(len(r), func(i int) bool { return r[i].last >= id })
}

func (r pilotSharedRetiredIDs) contains(id uint32) bool {
	i := r.search(id)
	return i < len(r) && r[i].first <= id
}

func (r *pilotSharedRetiredIDs) add(id uint32) bool {
	ranges := *r
	i := ranges.search(id)
	if i < len(ranges) && ranges[i].first <= id {
		return true
	}
	left := i > 0 && ranges[i-1].last == id-1
	right := i < len(ranges) && ranges[i].first-1 == id
	switch {
	case left && right:
		ranges[i-1].last = ranges[i].last
		*r = slices.Delete(ranges, i, i+1)
	case left:
		ranges[i-1].last = id
	case right:
		ranges[i].first = id
	case len(ranges) >= pilotSharedRetirementWindow:
		return false
	default:
		*r = slices.Insert(ranges, i, pilotSharedIDRange{id, id})
	}
	return true
}

type pilotSharedUDPAssociation struct {
	ctx          context.Context
	cancel       context.CancelFunc
	mapping      *pilotUDPMapping
	lastActivity atomic.Int64
	// The remaining fields are owned by the manager mutex.
	stopping     bool
	closed       bool
	opening      bool
	resetPending bool
	pending      []pilotPacket
	pendingBytes int
	retryAfter   time.Time
	setupFails   int
	byFlow       map[uint64]netip.AddrPort
	byEndpoint   map[netip.AddrPort]uint64
}

type pilotSharedUDPExit struct {
	ctx             context.Context
	cancel          context.CancelFunc
	mailbox         *pilotMailbox
	started         time.Time
	done            chan struct{}
	onFailure       func(error)
	resets          chan<- pilotAssociationReset
	maxAssociations int
	failOnce        sync.Once
	wg              sync.WaitGroup
	mu              sync.Mutex
	groups          map[uint32]*pilotSharedUDPAssociation
	retired         pilotSharedRetiredIDs
	closing         bool
	pendingPackets  int
	pendingBytes    int
}

func newPilotSharedUDPExit(parent context.Context, mailbox *pilotMailbox, maxAssociations int, onFailure func(error)) *pilotSharedUDPExit {
	return newPilotSharedUDPExitWithResets(parent, mailbox, maxAssociations, nil, onFailure)
}

func newPilotSharedUDPExitWithResets(parent context.Context, mailbox *pilotMailbox, maxAssociations int, resets chan<- pilotAssociationReset, onFailure func(error)) *pilotSharedUDPExit {
	ctx, cancel := context.WithCancel(parent)
	s := &pilotSharedUDPExit{
		ctx: ctx, cancel: cancel, mailbox: mailbox, started: time.Now(), done: make(chan struct{}), onFailure: onFailure,
		maxAssociations: maxAssociations, resets: resets,
		groups: make(map[uint32]*pilotSharedUDPAssociation),
	}
	go s.run()
	return s
}

func (s *pilotSharedUDPExit) send(ctx context.Context, packet pilotPacket) {
	id := uint32(packet.flow >> 32)
	if id == 0 || uint32(packet.flow) == 0 || len(packet.body) > pilotMaxPayload ||
		!permitPreviewTarget(s.mailbox.allowed, s.mailbox.publicIPv4, s.mailbox.publicIPv6, packet.target) {
		s.mailbox.rejected.Add(1)
		return
	}
	if s.mailbox.exitProxy != "" {
		if reason := s.mailbox.incomingExpiry(packet, time.Now()); reason != pilotPacketFresh {
			s.mailbox.recordExpiry(reason, 1)
			return
		}
	}
	s.mu.Lock()
	if ctx.Err() != nil || s.ctx.Err() != nil || s.closing {
		s.mu.Unlock()
		return
	}
	group := s.groups[id]
	if s.retired.contains(id) {
		s.mailbox.rejected.Add(1)
		s.mu.Unlock()
		return
	}
	if group == nil {
		if len(s.groups) >= s.maxAssociations {
			s.mailbox.flowCap.Add(1)
			s.mu.Unlock()
			return
		}
		group = &pilotSharedUDPAssociation{}
		s.groups[id] = group
	}
	if group.closed || group.stopping || time.Now().Before(group.retryAfter) {
		s.mailbox.dropped.Add(1)
		s.mu.Unlock()
		return
	}
	if s.mailbox.exitProxy != "" && (group.opening || group.mapping == nil) {
		s.queueProxySetupLocked(id, group, packet)
		s.mu.Unlock()
		return
	}
	if group.mapping == nil {
		if err := s.startMappingLocked(id, group); err != nil {
			s.mailbox.dropped.Add(1)
			terminal := s.recordSetupFailureLocked(id, group, err)
			s.mu.Unlock()
			if terminal != nil {
				s.fail(terminal)
			}
			return
		}
	}
	mapping, mappingCtx := group.mapping, group.ctx
	s.mu.Unlock()
	mapping.send(mappingCtx, packet)
}

func (s *pilotSharedUDPExit) startMappingLocked(id uint32, group *pilotSharedUDPAssociation) error {
	group.ctx, group.cancel = context.WithCancel(s.ctx)
	group.lastActivity.Store(time.Since(s.started).Nanoseconds())
	mapping, err := s.newMapping(id, group, group.ctx)
	if err != nil {
		group.cancel()
		group.cancel = nil
		return err
	}
	s.installMappingLocked(id, group, mapping)
	return nil
}

func (s *pilotSharedUDPExit) newMapping(id uint32, group *pilotSharedUDPAssociation, attempt context.Context) (*pilotUDPMapping, error) {
	return newPilotUDPMappingWithCallbacks(attempt, s.mailbox, func(err error) {
		s.mappingFailed(id, group, attempt, err)
	}, func() {
		group.lastActivity.Store(time.Since(s.started).Nanoseconds())
	}, func(flow uint64, source netip.AddrPort, body []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Serialize reply admission with retirement and queue cleanup.
		if s.ownsMappingAttemptLocked(id, group, attempt) {
			s.mailbox.offer(flow, source, body)
		}
	})
}

func (s *pilotSharedUDPExit) mappingFailed(id uint32, group *pilotSharedUDPAssociation, attempt context.Context, err error) {
	s.mu.Lock()
	if !s.ownsMappingAttemptLocked(id, group, attempt) {
		s.mu.Unlock()
		return
	}
	terminal := s.resetAssociationLocked(id, err)
	s.mu.Unlock()
	if terminal != nil {
		s.fail(terminal)
	}
}

func (s *pilotSharedUDPExit) ownsMappingAttemptLocked(id uint32, group *pilotSharedUDPAssociation, attempt context.Context) bool {
	return !s.closing && s.groups[id] == group && group.ctx == attempt &&
		!group.closed && !group.stopping && attempt.Err() == nil
}

func (s *pilotSharedUDPExit) recordSetupFailureLocked(id uint32, group *pilotSharedUDPAssociation, err error) error {
	if errors.Is(err, errSOCKSUDPUnsupported) || errors.Is(err, errSOCKS5Authentication) {
		s.closing = true
		return err
	}
	group.setupFails++
	if group.setupFails >= pilotSharedSocketAttempts {
		return s.resetAssociationLocked(id, errors.Join(errSharedUDPSetupExhausted, err))
	}
	group.retryAfter = time.Now().Add(pilotSharedSocketRetry << (group.setupFails - 1))
	return nil
}

func (s *pilotSharedUDPExit) queueProxySetupLocked(id uint32, group *pilotSharedUDPAssociation, packet pilotPacket) {
	now := time.Now()
	s.expireProxyPendingLocked(now)
	if reason := s.mailbox.incomingExpiry(packet, now); reason != pilotPacketFresh {
		s.mailbox.recordExpiry(reason, 1)
		return
	}
	if len(group.pending) >= pilotSharedProxyPending {
		s.popProxyPendingLocked(group)
		s.mailbox.dropped.Add(1)
	}
	cost := pilotPacketHeader + len(packet.body)
	for s.pendingPackets >= pilotSharedProxyPackets || s.pendingBytes+cost > pilotSharedProxyBytes {
		s.dropProxyPendingLocked(group, s.pendingBytes+cost > pilotSharedProxyBytes)
	}
	// A packet body may otherwise retain its entire decrypted Drive batch.
	packet.body = bytes.Clone(packet.body)
	group.pending = append(group.pending, packet)
	group.pendingBytes += cost
	s.pendingPackets++
	s.pendingBytes += cost
	if group.opening {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	group.ctx, group.cancel, group.opening = ctx, cancel, true
	group.lastActivity.Store(time.Since(s.started).Nanoseconds())
	s.wg.Go(func() { s.openProxyMapping(id, group, ctx, cancel) })
}

func (s *pilotSharedUDPExit) popProxyPendingLocked(group *pilotSharedUDPAssociation) pilotPacket {
	packet := group.pending[0]
	group.pending[0] = pilotPacket{}
	group.pending = group.pending[1:]
	cost := pilotPacketHeader + len(packet.body)
	group.pendingBytes -= cost
	s.pendingBytes -= cost
	s.pendingPackets--
	return packet
}

func (s *pilotSharedUDPExit) dropProxyPendingLocked(incoming *pilotSharedUDPAssociation, byBytes bool) {
	victim := incoming
	for _, group := range s.groups {
		if byBytes && group.pendingBytes > victim.pendingBytes || !byBytes && len(group.pending) > len(victim.pending) {
			victim = group
		}
	}
	s.popProxyPendingLocked(victim)
	s.mailbox.dropped.Add(1)
}

func (s *pilotSharedUDPExit) expireProxyPendingLocked(now time.Time) {
	if s.pendingPackets == 0 {
		return
	}
	for _, group := range s.groups {
		fresh := group.pending[:0]
		for _, packet := range group.pending {
			reason := s.mailbox.incomingExpiry(packet, now)
			if reason == pilotPacketFresh {
				fresh = append(fresh, packet)
				continue
			}
			cost := pilotPacketHeader + len(packet.body)
			group.pendingBytes -= cost
			s.pendingBytes -= cost
			s.pendingPackets--
			s.mailbox.recordExpiry(reason, 1)
		}
		clear(group.pending[len(fresh):])
		group.pending = fresh
	}
}

func (s *pilotSharedUDPExit) discardProxyPendingLocked(group *pilotSharedUDPAssociation) {
	s.mailbox.dropped.Add(uint64(len(group.pending)))
	s.pendingPackets -= len(group.pending)
	s.pendingBytes -= group.pendingBytes
	group.pending, group.pendingBytes = nil, 0
}

func (s *pilotSharedUDPExit) openProxyMapping(id uint32, group *pilotSharedUDPAssociation, ctx context.Context, cancel context.CancelFunc) {
	// Upstream TCP/auth/UDP negotiation may block. Keep it outside the
	// shared manager lock and independent of Drive discovery and close reads.
	mapping, err := s.newMapping(id, group, ctx)
	s.mu.Lock()
	if group.closed || ctx.Err() != nil || s.closing || s.groups[id] != group {
		s.discardProxyPendingLocked(group)
		s.mu.Unlock()
		cancel()
		if mapping != nil {
			<-mapping.done
		}
		s.mu.Lock()
		group.opening = false
		if s.groups[id] == group && !group.resetPending {
			delete(s.groups, id)
		}
		s.mu.Unlock()
		return
	}
	if err != nil {
		cancel()
		group.cancel = nil
		s.discardProxyPendingLocked(group)
		group.opening = false
		terminal := s.recordSetupFailureLocked(id, group, err)
		s.mu.Unlock()
		if terminal != nil {
			s.fail(terminal)
		}
		return
	}
	s.installMappingLocked(id, group, mapping)
	s.mu.Unlock()
	s.drainProxyPending(id, group, mapping, ctx)
}

func (s *pilotSharedUDPExit) drainProxyPending(id uint32, group *pilotSharedUDPAssociation, mapping *pilotUDPMapping, ctx context.Context) {
	for {
		s.mu.Lock()
		if group.closed || group.stopping || ctx.Err() != nil || s.closing || s.groups[id] != group || group.mapping != mapping {
			s.discardProxyPendingLocked(group)
			group.opening = false
			s.releaseRetiredLocked(id, group)
			s.mu.Unlock()
			return
		}
		s.expireProxyPendingLocked(time.Now())
		if len(group.pending) == 0 {
			// Only now can newer packets go directly to the socket.
			group.opening = false
			s.mu.Unlock()
			return
		}
		packet := s.popProxyPendingLocked(group)
		s.mu.Unlock()
		if ctx.Err() != nil {
			s.mailbox.dropped.Add(1)
			continue
		}
		mapping.send(ctx, packet)
	}
}

func (s *pilotSharedUDPExit) installMappingLocked(id uint32, group *pilotSharedUDPAssociation, mapping *pilotUDPMapping) {
	group.mapping = mapping
	group.setupFails = 0
	group.retryAfter = time.Time{}
	if group.byFlow != nil {
		mapping.mu.Lock()
		mapping.byFlow, mapping.byEndpoint = maps.Clone(group.byFlow), maps.Clone(group.byEndpoint)
		mapping.mu.Unlock()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-mapping.done
		mapping.mu.Lock()
		byFlow, byEndpoint := maps.Clone(mapping.byFlow), maps.Clone(mapping.byEndpoint)
		mapping.mu.Unlock()
		s.mu.Lock()
		if s.groups[id] == group && group.mapping == mapping {
			group.byFlow, group.byEndpoint = byFlow, byEndpoint
			group.mapping, group.cancel, group.stopping = nil, nil, false
			s.releaseRetiredLocked(id, group)
		}
		s.mu.Unlock()
	}()
}

func (s *pilotSharedUDPExit) closeAssociation(id uint32) error {
	if id == 0 {
		return errors.New("invalid shared UDP association close")
	}
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil
	}
	err := s.retireAssociationLocked(id)
	s.mu.Unlock()
	if err != nil {
		s.fail(err)
	}
	return err
}

func (s *pilotSharedUDPExit) resetAssociationLocked(id uint32, err error) error {
	if s.resets == nil {
		// Legacy control cannot notify a single local association.
		s.closing = true
		return err
	}
	group := s.groups[id]
	if group == nil || group.closed {
		return nil
	}
	// The old socket and its control notification share one admission slot.
	// A new datagram cannot refill that slot while the control writer stalls.
	group.resetPending = true
	if err := s.retireAssociationLocked(id); err != nil {
		return err
	}
	s.mailbox.closeQueuedAssociation(id)
	reset := pilotAssociationReset{id: id, written: func() {
		s.mu.Lock()
		group.resetPending = false
		s.releaseRetiredLocked(id, group)
		s.mu.Unlock()
	}}
	select {
	case s.resets <- reset:
		return nil
	default:
		// Each queued or in-flight reset owns a group in the bounded map.
		s.closing = true
		return errSharedUDPResetQueueFull
	}
}

func (s *pilotSharedUDPExit) retireAssociationLocked(id uint32) error {
	if !s.retired.add(id) {
		s.closing = true
		return errSharedUDPRetirementLimit
	}
	if group := s.groups[id]; group != nil {
		group.closed = true
		s.discardProxyPendingLocked(group)
		if group.opening && group.cancel != nil {
			group.cancel()
		} else if group.mapping != nil && !group.stopping {
			group.stopping = true
			group.cancel()
		}
		s.releaseRetiredLocked(id, group)
	}
	return nil
}

func (s *pilotSharedUDPExit) releaseRetiredLocked(id uint32, group *pilotSharedUDPAssociation) {
	if s.groups[id] == group && group.closed && !group.opening && group.mapping == nil && !group.resetPending {
		delete(s.groups, id)
	}
}

func (s *pilotSharedUDPExit) fail(err error) {
	s.failOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.cancel()
		s.mu.Unlock()
		if s.onFailure != nil {
			s.onFailure(err)
		}
	})
}

func (s *pilotSharedUDPExit) run() {
	defer close(s.done)
	defer s.cancel()
	ticker := time.NewTicker(pilotSharedReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.mu.Lock()
			s.closing = true
			for id, group := range s.groups {
				group.closed = true
				s.discardProxyPendingLocked(group)
				if group.cancel != nil {
					group.cancel()
				} else {
					delete(s.groups, id)
				}
			}
			s.mu.Unlock()
			s.wg.Wait()
			s.mu.Lock()
			clear(s.groups)
			s.mu.Unlock()
			return
		case now := <-ticker.C:
			s.reap(now)
		}
	}
}

func (s *pilotSharedUDPExit) reap(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireProxyPendingLocked(now)
	for _, group := range s.groups {
		if group.mapping != nil && !group.stopping && now.Sub(s.started)-time.Duration(group.lastActivity.Load()) >= udpPreviewStableIdle {
			// The local association still exists. Retain its identity while
			// reclaiming the idle socket pair, like an expired NAT mapping.
			group.stopping = true
			group.cancel()
		}
	}
}

func servePilotSharedUDPControl(reader io.Reader, closeAssociation func(uint32) error) error {
	if closeAssociation == nil {
		return errors.New("shared UDP association control unavailable")
	}
	var frame [4]byte
	for {
		if _, err := io.ReadFull(reader, frame[:]); err != nil {
			return err
		}
		if err := closeAssociation(binary.BigEndian.Uint32(frame[:])); err != nil {
			return err
		}
	}
}
