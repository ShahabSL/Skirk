package skirk

// Research only: this protocol is linked exclusively into test binaries.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	fsHybridWireMax     = 64 << 10
	fsHybridInlineMax   = 16 << 10
	fsHybridPhysicalMax = (4 << 20) + (64 << 10)
	fsHybridNameMax     = 1024
	fsHybridClaimsMax   = 4096
	fsHybridQueueMax    = 32
	fsHybridCacheMax    = 8 << 20
	fsHybridMagic       = "FSH1"
)

var (
	errFSHybridBounds    = errors.New("Firestore hybrid research bound reached")
	errFSHybridIntegrity = errors.New("Firestore hybrid immutable identity or authenticated state conflict")
	errFSHybridReplay    = errors.New("Firestore hybrid old snapshot")
	errFSHybridClosed    = errors.New("Firestore hybrid finite research session closed")
	errFSHybridBusy      = errors.New("Firestore hybrid bounded admission unavailable")
)

type fsHybridRecord struct {
	Sequence uint64 `json:"sequence"`
	ByteEnd  uint64 `json:"byte_end"`
	Name     string `json:"name"`
	Sealed   []byte `json:"sealed"`
}

func (r fsHybridRecord) cost() uint64 { return uint64(len(r.Name) + len(r.Sealed) + 64) }
func (r fsHybridRecord) clone() fsHybridRecord {
	r.Sealed = bytes.Clone(r.Sealed)
	return r
}
func fsHybridIdentity(name string, sealed []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(sealed)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func fsHybridID(name string, sealed []byte) string {
	digest := fsHybridIdentity(name, sealed)
	return "firestore-priority:" + hex.EncodeToString(digest[:])
}

// Credit is a cumulative admission watermark, not a reusable free-slot count.
// A sender debits Sequence and ByteEnd for every new offered identity.
type fsHybridSnapshot struct {
	Epoch       string           `json:"epoch"`
	Direction   byte             `json:"direction"`
	Revision    uint64           `json:"revision"`
	Processed   uint64           `json:"processed"`
	CreditSeq   uint64           `json:"credit_seq"`
	CreditBytes uint64           `json:"credit_bytes"`
	Records     []fsHybridRecord `json:"records"`
}

type fsHybridCodec struct {
	aead      cipher.AEAD
	epoch     [16]byte
	direction byte
	secret    string
	sid       [16]byte
	clientID  string
	runID     string
	wireMax   int
}

func (c *fsHybridCodec) wireLimit() int {
	if c.wireMax == 0 {
		return fsHybridWireMax
	}
	return c.wireMax
}

func newFSHybridCodec(secret string, sid, epoch [16]byte, direction byte, clientID, runID string) (*fsHybridCodec, error) {
	if epoch == [16]byte{} || sid == [16]byte{} || (direction != DirectionUp && direction != DirectionDown) || !isSafeObjectSegment(clientID) || !isSafeObjectSegment(runID) || len(clientID) > 64 || len(runID) > 64 {
		return nil, errFSHybridBounds
	}
	base, err := DeriveKey(secret)
	if err != nil {
		return nil, err
	}
	info := append([]byte("skirk-research-firestore-priority-v1\x00"+clientID+"\x00"+runID+"\x00"), epoch[:]...)
	info = append(info, direction)
	key := hkdfSHA256(base, sid[:], info, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &fsHybridCodec{aead: aead, epoch: epoch, direction: direction, secret: secret, sid: sid, clientID: clientID, runID: runID}, nil
}

func (c *fsHybridCodec) validateMux(name string, sealed []byte) (muxObjectMeta, error) {
	return c.validateMuxMax(name, sealed, fsHybridInlineMax)
}

func (c *fsHybridCodec) validateMuxMax(name string, sealed []byte, limit int) (muxObjectMeta, error) {
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name})
	if !ok || !meta.Priority || meta.MetadataOnly || len(name) > fsHybridNameMax || !strings.HasPrefix(name, muxDirPrefix(c.sid, c.direction, c.clientID, c.runID)) || meta.ClientID != c.clientID || meta.RunID != c.runID || meta.Lane < 0 || meta.Lane >= muxLaneCount || meta.Seq == 0 || len(sealed) == 0 || len(sealed) > limit {
		return muxObjectMeta{}, errFSHybridIntegrity
	}
	key, err := DeriveMuxLaneKeyV4(c.secret, c.sid, c.direction, c.clientID, c.runID, meta.Lane)
	if err != nil {
		return muxObjectMeta{}, err
	}
	envelope, raw, err := OpenEnvelope(key, sealed)
	if err != nil || envelope.SessionID != c.sid || envelope.Direction != c.direction || envelope.Sequence != meta.Seq || envelope.Flags != FlagData || meta.PlainBytes != len(raw) {
		return muxObjectMeta{}, errFSHybridIntegrity
	}
	// The production decoder accepts a uint16 count before allocating. Bound
	// that authenticated declaration before any frame-slice allocation here.
	if len(raw) < muxBatchHeaderSize {
		return muxObjectMeta{}, errFSHybridIntegrity
	}
	count := int(binary.BigEndian.Uint16(raw[5:7]))
	if count < 1 || count > muxMaxFrames || count > (len(raw)-muxBatchHeaderSize)/muxFrameHeaderSize {
		return muxObjectMeta{}, errFSHybridIntegrity
	}
	frames, err := decodeMuxBatch(raw)
	if err != nil || len(frames) == 0 || frames[0].StreamID != meta.StreamID || !slices.Equal(muxBatchStreamIDs(frames), meta.StreamIDs) {
		return muxObjectMeta{}, errFSHybridIntegrity
	}
	for _, frame := range frames {
		if frame.StreamID == 0 {
			return muxObjectMeta{}, errFSHybridIntegrity
		}
		switch frame.Kind {
		case muxFrameOpen:
			target, _, openErr := decodeMuxOpenPayload(frame.Payload)
			host, port, splitErr := net.SplitHostPort(target)
			n, portErr := strconv.Atoi(port)
			if c.direction != DirectionUp || frame.Seq != 0 || openErr != nil || splitErr != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
				return muxObjectMeta{}, errFSHybridIntegrity
			}
		case muxFrameData:
			if frame.Seq == 0 {
				return muxObjectMeta{}, errFSHybridIntegrity
			}
		case muxFrameFIN:
			if frame.Seq == 0 || len(frame.Payload) != 0 {
				return muxObjectMeta{}, errFSHybridIntegrity
			}
		case muxFrameRST:
			if frame.Seq != 0 || len(frame.Payload) > 1024 {
				return muxObjectMeta{}, errFSHybridIntegrity
			}
		case muxFrameMetadataCapability:
			if frame.Seq != 0 || !fsHybridMetadataCapability(c.direction, frame.Payload) {
				return muxObjectMeta{}, errFSHybridIntegrity
			}
		default:
			return muxObjectMeta{}, errFSHybridIntegrity
		}
	}
	if meta.FrameRangeKnown && len(meta.StreamIDs) == 1 {
		first, last := muxBatchFrameSeqRange(frames)
		if first != meta.FrameMinSeq || last != meta.FrameMaxSeq {
			return muxObjectMeta{}, errFSHybridIntegrity
		}
	}
	return meta, nil
}

