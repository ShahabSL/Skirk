package skirk

import (
	"bytes"
	"context"
	"fmt"
	"sync"
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
				InlineData:      encodeMuxOpenPayload("example.com:443", []byte("GET / HTTP/1.1\r\n\r\n")),
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
				DataObjectName:  "muxv5/session/down/d/client/run/epoch-a/s000000000000000b/l02/0000000000000011.data",
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
	if len(opened.Records) != 2 || opened.Records[1].DataFileID != "drive-file-id" || opened.Records[1].DataObjectName == "" || opened.Records[1].DataOffset != 4096 || opened.Records[1].FrameCount != 16 {
		t.Fatalf("opened records = %+v, want data manifest record", opened.Records)
	}
	target, initial, err := decodeMuxOpenPayload(opened.Records[0].InlineData)
	if err != nil {
		t.Fatal(err)
	}
	if target != "example.com:443" || string(initial) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("inline open = target %q initial %q", target, string(initial))
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
	manifestFromRef := func(ref muxV5DataRecordRef) muxV5ControlRecord {
		return muxV5ControlRecord{
			Type:            muxV5RecordData,
			PriorityClass:   ref.PriorityClass,
			StreamID:        ref.StreamID,
			StreamSeqMin:    ref.StreamSeqMin,
			StreamSeqMax:    ref.StreamSeqMax,
			PlainBytes:      ref.PlainBytes,
			SealedBytes:     ref.SealedBytes,
			DataFileID:      ref.DataFileID,
			DataObjectName:  ref.ObjectName,
			DataOffset:      ref.DataOffset,
			DataLength:      ref.DataLength,
			CreatedUnixNano: time.Date(2026, 5, 14, 1, 0, 2, 0, time.UTC).UnixNano(),
		}
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
			{RecordIndex: 0, PriorityClass: muxV5ClassBulk, StreamID: 22, StreamSeqMin: 10, StreamSeqMax: 12, Plaintext: []byte("first-record")},
			{RecordIndex: 1, PriorityClass: muxV5ClassBulk, StreamID: 22, StreamSeqMin: 13, StreamSeqMax: 15, StreamByteStart: 12, Plaintext: []byte("range-open-me")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(refs))
	}
	ref := refs[1]
	start := int(ref.DataOffset)
	end := start + int(ref.DataLength)
	recordBytes := append([]byte(nil), sealed[start:end]...)
	if _, _, err := openMuxV5DataSlab(key, recordBytes); err == nil {
		t.Fatal("record byte range opened as a full slab")
	}
	manifest := manifestFromRef(ref)
	record, openedRef, err := openMuxV5DataRecordFromManifest(key, manifest, recordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if openedRef != ref {
		t.Fatalf("opened ref = %+v, want %+v", openedRef, ref)
	}
	if string(record.Plaintext) != "range-open-me" {
		t.Fatalf("plaintext = %q, want range-open-me", string(record.Plaintext))
	}
	recordFromRef, err := openMuxV5DataRecordFromRef(key, ref, recordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordFromRef.Plaintext, record.Plaintext) {
		t.Fatalf("ref plaintext = %q, want %q", string(recordFromRef.Plaintext), string(record.Plaintext))
	}

	shiftedRange := sealed[start-1 : end-1]
	truncatedRange := recordBytes[:len(recordBytes)-1]
	otherRef := refs[0]
	otherStart := int(otherRef.DataOffset)
	otherEnd := otherStart + int(otherRef.DataLength)
	otherRecordBytes := sealed[otherStart:otherEnd]
	tampered := append([]byte(nil), recordBytes...)
	tampered[len(tampered)-1] ^= 0x80

	for _, tt := range []struct {
		name     string
		manifest muxV5ControlRecord
		data     []byte
	}{
		{name: "shifted range", manifest: manifest, data: shiftedRange},
		{name: "truncated range", manifest: manifest, data: truncatedRange},
		{name: "other record range", manifest: manifest, data: otherRecordBytes},
		{name: "tampered ciphertext", manifest: manifest, data: tampered},
		{name: "wrong offset", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.DataOffset++
			return wrong
		}(), data: recordBytes},
		{name: "wrong length", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.DataLength--
			return wrong
		}(), data: recordBytes},
		{name: "wrong file id", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.DataFileID = "other-drive-file-id"
			return wrong
		}(), data: recordBytes},
		{name: "wrong object name", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.DataObjectName = "other-object-name"
			return wrong
		}(), data: recordBytes},
		{name: "wrong stream id", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.StreamID++
			return wrong
		}(), data: recordBytes},
		{name: "wrong sealed length", manifest: func() muxV5ControlRecord {
			wrong := manifest
			wrong.SealedBytes--
			return wrong
		}(), data: recordBytes},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := openMuxV5DataRecordFromManifest(key, tt.manifest, tt.data); err == nil {
				t.Fatal("record opened with wrong range or manifest")
			}
		})
	}

	wrongRef := ref
	wrongRef.DataOffset++
	if _, err := openMuxV5DataRecordFromRef(key, wrongRef, recordBytes); err == nil {
		t.Fatal("record opened with wrong ref offset")
	}
	wrongRef = ref
	wrongRef.DataFileID = "other-drive-file-id"
	if _, err := openMuxV5DataRecordFromRef(key, wrongRef, recordBytes); err == nil {
		t.Fatal("record opened with wrong ref file id")
	}
}

