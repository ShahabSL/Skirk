package skirk

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"sort"
	"time"
)

// The store mutex owns this state. No method performs I/O or invokes mux code.
type fsHybridOutboundTraceFlags struct{ patchStart, patchDone, acked bool }
type fsHybridInboundTraceFlags struct{ metadataObserved, physicalObserved, processed bool }

type fsHybridOutbound struct {
	name       string
	digest     [32]byte
	sealed     []byte
	streams    []uint64
	record     fsHybridRecord // Sequence zero means immutable-only admission.
	info       ObjectInfo
	pubDone    chan struct{}
	pubClosed  bool
	published  bool
	processed  bool
	deadline   time.Time
	fbQueued   bool
	fbAttempts int
	fbDone     bool
	fbErr      error
	physicalID string
	trace      fsHybridOutboundTraceFlags
}

type fsHybridInbound struct {
	info        ObjectInfo
	digest      [32]byte
	sealed      []byte
	sequence    uint64
	byteEnd     uint64
	done        bool
	queued      bool
	physicalIDs map[string]struct{}
	trace       fsHybridInboundTraceFlags
}

type fsHybridState struct {
	outgoing        *fsHybridCodec
	incoming        *fsHybridCodec
	outNames        map[string]*fsHybridOutbound
	outCipher       map[[32]byte]*fsHybridOutbound
	outQueue        []*fsHybridOutbound
	outSeq          uint64
	outBytes        uint64
	offered         uint64
	acked           uint64
	outByStream     map[uint64]int
	outPerStreamMax int
	outRetained     int

	rxNames          map[string]*fsHybridInbound
	rxIDs            map[string]*fsHybridInbound
	rxSeq            map[uint64]*fsHybridInbound
	rxCipher         map[[32]byte]*fsHybridInbound
	rxPhysical       map[string]*fsHybridInbound
	rxHigh           uint64
	rxProcessed      uint64
	rxProcessedBytes uint64
	rxBytes          int
	peerRevision     uint64
	peerCreditSeq    uint64
	peerCreditBytes  uint64
	grantedSeq       uint64
	grantedBytes     uint64
	revision         uint64
}

func newFSHybridState(outgoing, incoming *fsHybridCodec) *fsHybridState {
	return &fsHybridState{outgoing: outgoing, incoming: incoming, outNames: make(map[string]*fsHybridOutbound), outCipher: make(map[[32]byte]*fsHybridOutbound), outByStream: make(map[uint64]int), outPerStreamMax: 4, rxNames: make(map[string]*fsHybridInbound), rxIDs: make(map[string]*fsHybridInbound), rxSeq: make(map[uint64]*fsHybridInbound), rxCipher: make(map[[32]byte]*fsHybridInbound), rxPhysical: make(map[string]*fsHybridInbound)}
}

func (s *fsHybridState) admission(name string, sealed []byte, deadline time.Time) (*fsHybridOutbound, bool, error) {
	digest := fsHybridIdentity(name, sealed)
	if old := s.outNames[name]; old != nil {
		if old.digest != digest {
			return nil, false, errFSHybridIntegrity
		}
		return old, false, nil
	}
	if len(s.outNames) >= fsHybridClaimsMax {
		return nil, false, errFSHybridClosed
	}
	if prior := s.outCipher[sha256.Sum256(sealed)]; prior != nil {
		return nil, false, errFSHybridIntegrity
	}
	meta, err := s.outgoing.validateMuxMax(name, sealed, fsHybridPhysicalMax)
	if err != nil {
		return nil, false, err
	}
	// Every retained fallback body also consumes this global bound. Claims are
	// never evicted; closing this finite session is the only cap transition.
	limit := fsHybridCacheMax
	if len(sealed) > fsHybridInlineMax {
		limit -= s.outgoing.wireLimit()
	}
	if s.outRetained+len(sealed) > limit {
		return nil, false, errFSHybridBusy
	}
	r := &fsHybridOutbound{name: name, digest: digest, sealed: bytes.Clone(sealed), streams: append([]uint64(nil), meta.StreamIDs...), pubDone: make(chan struct{}), deadline: deadline}
	s.outNames[name] = r
	s.outCipher[sha256.Sum256(sealed)] = r
	s.outRetained += len(r.sealed)
	return r, true, nil
}

