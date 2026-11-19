package skirk

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// A finite, test-only trace shared by both peers in the same local process.
// Its per-run HMAC keys correlate phases without exposing Drive names or data.
type fsHybridPhaseTrace struct {
	mu      sync.Mutex
	start   time.Time
	key     [32]byte
	limit   int
	events  []fsHybridPhaseEvent
	dropped int
}

type fsHybridPhaseEvent struct {
	Direction   byte     `json:"direction"`
	Claim       string   `json:"claim"`
	Sequence    uint64   `json:"sequence"`
	Phase       string   `json:"phase"`
	ElapsedMS   float64  `json:"elapsed_ms"`
	StreamCount int      `json:"stream_count,omitempty"`
	StreamIDs   []uint64 `json:"stream_ids,omitempty"`
}

type fsHybridPhaseSnapshot struct {
	Limit   int                  `json:"limit"`
	Dropped int                  `json:"dropped"`
	Events  []fsHybridPhaseEvent `json:"events"`
}

func newFSHybridPhaseTrace(limit int) (*fsHybridPhaseTrace, error) {
	if limit < 1 || limit > 1024 {
		return nil, errFSHybridBounds
	}
	t := &fsHybridPhaseTrace{start: time.Now(), limit: limit, events: make([]fsHybridPhaseEvent, 0, limit)}
	if _, err := rand.Read(t.key[:]); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *fsHybridPhaseTrace) record(direction byte, name string, sequence uint64, phase string) {
	if t != nil {
		t.recordAt(time.Now(), direction, name, sequence, phase)
	}
}

func (t *fsHybridPhaseTrace) recordClaim(direction byte, name string, sequence uint64, phase string, streams []uint64) {
	if t != nil {
		t.recordAt(time.Now(), direction, name, sequence, phase, streams...)
	}
}

func (t *fsHybridPhaseTrace) recordAt(now time.Time, direction byte, name string, sequence uint64, phase string, streams ...uint64) {
	if t == nil {
		return
	}
	mac := hmac.New(sha256.New, t.key[:])
	_, _ = mac.Write([]byte(name))
	var identity [8]byte
	copy(identity[:], mac.Sum(nil))
	ms := now.Sub(t.start).Seconds() * 1000
	if ms < 0 {
		ms = 0
	}
	event := fsHybridPhaseEvent{Direction: direction, Claim: hex.EncodeToString(identity[:]), Sequence: sequence, Phase: phase, ElapsedMS: ms}
	if len(streams) > 0 {
		event.StreamCount = len(streams)
		event.StreamIDs = append([]uint64(nil), streams[:min(len(streams), 4)]...)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) >= t.limit {
		t.dropped++
		return
	}
	t.events = append(t.events, event)
}

func (t *fsHybridPhaseTrace) snapshot() fsHybridPhaseSnapshot {
	if t == nil {
		return fsHybridPhaseSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	events := append([]fsHybridPhaseEvent(nil), t.events...)
	for i := range events {
		events[i].StreamIDs = append([]uint64(nil), events[i].StreamIDs...)
	}
	return fsHybridPhaseSnapshot{Limit: t.limit, Dropped: t.dropped, Events: events}
}