func TestSealMuxV5DataSlabRejectsNonSequentialRecordIndexes(t *testing.T) {
	sid := [16]byte{1, 2, 3}
	key, err := DeriveMuxDataKeyV5("secret", sid, DirectionDown, "client-a", "run-a", "epoch-a")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = sealMuxV5DataSlab(key, muxV5DataSlab{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		DataFileID: "drive-file-id",
		ObjectName: "muxv5/session/down/client/run/d/epoch-a/0/0001.data",
		Lane:       1,
		SlabSeq:    5,
		Records: []muxV5DataRecord{
			{RecordIndex: 0, PriorityClass: muxV5ClassBulk, StreamID: 22, StreamSeqMin: 10, StreamSeqMax: 12, Plaintext: []byte("first")},
			{RecordIndex: 0, PriorityClass: muxV5ClassBulk, StreamID: 22, StreamSeqMin: 13, StreamSeqMax: 14, Plaintext: []byte("second")},
		},
	})
	if err == nil {
		t.Fatal("slab with duplicate record indexes sealed successfully")
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

func TestMuxV5ControlPageRejectsTruncatedInlineData(t *testing.T) {
	raw, err := encodeMuxV5ControlPage(muxV5ControlPage{
		Direction:  DirectionDown,
		ClientID:   "client-a",
		RunID:      "run-a",
		Epoch:      "epoch-a",
		ControlSeq: 1,
		Records: []muxV5ControlRecord{
			{Type: muxV5RecordData, PriorityClass: muxV5ClassInteractive, StreamID: 1, InlineData: []byte("hello")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw = raw[:len(raw)-1]
	if _, err := decodeMuxV5ControlPage(raw); err == nil {
		t.Fatal("control page with truncated inline data decoded successfully")
	}
}

func TestMuxV5ControlObjectNameRoundTrip(t *testing.T) {
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	name := muxV5ControlObjectName(sid, DirectionDown, "client-a", "run-a", "epoch-a", 0x1234, 2, 9, 3, 4, 6, 4096, true)
	wantPrefix := "muxv5/aabbcc00000000000000000000000000/down/c/client-a/run-a/epoch-a/p0/s0000000000001234/l02/"
	if len(name) <= len(wantPrefix) || name[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("name = %q, want prefix %q", name, wantPrefix)
	}
	meta, ok := parseMuxV5ControlObjectInfo(ObjectInfo{Name: name, ID: "control-file-id", Size: 512})
	if !ok {
		t.Fatalf("parseMuxV5ControlObjectInfo(%q) failed", name)
	}
	if meta.ID != "control-file-id" || meta.ClientID != "client-a" || meta.RunID != "run-a" || meta.Epoch != "epoch-a" || meta.StreamID != 0x1234 || meta.Lane != 2 || meta.Seq != 9 || !meta.Priority {
		t.Fatalf("meta = %+v, want v5 control identity", meta)
	}
	if !meta.FrameRangeKnown || meta.FrameMinSeq != 4 || meta.FrameMaxSeq != 6 || meta.PlainBytes != 4096 {
		t.Fatalf("meta range/bytes = %+v, want parsed frame range and bytes", meta)
	}
}

func TestMuxV5BulkObjectNameRoundTrip(t *testing.T) {
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	name := muxV5BulkObjectName(sid, DirectionDown, "client-a", "run-a", "epoch-a", 0x1234, 2, 9, 3, 4, 6, 4096, false)
	wantPrefix := "muxv5/aabbcc00000000000000000000000000/down/b/client-a/run-a/epoch-a/p1/s0000000000001234/l02/"
	if len(name) <= len(wantPrefix) || name[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("name = %q, want prefix %q", name, wantPrefix)
	}
	meta, ok := parseMuxV5BulkObjectInfo(ObjectInfo{Name: name, ID: "bulk-file-id", Size: 512})
	if !ok {
		t.Fatalf("parseMuxV5BulkObjectInfo(%q) failed", name)
	}
	if meta.ID != "bulk-file-id" || meta.ClientID != "client-a" || meta.RunID != "run-a" || meta.Epoch != "epoch-a" || meta.StreamID != 0x1234 || meta.Lane != 2 || meta.Seq != 9 || meta.Priority {
		t.Fatalf("meta = %+v, want v5 bulk identity", meta)
	}
	if !meta.FrameRangeKnown || meta.FrameMinSeq != 4 || meta.FrameMaxSeq != 6 || meta.PlainBytes != 4096 {
		t.Fatalf("meta range/bytes = %+v, want parsed frame range and bytes", meta)
	}
}

func TestMuxV5ControlPrefixExcludesDataAndBulkPlanes(t *testing.T) {
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	controlPrefix := muxV5DirPrefix(sid, DirectionUp, muxV5PlaneControl, "", "")
	dataName := muxV5DataObjectName(sid, DirectionUp, "client-a", "run-a", "epoch-a", 1, 0, 1)
	if len(dataName) >= len(controlPrefix) && dataName[:len(controlPrefix)] == controlPrefix {
		t.Fatalf("data name %q matched control prefix %q", dataName, controlPrefix)
	}
	bulkName := muxV5BulkObjectName(sid, DirectionUp, "client-a", "run-a", "epoch-a", 1, 0, 1, 1, 1, 1, 4096, false)
	if len(bulkName) >= len(controlPrefix) && bulkName[:len(controlPrefix)] == controlPrefix {
		t.Fatalf("bulk name %q matched control prefix %q", bulkName, controlPrefix)
	}
}

func TestMuxV5IDBatchSizeIsConservative(t *testing.T) {
	tests := []struct {
		name              string
		uploadConcurrency int
		want              int
	}{
		{name: "minimum", uploadConcurrency: 1, want: 8},
		{name: "typical", uploadConcurrency: 16, want: 32},
		{name: "maximum", uploadConcurrency: 64, want: 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := &driveMux{t: &Tunnel{UploadConcurrency: tt.uploadConcurrency}}
			if got := mux.v5IDBatchSize(); got != tt.want {
				t.Fatalf("v5IDBatchSize() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMuxV5ObjectIDReservationSingleflightsEmptyPool(t *testing.T) {
	store := newMuxV5IDReserveTestStore()
	mux := &driveMux{t: &Tunnel{Data: store, UploadConcurrency: 8}}

	const reservations = 12
	start := make(chan struct{})
	ids := make(chan string, reservations)
	errs := make(chan error, reservations)
	var wg sync.WaitGroup
	for i := 0; i < reservations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := mux.reserveMuxV5ObjectID(context.Background())
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}

	close(start)
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		close(store.release)
		wg.Wait()
		t.Fatal("GenerateObjectIDs was not called")
	}
	extraBeforeRelease := false
	select {
	case <-store.entered:
		extraBeforeRelease = true
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	if extraBeforeRelease {
		t.Fatal("more than one GenerateObjectIDs call entered before the first reservation batch completed")
	}
	if got := store.callCount(); got != 1 {
		t.Fatalf("GenerateObjectIDs calls = %d, want 1", got)
	}
	if got := store.maxConcurrentCalls(); got != 1 {
		t.Fatalf("concurrent GenerateObjectIDs calls = %d, want 1", got)
	}
	if got, want := store.firstCount(), mux.v5IDBatchSize(); got != want {
		t.Fatalf("GenerateObjectIDs count = %d, want %d", got, want)
	}
	seen := map[string]struct{}{}
	for id := range ids {
		if id == "" {
			t.Fatal("empty generated id returned")
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate generated id returned: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != reservations {
		t.Fatalf("reserved ids = %d, want %d", len(seen), reservations)
	}
}

func TestMuxV5BBulkPrefixPollSkipsAfterControlWorkWithoutStarving(t *testing.T) {
	sid := [16]byte{0xaa, 0xbb, 0xcc}
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	store := &muxV5PollPressureStore{
		MemoryStore: NewMemoryStore(),
		sid:         sid,
		now:         now,
	}
	mux := &driveMux{
		t:                &Tunnel{Data: store, SessionID: sid, ClientID: "client-a", RunID: "run-a"},
		role:             "client",
		recvDir:          DirectionDown,
		transport:        "muxv5b",
		startedAt:        now.Add(-time.Minute),
		seen:             map[string]struct{}{},
		queued:           map[string]struct{}{},
		recvUrgent:       make(chan muxObjectMeta, muxV5BulkPollAfterControl+1),
		recvNormalReady:  make(chan muxStreamKey, 1),
		recvNormalFlows:  map[muxStreamKey][]muxObjectMeta{},
		recvNormalActive: map[muxStreamKey]int{},
		recvNormalSent:   map[muxStreamKey]bool{},
	}

	for i := 0; i < muxV5BulkPollAfterControl; i++ {
		if !mux.pollMuxV5Objects(context.Background()) {
			t.Fatalf("poll %d returned no work", i+1)
		}
		_, bulkCalls := store.counts()
		if i < muxV5BulkPollAfterControl-1 && bulkCalls != 0 {
			t.Fatalf("bulk prefix was polled after %d control polls, want defer until poll %d", i+1, muxV5BulkPollAfterControl)
		}
	}
	controlCalls, bulkCalls := store.counts()
	if controlCalls != muxV5BulkPollAfterControl {
		t.Fatalf("control prefix calls = %d, want %d", controlCalls, muxV5BulkPollAfterControl)
	}
	if bulkCalls != 1 {
		t.Fatalf("bulk prefix calls = %d, want 1 after bounded deferral", bulkCalls)
	}
}

type muxV5IDReserveTestStore struct {
	*MemoryStore

	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
	counts    []int
	next      int
	entered   chan struct{}
	release   chan struct{}
}

func newMuxV5IDReserveTestStore() *muxV5IDReserveTestStore {
	return &muxV5IDReserveTestStore{
		MemoryStore: NewMemoryStore(),
		entered:     make(chan struct{}, 32),
		release:     make(chan struct{}),
	}
}

func (s *muxV5IDReserveTestStore) GenerateObjectIDs(ctx context.Context, count int) ([]string, error) {
	s.mu.Lock()
	s.calls++
	s.inFlight++
	if s.inFlight > s.maxFlight {
		s.maxFlight = s.inFlight
	}
	s.counts = append(s.counts, count)
	s.mu.Unlock()

	s.entered <- struct{}{}
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		s.next++
		ids = append(ids, fmt.Sprintf("reserved-id-%03d", s.next))
	}
	return ids, nil
}

func (s *muxV5IDReserveTestStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *muxV5IDReserveTestStore) maxConcurrentCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxFlight
}

func (s *muxV5IDReserveTestStore) firstCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counts) == 0 {
		return 0
	}
	return s.counts[0]
}

type muxV5PollPressureStore struct {
	*MemoryStore

	mu           sync.Mutex
	sid          [16]byte
	now          time.Time
	controlCalls int
	bulkCalls    int
}

func (s *muxV5PollPressureStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	controlPrefix := muxV5DirPrefix(s.sid, DirectionDown, muxV5PlaneControl, "client-a", "run-a")
	bulkPrefix := muxV5DirPrefix(s.sid, DirectionDown, muxV5PlaneBulk, "client-a", "run-a")
	switch prefix {
	case controlPrefix:
		s.controlCalls++
		seq := uint64(s.controlCalls)
		name := muxV5ControlObjectName(s.sid, DirectionDown, "client-a", "run-a", "epoch-a", 0x1234, 0, seq, 1, seq, seq, 256, true)
		return []ObjectInfo{{Name: name, ID: fmt.Sprintf("control-id-%d", seq), Updated: s.now.Format(time.RFC3339Nano)}}, nil
	case bulkPrefix:
		s.bulkCalls++
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *muxV5PollPressureStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlCalls, s.bulkCalls
}