func (s *fsHybridState) enqueue(r *fsHybridOutbound) bool {
	if r.record.Sequence != 0 {
		return true
	}
	if len(r.sealed) > fsHybridInlineMax {
		return false
	}
	if len(s.outQueue) >= fsHybridQueueMax || s.outSeq >= fsHybridClaimsMax || s.outBytes > math.MaxUint64-uint64(len(r.name)+len(r.sealed)+64) {
		return false
	}
	for _, stream := range r.streams {
		if s.outByStream[stream] >= s.outPerStreamMax {
			return false
		}
	}
	record := fsHybridRecord{Sequence: s.outSeq + 1, Name: r.name, Sealed: r.sealed}
	record.ByteEnd = s.outBytes + record.cost()
	if record.Sequence > s.peerCreditSeq || record.ByteEnd > s.peerCreditBytes {
		return false
	}
	queue := append(append([]*fsHybridOutbound(nil), s.outQueue...), r)
	records := make([]fsHybridRecord, 0, len(queue))
	for _, entry := range s.outQueue {
		records = append(records, entry.record)
	}
	records = append(records, record)
	// Check exact encoding size without generating a second ciphertext or
	// consuming a revision. Only prepare creates the cached retry identity.
	raw, err := json.Marshal(s.outgoing.snapshot(s.revision+1, s.rxProcessed, s.grantedSeq, s.grantedBytes, records))
	if err != nil || len(raw)+len(fsHybridMagic)+s.outgoing.aead.NonceSize()+s.outgoing.aead.Overhead() > s.outgoing.wireLimit() {
		return false
	}
	r.record = record
	s.outSeq, s.outBytes = record.Sequence, record.ByteEnd
	s.outQueue = queue
	for _, stream := range r.streams {
		s.outByStream[stream]++
	}
	return true
}

func (s *fsHybridState) credit() (uint64, uint64) {
	seq := min(uint64(fsHybridClaimsMax), s.rxProcessed+fsHybridQueueMax)
	// Reserve lifetime claims for every sequence already promised but unseen.
	outstanding := s.grantedSeq - s.rxHigh
	free := fsHybridClaimsMax - len(s.rxNames) - int(outstanding)
	if free < 0 {
		free = 0
	}
	if seq > s.grantedSeq {
		seq = s.grantedSeq + min(seq-s.grantedSeq, uint64(free))
	} else {
		seq = s.grantedSeq
	}
	// At most one cumulative document of unseen inline data is promised. The
	// physical fallback cache reserves that same space separately below.
	byteCredit := s.rxProcessedBytes + uint64(s.incoming.wireLimit())
	if byteCredit < s.grantedBytes {
		byteCredit = s.grantedBytes
	}
	return seq, byteCredit
}

func (s *fsHybridState) prepare() ([]byte, uint64, error) {
	if s.revision == math.MaxUint64 {
		return nil, 0, errFSHybridClosed
	}
	creditSeq, creditBytes := s.credit()
	records := make([]fsHybridRecord, 0, len(s.outQueue))
	for _, r := range s.outQueue {
		records = append(records, r.record)
	}
	wire, err := s.outgoing.seal(s.outgoing.snapshot(s.revision+1, s.rxProcessed, creditSeq, creditBytes, records))
	if err != nil {
		return nil, 0, err
	}
	// A prepared advertisement may become visible before its HTTP response;
	// reserve its credits and offered identities before making the request.
	s.grantedSeq, s.grantedBytes = creditSeq, creditBytes
	s.revision++
	high := s.acked
	if len(records) > 0 {
		high = records[len(records)-1].Sequence
	}
	if high > s.offered {
		s.offered = high
	}
	return wire, high, nil
}

func fsHybridMarkPublished(r *fsHybridOutbound, info ObjectInfo) {
	r.published = true
	if r.info.Name == "" {
		r.info = info
	}
	if !r.pubClosed {
		close(r.pubDone)
		r.pubClosed = true
	}
}

