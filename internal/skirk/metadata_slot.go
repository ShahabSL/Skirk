package skirk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

// The opt-in runtime shadow probe uses this codec to validate candidate slot
// traffic. It does not deliver application bytes through the slot. A slot
// record is not a Drive object and must never enter object deletion queues.
const (
	metadataSlotMagic          = "SKS1"
	metadataSlotVersion        = byte(1)
	metadataSlotMaxDescription = 8 * 1024
	metadataSlotMaxRecords     = 64
	metadataSlotHeaderBytes    = 4 + 1 + 12
	metadataSlotPlainBytes     = 16 + 1 + 8 + 8 + 2
)

var (
	errMetadataSlotOverflow = errors.New("metadata slot queue or description is full")
	errMetadataSlotBusy     = errors.New("metadata slot PATCH is unresolved")
	errMetadataSlotReplay   = errors.New("metadata slot snapshot is stale or replayed")
	errMetadataSlotClosed   = errors.New("metadata slot generation is closed")
)

type metadataSlotLimits struct {
	DescriptionBytes int
	Records          int
}

func (l metadataSlotLimits) normalized() (metadataSlotLimits, error) {
	if l.DescriptionBytes == 0 {
		l.DescriptionBytes = metadataSlotMaxDescription
	}
	if l.Records == 0 {
		l.Records = metadataSlotMaxRecords
	}
	if l.DescriptionBytes < 128 || l.DescriptionBytes > metadataSlotMaxDescription || l.Records < 1 || l.Records > metadataSlotMaxRecords {
		return metadataSlotLimits{}, errors.New("metadata slot limits out of range")
	}
	return l, nil
}

type metadataSlotRecord struct {
	SlotSeq   uint64
	MuxName   string
	MuxSealed []byte
}

func (r metadataSlotRecord) clone() metadataSlotRecord {
	r.MuxSealed = bytes.Clone(r.MuxSealed)
	return r
}

type metadataSlotSnapshot struct {
	generation [16]byte
	direction  byte
	revision   uint64
	ack        uint64
	records    []metadataSlotRecord
}

type metadataSlotCodec struct {
	key        [keyLen]byte
	generation [16]byte
	direction  byte
	prefix     string
	clientID   string
	runID      string
	limits     metadataSlotLimits
}

func newMetadataSlotGeneration() ([16]byte, error) {
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return generation, err
	}
	return generation, nil
}

func newMetadataSlotCodec(secret string, sid [16]byte, direction byte, clientID, runID string, generation [16]byte, limits metadataSlotLimits) (*metadataSlotCodec, error) {
	var zero [16]byte
	if generation == zero || (direction != DirectionUp && direction != DirectionDown) {
		return nil, errors.New("metadata slot generation or direction invalid")
	}
	if !metadataSlotLabelValid(clientID) || !metadataSlotLabelValid(runID) {
		return nil, errors.New("metadata slot client or run label invalid")
	}
	limits, err := limits.normalized()
	if err != nil {
		return nil, err
	}
	base, err := DeriveKey(secret)
	if err != nil {
		return nil, err
	}
	info := make([]byte, 0, 96+len(clientID)+len(runID))
	info = append(info, "skirk-fixed-metadata-slot-aead-v1"...)
	info = append(info, sid[:]...)
	info = append(info, direction)
	info = append(info, byte(len(clientID)))
	info = append(info, clientID...)
	info = append(info, byte(len(runID)))
	info = append(info, runID...)
	info = append(info, generation[:]...)
	derived := hkdfSHA256(base, []byte("skirk-fixed-metadata-slot-salt-v1"), info, keyLen)
	codec := &metadataSlotCodec{
		generation: generation,
		direction:  direction,
		prefix:     muxDirPrefix(sid, direction, clientID, runID),
		clientID:   clientID,
		runID:      runID,
		limits:     limits,
	}
	copy(codec.key[:], derived)
	return codec, nil
}

func metadataSlotLabelValid(s string) bool {
	return s == strings.TrimSpace(s) && isSafeObjectSegment(s)
}

