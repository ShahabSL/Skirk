package skirk

import "time"

// Full binary batches may use saved publication credits between ordinary
// ticks. The two RPC slots and adaptive upload gates remain independent.
type pilotPublishMode uint8

const (
	pilotPublishFixed pilotPublishMode = iota
	pilotPublishFullBinary
)

type pilotPublishCredits struct {
	available time.Duration
	updated   time.Time
}

func newPilotPublishCredits(now time.Time) pilotPublishCredits {
	return pilotPublishCredits{available: 2 * pilotPublishTick, updated: now}
}

func (c *pilotPublishCredits) update(now time.Time) {
	if now.After(c.updated) {
		c.available = min(2*pilotPublishTick, c.available+min(2*pilotPublishTick, now.Sub(c.updated)))
		c.updated = now
	}
}

func (c *pilotPublishCredits) delay(now time.Time) time.Duration {
	c.update(now)
	return max(0, pilotPublishTick-c.available)
}

func (c *pilotPublishCredits) take(now time.Time) {
	c.update(now)
	c.available -= pilotPublishTick
}

func (p *pilotMailbox) signalPublish() {
	if p.publishMode == pilotPublishFixed {
		return
	}
	select {
	case p.publishWake <- struct{}{}:
	default:
	}
}

// A byte-full batch has less than one maximum-sized packet record free. The
// other full condition is the negotiated packet cap. Neither uses app type.
func pilotBatchFull(packets, size, limit int) bool {
	return packets >= pilotMediaPacketLimit(limit) || size+pilotFairQuantum > limit
}

func (q *pilotFairQueue) canCoalesce(limit int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.packets > 1 && !pilotBatchFull(q.packets, len(pilotDatagramMagic)+q.bytes, limit)
}

func (p *pilotMailbox) collectFullFairBatch(limit int) ([]pilotPacket, int) {
	return p.collectQueuedBinary(limit, true)
}

// Select against a copy of the bounded fair queue. A failed trial must leave
// packet order, expiry accounting and association credits unchanged. Packet
// bodies are immutable and remain shared with the original queue.
func (p *pilotMailbox) collectQueuedBinary(limit int, requireFull bool) ([]pilotPacket, int) {
	q := p.fairQueue
	if q == nil || limit <= p.batchBytes {
		return nil, 0
	}
	now := time.Now()
	if p.timing != nil {
		now = p.timing.nowTime()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	queuedBytes := q.bytes + len(pilotDatagramMagic)
	if queuedBytes <= p.batchBytes {
		return nil, 0
	}
	if requireFull {
		if !pilotBatchFull(q.packets, queuedBytes, limit) {
			return nil, 0
		}
	} else if q.packets <= pilotMaxBatch {
		return nil, 0
	}
	trial := newPilotFairQueue(q.maxPackets, q.maxBytes)
	trial.packets, trial.bytes = q.packets, q.bytes
	for _, group := range q.ready {
		copy := &pilotFairGroup{id: group.id, bytes: group.bytes, credit: group.credit,
			packets: append([]pilotPacket(nil), group.packets...)}
		trial.groups[copy.id] = copy
		trial.ready = append(trial.ready, copy)
	}
	var expired [4]int
	packets, size := p.collectFairBatchUsing(trial.take, limit, now, func(reason pilotExpiryReason, count int) {
		expired[reason] += count
	}, true)
	if size <= p.batchBytes || (requireFull && !pilotBatchFull(len(packets), size, limit)) {
		return nil, 0
	}
	q.groups, q.ready, q.packets, q.bytes = trial.groups, trial.ready, trial.packets, trial.bytes
	for reason, count := range expired {
		p.recordExpiry(pilotExpiryReason(reason), count)
	}
	return packets, size
}