func (s *fsHybridState) published(high uint64) {
	for _, r := range s.outQueue {
		if r.record.Sequence > high {
			break
		}
		fsHybridMarkPublished(r, ObjectInfo{Name: r.name, ID: fsHybridID(r.name, r.sealed), Size: int64(len(r.sealed))})
	}
}

// receive preflights the entire authenticated snapshot before ACKs, credits,
// queues or cache entries change. Replays cannot regress a newer ACK.
func (s *fsHybridState) receive(wire []byte, now time.Time) ([]*fsHybridInbound, bool, error) {
	snapshot, err := s.incoming.open(wire)
	if err != nil {
		return nil, false, err
	}
	if snapshot.Revision <= s.peerRevision {
		return nil, false, errFSHybridReplay
	}
	if snapshot.Processed < s.acked || snapshot.Processed > s.offered || snapshot.CreditSeq < s.peerCreditSeq || snapshot.CreditBytes < s.peerCreditBytes {
		return nil, false, errFSHybridIntegrity
	}
	if len(snapshot.Records) == 0 && s.rxHigh > s.rxProcessed {
		return nil, false, errFSHybridIntegrity
	}
	if len(snapshot.Records) > 0 && (snapshot.Records[0].Sequence > s.rxProcessed+1 || snapshot.Records[len(snapshot.Records)-1].Sequence < s.rxHigh) {
		return nil, false, errFSHybridIntegrity
	}
	freshNames, extraBytes, high := 0, 0, s.rxHigh
	var tailBytes uint64
	if high > 0 {
		tailBytes = s.rxSeq[high].byteEnd
	}
	for _, r := range snapshot.Records {
		digest := fsHybridIdentity(r.Name, r.Sealed)
		entry := s.rxNames[r.Name]
		if prior := s.rxCipher[sha256.Sum256(r.Sealed)]; prior != nil && prior != entry {
			return nil, false, errFSHybridIntegrity
		}
		if entry != nil && (entry.digest != digest || entry.sequence != 0 && (entry.sequence != r.Sequence || entry.byteEnd != r.ByteEnd)) {
			return nil, false, errFSHybridIntegrity
		}
		if old := s.rxSeq[r.Sequence]; old != nil && old != entry {
			return nil, false, errFSHybridIntegrity
		}
		if r.Sequence <= s.rxHigh {
			if s.rxSeq[r.Sequence] != entry || entry == nil {
				return nil, false, errFSHybridIntegrity
			}
			continue
		}
		if r.Sequence != high+1 || r.Sequence > s.grantedSeq || r.ByteEnd > s.grantedBytes || tailBytes > math.MaxUint64-r.cost() || r.ByteEnd != tailBytes+r.cost() {
			return nil, false, errFSHybridIntegrity
		}
		high = r.Sequence
		tailBytes = r.ByteEnd
		if entry == nil {
			freshNames++
			extraBytes += len(r.Sealed)
		}
	}
	if len(s.rxNames)+freshNames > fsHybridClaimsMax {
		return nil, false, errFSHybridClosed
	}
	if s.rxBytes+extraBytes > fsHybridCacheMax {
		return nil, false, errFSHybridBusy
	}
	// No mutation above this point.
	s.peerRevision = snapshot.Revision
	s.peerCreditSeq, s.peerCreditBytes = snapshot.CreditSeq, snapshot.CreditBytes
	s.acked = snapshot.Processed
	for len(s.outQueue) > 0 && s.outQueue[0].record.Sequence <= s.acked {
		r := s.outQueue[0]
		r.processed = true
		fsHybridMarkPublished(r, ObjectInfo{Name: r.name, ID: fsHybridID(r.name, r.sealed), Size: int64(len(r.sealed))})
		for _, stream := range r.streams {
			s.outByStream[stream]--
		}
		if len(r.sealed) > 0 {
			s.outRetained -= len(r.sealed)
			r.sealed = nil
			r.record.Sealed = nil
		}
		s.outQueue = s.outQueue[1:]
	}
	var ready []*fsHybridInbound
	for _, r := range snapshot.Records {
		entry := s.rxNames[r.Name]
		if entry == nil {
			entry = &fsHybridInbound{info: ObjectInfo{Name: r.Name, ID: fsHybridID(r.Name, r.Sealed), Size: int64(len(r.Sealed)), Updated: now.UTC().Format(time.RFC3339Nano)}, digest: fsHybridIdentity(r.Name, r.Sealed), sealed: bytes.Clone(r.Sealed), physicalIDs: make(map[string]struct{})}
			s.rxNames[r.Name], s.rxIDs[entry.info.ID] = entry, entry
			s.rxCipher[sha256.Sum256(r.Sealed)] = entry
			s.rxBytes += len(entry.sealed)
		}
		if entry.sequence == 0 {
			entry.sequence, entry.byteEnd = r.Sequence, r.ByteEnd
			s.rxSeq[r.Sequence] = entry
		}
		if !entry.done {
			ready = append(ready, entry)
		}
	}
	s.rxHigh = high
	advanced := s.advanceProcessed()
	return ready, advanced, nil
}

