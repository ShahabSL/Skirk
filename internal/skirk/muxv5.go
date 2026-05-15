package skirk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	muxV5ObjectPrefix         = "muxv5"
	muxV6ObjectPrefix         = "muxv6"
	muxV5ControlMagic         = "SKC5"
	muxV5ControlVersion       = byte(1)
	muxV5DataMagic            = "SKD5"
	muxV5DataVersion          = byte(1)
	muxV5DataRecordMagic      = "SKR5"
	muxV5DataRecordVersion    = byte(1)
	muxV5DataRecordHeaderSize = 61
	muxV5MaxSlabSeq           = uint64(1<<48 - 1)

	muxV5RecordOpen   = byte(1)
	muxV5RecordData   = byte(2)
	muxV5RecordFIN    = byte(3)
	muxV5RecordRST    = byte(4)
	muxV5RecordACK    = byte(5)
	muxV5RecordCredit = byte(6)

	muxV5ClassControl     = byte(1)
	muxV5ClassInteractive = byte(2)
	muxV5ClassBurst       = byte(3)
	muxV5ClassBulk        = byte(4)
)

const (
	muxV5PlaneControl  = "c"
	muxV5PlaneData     = "d"
	muxV5PlaneBulk     = "b"
	muxV5ClassHotName  = "p0"
	muxV5ClassBulkName = "p1"
)

type muxV5ControlPage struct {
	Direction  byte
	ClientID   string
	RunID      string
	Epoch      string
	ControlSeq uint64
	Records    []muxV5ControlRecord
}

type muxV5ControlRecord struct {
	Type            byte
	PriorityClass   byte
	StreamID        uint64
	StreamSeqMin    uint64
	StreamSeqMax    uint64
	PlainBytes      uint64
	SealedBytes     uint64
	DataFileID      string
	DataObjectName  string
	DataOffset      uint64
	DataLength      uint64
	FrameCount      uint32
	CreditBytes     uint64
	AckByteOffset   uint64
	AckStreamSeq    uint64
	CreatedUnixNano int64
	InlineData      []byte
}

type muxV5DataSlab struct {
	Direction  byte
	ClientID   string
	RunID      string
	Epoch      string
	DataFileID string
	ObjectName string
	Lane       int
	SlabSeq    uint64
	Records    []muxV5DataRecord
}

type muxV5DataRecord struct {
	RecordIndex     uint32
	PriorityClass   byte
	Flags           byte
	StreamID        uint64
	StreamSeqMin    uint64
	StreamSeqMax    uint64
	StreamByteStart uint64
	Plaintext       []byte
}

type muxV5DataRecordRef struct {
	DataFileID      string
	ObjectName      string
	Direction       byte
	Lane            int
	SlabSeq         uint64
	RecordIndex     uint32
	PriorityClass   byte
	Flags           byte
	StreamID        uint64
	StreamSeqMin    uint64
	StreamSeqMax    uint64
	StreamByteStart uint64
	PlainBytes      uint64
	SealedBytes     uint64
	DataOffset      uint64
	DataLength      uint64
}

type muxV5DataObjectRoute struct {
	SessionID string
	Direction byte
	ClientID  string
	RunID     string
	Epoch     string
	StreamID  uint64
	Lane      int
	Seq       uint64
}

func sealMuxV5ControlPage(key []byte, sid [16]byte, page muxV5ControlPage) ([]byte, error) {
	raw, err := encodeMuxV5ControlPage(page)
	if err != nil {
		return nil, err
	}
	return Seal(key, sid, page.Direction, page.ControlSeq, raw, false)
}

func openMuxV5ControlPage(key []byte, sealed []byte) (muxV5ControlPage, error) {
	env, raw, err := OpenEnvelope(key, sealed)
	if err != nil {
		return muxV5ControlPage{}, err
	}
	page, err := decodeMuxV5ControlPage(raw)
	if err != nil {
		return muxV5ControlPage{}, err
	}
	if env.Direction != page.Direction {
		return muxV5ControlPage{}, fmt.Errorf("mux v5 control direction mismatch envelope=%d page=%d", env.Direction, page.Direction)
	}
	if env.Sequence != page.ControlSeq {
		return muxV5ControlPage{}, fmt.Errorf("mux v5 control sequence mismatch envelope=%d page=%d", env.Sequence, page.ControlSeq)
	}
	return page, nil
}

