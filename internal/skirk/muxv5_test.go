package skirk

import (
	"bytes"
	"testing"
	"time"
)

func TestMuxV5ControlPageSealOpenRoundTrip(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	key, err := DeriveMuxControlKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	page := muxV5ControlPage{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		ControlSeq: 17,
		Records: []muxV5ControlRecord{
			{
				Type:            muxV5RecordOpen,
				PriorityClass:   muxV5ClassControl,
				StreamID:        11,
				StreamSeqMin:    0,
				StreamSeqMax:    0,
				CreatedUnixNano: time.Date(2026, 5, 14, 1, 0, 0, 0, time.UTC).UnixNano(),
			},
			{
				Type:            muxV5RecordData,
				PriorityClass:   muxV5ClassBulk,
				StreamID:        11,
				StreamSeqMin:    1,
				StreamSeqMax:    16,
				PlainBytes:      1024,
				SealedBytes:     1100,
				DataFileID:      "drive-file-id",
				DataOffset:      4096,
				DataLength:      1100,
				FrameCount:      16,
				CreditBytes:     8192,
				AckByteOffset:   2048,
				AckStreamSeq:    8,
				CreatedUnixNano: time.Date(2026, 5, 14, 1, 0, 1, 0, time.UTC).UnixNano(),
			},
		},
	}

	sealed, err := sealMuxV5ControlPage(key, sid, page)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openMuxV5ControlPage(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ControlSeq != page.ControlSeq || opened.Direction != page.Direction || opened.ClientID != page.ClientID || opened.RunID != page.RunID || opened.Epoch != page.Epoch {
		t.Fatalf("opened page = %+v, want header from %+v", opened, page)
	}
	if len(opened.Records) != 2 || opened.Records[1].DataFileID != "drive-file-id" || opened.Records[1].DataOffset != 4096 || opened.Records[1].FrameCount != 16 {
		t.Fatalf("opened records = %+v, want data manifest record", opened.Records)
	}
}

func TestMuxV5ControlPageRejectsTamper(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	key, err := DeriveMuxControlKeyV5("secret", sid, DirectionUp, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealMuxV5ControlPage(key, sid, muxV5ControlPage{
		Direction:  DirectionUp,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		ControlSeq: 1,
		Records:    []muxV5ControlRecord{{Type: muxV5RecordACK, PriorityClass: muxV5ClassControl, StreamID: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0x80
	if _, err := openMuxV5ControlPage(key, sealed); err == nil {
		t.Fatal("tampered control page opened successfully")
	}
}

func TestMuxV5DataSlabSealOpenRoundTrip(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	key, err := DeriveMuxDataKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	slab := muxV5DataSlab{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		DataFileID: "drive-file-id",
		ObjectName: "muxv5/session/down/client/run/d/epoch-a/0/0001.data",
		Lane:       2,
		SlabSeq:    77,
		Records: []muxV5DataRecord{
			{
				RecordIndex:     0,
				PriorityClass:   muxV5ClassBurst,
				StreamID:        9,
				StreamSeqMin:    1,
				StreamSeqMax:    2,
				StreamByteStart: 0,
				Plaintext:       []byte("first record"),
			},
			{
				RecordIndex:     1,
				PriorityClass:   muxV5ClassBulk,
				StreamID:        9,
				StreamSeqMin:    3,
				StreamSeqMax:    4,
				StreamByteStart: 12,
				Plaintext:       []byte("second record"),
			},
		},
	}

	sealed, refs, err := sealMuxV5DataSlab(key, slab)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(refs))
	}
	if refs[0].DataOffset == refs[1].DataOffset || refs[1].DataOffset <= refs[0].DataOffset {
		t.Fatalf("refs offsets = %+v, want increasing offsets", refs)
	}
	opened, openedRefs, err := openMuxV5DataSlab(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Direction != slab.Direction || opened.ClientID != slab.ClientID || opened.RunID != slab.RunID || opened.Epoch != slab.Epoch || opened.Lane != slab.Lane || opened.SlabSeq != slab.SlabSeq {
		t.Fatalf("opened slab = %+v, want slab identity %+v", opened, slab)
	}
	if len(opened.Records) != 2 || string(opened.Records[0].Plaintext) != "first record" || string(opened.Records[1].Plaintext) != "second record" {
		t.Fatalf("opened records = %+v, want plaintext records", opened.Records)
	}
	if len(openedRefs) != 2 || openedRefs[1] != refs[1] {
		t.Fatalf("opened refs = %+v, want %+v", openedRefs, refs)
	}
}

func TestMuxV5DataRecordRangeOpenAuthenticatesManifest(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	key, err := DeriveMuxDataKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	sealed, refs, err := sealMuxV5DataSlab(key, muxV5DataSlab{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		DataFileID: "drive-file-id",
		ObjectName: "muxv5/session/down/client/run/d/epoch-a/0/0001.data",
		Lane:       1,
		SlabSeq:    5,
		Records: []muxV5DataRecord{
			{RecordIndex: 0, PriorityClass: muxV5ClassBulk, StreamID: 22, StreamSeqMin: 10, StreamSeqMax: 12, Plaintext: []byte("range-open-me")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := refs[0]
	recordBytes := sealed[ref.DataOffset : ref.DataOffset+ref.DataLength]
	gcm, err := muxV5GCM(key)
	if err != nil {
		t.Fatal(err)
	}
	record, err := openMuxV5DataRecord(gcm, ref, recordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(record.Plaintext) != "range-open-me" {
		t.Fatalf("plaintext = %q, want range-open-me", string(record.Plaintext))
	}
	wrongRef := ref
	wrongRef.DataOffset++
	if _, err := openMuxV5DataRecord(gcm, wrongRef, recordBytes); err == nil {
		t.Fatal("record opened with wrong manifest offset")
	}
	wrongRef = ref
	wrongRef.DataFileID = "other-drive-file-id"
	if _, err := openMuxV5DataRecord(gcm, wrongRef, recordBytes); err == nil {
		t.Fatal("record opened with wrong manifest file id")
	}
	wrongRef = ref
	wrongRef.ObjectName = "other-object-name"
	if _, err := openMuxV5DataRecord(gcm, wrongRef, recordBytes); err == nil {
		t.Fatal("record opened with wrong manifest object name")
	}
	tampered := append([]byte(nil), recordBytes...)
	tampered[len(tampered)-1] ^= 0x80
	if _, err := openMuxV5DataRecord(gcm, ref, tampered); err == nil {
		t.Fatal("tampered record opened successfully")
	}
}

func TestDeriveMuxV5KeysSeparateEpochAndPurpose(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	controlA, err := DeriveMuxControlKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	controlA2, err := DeriveMuxControlKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	dataA, err := DeriveMuxDataKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	controlB, err := DeriveMuxControlKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-b")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(controlA, controlA2) {
		t.Fatal("same v5 key inputs produced different keys")
	}
	if bytes.Equal(controlA, dataA) {
		t.Fatal("v5 control and data keys should differ")
	}
	if bytes.Equal(controlA, controlB) {
		t.Fatal("v5 keys should differ across epochs")
	}
}

func TestMuxV5ControlPageRejectsTrailingBytes(t *testing.T) {
	raw, err := encodeMuxV5ControlPage(muxV5ControlPage{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		ControlSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 0)
	if _, err := decodeMuxV5ControlPage(raw); err == nil {
		t.Fatal("control page with trailing bytes decoded successfully")
	}
}