func (c *metadataSlotCodec) validateRecord(r metadataSlotRecord) error {
	if r.SlotSeq == 0 || len(r.MuxName) == 0 || len(r.MuxName) > 1024 || len(r.MuxSealed) == 0 || len(r.MuxSealed) > metadataSlotMaxDescription {
		return errors.New("metadata slot record size or sequence invalid")
	}
	if !strings.HasPrefix(r.MuxName, c.prefix) {
		return errors.New("metadata slot record outside client run namespace")
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: r.MuxName})
	if !ok || !meta.Priority || meta.ClientID != c.clientID || meta.RunID != c.runID || meta.Lane < 0 || meta.Lane >= muxLaneCount || meta.Seq == 0 {
		return errors.New("metadata slot record is not a priority mux object")
	}
	return nil
}

func (c *metadataSlotCodec) validateSnapshot(s metadataSlotSnapshot) error {
	if s.generation != c.generation || s.direction != c.direction || s.revision == 0 || len(s.records) > c.limits.Records {
		return errors.New("metadata slot snapshot header invalid")
	}
	seenNames := make(map[string]struct{}, len(s.records))
	var previous uint64
	for i, r := range s.records {
		if err := c.validateRecord(r); err != nil {
			return err
		}
		if i > 0 && (previous == math.MaxUint64 || r.SlotSeq != previous+1) {
			return errors.New("metadata slot records not contiguous")
		}
		if _, exists := seenNames[r.MuxName]; exists {
			return errors.New("metadata slot duplicate mux name")
		}
		seenNames[r.MuxName] = struct{}{}
		previous = r.SlotSeq
	}
	return nil
}

func (c *metadataSlotCodec) descriptionLength(records []metadataSlotRecord) int {
	plain := metadataSlotPlainBytes
	for _, r := range records {
		plain += 8 + 2 + 2 + len(r.MuxName) + len(r.MuxSealed)
	}
	return base64.RawURLEncoding.EncodedLen(metadataSlotHeaderBytes + plain + 16)
}