func fsHybridMetadataCapability(direction byte, payload []byte) bool {
	if len(payload) != muxMetadataCapabilityPayloadBytes && len(payload) != muxMetadataCapabilityV2PayloadBytes {
		return false
	}
	kind := payload[0]
	if len(payload) == muxMetadataCapabilityV2PayloadBytes {
		limit := int(binary.BigEndian.Uint16(payload[18:]))
		if payload[1] != 2 || (limit != driveInlineMaxBytes && limit != driveMetadataOnlyLargeMaxBytes) {
			return false
		}
	}
	if direction == DirectionUp {
		return kind == muxMetadataCapabilityRequest && len(payload) == muxMetadataCapabilityPayloadBytes || kind == muxMetadataCapabilityRequestV2 && len(payload) == muxMetadataCapabilityV2PayloadBytes
	}
	return kind == muxMetadataCapabilityEcho && len(payload) == muxMetadataCapabilityPayloadBytes || kind == muxMetadataCapabilityEchoV2 && len(payload) == muxMetadataCapabilityV2PayloadBytes
}

func (c *fsHybridCodec) validate(s fsHybridSnapshot) error {
	if s.Epoch != hex.EncodeToString(c.epoch[:]) || s.Direction != c.direction || s.Revision == 0 || s.Processed > fsHybridClaimsMax || s.CreditSeq > fsHybridClaimsMax || s.CreditBytes > fsHybridClaimsMax*(fsHybridInlineMax+fsHybridNameMax+64)+fsHybridCacheMax || len(s.Records) > fsHybridQueueMax {
		return errFSHybridBounds
	}
	seen := make(map[string]struct{}, len(s.Records))
	seenCipher := make(map[[32]byte]struct{}, len(s.Records))
	for i, record := range s.Records {
		if record.Sequence == 0 || record.Sequence > fsHybridClaimsMax || record.ByteEnd < record.cost() {
			return errFSHybridBounds
		}
		if _, exists := seen[record.Name]; exists {
			return errFSHybridIntegrity
		}
		seen[record.Name] = struct{}{}
		if _, exists := seenCipher[sha256.Sum256(record.Sealed)]; exists {
			return errFSHybridIntegrity
		}
		seenCipher[sha256.Sum256(record.Sealed)] = struct{}{}
		if _, err := c.validateMux(record.Name, record.Sealed); err != nil {
			return err
		}
		if i > 0 {
			prior := s.Records[i-1]
			if prior.Sequence == math.MaxUint64 || record.Sequence != prior.Sequence+1 || prior.ByteEnd > math.MaxUint64-record.cost() || record.ByteEnd != prior.ByteEnd+record.cost() {
				return errFSHybridIntegrity
			}
		}
	}
	return nil
}

