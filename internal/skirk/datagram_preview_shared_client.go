package skirk

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errSharedUDPNoAssociations = errors.New("shared UDP has no local associations")
	errSharedUDPCloseQueueFull = errors.New("shared UDP close queue full")
	errSharedUDPRetiring       = errors.New("shared UDP generation retiring")
)

// pilotSharedClient owns one lazily opened lease for a client mux. A pending
// local association holds a reference while the mux authenticates the peer.
type pilotSharedClient struct {
	ctx             context.Context
	open            func(context.Context) (*pilotClientLease, error)
	onLimit         func()
	maxAssociations int

	mu      sync.Mutex
	current *pilotSharedClientGeneration
}

type pilotSharedClientGeneration struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	ready   chan struct{}
	done    chan struct{}
	closed  chan uint32
	started time.Time

	links         map[uint32]*pilotSharedClientLink // Includes waiters for authentication.
	pendingCloses int                               // Queued and in-flight, protected by the client mutex.
	nextID        uint32                            // Protected by the client mutex, like links.
	lease         *pilotClientLease
	err           error // lease and err are published by closing ready.
}

type pilotSharedClientLink struct {
	control      net.Conn
	started      time.Time
	active       atomic.Bool
	lastActivity atomic.Int64 // Negative after expiry or release.
	remoteReset  bool         // Protected by the client mutex.
}

// The lease control registers this handler before associations can send.
type pilotSharedAssociationResetSource interface {
	setAssociationResetHandler(func(uint32))
}

func (g *pilotSharedClientGeneration) retiring() bool {
	if g.ctx.Err() != nil {
		return true
	}
	select {
	case <-g.ready:
		return g.lease != nil && g.lease.ctx.Err() != nil
	default:
		return false
	}
}

func (l *pilotSharedClientLink) touch() {
	now := time.Since(l.started).Nanoseconds()
	for last := l.lastActivity.Load(); last >= 0 && now > last; last = l.lastActivity.Load() {
		if l.lastActivity.CompareAndSwap(last, now) {
			return
		}
	}
}

func (s *pilotSharedClient) acquire(ctx context.Context, association net.Conn) (*pilotClientLease, uint32, func(), func(), error) {
	limit := pilotSharedAssociationLimit(s.maxAssociations)
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, nil, err
		}
		if err := s.ctx.Err(); err != nil {
			return nil, 0, nil, nil, err
		}
		s.mu.Lock()
		generation := s.current
		if generation != nil && generation.retiring() {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, 0, nil, nil, ctx.Err()
			case <-generation.done:
				continue
			}
		}
		if generation == nil {
			leaseCtx, cancel := context.WithCancelCause(s.ctx)
			generation = &pilotSharedClientGeneration{
				ctx: leaseCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}),
				closed: make(chan uint32, limit), started: time.Now(),
				links: make(map[uint32]*pilotSharedClientLink),
			}
			s.current = generation
			go s.run(generation)
		}
		if len(generation.links)+generation.pendingCloses >= limit || generation.nextID == ^uint32(0) {
			s.mu.Unlock()
			if s.onLimit != nil {
				s.onLimit()
			}
			return nil, 0, nil, nil, errors.New("shared UDP local association limit reached")
		}
		generation.nextID++
		id := generation.nextID
		link := &pilotSharedClientLink{control: association, started: generation.started}
		generation.links[id] = link
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			s.release(generation, id, false)
			return nil, 0, nil, nil, ctx.Err()
		case <-generation.ready:
		}
		if generation.err != nil {
			s.release(generation, id, false)
			return nil, 0, nil, nil, generation.err
		}
		if err := ctx.Err(); err != nil {
			s.release(generation, id, false)
			return nil, 0, nil, nil, err
		}
		lease := generation.lease
		if lease.ctx.Err() != nil {
			s.release(generation, id, false)
			select {
			case <-ctx.Done():
				return nil, 0, nil, nil, ctx.Err()
			case <-generation.done:
				continue
			}
		}
		stop := context.AfterFunc(lease.ctx, func() { _ = association.Close() })
		link.touch()
		link.active.Store(true)
		var once sync.Once
		return lease, id, func() {
			once.Do(func() {
				stop()
				s.release(generation, id, true)
			})
		}, link.touch, nil
	}
}