func (c *metadataSlotCodec) seal(s metadataSlotSnapshot) (string, error) {
	if err := c.validateSnapshot(s); err != nil {
		return "", err
	}
	if c.descriptionLength(s.records) > c.limits.DescriptionBytes {
		return "", errMetadataSlotOverflow
	}
	plain := make([]byte, 0, metadataSlotPlainBytes+c.descriptionLength(s.records))
	plain = append(plain, s.generation[:]...)
	plain = append(plain, s.direction)
	plain = binary.BigEndian.AppendUint64(plain, s.revision)
	plain = binary.BigEndian.AppendUint64(plain, s.ack)
	plain = binary.BigEndian.AppendUint16(plain, uint16(len(s.records)))
	for _, r := range s.records {
		plain = binary.BigEndian.AppendUint64(plain, r.SlotSeq)
		plain = binary.BigEndian.AppendUint16(plain, uint16(len(r.MuxName)))
		plain = binary.BigEndian.AppendUint16(plain, uint16(len(r.MuxSealed)))
		plain = append(plain, r.MuxName...)
		plain = append(plain, r.MuxSealed...)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	header := make([]byte, metadataSlotHeaderBytes)
	copy(header, metadataSlotMagic)
	header[4] = metadataSlotVersion
	if _, err := rand.Read(header[5:]); err != nil {
		return "", err
	}
	sealed := aead.Seal(header, header[5:], plain, header)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *metadataSlotCodec) open(description string) (metadataSlotSnapshot, error) {
	if len(description) == 0 || len(description) > c.limits.DescriptionBytes {
		return metadataSlotSnapshot{}, errors.New("metadata slot description size invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(description)
	if err != nil || len(raw) < metadataSlotHeaderBytes+16+metadataSlotPlainBytes {
		return metadataSlotSnapshot{}, errors.New("metadata slot description encoding invalid")
	}
	header := raw[:metadataSlotHeaderBytes]
	if string(header[:4]) != metadataSlotMagic || header[4] != metadataSlotVersion {
		return metadataSlotSnapshot{}, errors.New("metadata slot magic or version invalid")
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return metadataSlotSnapshot{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return metadataSlotSnapshot{}, err
	}
	plain, err := aead.Open(nil, header[5:], raw[metadataSlotHeaderBytes:], header)
	if err != nil {
		return metadataSlotSnapshot{}, fmt.Errorf("metadata slot authentication failed: %w", err)
	}
	cursor := metadataSlotCursor{data: plain}
	genBytes, err := cursor.take(16)
	if err != nil {
		return metadataSlotSnapshot{}, err
	}
	var snapshot metadataSlotSnapshot
	copy(snapshot.generation[:], genBytes)
	dirBytes, err := cursor.take(1)
	if err != nil {
		return metadataSlotSnapshot{}, err
	}
	snapshot.direction = dirBytes[0]
	if snapshot.revision, err = cursor.uint64(); err != nil {
		return metadataSlotSnapshot{}, err
	}
	if snapshot.ack, err = cursor.uint64(); err != nil {
		return metadataSlotSnapshot{}, err
	}
	count, err := cursor.uint16()
	if err != nil {
		return metadataSlotSnapshot{}, err
	}
	if int(count) > c.limits.Records {
		return metadataSlotSnapshot{}, errMetadataSlotOverflow
	}
	snapshot.records = make([]metadataSlotRecord, 0, int(count))
	for i := 0; i < int(count); i++ {
		var record metadataSlotRecord
		if record.SlotSeq, err = cursor.uint64(); err != nil {
			return metadataSlotSnapshot{}, err
		}
		nameLen, err := cursor.uint16()
		if err != nil {
			return metadataSlotSnapshot{}, err
		}
		sealedLen, err := cursor.uint16()
		if err != nil {
			return metadataSlotSnapshot{}, err
		}
		name, err := cursor.take(int(nameLen))
		if err != nil {
			return metadataSlotSnapshot{}, err
		}
		record.MuxName = string(name)
		record.MuxSealed, err = cursor.take(int(sealedLen))
		if err != nil {
			return metadataSlotSnapshot{}, err
		}
		snapshot.records = append(snapshot.records, record)
	}
	if cursor.remaining() != 0 {
		return metadataSlotSnapshot{}, errors.New("metadata slot trailing plaintext")
	}
	if err := c.validateSnapshot(snapshot); err != nil {
		return metadataSlotSnapshot{}, err
	}
	return snapshot, nil
}

type metadataSlotCursor struct {
	data []byte
	off  int
}

func (c *metadataSlotCursor) remaining() int { return len(c.data) - c.off }

func (c *metadataSlotCursor) take(n int) ([]byte, error) {
	if n < 0 || n > c.remaining() {
		return nil, errors.New("metadata slot plaintext truncated")
	}
	part := c.data[c.off : c.off+n]
	c.off += n
	return part, nil
}

func (c *metadataSlotCursor) uint16() (uint16, error) {
	b, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (c *metadataSlotCursor) uint64() (uint64, error) {
	b, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

type metadataSlotPatch struct {
	revision    uint64
	highestSeq  uint64
	description string
	ambiguous   bool
}

type metadataSlotWriter struct {
	codec         *metadataSlotCodec
	pending       []metadataSlotRecord
	acked         uint64
	nextSeq       uint64
	nextRevision  uint64
	maxOfferedSeq uint64
	inFlight      *metadataSlotPatch
	closed        bool
}

func (w *metadataSlotWriter) enqueue(name string, sealed []byte) (metadataSlotRecord, error) {
	if w.closed {
		return metadataSlotRecord{}, errMetadataSlotClosed
	}
	if w.nextSeq == math.MaxUint64 {
		return metadataSlotRecord{}, errors.New("metadata slot sequence exhausted")
	}
	record := metadataSlotRecord{SlotSeq: w.nextSeq + 1, MuxName: name, MuxSealed: bytes.Clone(sealed)}
	if err := w.codec.validateRecord(record); err != nil {
		return metadataSlotRecord{}, err
	}
	for _, existing := range w.pending {
		if existing.MuxName == record.MuxName {
			return metadataSlotRecord{}, errors.New("metadata slot mux name already queued")
		}
	}
	candidate := append(append([]metadataSlotRecord(nil), w.pending...), record)
	if len(candidate) > w.codec.limits.Records || w.codec.descriptionLength(candidate) > w.codec.limits.DescriptionBytes {
		return metadataSlotRecord{}, errMetadataSlotOverflow
	}
	w.nextSeq++
	w.pending = append(w.pending, record)
	return record.clone(), nil
}

func (w *metadataSlotWriter) prepare(localAck uint64) (metadataSlotPatch, error) {
	if w.closed {
		return metadataSlotPatch{}, errMetadataSlotClosed
	}
	if w.inFlight != nil {
		return metadataSlotPatch{}, errMetadataSlotBusy
	}
	if w.nextRevision == math.MaxUint64 {
		return metadataSlotPatch{}, errors.New("metadata slot revision exhausted")
	}
	snapshot := metadataSlotSnapshot{generation: w.codec.generation, direction: w.codec.direction, revision: w.nextRevision + 1, ack: localAck, records: w.pending}
	description, err := w.codec.seal(snapshot)
	if err != nil {
		return metadataSlotPatch{}, err
	}
	patch := metadataSlotPatch{revision: snapshot.revision, description: description}
	if len(w.pending) > 0 {
		patch.highestSeq = w.pending[len(w.pending)-1].SlotSeq
		w.maxOfferedSeq = patch.highestSeq
	}
	w.nextRevision++
	w.inFlight = &patch
	return patch, nil
}

func (w *metadataSlotWriter) validatePatch(p metadataSlotPatch) error {
	if w.closed {
		return errMetadataSlotClosed
	}
	if w.inFlight == nil || p.revision != w.inFlight.revision || p.description != w.inFlight.description {
		return errors.New("metadata slot PATCH identity mismatch")
	}
	return nil
}

func (w *metadataSlotWriter) published(p metadataSlotPatch) error {
	if err := w.validatePatch(p); err != nil {
		return err
	}
	if w.inFlight.ambiguous {
		return errMetadataSlotBusy
	}
	w.inFlight = nil
	return nil
}

func (w *metadataSlotWriter) ambiguous(p metadataSlotPatch) error {
	if err := w.validatePatch(p); err != nil {
		return err
	}
	w.inFlight.ambiguous = true
	return nil
}

// resolveAmbiguous requires the caller to have drained/cancelled the original
// HTTP request. A mismatched readback cannot prove that a late PATCH will not
// overwrite a newer snapshot, so it leaves the writer blocked for fallback.
func (w *metadataSlotWriter) resolveAmbiguous(p metadataSlotPatch, readback string) error {
	if err := w.validatePatch(p); err != nil {
		return err
	}
	if !w.inFlight.ambiguous || readback != w.inFlight.description {
		return errMetadataSlotBusy
	}
	w.inFlight = nil
	return nil
}

func (w *metadataSlotWriter) validateAck(ack uint64) error {
	if ack < w.acked || ack > w.maxOfferedSeq {
		return errors.New("metadata slot ACK regressed or exceeds offered records")
	}
	return nil
}

func (w *metadataSlotWriter) applyAck(ack uint64) {
	w.acked = ack
	for len(w.pending) > 0 && w.pending[0].SlotSeq <= ack {
		w.pending[0] = metadataSlotRecord{}
		w.pending = w.pending[1:]
	}
	if w.inFlight != nil && w.inFlight.ambiguous && w.inFlight.highestSeq != 0 && ack >= w.inFlight.highestSeq {
		w.inFlight = nil
	}
}

func (w *metadataSlotWriter) fallbackAndClose() []metadataSlotRecord {
	w.closed = true
	w.inFlight = nil
	result := make([]metadataSlotRecord, len(w.pending))
	for i, r := range w.pending {
		result[i] = r.clone()
		w.pending[i] = metadataSlotRecord{}
	}
	w.pending = nil
	return result
}

type metadataSlotStaged struct {
	record       metadataSlotRecord
	externalDone bool
}

type metadataSlotProcessedIdentity struct {
	seq  uint64
	name string
}

type metadataSlotReader struct {
	codec             *metadataSlotCodec
	lastRevision      uint64
	processedSeq      uint64
	staged            []metadataSlotStaged
	processed         map[uint64][32]byte
	processedNames    map[string]uint64
	processedOrder    []metadataSlotProcessedIdentity
	fallbackProcessed map[string][32]byte
	fallbackOrder     []string
}

func metadataSlotRecordDigest(r metadataSlotRecord) [32]byte {
	h := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(r.MuxName)))
	h.Write(length[:])
	h.Write([]byte(r.MuxName))
	h.Write(r.MuxSealed)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func (r *metadataSlotReader) accept(s metadataSlotSnapshot) ([]metadataSlotRecord, error) {
	if s.generation != r.codec.generation || s.direction != r.codec.direction {
		return nil, errors.New("metadata slot snapshot belongs to another generation or direction")
	}
	if s.revision <= r.lastRevision {
		return nil, errMetadataSlotReplay
	}
	if len(s.records) == 0 {
		if len(r.staged) != 0 {
			return nil, errors.New("metadata slot snapshot omitted unprocessed records")
		}
		r.lastRevision = s.revision
		return nil, nil
	}
	if r.processedSeq == math.MaxUint64 || s.records[0].SlotSeq > r.processedSeq+1 {
		return nil, errors.New("metadata slot record gap")
	}
	if uint64(len(r.staged)) > math.MaxUint64-r.processedSeq {
		return nil, errors.New("metadata slot staged sequence overflow")
	}
	stagedEnd := r.processedSeq + uint64(len(r.staged))
	last := s.records[len(s.records)-1].SlotSeq
	if last < stagedEnd {
		return nil, errors.New("metadata slot snapshot omitted staged records")
	}
	newRecords := make([]metadataSlotRecord, 0, len(s.records))
	for _, record := range s.records {
		digest := metadataSlotRecordDigest(record)
		switch {
		case record.SlotSeq <= r.processedSeq:
			old, found := r.processed[record.SlotSeq]
			if !found || old != digest {
				return nil, errors.New("metadata slot processed record identity changed or aged out")
			}
		case record.SlotSeq <= stagedEnd:
			old := r.staged[record.SlotSeq-r.processedSeq-1].record
			if metadataSlotRecordDigest(old) != digest {
				return nil, errors.New("metadata slot staged record identity changed")
			}
		default:
			if _, found := r.processedNames[record.MuxName]; found {
				return nil, errors.New("metadata slot mux name already processed")
			}
			if old, found := r.fallbackProcessed[record.MuxName]; found {
				if old != digest {
					return nil, errors.New("metadata slot fallback identity changed")
				}
			} else {
				newRecords = append(newRecords, record.clone())
			}
		}
	}
	for _, record := range s.records {
		if record.SlotSeq <= stagedEnd {
			continue
		}
		_, externalDone := r.fallbackProcessed[record.MuxName]
		r.staged = append(r.staged, metadataSlotStaged{record: record.clone(), externalDone: externalDone})
	}
	r.lastRevision = s.revision
	r.advanceProcessed()
	return newRecords, nil
}

func (r *metadataSlotReader) advanceProcessed() {
	for len(r.staged) > 0 && r.staged[0].externalDone {
		record := r.staged[0].record
		r.processedSeq = record.SlotSeq
		if r.processed == nil {
			r.processed = make(map[uint64][32]byte)
			r.processedNames = make(map[string]uint64)
		}
		r.processed[record.SlotSeq] = metadataSlotRecordDigest(record)
		r.processedNames[record.MuxName] = record.SlotSeq
		r.processedOrder = append(r.processedOrder, metadataSlotProcessedIdentity{seq: record.SlotSeq, name: record.MuxName})
		if len(r.processedOrder) > r.codec.limits.Records {
			oldest := r.processedOrder[0]
			// Names are bounded to the same active slot window as sequence
			// fingerprints; the mux layer keeps its own longer-lived dedup set.
			delete(r.processedNames, oldest.name)
			delete(r.processed, oldest.seq)
			r.processedOrder = r.processedOrder[1:]
		}
		r.staged[0] = metadataSlotStaged{}
		r.staged = r.staged[1:]
	}
}

func (r *metadataSlotReader) markProcessed(seq uint64, name string) error {
	if len(r.staged) == 0 || r.staged[0].record.SlotSeq != seq || r.staged[0].record.MuxName != name {
		return errors.New("metadata slot processing must acknowledge the staged head")
	}
	r.staged[0].externalDone = true
	r.advanceProcessed()
	return nil
}

// noteFallbackProcessed is called only after the original immutable mux
// record has passed the existing mux receive path. It suppresses a later slot
// copy with the same name and sealed bytes, within the bounded slot window.
func (r *metadataSlotReader) noteFallbackProcessed(name string, sealed []byte) error {
	record := metadataSlotRecord{SlotSeq: 1, MuxName: name, MuxSealed: sealed}
	if err := r.codec.validateRecord(record); err != nil {
		return err
	}
	digest := metadataSlotRecordDigest(record)
	if old, found := r.fallbackProcessed[name]; found && old != digest {
		return errors.New("metadata slot fallback mux identity changed")
	}
	for i := range r.staged {
		if r.staged[i].record.MuxName == name && metadataSlotRecordDigest(r.staged[i].record) != digest {
			return errors.New("metadata slot staged fallback identity changed")
		}
	}
	if r.fallbackProcessed == nil {
		r.fallbackProcessed = make(map[string][32]byte)
	}
	if _, found := r.fallbackProcessed[name]; !found {
		r.fallbackOrder = append(r.fallbackOrder, name)
	}
	r.fallbackProcessed[name] = digest
	for len(r.fallbackOrder) > r.codec.limits.Records {
		delete(r.fallbackProcessed, r.fallbackOrder[0])
		r.fallbackOrder = r.fallbackOrder[1:]
	}
	for i := range r.staged {
		if r.staged[i].record.MuxName == name {
			r.staged[i].externalDone = true
		}
	}
	r.advanceProcessed()
	return nil
}

// metadataSlotPeer owns both state machines under one mutex. It performs ACK
// validation before accepting a received snapshot, so callers cannot trim the
// send queue from unauthenticated metadata.
type metadataSlotPeer struct {
	mu       sync.Mutex
	outbound metadataSlotWriter
	inbound  metadataSlotReader
}

func newMetadataSlotPeer(secret string, sid [16]byte, sendDirection byte, clientID, runID string, generation [16]byte, limits metadataSlotLimits) (*metadataSlotPeer, error) {
	if sendDirection != DirectionUp && sendDirection != DirectionDown {
		return nil, errors.New("metadata slot send direction invalid")
	}
	receiveDirection := DirectionUp
	if sendDirection == DirectionUp {
		receiveDirection = DirectionDown
	}
	sendCodec, err := newMetadataSlotCodec(secret, sid, sendDirection, clientID, runID, generation, limits)
	if err != nil {
		return nil, err
	}
	receiveCodec, err := newMetadataSlotCodec(secret, sid, receiveDirection, clientID, runID, generation, limits)
	if err != nil {
		return nil, err
	}
	return &metadataSlotPeer{outbound: metadataSlotWriter{codec: sendCodec}, inbound: metadataSlotReader{codec: receiveCodec}}, nil
}

func (p *metadataSlotPeer) enqueue(name string, sealed []byte) (metadataSlotRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.enqueue(name, sealed)
}

func (p *metadataSlotPeer) prepare() (metadataSlotPatch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.prepare(p.inbound.processedSeq)
}

func (p *metadataSlotPeer) published(patch metadataSlotPatch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.published(patch)
}

func (p *metadataSlotPeer) ambiguous(patch metadataSlotPatch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.ambiguous(patch)
}

func (p *metadataSlotPeer) resolveAmbiguous(patch metadataSlotPatch, readback string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.resolveAmbiguous(patch, readback)
}

func (p *metadataSlotPeer) receive(description string) ([]metadataSlotRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot, err := p.inbound.codec.open(description)
	if err != nil {
		return nil, err
	}
	// A cached authenticated older snapshot can legitimately carry an ACK
	// below the latest one. Ignore its replay before validating current ACK
	// monotonicity; only a fresh revision may change either direction's state.
	if snapshot.revision <= p.inbound.lastRevision {
		return nil, errMetadataSlotReplay
	}
	if err := p.outbound.validateAck(snapshot.ack); err != nil {
		return nil, err
	}
	records, err := p.inbound.accept(snapshot)
	if err != nil {
		return nil, err
	}
	p.outbound.applyAck(snapshot.ack)
	return records, nil
}

func (p *metadataSlotPeer) markProcessed(seq uint64, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inbound.markProcessed(seq, name)
}

func (p *metadataSlotPeer) noteFallbackProcessed(name string, sealed []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inbound.noteFallbackProcessed(name, sealed)
}

func (p *metadataSlotPeer) fallbackAndClose() []metadataSlotRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.fallbackAndClose()
}

func (p *metadataSlotPeer) acked() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inbound.processedSeq
}

func (p *metadataSlotPeer) outboundAcked() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound.acked
}

func (p *metadataSlotPeer) stagedHead() (metadataSlotRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inbound.staged) == 0 || p.inbound.staged[0].externalDone {
		return metadataSlotRecord{}, false
	}
	return p.inbound.staged[0].record.clone(), true
}