func (c *fsHybridCodec) seal(s fsHybridSnapshot) ([]byte, error) {
	if err := c.validate(s); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil || len(raw)+len(fsHybridMagic)+c.aead.NonceSize()+c.aead.Overhead() > c.wireLimit() {
		return nil, errFSHybridBounds
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	wire := append([]byte(fsHybridMagic), nonce...)
	return c.aead.Seal(wire, nonce, raw, []byte(fsHybridMagic)), nil
}

func (c *fsHybridCodec) open(wire []byte) (fsHybridSnapshot, error) {
	var snapshot fsHybridSnapshot
	if len(wire) < len(fsHybridMagic)+c.aead.NonceSize()+c.aead.Overhead() || len(wire) > c.wireLimit() || string(wire[:len(fsHybridMagic)]) != fsHybridMagic {
		return snapshot, errFSHybridBounds
	}
	nonce := wire[len(fsHybridMagic) : len(fsHybridMagic)+c.aead.NonceSize()]
	raw, err := c.aead.Open(nil, nonce, wire[len(fsHybridMagic)+c.aead.NonceSize():], []byte(fsHybridMagic))
	if err != nil {
		return snapshot, errFSHybridIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(new(any)) != io.EOF {
		return snapshot, errFSHybridIntegrity
	}
	canonical, err := json.Marshal(snapshot)
	if err != nil || !bytes.Equal(canonical, raw) {
		// There is one wire representation, including for repeated JSON keys.
		return fsHybridSnapshot{}, errFSHybridIntegrity
	}
	if err = c.validate(snapshot); err != nil {
		return fsHybridSnapshot{}, err
	}
	return snapshot, nil
}

func (c *fsHybridCodec) snapshot(revision, processed, creditSeq, creditBytes uint64, records []fsHybridRecord) fsHybridSnapshot {
	return fsHybridSnapshot{Epoch: hex.EncodeToString(c.epoch[:]), Direction: c.direction, Revision: revision, Processed: processed, CreditSeq: creditSeq, CreditBytes: creditBytes, Records: records}
}

type fsPriorityFenceKind byte

const (
	fsPriorityFirestoreFence fsPriorityFenceKind = iota
	fsPriorityDriveV2Fence
)

type fsPriorityDocument struct {
	Ciphertext  []byte
	UpdateTime  string
	FenceKind   fsPriorityFenceKind
	OpaqueFence string
}

// Watch notifications may coalesce. The current cumulative snapshot, not the
// count of callbacks, carries delivery state. Every call is session bounded.
type fsPriorityCarrier interface {
	WriteOwn(context.Context, string, []byte) (fsPriorityDocument, error)
	ReadOwn(context.Context) (fsPriorityDocument, error)
	ReadPeer(context.Context) (fsPriorityDocument, error)
	WatchPeer(context.Context, func(fsPriorityDocument) error) error
	Close() error
}

// Server timestamps remain opaque strings; no floating point round trip.
func (d fsPriorityDocument) valid() bool {
	if len(d.Ciphertext) > fsHybridWireMax {
		return false
	}
	if d.FenceKind == fsPriorityDriveV2Fence {
		if d.UpdateTime != "" || len(d.OpaqueFence) < 3 || len(d.OpaqueFence) > 1024 || d.OpaqueFence[0] != '"' || d.OpaqueFence[len(d.OpaqueFence)-1] != '"' {
			return false
		}
		for _, v := range []byte(d.OpaqueFence[1 : len(d.OpaqueFence)-1]) {
			if v < 0x21 || v > 0x7e || v == '"' {
				return false
			}
		}
		return true
	}
	if d.FenceKind != fsPriorityFirestoreFence || d.OpaqueFence != "" || len(d.UpdateTime) > 64 {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, d.UpdateTime)
	return err == nil && t.Nanosecond()%1000 == 0
}
func (d fsPriorityDocument) fence() string {
	if d.FenceKind == fsPriorityDriveV2Fence {
		return d.OpaqueFence
	}
	return d.UpdateTime
}
func (d fsPriorityDocument) sameFence(other fsPriorityDocument) bool {
	return d.valid() && other.valid() && d.FenceKind == other.FenceKind && d.fence() == other.fence()
}
