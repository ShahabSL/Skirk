package skirk

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type pilotMediaKey struct {
	seq uint64
	id  string
}

type pilotMediaReadRecovery uint8

const (
	pilotMediaRecoveryUnused pilotMediaReadRecovery = iota
	pilotMediaRecoveryPending
	pilotMediaRecoveryRunning
	pilotMediaRecoverySpent
)

type pilotMediaRead struct {
	key      pilotMediaKey
	size     int
	first    time.Time
	readyAt  time.Time
	attempts int
	active   bool
	recovery pilotMediaReadRecovery
}

// All state is owned under recvMu. The catalogue includes active reads.
// Retire failed IDs separately so pressure on pending work doesn't immediately
// reset retries. Both histories are limited to one page of candidate IDs.
type pilotMediaReader struct {
	reads       map[pilotMediaKey]*pilotMediaRead
	slots       [pilotMediaReadSlots]*pilotMediaRead
	retired     [pilotMaxListFiles]pilotMediaKey
	retiredNext int
	wake        chan struct{}
	stats       pilotMediaReadStats
}

type pilotMediaReadStats struct {
	dispatched, retries, quotaSkipped, fetchTimeouts, retentionTimeouts uint64
	exhausted, evicted, capacitySkipped, retentionExpired               uint64
	firstWaitUS, maxFirstWaitUS, fetchUS, maxFetchUS                    uint64
}

func (p *pilotMailbox) admitMediaReads(ctx context.Context, candidates []pilotMediaRead) {
	p.recvMu.Lock()
	defer p.recvMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	r := &p.mediaReader
	if r.reads == nil {
		r.reads = make(map[pilotMediaKey]*pilotMediaRead)
	}
	now := time.Now()
	p.pruneMediaReadsLocked(now)
	for _, candidate := range candidates {
		if _, known := r.reads[candidate.key]; known || p.replay.contains(candidate.key.seq) || r.isRetired(candidate.key) {
			continue
		}
		if len(r.reads) == pilotMaxListFiles && !r.makeRoom(candidate.key) {
			r.stats.capacitySkipped++
			r.retire(candidate.key)
			continue
		}
		candidate.first = now
		r.reads[candidate.key] = &candidate
	}
	p.fillMediaReadSlotsLocked(ctx, now)
	r.signal()
}

func (r *pilotMediaReader) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Retry eligibility is observed independently of the provider's next list.
// One timer per lease handles both delayed reads and catalogue expiration.
func (p *pilotMailbox) mediaReadWakeLoop(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		p.recvMu.Lock()
		now := time.Now()
		p.pruneMediaReadsLocked(now)
		p.fillMediaReadSlotsLocked(ctx, now)
		next := p.nextMediaReadWakeLocked(now)
		p.recvMu.Unlock()

		var alarm <-chan time.Time
		if !next.IsZero() {
			timer.Reset(time.Until(next))
			alarm = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-p.mediaReader.wake:
		case <-alarm:
		}
		timer.Stop()
	}
}