func sealMuxV5DataSlab(key []byte, slab muxV5DataSlab) ([]byte, []muxV5DataRecordRef, error) {
	if len(key) != keyLen {
		return nil, nil, fmt.Errorf("key must be %d bytes", keyLen)
	}
	if slab.Lane < 0 || slab.Lane > 255 {
		return nil, nil, fmt.Errorf("mux v5 data lane out of range: %d", slab.Lane)
	}
	if slab.SlabSeq > muxV5MaxSlabSeq {
		return nil, nil, errors.New("mux v5 data slab sequence out of range")
	}
	if len(slab.Records) > math.MaxUint16 {
		return nil, nil, errors.New("too many mux v5 data records")
	}
	recordIndexes := make(map[uint32]struct{}, len(slab.Records))
	for i, record := range slab.Records {
		if record.RecordIndex != uint32(i) {
			return nil, nil, fmt.Errorf("mux v5 data record index=%d want %d", record.RecordIndex, i)
		}
		if _, ok := recordIndexes[record.RecordIndex]; ok {
			return nil, nil, fmt.Errorf("duplicate mux v5 data record index %d", record.RecordIndex)
		}
		recordIndexes[record.RecordIndex] = struct{}{}
	}
	gcm, err := muxV5GCM(key)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := encodeMuxV5DataSlabHeader(&buf, slab); err != nil {
		return nil, nil, err
	}
	refs := make([]muxV5DataRecordRef, 0, len(slab.Records))
	for i, record := range slab.Records {
		offset := uint64(buf.Len())
		sealed, ref, err := sealMuxV5DataRecord(gcm, slab, record, offset)
		if err != nil {
			return nil, nil, fmt.Errorf("record %d: %w", i, err)
		}
		buf.Write(sealed)
		refs = append(refs, ref)
	}
	return buf.Bytes(), refs, nil
}

func openMuxV5DataSlab(key []byte, data []byte) (muxV5DataSlab, []muxV5DataRecordRef, error) {
	if len(key) != keyLen {
		return muxV5DataSlab{}, nil, fmt.Errorf("key must be %d bytes", keyLen)
	}
	gcm, err := muxV5GCM(key)
	if err != nil {
		return muxV5DataSlab{}, nil, err
	}
	reader := bytes.NewReader(data)
	slab, count, err := decodeMuxV5DataSlabHeader(reader)
	if err != nil {
		return muxV5DataSlab{}, nil, err
	}
	records := make([]muxV5DataRecord, 0, count)
	refs := make([]muxV5DataRecordRef, 0, count)
	for i := 0; i < int(count); i++ {
		offset := uint64(len(data) - reader.Len())
		recordBytes, err := readMuxV5DataRecordBytes(reader)
		if err != nil {
			return muxV5DataSlab{}, nil, fmt.Errorf("record %d: %w", i, err)
		}
		ref, err := muxV5DataRecordRefFromBytes(slab, recordBytes, offset)
		if err != nil {
			return muxV5DataSlab{}, nil, fmt.Errorf("record %d: %w", i, err)
		}
		record, err := openMuxV5DataRecord(gcm, ref, recordBytes)
		if err != nil {
			return muxV5DataSlab{}, nil, fmt.Errorf("record %d: %w", i, err)
		}
		records = append(records, record)
		refs = append(refs, ref)
	}
	if reader.Len() != 0 {
		return muxV5DataSlab{}, nil, errors.New("trailing mux v5 data slab bytes")
	}
	slab.Records = records
	return slab, refs, nil
}

func openMuxV5DataRecordFromRef(key []byte, ref muxV5DataRecordRef, data []byte) (muxV5DataRecord, error) {
	if len(key) != keyLen {
		return muxV5DataRecord{}, fmt.Errorf("key must be %d bytes", keyLen)
	}
	gcm, err := muxV5GCM(key)
	if err != nil {
		return muxV5DataRecord{}, err
	}
	return openMuxV5DataRecord(gcm, ref, data)
}

func openMuxV5DataRecordFromManifest(key []byte, manifest muxV5ControlRecord, data []byte) (muxV5DataRecord, muxV5DataRecordRef, error) {
	if manifest.DataFileID == "" || manifest.DataObjectName == "" || manifest.DataLength == 0 {
		return muxV5DataRecord{}, muxV5DataRecordRef{}, errors.New("mux v5 data record missing data reference")
	}
	ref, err := muxV5DataRecordRefFromRangeBytes(data, manifest.DataFileID, manifest.DataObjectName, manifest.DataOffset)
	if err != nil {
		return muxV5DataRecord{}, muxV5DataRecordRef{}, err
	}
	if ref.DataLength != manifest.DataLength ||
		ref.PriorityClass != manifest.PriorityClass ||
		ref.StreamID != manifest.StreamID ||
		ref.StreamSeqMin != manifest.StreamSeqMin ||
		ref.StreamSeqMax != manifest.StreamSeqMax ||
		ref.PlainBytes != manifest.PlainBytes ||
		ref.SealedBytes != manifest.SealedBytes {
		return muxV5DataRecord{}, muxV5DataRecordRef{}, errors.New("mux v5 data manifest record mismatch")
	}
	record, err := openMuxV5DataRecordFromRef(key, ref, data)
	if err != nil {
		return muxV5DataRecord{}, muxV5DataRecordRef{}, err
	}
	return record, ref, nil
}

