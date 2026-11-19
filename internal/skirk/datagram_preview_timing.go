package skirk

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	pilotTimingRenewInterval = 30 * time.Second
	pilotTimingMaxEpochAge   = 60 * time.Second
	pilotTimingRetireAfter   = pilotLifetime
	pilotTimingWallJump      = 250 * time.Millisecond
	pilotTimingMaxEpochs     = 3

	// A finite engineering bound, not a claim that network samples prove clock
	// rate. Both clocks may differ from elapsed real time by at most 1000 ppm.
	// Outward rounding bounds age uncertainty. Epoch renewal prevents the
	// clock-rate contribution from growing indefinitely.
	pilotTimingRatePPM = int64(1000)
	pilotTimingPPM     = int64(1_000_000)
)

var (
	errPilotTimingUncertain = errors.New("UDP timing unavailable; retry when the connection is stable")
	errPilotTimingInvalid   = errors.New("invalid UDP timing epoch")
)

type pilotTimingEpoch struct {
	sendOrigin      time.Time
	receiveOrigin   time.Time
	retireReceiveAt time.Time
	retireSendAt    time.Time
}

// pilotTimingState is independent of UDP associations and their origin sockets.
// Each authenticated packet names an immutable epoch. Renewing a timing epoch
// must never relabel packets already queued under an earlier epoch.
type pilotTimingState struct {
	now    func() time.Time
	notify func() // Called without mu held.

	mu               sync.Mutex
	epochs           map[uint64]pilotTimingEpoch
	active           uint64
	highID           uint64
	peerHighID       uint64
	lastObserved     time.Time
	continuityOrigin time.Time
	discontinuous    time.Time
}

func newPilotTimingState() *pilotTimingState {
	return &pilotTimingState{epochs: make(map[uint64]pilotTimingEpoch)}
}

func (p *pilotTimingState) clockNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *pilotTimingState) begin() time.Time { return p.nowTime() }

func (p *pilotTimingState) setNotify(fn func()) {
	p.mu.Lock()
	p.notify = fn
	p.mu.Unlock()
}

func (p *pilotTimingState) nowTime() time.Time {
	now := p.clockNow()
	p.mu.Lock()
	changed := p.observeLocked(now)
	p.mu.Unlock()
	p.signal(changed)
	return now
}