func (p *pilotMailbox) nextMediaReadWakeLocked(now time.Time) time.Time {
	r := &p.mediaReader
	free := false
	recovering := r.hasRecoveryRead()
	for _, read := range r.slots {
		if read == nil {
			free = true
			break
		}
	}
	backoff := p.drive.QuotaBackoffSnapshot()
	gate := now
	if backoff.Active {
		gate = time.Now().Add(max(time.Nanosecond, time.Duration(backoff.WaitSeconds*float64(time.Second))))
	}
	var next time.Time
	for _, read := range r.reads {
		if read.active {
			continue
		}
		at := now.Add(pilotLifetime - pilotElapsedUpper(read.first, now))
		if free && !r.sequenceInFlight(read.key.seq) && !(recovering && read.recovery == pilotMediaRecoveryPending) {
			eligible := gate
			if read.readyAt.After(eligible) {
				eligible = read.readyAt
			}
			if eligible.Before(at) {
				at = eligible
			}
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

func (p *pilotMailbox) pruneMediaReadsLocked(now time.Time) {
	r := &p.mediaReader
	for key, read := range r.reads {
		if read.active {
			continue
		}
		if p.replay.contains(key.seq) {
			delete(r.reads, key)
		} else if pilotElapsedUpper(read.first, now) >= pilotLifetime {
			r.stats.retentionExpired++
			r.retire(key)
		}
	}
}

func (r *pilotMediaReader) isRetired(key pilotMediaKey) bool {
	for _, retired := range r.retired {
		if retired == key {
			return true
		}
	}
	return false
}

func (r *pilotMediaReader) retire(key pilotMediaKey) {
	r.retired[r.retiredNext] = key
	r.retiredNext = (r.retiredNext + 1) % len(r.retired)
	delete(r.reads, key)
}

// Drop an inactive retry first. Only drop an unattempted object for a newer
// one if the remaining entries are all first attempts.
func (r *pilotMediaReader) makeRoom(key pilotMediaKey) bool {
	var oldest *pilotMediaRead
	for _, read := range r.reads {
		if read.active {
			continue
		}
		if oldest == nil || read.attempts > oldest.attempts ||
			read.attempts == oldest.attempts && read.key.before(oldest.key) {
			oldest = read
		}
	}
	if oldest == nil || oldest.attempts == 0 && !oldest.key.before(key) {
		return false
	}
	r.stats.evicted++
	r.retire(oldest.key)
	return true
}

func (key pilotMediaKey) before(other pilotMediaKey) bool {
	return key.seq < other.seq || key.seq == other.seq && key.id < other.id
}

func (r *pilotMediaReader) sequenceInFlight(seq uint64) bool {
	for _, read := range r.slots {
		if read != nil && read.key.seq == seq {
			return true
		}
	}
	return false
}

func (r *pilotMediaReader) hasRecoveryRead() bool {
	for _, read := range r.slots {
		if read != nil && read.recovery == pilotMediaRecoveryRunning {
			return true
		}
	}
	return false
}

func (p *pilotMailbox) fillMediaReadSlotsLocked(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || p.drive.QuotaBackoffSnapshot().Active {
		return
	}
	r := &p.mediaReader
	recovering := r.hasRecoveryRead()
	for slot, busy := range r.slots {
		if ctx.Err() != nil {
			return
		}
		if busy != nil {
			continue
		}
		var next *pilotMediaRead
		for _, read := range r.reads {
			if read.active || now.Before(read.readyAt) || p.replay.contains(read.key.seq) || r.sequenceInFlight(read.key.seq) {
				continue
			}
			if recovering && read.recovery == pilotMediaRecoveryPending {
				continue
			}
			// All reads have the same retention budget. The earliest first
			// observation has the earliest deadline among eligible reads.
			if next == nil || read.first.Before(next.first) ||
				read.first.Equal(next.first) && read.key.before(next.key) {
				next = read
			}
		}
		if next == nil {
			return
		}
		next.active = true
		next.attempts++
		r.slots[slot] = next
		timeout := pilotMediaReadTimeout
		if next.recovery == pilotMediaRecoveryPending {
			next.recovery = pilotMediaRecoveryRunning
			recovering = true
			timeout = pilotMediaRecoveryTimeout
		}
		r.stats.dispatched++
		if next.attempts == 1 {
			wait := uint64(now.Sub(next.first).Microseconds())
			r.stats.firstWaitUS += wait
			r.stats.maxFirstWaitUS = max(r.stats.maxFirstWaitUS, wait)
		} else {
			r.stats.retries++
		}
		p.liveCalls.Go(func() { p.runMediaRead(ctx, slot, next, timeout) })
	}
}

func (p *pilotMailbox) runMediaRead(ctx context.Context, slot int, read *pilotMediaRead, timeout time.Duration) {
	start := time.Now()
	op, cancel := context.WithTimeout(ctx, pilotLifetime-pilotElapsedUpper(read.first, start))
	attempt, stop := context.WithTimeout(op, timeout)
	wire, err := p.fetchMediaBatch(attempt, read.key.id, read.size)
	retentionTimeout := errors.Is(err, context.DeadlineExceeded) && errors.Is(op.Err(), context.DeadlineExceeded)
	var bodyErr *googleResponseBodyError
	progressTimeout := !retentionTimeout && errors.Is(attempt.Err(), context.DeadlineExceeded) &&
		errors.As(err, &bodyErr) && bodyErr.status == http.StatusOK && bodyErr.bytesRead > 0 &&
		errors.Is(err, context.DeadlineExceeded)
	stop()
	cancel()
	fetchUS := uint64(time.Since(start).Microseconds())
	if err == nil && ctx.Err() == nil {
		// The discovery deadline bounds read work. The authenticated packet
		// age continues to govern delivery of the fetched result.
		p.mediaFetched.Add(1)
		batchBytes := p.mediaPlainLimit()
		p.acceptBatch(ctx, read.key.id, read.key.seq, wire, batchBytes, pilotMediaPacketLimit(batchBytes))
	} else if ctx.Err() == nil && !retentionTimeout && !errors.Is(err, errPilotMediaBackoff) {
		p.errors.Add(1)
	}

	p.recvMu.Lock()
	defer p.recvMu.Unlock()
	now := time.Now()
	r := &p.mediaReader
	r.slots[slot] = nil
	read.active = false
	if read.recovery == pilotMediaRecoveryRunning {
		if errors.Is(err, errPilotMediaBackoff) {
			read.recovery = pilotMediaRecoveryPending
		} else {
			read.recovery = pilotMediaRecoverySpent
		}
	}
	r.stats.fetchUS += fetchUS
	r.stats.maxFetchUS = max(r.stats.maxFetchUS, fetchUS)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		if retentionTimeout {
			r.stats.retentionTimeouts++
		} else {
			r.stats.fetchTimeouts++
		}
	}
	switch {
	case ctx.Err() != nil || p.replay.contains(read.key.seq):
		delete(r.reads, read.key)
	case errors.Is(err, errPilotMediaBackoff):
		// No GET happened. Retain the attempt budget and defer eligibility.
		r.stats.quotaSkipped++
		read.attempts--
		read.readyAt = now.Add(pilotPollTick)
	case err == nil:
		// Complete bytes that failed authentication or parsing stay in
		// Drive for scoped expiry. Only acceptBatch may claim cleanup.
		r.stats.exhausted++
		r.retire(read.key)
	default:
		// The first-discovery deadline bounds both queued work and the
		// exponential eligibility for retries. Relisting never resets it.
		read.readyAt = now.Add(pilotPollTick << (read.attempts - 1))
		if read.recovery == pilotMediaRecoveryUnused && progressTimeout &&
			pilotLifetime-pilotElapsedUpper(read.first, read.readyAt) > pilotMediaReadTimeout {
			// Reading the same partial body under the same timeout would
			// waste the retry. Grant one longer attempt while leaving the
			// other download slot available for ordinary reads.
			read.recovery = pilotMediaRecoveryPending
		}
	}
	// Refill while this worker is still counted by liveCalls. Lease stop
	// cancels the context before joining the poll loop and the workers.
	p.pruneMediaReadsLocked(now)
	p.fillMediaReadSlotsLocked(ctx, now)
	r.signal()
}