func openMuxV5DataRecord(gcm cipher.AEAD, ref muxV5DataRecordRef, data []byte) (muxV5DataRecord, error) {
	parsed, err := muxV5DataRecordRefFromRangeBytes(data, ref.DataFileID, ref.ObjectName, ref.DataOffset)
	if err != nil {
		return muxV5DataRecord{}, err
	}
	if parsed != ref {
		return muxV5DataRecord{}, errors.New("mux v5 data record manifest mismatch")
	}
	header := data[:muxV5DataRecordHeaderSize]
	ciphertext := data[muxV5DataRecordHeaderSize:]
	aad, err := muxV5DataRecordAAD(header, ref)
	if err != nil {
		return muxV5DataRecord{}, err
	}
	plaintext, err := gcm.Open(nil, muxV5DataRecordNonce(ref.Direction, ref.Lane, ref.SlabSeq, ref.RecordIndex), ciphertext, aad)
	if err != nil {
		return muxV5DataRecord{}, err
	}
	if uint64(len(plaintext)) != ref.PlainBytes {
		return muxV5DataRecord{}, errors.New("mux v5 data record plaintext length mismatch")
	}
	return muxV5DataRecord{
		RecordIndex:     ref.RecordIndex,
		PriorityClass:   ref.PriorityClass,
		Flags:           ref.Flags,
		StreamID:        ref.StreamID,
		StreamSeqMin:    ref.StreamSeqMin,
		StreamSeqMax:    ref.StreamSeqMax,
		StreamByteStart: ref.StreamByteStart,
		Plaintext:       plaintext,
	}, nil
}

func encodeMuxV5ControlPage(page muxV5ControlPage) ([]byte, error) {
	if len(page.Records) > math.MaxUint16 {
		return nil, errors.New("too many mux v5 control records")
	}
	var buf bytes.Buffer
	buf.WriteString(muxV5ControlMagic)
	buf.WriteByte(muxV5ControlVersion)
	buf.WriteByte(page.Direction)
	writeUint64(&buf, page.ControlSeq)
	if err := writeString16(&buf, page.ClientID); err != nil {
		return nil, fmt.Errorf("client id: %w", err)
	}
	if err := writeString16(&buf, page.RunID); err != nil {
		return nil, fmt.Errorf("run id: %w", err)
	}
	if err := writeString16(&buf, page.Epoch); err != nil {
		return nil, fmt.Errorf("epoch: %w", err)
	}
	writeUint16(&buf, uint16(len(page.Records)))
	for i, record := range page.Records {
		if err := encodeMuxV5ControlRecord(&buf, record); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
	}
	return buf.Bytes(), nil
}

func encodeMuxV5ControlRecord(buf *bytes.Buffer, record muxV5ControlRecord) error {
	if len(record.InlineData) > math.MaxUint32 {
		return errors.New("mux v5 inline control data too large")
	}
	buf.WriteByte(record.Type)
	buf.WriteByte(record.PriorityClass)
	writeUint64(buf, record.StreamID)
	writeUint64(buf, record.StreamSeqMin)
	writeUint64(buf, record.StreamSeqMax)
	writeUint64(buf, record.PlainBytes)
	writeUint64(buf, record.SealedBytes)
	writeUint64(buf, record.DataOffset)
	writeUint64(buf, record.DataLength)
	writeUint32(buf, record.FrameCount)
	writeUint64(buf, record.CreditBytes)
	writeUint64(buf, record.AckByteOffset)
	writeUint64(buf, record.AckStreamSeq)
	writeUint64(buf, uint64(record.CreatedUnixNano))
	if err := writeString16(buf, record.DataFileID); err != nil {
		return fmt.Errorf("data file id: %w", err)
	}
	if err := writeString16(buf, record.DataObjectName); err != nil {
		return fmt.Errorf("data object name: %w", err)
	}
	writeUint32(buf, uint32(len(record.InlineData)))
	buf.Write(record.InlineData)
	return nil
}

func decodeMuxV5ControlPage(data []byte) (muxV5ControlPage, error) {
	reader := bytes.NewReader(data)
	magic := make([]byte, len(muxV5ControlMagic))
	if _, err := reader.Read(magic); err != nil {
		return muxV5ControlPage{}, errors.New("mux v5 control page too short")
	}
	if string(magic) != muxV5ControlMagic {
		return muxV5ControlPage{}, errors.New("bad mux v5 control magic")
	}
	version, err := reader.ReadByte()
	if err != nil {
		return muxV5ControlPage{}, errors.New("mux v5 control page missing version")
	}
	if version != muxV5ControlVersion {
		return muxV5ControlPage{}, fmt.Errorf("unsupported mux v5 control version %d", version)
	}
	direction, err := reader.ReadByte()
	if err != nil {
		return muxV5ControlPage{}, errors.New("mux v5 control page missing direction")
	}
	controlSeq, err := readUint64(reader)
	if err != nil {
		return muxV5ControlPage{}, err
	}
	clientID, err := readString16(reader)
	if err != nil {
		return muxV5ControlPage{}, fmt.Errorf("client id: %w", err)
	}
	runID, err := readString16(reader)
	if err != nil {
		return muxV5ControlPage{}, fmt.Errorf("run id: %w", err)
	}
	epoch, err := readString16(reader)
	if err != nil {
		return muxV5ControlPage{}, fmt.Errorf("epoch: %w", err)
	}
	count, err := readUint16(reader)
	if err != nil {
		return muxV5ControlPage{}, err
	}
	page := muxV5ControlPage{
		Direction:  direction,
		ClientID:   clientID,
		RunID:      runID,
		Epoch:      epoch,
		ControlSeq: controlSeq,
		Records:    make([]muxV5ControlRecord, 0, count),
	}
	for i := 0; i < int(count); i++ {
		record, err := decodeMuxV5ControlRecord(reader)
		if err != nil {
			return muxV5ControlPage{}, fmt.Errorf("record %d: %w", i, err)
		}
		page.Records = append(page.Records, record)
	}
	if reader.Len() != 0 {
		return muxV5ControlPage{}, errors.New("trailing mux v5 control bytes")
	}
	return page, nil
}