func (p *pilotTimingState) signal(needed bool) {
	if needed {
		p.mu.Lock()
		fn := p.notify
		p.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
}

// This helper lets tests represent a suspend on systems whose monotonic clock
// pauses, without manufacturing time.Time's private monotonic representation.
func pilotTimingClockDiscontinuous(monotonicElapsed, wallElapsed time.Duration) bool {
	if monotonicElapsed < 0 {
		return true
	}
	var difference uint64
	if monotonicElapsed >= wallElapsed {
		difference = uint64(monotonicElapsed) - uint64(wallElapsed)
	} else {
		difference = uint64(wallElapsed) - uint64(monotonicElapsed)
	}
	return difference > uint64(pilotTimingWallJump)
}

// Use both local elapsed observations conservatively. A stopped monotonic
// clock cannot make received data younger while the wall clock advances.
// A wall correction cannot make it younger while monotonic time advances.
// Simultaneous faults in both clocks are outside the stated rate model.
func pilotElapsedBounds(origin, now time.Time) (time.Duration, time.Duration) {
	monotonic := now.Sub(origin)
	wall := now.Round(0).Sub(origin.Round(0))
	return min(monotonic, wall), max(monotonic, wall)
}

func pilotElapsedUpper(origin, now time.Time) time.Duration {
	_, high := pilotElapsedBounds(origin, now)
	return high
}

func (p *pilotTimingState) observeLocked(now time.Time) bool {
	if p.lastObserved.IsZero() {
		p.lastObserved = now
		p.continuityOrigin = now
		return false
	}
	// Callers may capture an ingress time before another goroutine samples a
	// later instant. Such a delayed observation is not a clock regression.
	if now.Before(p.lastObserved) {
		return false
	}
	monotonicElapsed := now.Sub(p.continuityOrigin)
	wallElapsed := now.Round(0).Sub(p.continuityOrigin.Round(0))
	p.lastObserved = now
	return p.observeElapsedLocked(now, monotonicElapsed, wallElapsed)
}

func (p *pilotTimingState) observeElapsedLocked(now time.Time, monotonicElapsed, wallElapsed time.Duration) bool {
	if !pilotTimingClockDiscontinuous(monotonicElapsed, wallElapsed) {
		return false
	}
	p.invalidateLocked(now)
	return true
}

func (p *pilotTimingState) invalidateLocked(now time.Time) {
	clear(p.epochs)
	p.active = 0
	p.discontinuous = now
	p.continuityOrigin = now
}

func (p *pilotTimingState) invalidate() {
	now := p.clockNow()
	p.mu.Lock()
	p.invalidateLocked(now)
	p.lastObserved = now
	p.mu.Unlock()
	p.signal(true)
}

// Durations are bounded before multiplication, so the integer arithmetic
// cannot overflow. Positive terms round up and subtracted terms round down.
func pilotTimingScaleUp(elapsed time.Duration) time.Duration {
	n := int64(elapsed) * pilotTimingPPM
	d := pilotTimingPPM - pilotTimingRatePPM
	return time.Duration((n + d - 1) / d)
}

func pilotTimingScaleDown(elapsed time.Duration) time.Duration {
	return time.Duration(int64(elapsed) * pilotTimingPPM / (pilotTimingPPM + pilotTimingRatePPM))
}

func (p *pilotTimingState) install(id uint64, sendOrigin, receiveOrigin time.Time) error {
	now := p.clockNow()
	p.mu.Lock()
	changed := p.observeLocked(now)
	changed = p.pruneLocked(now) || changed
	defer func() { p.mu.Unlock(); p.signal(changed) }()
	localEpoch := sendOrigin
	if receiveOrigin.Before(localEpoch) {
		localEpoch = receiveOrigin
	}
	if !localEpoch.IsZero() && !now.Before(localEpoch) && pilotTimingClockDiscontinuous(now.Sub(localEpoch), now.Round(0).Sub(localEpoch.Round(0))) {
		p.invalidateLocked(now)
		changed = true
		return fmt.Errorf("%w: clock changed during timing exchange", errPilotTimingUncertain)
	}
	if !p.discontinuous.IsZero() && localEpoch.Before(p.discontinuous) {
		return fmt.Errorf("%w: sample predates a clock continuity reset", errPilotTimingUncertain)
	}
	if id == 0 || id <= p.highID || sendOrigin.IsZero() || receiveOrigin.IsZero() || sendOrigin.After(now) || receiveOrigin.After(now) ||
		pilotElapsedUpper(localEpoch, now) > pilotTimingMaxEpochAge {
		return fmt.Errorf("%w: stale, replayed, or discontinuous sample", errPilotTimingInvalid)
	}
	if len(p.epochs) >= pilotTimingMaxEpochs {
		return fmt.Errorf("%w: timing epochs are still retiring", errPilotTimingUncertain)
	}
	if p.epochs == nil {
		p.epochs = make(map[uint64]pilotTimingEpoch)
	}
	p.epochs[id] = pilotTimingEpoch{sendOrigin: sendOrigin, receiveOrigin: receiveOrigin}
	p.highID = id
	return nil
}

// A newer PREPARE supersedes an unacknowledged prepared map. Ordered control
// frames ensure its earlier ACK cannot subsequently overtake that PREPARE.
func (p *pilotTimingState) abandon(id uint64) {
	p.mu.Lock()
	if id != p.active {
		delete(p.epochs, id)
	}
	p.mu.Unlock()
}

func (p *pilotTimingState) activate(id uint64) error {
	now := p.clockNow()
	p.mu.Lock()
	changed := p.observeLocked(now)
	changed = p.pruneLocked(now) || changed
	defer func() { p.mu.Unlock(); p.signal(changed) }()
	if _, ok := p.epochs[id]; !ok || id < p.active {
		return fmt.Errorf("%w: missing or retired epoch", errPilotTimingInvalid)
	}
	if id == p.active {
		return nil
	}
	for oldID, old := range p.epochs {
		if oldID < id && old.retireSendAt.IsZero() {
			old.retireSendAt = now.Add(pilotLifetime)
			p.epochs[oldID] = old
		}
	}
	p.active = id
	return nil
}

func (p *pilotTimingState) pruneLocked(now time.Time) bool {
	notify := false
	for id, epoch := range p.epochs {
		retired := !epoch.retireReceiveAt.IsZero() && pilotElapsedUpper(epoch.retireReceiveAt, now) >= 0
		sendRetired := !epoch.retireSendAt.IsZero() && pilotElapsedUpper(epoch.retireSendAt, now) >= 0
		if max(pilotElapsedUpper(epoch.sendOrigin, now), pilotElapsedUpper(epoch.receiveOrigin, now)) > pilotTimingMaxEpochAge || retired && sendRetired && id != p.active {
			delete(p.epochs, id)
			if p.active == id {
				p.active = 0
				notify = true
			}
		}
	}
	return notify
}

func (p *pilotTimingState) stamp(now time.Time) (uint64, time.Duration, time.Duration, bool) {
	p.mu.Lock()
	changed := p.observeLocked(now)
	changed = p.pruneLocked(now) || changed
	epoch, ok := p.epochs[p.active]
	id := p.active
	elapsed, _ := pilotElapsedBounds(epoch.sendOrigin, now)
	_, receiveElapsed := pilotElapsedBounds(epoch.receiveOrigin, now)
	ok = ok && id != 0 && elapsed >= 0 && elapsed <= pilotTimingMaxEpochAge &&
		receiveElapsed >= 0 && receiveElapsed <= pilotTimingMaxEpochAge
	p.mu.Unlock()
	p.signal(changed || !ok)
	if !ok {
		return 0, 0, 0, false
	}
	return id, elapsed, receiveElapsed, true
}

func (p *pilotTimingState) valid(id uint64, now time.Time) bool {
	p.mu.Lock()
	changed := p.observeLocked(now)
	changed = p.pruneLocked(now) || changed
	epoch, ok := p.epochs[id]
	ok = ok && id != 0 && now.Sub(epoch.sendOrigin) >= 0 && (epoch.retireSendAt.IsZero() || pilotElapsedUpper(epoch.retireSendAt, now) < 0)
	p.mu.Unlock()
	p.signal(changed)
	return ok
}

type pilotExpiryReason uint8

const (
	pilotPacketFresh pilotExpiryReason = iota
	pilotLocalQueueExpired
	pilotIncomingCertainlyStale
	pilotInvalidTiming
)

func (p *pilotTimingState) accept(id uint64, sentElapsed, receiveElapsed time.Duration, now time.Time) bool {
	_, reason := p.received(id, sentElapsed, receiveElapsed, now)
	return reason == pilotPacketFresh
}

// The reciprocal causal origins bound age from both directions. The receiver's
// send origin follows the peer's receive origin; its receive origin precedes
// the peer's send origin. No one-way delay or shared wall clock is assumed.
func pilotTimingAgeBounds(localLow, localHigh, peerLow, peerHigh time.Duration) (time.Duration, time.Duration, bool) {
	for _, elapsed := range []time.Duration{localLow, localHigh, peerLow, peerHigh} {
		if elapsed < 0 || elapsed > pilotTimingMaxEpochAge {
			return 0, 0, false
		}
	}
	lower := max(0, pilotTimingScaleDown(localLow)-pilotTimingScaleUp(peerHigh))
	upper := pilotTimingScaleUp(localHigh) - pilotTimingScaleDown(peerLow)
	return lower, upper, upper >= lower
}

// Rebase retained proxy data to its age lower bound, preserving only the
// remaining QoS budget. Admission is not a physical six-second age guarantee:
// an ambiguous packet may be older by the measured interval's width.
func (p *pilotTimingState) received(id uint64, sentElapsed, receiveElapsed time.Duration, now time.Time) (time.Time, pilotExpiryReason) {
	p.mu.Lock()
	changed := p.observeLocked(now)
	changed = p.pruneLocked(now) || changed
	epoch, ok := p.epochs[id]
	localLow, _ := pilotElapsedBounds(epoch.sendOrigin, now)
	_, localHigh := pilotElapsedBounds(epoch.receiveOrigin, now)
	lowerAge, _, valid := pilotTimingAgeBounds(localLow, localHigh, sentElapsed, receiveElapsed)
	if !ok || id == 0 || !valid || !epoch.retireReceiveAt.IsZero() && pilotElapsedUpper(epoch.retireReceiveAt, now) >= 0 {
		p.mu.Unlock()
		p.signal(changed)
		return time.Time{}, pilotInvalidTiming
	}
	if lowerAge <= pilotLifetime && id > p.peerHighID {
		p.peerHighID = id
		for oldID, old := range p.epochs {
			if oldID < id && old.retireReceiveAt.IsZero() {
				old.retireReceiveAt = now.Add(pilotTimingRetireAfter)
				p.epochs[oldID] = old
			}
		}
	}
	p.mu.Unlock()
	p.signal(changed)
	if lowerAge > pilotLifetime {
		return time.Time{}, pilotIncomingCertainlyStale
	}
	return now.Add(-lowerAge), pilotPacketFresh
}
