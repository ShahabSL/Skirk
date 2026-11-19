package skirk

import (
	"context"
	"net"
	"sync"
	"time"
)

// Local sockets that are waiting for traffic or only resolving DNS do not
// reserve remote UDP state. Bound those sockets independently.
const pilotSharedLocalFrontendLimit = 128

func (p *datagramPreview) frontendLimit() int {
	if p.sharedUDP {
		return pilotSharedLocalFrontendLimit
	}
	return pilotSharedAssociationLimit(p.sharedLimit)
}

// Admission precedes the local UDP socket and HEV writer, including frontends
// that only send DNS and therefore never attach to a Drive carrier.
func (p *datagramPreview) admitFrontend() (func(), bool) {
	p.frontendMu.Lock()
	defer p.frontendMu.Unlock()
	if p.frontendCount >= p.frontendLimit() {
		p.frontendLimitHits.Add(1)
		return nil, false
	}
	p.frontendCount++
	return func() {
		p.frontendMu.Lock()
		p.frontendCount--
		p.frontendMu.Unlock()
	}, true
}

// DNS activity keeps a local RFC socket alive without extending a media lease.
func pilotFrontendIdleContext(parent context.Context, idle time.Duration) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancel(parent)
	var mu sync.Mutex
	lastActivity := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				mu.Lock()
				remaining := idle - time.Since(lastActivity)
				if remaining <= 0 {
					cancel()
					mu.Unlock()
					return
				}
				timer.Reset(remaining)
				mu.Unlock()
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, func() {
		mu.Lock()
		lastActivity = time.Now()
		mu.Unlock()
	}
}

type pilotMailboxFrontend struct{ stop func() }

// A shared idle/failed lease must also interrupt inline DNS work, which is
// not reading the control socket while it waits for the resolver.
type pilotFrontendControl struct {
	net.Conn
	cancel context.CancelFunc
}

func (c pilotFrontendControl) Close() error {
	c.cancel()
	return c.Conn.Close()
}

// The closing gate makes Add/Wait safe and prevents a terminating carrier from
// accepting a frontend after its final metrics snapshot.
func (p *pilotMailbox) attachFrontend(stop func()) (func(), bool) {
	p.frontendMu.Lock()
	defer p.frontendMu.Unlock()
	if p.frontendsClosing {
		return nil, false
	}
	if p.frontends == nil {
		p.frontends = make(map[*pilotMailboxFrontend]struct{})
	}
	frontend := &pilotMailboxFrontend{stop: stop}
	p.frontends[frontend] = struct{}{}
	p.frontendWG.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			p.frontendMu.Lock()
			delete(p.frontends, frontend)
			p.frontendMu.Unlock()
			p.frontendWG.Done()
		})
	}, true
}

func (p *pilotMailbox) stopFrontends() {
	p.frontendMu.Lock()
	p.frontendsClosing = true
	stops := make([]func(), 0, len(p.frontends))
	for frontend := range p.frontends {
		stops = append(stops, frontend.stop)
	}
	p.frontendMu.Unlock()
	for _, stop := range stops {
		stop()
	}
}