func decodeMuxV5ControlRecord(reader *bytes.Reader) (muxV5ControlRecord, error) {
	recordType, err := reader.ReadByte()
	if err != nil {
		return muxV5ControlRecord{}, errors.New("missing record type")
	}
	priorityClass, err := reader.ReadByte()
	if err != nil {
		return muxV5ControlRecord{}, errors.New("missing priority class")
	}
	streamID, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	streamSeqMin, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	streamSeqMax, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	plainBytes, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	sealedBytes, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	dataOffset, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	dataLength, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	frameCount, err := readUint32(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	creditBytes, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	ackByteOffset, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	ackStreamSeq, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	createdUnixNano, err := readUint64(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	dataFileID, err := readString16(reader)
	if err != nil {
		return muxV5ControlRecord{}, fmt.Errorf("data file id: %w", err)
	}
	dataObjectName, err := readString16(reader)
	if err != nil {
		return muxV5ControlRecord{}, fmt.Errorf("data object name: %w", err)
	}
	inlineSize, err := readUint32(reader)
	if err != nil {
		return muxV5ControlRecord{}, err
	}
	if uint64(inlineSize) > uint64(reader.Len()) {
		return muxV5ControlRecord{}, errors.New("truncated mux v5 inline control data")
	}
	inlineData := make([]byte, int(inlineSize))
	if inlineSize > 0 {
		if _, err := reader.Read(inlineData); err != nil {
			return muxV5ControlRecord{}, err
		}
	}
	return muxV5ControlRecord{
		Type:            recordType,
		PriorityClass:   priorityClass,
		StreamID:        streamID,
		StreamSeqMin:    streamSeqMin,
		StreamSeqMax:    streamSeqMax,
		PlainBytes:      plainBytes,
		SealedBytes:     sealedBytes,
		DataFileID:      dataFileID,
		DataObjectName:  dataObjectName,
		DataOffset:      dataOffset,
		DataLength:      dataLength,
		FrameCount:      frameCount,
		CreditBytes:     creditBytes,
		AckByteOffset:   ackByteOffset,
		AckStreamSeq:    ackStreamSeq,
		CreatedUnixNano: int64(createdUnixNano),
		InlineData:      inlineData,
	}, nil
}

func muxV5GCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encodeMuxV5DataSlabHeader(buf *bytes.Buffer, slab muxV5DataSlab) error {
	buf.WriteString(muxV5DataMagic)
	buf.WriteByte(muxV5DataVersion)
	buf.WriteByte(slab.Direction)
	buf.WriteByte(byte(slab.Lane))
	writeUint64(buf, slab.SlabSeq)
	if err := writeString16(buf, slab.ClientID); err != nil {
		return fmt.Errorf("client id: %w", err)
	}
	if err := writeString16(buf, slab.RunID); err != nil {
		return fmt.Errorf("run id: %w", err)
	}
	if err := writeString16(buf, slab.Epoch); err != nil {
		return fmt.Errorf("epoch: %w", err)
	}
	if err := writeString16(buf, slab.DataFileID); err != nil {
		return fmt.Errorf("data file id: %w", err)
	}
	if err := writeString16(buf, slab.ObjectName); err != nil {
		return fmt.Errorf("object name: %w", err)
	}
	writeUint16(buf, uint16(len(slab.Records)))
	return nil
}

func decodeMuxV5DataSlabHeader(reader *bytes.Reader) (muxV5DataSlab, uint16, error) {
	magic := make([]byte, len(muxV5DataMagic))
	if _, err := reader.Read(magic); err != nil {
		return muxV5DataSlab{}, 0, errors.New("mux v5 data slab too short")
	}
	if string(magic) != muxV5DataMagic {
		return muxV5DataSlab{}, 0, errors.New("bad mux v5 data magic")
	}
	version, err := reader.ReadByte()
	if err != nil {
		return muxV5DataSlab{}, 0, errors.New("mux v5 data slab missing version")
	}
	if version != muxV5DataVersion {
		return muxV5DataSlab{}, 0, fmt.Errorf("unsupported mux v5 data version %d", version)
	}
	direction, err := reader.ReadByte()
	if err != nil {
		return muxV5DataSlab{}, 0, errors.New("mux v5 data slab missing direction")
	}
	lane, err := reader.ReadByte()
	if err != nil {
		return muxV5DataSlab{}, 0, errors.New("mux v5 data slab missing lane")
	}
	slabSeq, err := readUint64(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, err
	}
	clientID, err := readString16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, fmt.Errorf("client id: %w", err)
	}
	runID, err := readString16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, fmt.Errorf("run id: %w", err)
	}
	epoch, err := readString16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, fmt.Errorf("epoch: %w", err)
	}
	dataFileID, err := readString16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, fmt.Errorf("data file id: %w", err)
	}
	objectName, err := readString16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, fmt.Errorf("object name: %w", err)
	}
	count, err := readUint16(reader)
	if err != nil {
		return muxV5DataSlab{}, 0, err
	}
	return muxV5DataSlab{
		Direction:  direction,
		ClientID:   clientID,
		RunID:      runID,
		Epoch:      epoch,
		DataFileID: dataFileID,
		ObjectName: objectName,
		Lane:       int(lane),
		SlabSeq:    slabSeq,
	}, count, nil
}