func (s *pilotSharedClient) release(generation *pilotSharedClientGeneration, id uint32, attached bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link := generation.links[id]
	link.lastActivity.Store(-1)
	delete(generation.links, id)
	if len(generation.links) == 0 {
		generation.cancel(errSharedUDPNoAssociations)
		return
	}
	if !attached || link.remoteReset || generation.ctx.Err() != nil {
		return
	}
	select {
	case generation.closed <- id:
		// Keep the slot charged while the notification is queued or being
		// written so churn cannot fill the queue and cancel active flows.
		generation.pendingCloses++
	default:
		// The shared admission budget prevents this for a live generation.
		generation.cancel(errSharedUDPCloseQueueFull)
	}
}

func (s *pilotSharedClient) resetAssociation(generation *pilotSharedClientGeneration, id uint32) {
	s.mu.Lock()
	link := generation.links[id]
	if s.current != generation || link == nil || link.remoteReset || !link.active.Load() {
		s.mu.Unlock()
		return
	}
	link.remoteReset = true
	link.lastActivity.Store(-1)
	s.mu.Unlock()
	// The frontend owns release and flow cleanup. Close outside s.mu.
	_ = link.control.Close()
}

func (s *pilotSharedClient) reap(ctx context.Context, generation *pilotSharedClientGeneration) {
	tick := time.NewTicker(pilotSharedReapInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			var expired []net.Conn
			s.mu.Lock()
			for _, link := range generation.links {
				last := link.lastActivity.Load()
				if link.active.Load() && last >= 0 && now.Sub(generation.started)-time.Duration(last) >= udpPreviewStableIdle &&
					link.lastActivity.CompareAndSwap(last, -1) {
					expired = append(expired, link.control)
				}
			}
			s.mu.Unlock()
			for _, control := range expired {
				_ = control.Close()
			}
		}
	}
}

func (s *pilotSharedClient) run(generation *pilotSharedClientGeneration) {
	defer func() {
		generation.cancel(nil)
		s.mu.Lock()
		s.current = nil
		close(generation.done)
		s.mu.Unlock()
	}()
	lease, err := s.open(generation.ctx)
	generation.lease, generation.err = lease, err
	if err != nil {
		generation.cancel(err)
	} else if control, ok := lease.control.(pilotSharedAssociationResetSource); ok {
		control.setAssociationResetHandler(func(id uint32) {
			s.resetAssociation(generation, id)
		})
	}
	close(generation.ready)
	if err != nil {
		return
	}
	reason := "mux_or_peer_closed"
	reapCtx, stopReaping := context.WithCancel(lease.ctx)
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		s.reap(reapCtx, generation)
	}()
	defer func() {
		// Stop admitting associations before waiting for the old Drive
		// workers and mux stream to finish. Acquirers wait on done.
		s.mu.Lock()
		generation.cancel(errSharedUDPRetiring)
		s.mu.Unlock()
		stopReaping()
		<-reaped
		lease.finish(reason)
	}()
	for {
		select {
		case <-lease.ctx.Done():
			return
		case id := <-generation.closed:
			var frame [pilotControlMaxBody]byte
			data := binary.BigEndian.AppendUint32(frame[:0], id)
			// Coalesce already queued closes into one reliable stream write.
			for n := min(len(generation.closed), pilotControlAssociationBatch-1); n > 0; n-- {
				data = binary.BigEndian.AppendUint32(data, <-generation.closed)
			}
			if err := lease.control.SetWriteDeadline(time.Now().Add(udpPreviewHandshake)); err != nil {
				reason = "shared_control_write_failed"
				return
			}
			if err := writeAll(lease.control, data); err != nil {
				reason = "shared_control_write_failed"
				return
			}
			s.mu.Lock()
			generation.pendingCloses -= len(data) / 4
			s.mu.Unlock()
		}
	}
}