func (s *fsHybridState) advanceProcessed() bool {
	old := s.rxProcessed
	for s.rxProcessed < s.rxHigh {
		entry := s.rxSeq[s.rxProcessed+1]
		if entry == nil || !entry.done {
			break
		}
		s.rxProcessed++
		s.rxProcessedBytes = entry.byteEnd
	}
	return old != s.rxProcessed
}

// normalizePhysical gives the mux the SAME cache ID regardless of which
// carrier won. A late Drive copy is authenticated before exact-ID deletion.
func (s *fsHybridState) normalizePhysical(info ObjectInfo, sealed []byte, now time.Time) (*fsHybridInbound, error) {
	if info.ID == "" || len(info.ID) > 128 || !isSafeObjectSegment(info.ID) {
		return nil, errFSHybridIntegrity
	}
	if _, err := s.incoming.validateMuxMax(info.Name, sealed, fsHybridPhysicalMax); err != nil {
		return nil, err
	}
	digest := fsHybridIdentity(info.Name, sealed)
	entry := s.rxNames[info.Name]
	if prior := s.rxPhysical[info.ID]; prior != nil && prior != entry {
		return nil, errFSHybridIntegrity
	}
	if prior := s.rxCipher[sha256.Sum256(sealed)]; prior != nil && prior != entry {
		return nil, errFSHybridIntegrity
	}
	if entry != nil {
		if entry.digest != digest {
			return nil, errFSHybridIntegrity
		}
		if len(entry.physicalIDs) >= 4 {
			if _, known := entry.physicalIDs[info.ID]; !known {
				return nil, errFSHybridBounds
			}
		}
		entry.physicalIDs[info.ID] = struct{}{}
		s.rxPhysical[info.ID] = entry
		return entry, nil
	}
	// Keep both memory and lifetime claim headroom for already granted but
	// unseen Firestore records. Physical-first copies initially have no seq.
	if len(s.rxNames)+int(s.grantedSeq-s.rxHigh) >= fsHybridClaimsMax {
		return nil, errFSHybridClosed
	}
	if s.rxBytes+len(sealed) > fsHybridCacheMax-s.incoming.wireLimit() {
		return nil, errFSHybridBusy
	}
	entry = &fsHybridInbound{info: ObjectInfo{Name: info.Name, ID: fsHybridID(info.Name, sealed), Size: int64(len(sealed)), Updated: now.UTC().Format(time.RFC3339Nano)}, digest: digest, sealed: bytes.Clone(sealed), physicalIDs: map[string]struct{}{info.ID: {}}}
	s.rxNames[info.Name], s.rxIDs[entry.info.ID] = entry, entry
	s.rxPhysical[info.ID] = entry
	s.rxCipher[sha256.Sum256(sealed)] = entry
	s.rxBytes += len(entry.sealed)
	return entry, nil
}

func (s *fsHybridState) markProcessed(id string) (*fsHybridInbound, []string, bool, error) {
	entry := s.rxIDs[id]
	if entry == nil {
		return nil, nil, false, errFSHybridIntegrity
	}
	if !entry.done {
		entry.done = true
		s.rxBytes -= len(entry.sealed)
		entry.sealed = nil
	}
	ids := make([]string, 0, len(entry.physicalIDs))
	for id := range entry.physicalIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return entry, ids, s.advanceProcessed(), nil
}