func sealMuxV5DataRecord(gcm cipher.AEAD, slab muxV5DataSlab, record muxV5DataRecord, offset uint64) ([]byte, muxV5DataRecordRef, error) {
	if len(record.Plaintext) > math.MaxUint32 {
		return nil, muxV5DataRecordRef{}, errors.New("mux v5 data record plaintext too large")
	}
	cipherLen := uint64(len(record.Plaintext) + gcm.Overhead())
	dataLength := uint64(muxV5DataRecordHeaderSize) + cipherLen
	ref := muxV5DataRecordRef{
		DataFileID:      slab.DataFileID,
		ObjectName:      slab.ObjectName,
		Direction:       slab.Direction,
		Lane:            slab.Lane,
		SlabSeq:         slab.SlabSeq,
		RecordIndex:     record.RecordIndex,
		PriorityClass:   record.PriorityClass,
		Flags:           record.Flags,
		StreamID:        record.StreamID,
		StreamSeqMin:    record.StreamSeqMin,
		StreamSeqMax:    record.StreamSeqMax,
		StreamByteStart: record.StreamByteStart,
		PlainBytes:      uint64(len(record.Plaintext)),
		SealedBytes:     cipherLen,
		DataOffset:      offset,
		DataLength:      dataLength,
	}
	header, err := encodeMuxV5DataRecordHeader(ref)
	if err != nil {
		return nil, muxV5DataRecordRef{}, err
	}
	aad, err := muxV5DataRecordAAD(header, ref)
	if err != nil {
		return nil, muxV5DataRecordRef{}, err
	}
	ciphertext := gcm.Seal(nil, muxV5DataRecordNonce(ref.Direction, ref.Lane, ref.SlabSeq, ref.RecordIndex), record.Plaintext, aad)
	out := make([]byte, 0, len(header)+len(ciphertext))
	out = append(out, header...)
	out = append(out, ciphertext...)
	return out, ref, nil
}

func encodeMuxV5DataRecordHeader(ref muxV5DataRecordRef) ([]byte, error) {
	if ref.Lane < 0 || ref.Lane > 255 {
		return nil, fmt.Errorf("mux v5 data lane out of range: %d", ref.Lane)
	}
	if ref.SlabSeq > muxV5MaxSlabSeq {
		return nil, errors.New("mux v5 data slab sequence out of range")
	}
	if ref.SealedBytes > math.MaxUint32 || ref.PlainBytes > math.MaxUint32 {
		return nil, errors.New("mux v5 data record length too large")
	}
	var buf bytes.Buffer
	buf.WriteString(muxV5DataRecordMagic)
	buf.WriteByte(muxV5DataRecordVersion)
	buf.WriteByte(ref.PriorityClass)
	buf.WriteByte(ref.Flags)
	buf.WriteByte(ref.Direction)
	buf.WriteByte(byte(ref.Lane))
	writeUint64(&buf, ref.SlabSeq)
	writeUint32(&buf, ref.RecordIndex)
	writeUint64(&buf, ref.StreamID)
	writeUint64(&buf, ref.StreamSeqMin)
	writeUint64(&buf, ref.StreamSeqMax)
	writeUint64(&buf, ref.StreamByteStart)
	writeUint32(&buf, uint32(ref.PlainBytes))
	writeUint32(&buf, uint32(ref.SealedBytes))
	if buf.Len() != muxV5DataRecordHeaderSize {
		return nil, fmt.Errorf("mux v5 data record header size=%d want=%d", buf.Len(), muxV5DataRecordHeaderSize)
	}
	return buf.Bytes(), nil
}

func muxV5DataRecordRefFromBytes(slab muxV5DataSlab, data []byte, offset uint64) (muxV5DataRecordRef, error) {
	ref, err := muxV5DataRecordRefFromRangeBytes(data, slab.DataFileID, slab.ObjectName, offset)
	if err != nil {
		return muxV5DataRecordRef{}, err
	}
	if ref.Direction != slab.Direction || ref.Lane != slab.Lane || ref.SlabSeq != slab.SlabSeq {
		return muxV5DataRecordRef{}, errors.New("mux v5 data record slab identity mismatch")
	}
	return ref, nil
}

