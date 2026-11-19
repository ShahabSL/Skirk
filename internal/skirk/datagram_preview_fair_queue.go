package skirk

import (
	"sync"
	"time"
)

const (
	pilotQueuePackets   = 128
	pilotFairQuantum    = pilotPacketHeader + pilotMaxPayload
	pilotFairQueueBytes = pilotQueuePackets * pilotFairQuantum
)

// A single association can use the whole queue. When several are active, byte
// rounds share publication capacity and pressure evicts the largest backlog.
type pilotFairQueue struct {
	mu         sync.Mutex
	groups     map[uint32]*pilotFairGroup
	ready      []*pilotFairGroup
	packets    int
	bytes      int
	maxPackets int
	maxBytes   int
}

type pilotFairGroup struct {
	id      uint32
	packets []pilotPacket
	bytes   int
	credit  int
}

func newPilotFairQueue(maxPackets, maxBytes int) *pilotFairQueue {
	return &pilotFairQueue{groups: make(map[uint32]*pilotFairGroup), maxPackets: maxPackets, maxBytes: maxBytes}
}

func newPilotMediaFairQueue(mediaPlain int) *pilotFairQueue {
	// Budget two maximum media objects, charging packet wire bytes without
	// batch markers. Completion-driven reads can produce larger catch-up bursts;
	// publisher scheduling and best-effort drops handle that bounded overflow.
	packets := max(pilotQueuePackets, pilotMediaReadSlots*pilotMediaPacketLimit(mediaPlain))
	bytes := max(pilotFairQueueBytes, pilotMediaReadSlots*(mediaPlain-len(pilotDatagramMagic)))
	return newPilotFairQueue(packets, bytes)
}

func (q *pilotFairQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.packets
}

func (q *pilotFairQueue) offer(packet pilotPacket) (dropped, stale int) {
	id := uint32(packet.flow >> 32)
	cost := pilotPacketHeader + len(packet.body)
	if id == 0 || uint32(packet.flow) == 0 || cost > pilotFairQuantum || cost > q.maxBytes || q.maxPackets <= 0 {
		return 1, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.packets >= q.maxPackets || q.bytes+cost > q.maxBytes {
		now := time.Now()
		for i := 0; i < len(q.ready); {
			group := q.ready[i]
			stale += q.expireHead(group, now)
			if len(group.packets) == 0 {
				q.remove(group)
				continue
			}
			i++
		}
	}
	for q.packets >= q.maxPackets || q.bytes+cost > q.maxBytes {
		byBytes := q.bytes+cost > q.maxBytes
		victim := q.groups[id]
		if victim == nil {
			victim = q.ready[0]
		}
		for _, group := range q.ready {
			if byBytes && group.bytes > victim.bytes || !byBytes && len(group.packets) > len(victim.packets) {
				victim = group
			}
		}
		q.pop(victim)
		if len(victim.packets) == 0 {
			q.remove(victim)
		}
		dropped++
	}
	group := q.groups[id]
	if group == nil {
		group = &pilotFairGroup{id: id}
		q.groups[id] = group
		q.ready = append(q.ready, group)
	}
	group.packets = append(group.packets, packet)
	group.bytes += cost
	q.bytes += cost
	q.packets++
	return dropped, stale
}

func (q *pilotFairQueue) take(now time.Time, fits func(pilotPacket) bool) (pilotPacket, bool, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	stale := 0
	for attempts := len(q.ready); attempts > 0; attempts-- {
		group := q.ready[0]
		stale += q.expireHead(group, now)
		if len(group.packets) == 0 {
			q.remove(group)
			continue
		}
		packet := group.packets[0]
		if !fits(packet) {
			q.rotate()
			continue
		}
		cost := pilotPacketHeader + len(packet.body)
		if group.credit < cost {
			group.credit += pilotFairQuantum
		}
		group.credit -= cost
		q.pop(group)
		if len(group.packets) == 0 {
			q.remove(group)
		} else if group.credit < pilotPacketHeader+len(group.packets[0].body) {
			q.rotate()
		}
		return packet, true, stale
	}
	return pilotPacket{}, false, stale
}

func (q *pilotFairQueue) expireHead(group *pilotFairGroup, now time.Time) (stale int) {
	for len(group.packets) != 0 && pilotElapsedUpper(group.packets[0].sent, now) >= pilotLifetime {
		q.pop(group)
		stale++
	}
	return stale
}

func (q *pilotFairQueue) pop(group *pilotFairGroup) {
	cost := pilotPacketHeader + len(group.packets[0].body)
	group.packets[0] = pilotPacket{}
	group.packets = group.packets[1:]
	group.bytes -= cost
	q.bytes -= cost
	q.packets--
}

func (q *pilotFairQueue) rotate() {
	first := q.ready[0]
	copy(q.ready, q.ready[1:])
	q.ready[len(q.ready)-1] = first
}

func (q *pilotFairQueue) remove(group *pilotFairGroup) {
	delete(q.groups, group.id)
	for i, candidate := range q.ready {
		if candidate == group {
			copy(q.ready[i:], q.ready[i+1:])
			q.ready[len(q.ready)-1] = nil
			q.ready = q.ready[:len(q.ready)-1]
			return
		}
	}
}

func (q *pilotFairQueue) closeAssociation(id uint32) (removed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if group := q.groups[id]; group != nil {
		removed = len(group.packets)
		q.packets -= removed
		q.bytes -= group.bytes
		q.remove(group)
	}
	return removed
}