func muxV5DataRecordRefFromRangeBytes(data []byte, dataFileID, objectName string, offset uint64) (muxV5DataRecordRef, error) {
	if len(data) < muxV5DataRecordHeaderSize {
		return muxV5DataRecordRef{}, errors.New("mux v5 data record too short")
	}
	header := data[:muxV5DataRecordHeaderSize]
	if string(header[:4]) != muxV5DataRecordMagic {
		return muxV5DataRecordRef{}, errors.New("bad mux v5 data record magic")
	}
	if header[4] != muxV5DataRecordVersion {
		return muxV5DataRecordRef{}, fmt.Errorf("unsupported mux v5 data record version %d", header[4])
	}
	ref := muxV5DataRecordRef{
		DataFileID:      dataFileID,
		ObjectName:      objectName,
		PriorityClass:   header[5],
		Flags:           header[6],
		Direction:       header[7],
		Lane:            int(header[8]),
		SlabSeq:         binary.BigEndian.Uint64(header[9:17]),
		RecordIndex:     binary.BigEndian.Uint32(header[17:21]),
		StreamID:        binary.BigEndian.Uint64(header[21:29]),
		StreamSeqMin:    binary.BigEndian.Uint64(header[29:37]),
		StreamSeqMax:    binary.BigEndian.Uint64(header[37:45]),
		StreamByteStart: binary.BigEndian.Uint64(header[45:53]),
		PlainBytes:      uint64(binary.BigEndian.Uint32(header[53:57])),
		SealedBytes:     uint64(binary.BigEndian.Uint32(header[57:61])),
		DataOffset:      offset,
	}
	ref.DataLength = uint64(muxV5DataRecordHeaderSize) + ref.SealedBytes
	if uint64(len(data)) != ref.DataLength {
		return muxV5DataRecordRef{}, fmt.Errorf("mux v5 data record length=%d want=%d", len(data), ref.DataLength)
	}
	return ref, nil
}

func readMuxV5DataRecordBytes(reader *bytes.Reader) ([]byte, error) {
	if reader.Len() < muxV5DataRecordHeaderSize {
		return nil, errors.New("truncated mux v5 data record header")
	}
	header := make([]byte, muxV5DataRecordHeaderSize)
	if _, err := reader.Read(header); err != nil {
		return nil, err
	}
	if string(header[:4]) != muxV5DataRecordMagic {
		return nil, errors.New("bad mux v5 data record magic")
	}
	sealedBytes := int(binary.BigEndian.Uint32(header[57:61]))
	if sealedBytes > reader.Len() {
		return nil, errors.New("truncated mux v5 data record ciphertext")
	}
	out := make([]byte, 0, muxV5DataRecordHeaderSize+sealedBytes)
	out = append(out, header...)
	ciphertext := make([]byte, sealedBytes)
	if _, err := reader.Read(ciphertext); err != nil {
		return nil, err
	}
	out = append(out, ciphertext...)
	return out, nil
}

func muxV5DataRecordAAD(header []byte, ref muxV5DataRecordRef) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(header)
	if err := writeString16(&buf, ref.DataFileID); err != nil {
		return nil, fmt.Errorf("data file id: %w", err)
	}
	if err := writeString16(&buf, ref.ObjectName); err != nil {
		return nil, fmt.Errorf("object name: %w", err)
	}
	writeUint64(&buf, ref.DataOffset)
	writeUint64(&buf, ref.DataLength)
	return buf.Bytes(), nil
}

func muxV5DataRecordNonce(direction byte, lane int, slabSeq uint64, recordIndex uint32) []byte {
	out := make([]byte, 12)
	out[0] = direction
	out[1] = byte(lane)
	for i := 0; i < 6; i++ {
		out[7-i] = byte(slabSeq >> (8 * i))
	}
	binary.BigEndian.PutUint32(out[8:12], recordIndex)
	return out
}

func muxV5DirPrefix(sid [16]byte, direction byte, plane, clientID, runID string) string {
	return muxVersionDirPrefix(muxV5ObjectPrefix, sid, direction, plane, clientID, runID)
}

func muxVersionDirPrefix(prefix string, sid [16]byte, direction byte, plane, clientID, runID string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = muxV5ObjectPrefix
	}
	base := fmt.Sprintf("%s/%s/%s/%s/", prefix, SessionString(sid), directionName(direction), plane)
	clientID = strings.TrimSpace(clientID)
	runID = strings.TrimSpace(runID)
	if clientID == "" {
		return base
	}
	if runID == "" {
		return fmt.Sprintf("%s%s/", base, clientID)
	}
	return fmt.Sprintf("%s%s/%s/", base, clientID, runID)
}

func muxV5ControlObjectName(sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64, frames int, frameMinSeq, frameMaxSeq uint64, bytes int, priority bool) string {
	return muxVersionControlObjectName(muxV5ObjectPrefix, sid, direction, clientID, runID, epoch, streamID, lane, seq, frames, frameMinSeq, frameMaxSeq, bytes, priority)
}

func muxVersionControlObjectName(prefix string, sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64, frames int, frameMinSeq, frameMaxSeq uint64, bytes int, priority bool) string {
	epoch = firstNonEmptyString(strings.TrimSpace(epoch), "0000000000000000")
	class := muxV5ClassBulkName
	if priority {
		class = muxV5ClassHotName
	}
	return fmt.Sprintf("%s%s/%s/s%016x/l%02d/%016x.f%d.r%016x-%016x.b%d.ctrl", muxVersionDirPrefix(prefix, sid, direction, muxV5PlaneControl, clientID, runID), epoch, class, streamID, lane, seq, frames, frameMinSeq, frameMaxSeq, bytes)
}

func muxV5DataObjectName(sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64) string {
	return muxVersionDataObjectName(muxV5ObjectPrefix, sid, direction, clientID, runID, epoch, streamID, lane, seq)
}

func muxVersionDataObjectName(prefix string, sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64) string {
	epoch = firstNonEmptyString(strings.TrimSpace(epoch), "0000000000000000")
	return fmt.Sprintf("%s%s/s%016x/l%02d/%016x.data", muxVersionDirPrefix(prefix, sid, direction, muxV5PlaneData, clientID, runID), epoch, streamID, lane, seq)
}

func parseMuxVersionDataObjectInfo(name, prefix string) (muxV5DataObjectRoute, bool) {
	parts := strings.Split(name, "/")
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = muxV5ObjectPrefix
	}
	if len(parts) != 10 || parts[0] != prefix || parts[3] != muxV5PlaneData || !strings.HasPrefix(parts[7], "s") || !strings.HasPrefix(parts[8], "l") {
		return muxV5DataObjectRoute{}, false
	}
	var direction byte
	switch parts[2] {
	case "up":
		direction = DirectionUp
	case "down":
		direction = DirectionDown
	default:
		return muxV5DataObjectRoute{}, false
	}
	streamID, err := strconv.ParseUint(strings.TrimPrefix(parts[7], "s"), 16, 64)
	if err != nil {
		return muxV5DataObjectRoute{}, false
	}
	lane, err := strconv.Atoi(strings.TrimPrefix(parts[8], "l"))
	if err != nil {
		return muxV5DataObjectRoute{}, false
	}
	base := strings.TrimSuffix(parts[9], ".data")
	if base == parts[9] {
		return muxV5DataObjectRoute{}, false
	}
	seq, err := strconv.ParseUint(base, 16, 64)
	if err != nil {
		return muxV5DataObjectRoute{}, false
	}
	return muxV5DataObjectRoute{
		SessionID: parts[1],
		Direction: direction,
		ClientID:  parts[4],
		RunID:     parts[5],
		Epoch:     parts[6],
		StreamID:  streamID,
		Lane:      lane,
		Seq:       seq,
	}, true
}

func muxV5BulkObjectName(sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64, frames int, frameMinSeq, frameMaxSeq uint64, bytes int, priority bool) string {
	return muxVersionBulkObjectName(muxV5ObjectPrefix, sid, direction, clientID, runID, epoch, streamID, lane, seq, frames, frameMinSeq, frameMaxSeq, bytes, priority)
}

func muxVersionBulkObjectName(prefix string, sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64, frames int, frameMinSeq, frameMaxSeq uint64, bytes int, priority bool) string {
	epoch = firstNonEmptyString(strings.TrimSpace(epoch), "0000000000000000")
	class := muxV5ClassBulkName
	if priority {
		class = muxV5ClassHotName
	}
	return fmt.Sprintf("%s%s/%s/s%016x/l%02d/%016x.f%d.r%016x-%016x.b%d.bulk", muxVersionDirPrefix(prefix, sid, direction, muxV5PlaneBulk, clientID, runID), epoch, class, streamID, lane, seq, frames, frameMinSeq, frameMaxSeq, bytes)
}

func parseMuxV5ControlObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	return parseMuxVersionPlaneObjectInfo(info, muxV5ObjectPrefix, muxV5PlaneControl, ".ctrl")
}

func parseMuxV5BulkObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	return parseMuxVersionPlaneObjectInfo(info, muxV5ObjectPrefix, muxV5PlaneBulk, ".bulk")
}

func parseMuxV6ControlObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	return parseMuxVersionPlaneObjectInfo(info, muxV6ObjectPrefix, muxV5PlaneControl, ".ctrl")
}

func parseMuxV6BulkObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	return parseMuxVersionPlaneObjectInfo(info, muxV6ObjectPrefix, muxV5PlaneBulk, ".bulk")
}

func parseMuxV5PlaneObjectInfo(info ObjectInfo, plane, suffix string) (muxObjectMeta, bool) {
	return parseMuxVersionPlaneObjectInfo(info, muxV5ObjectPrefix, plane, suffix)
}

func parseMuxVersionPlaneObjectInfo(info ObjectInfo, prefix, plane, suffix string) (muxObjectMeta, bool) {
	name := info.Name
	parts := strings.Split(name, "/")
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = muxV5ObjectPrefix
	}
	if len(parts) != 11 || parts[0] != prefix || parts[3] != plane || !strings.HasPrefix(parts[8], "s") || !strings.HasPrefix(parts[9], "l") {
		return muxObjectMeta{}, false
	}
	clientID := parts[4]
	runID := parts[5]
	epoch := parts[6]
	class := parts[7]
	if clientID == "" || runID == "" || epoch == "" || (class != muxV5ClassHotName && class != muxV5ClassBulkName) {
		return muxObjectMeta{}, false
	}
	streamID, err := strconv.ParseUint(strings.TrimPrefix(parts[8], "s"), 16, 64)
	if err != nil {
		return muxObjectMeta{}, false
	}
	lane, err := strconv.Atoi(strings.TrimPrefix(parts[9], "l"))
	if err != nil {
		return muxObjectMeta{}, false
	}
	base := parts[10]
	if !strings.HasSuffix(base, suffix) {
		return muxObjectMeta{}, false
	}
	base = strings.TrimSuffix(base, suffix)
	dot := strings.IndexByte(base, '.')
	if dot <= 0 {
		return muxObjectMeta{}, false
	}
	seq, err := strconv.ParseUint(base[:dot], 16, 64)
	if err != nil {
		return muxObjectMeta{}, false
	}
	plainBytes := 0
	var frameMinSeq uint64
	var frameMaxSeq uint64
	frameRangeKnown := false
	for _, segment := range strings.Split(base[dot+1:], ".") {
		switch {
		case strings.HasPrefix(segment, "b"):
			parsed, err := strconv.Atoi(strings.TrimPrefix(segment, "b"))
			if err == nil && parsed > 0 {
				plainBytes = parsed
			}
		case strings.HasPrefix(segment, "r"):
			bounds := strings.SplitN(strings.TrimPrefix(segment, "r"), "-", 2)
			if len(bounds) != 2 {
				continue
			}
			minSeq, minErr := strconv.ParseUint(bounds[0], 16, 64)
			maxSeq, maxErr := strconv.ParseUint(bounds[1], 16, 64)
			if minErr == nil && maxErr == nil && minSeq > 0 && maxSeq >= minSeq {
				frameMinSeq = minSeq
				frameMaxSeq = maxSeq
				frameRangeKnown = true
			}
		}
	}
	if plainBytes <= 0 && info.Size > 0 && info.Size <= int64(int(^uint(0)>>1)) {
		plainBytes = int(info.Size)
	}
	var updated time.Time
	if strings.TrimSpace(info.Updated) != "" {
		updated, _ = time.Parse(time.RFC3339Nano, info.Updated)
	}
	return muxObjectMeta{Name: name, ID: info.ID, ClientID: clientID, RunID: runID, Epoch: epoch, StreamID: streamID, Lane: lane, Seq: seq, Priority: class == muxV5ClassHotName, PlainBytes: plainBytes, FrameMinSeq: frameMinSeq, FrameMaxSeq: frameMaxSeq, FrameRangeKnown: frameRangeKnown, Updated: updated}, true
}

func writeString16(buf *bytes.Buffer, value string) error {
	if len(value) > math.MaxUint16 {
		return errors.New("string too long")
	}
	writeUint16(buf, uint16(len(value)))
	buf.WriteString(value)
	return nil
}

func readString16(reader *bytes.Reader) (string, error) {
	size, err := readUint16(reader)
	if err != nil {
		return "", err
	}
	if int(size) > reader.Len() {
		return "", errors.New("truncated string")
	}
	raw := make([]byte, int(size))
	if _, err := reader.Read(raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func writeUint16(buf *bytes.Buffer, value uint16) {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], value)
	buf.Write(tmp[:])
}

func writeUint32(buf *bytes.Buffer, value uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], value)
	buf.Write(tmp[:])
}

func writeUint64(buf *bytes.Buffer, value uint64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], value)
	buf.Write(tmp[:])
}

func readUint16(reader *bytes.Reader) (uint16, error) {
	var tmp [2]byte
	if _, err := reader.Read(tmp[:]); err != nil {
		return 0, errors.New("truncated uint16")
	}
	return binary.BigEndian.Uint16(tmp[:]), nil
}

func readUint32(reader *bytes.Reader) (uint32, error) {
	var tmp [4]byte
	if _, err := reader.Read(tmp[:]); err != nil {
		return 0, errors.New("truncated uint32")
	}
	return binary.BigEndian.Uint32(tmp[:]), nil
}

func readUint64(reader *bytes.Reader) (uint64, error) {
	var tmp [8]byte
	if _, err := reader.Read(tmp[:]); err != nil {
		return 0, errors.New("truncated uint64")
	}
	return binary.BigEndian.Uint64(tmp[:]), nil
}
