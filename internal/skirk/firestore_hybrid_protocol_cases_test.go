package skirk

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fsHybridTestCodec(t *testing.T, direction byte) *fsHybridCodec {
	t.Helper()
	codec, err := newFSHybridCodec("public-firestore-fixture", [16]byte{1}, [16]byte{2}, direction, "client", "run")
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func fsHybridTestRecord(t *testing.T, codec *fsHybridCodec, sequence uint64, priorBytes uint64, frames ...muxFrame) fsHybridRecord {
	t.Helper()
	raw, err := encodeMuxBatch(frames)
	if err != nil || len(frames) == 0 {
		t.Fatal("invalid public frame fixture", err)
	}
	key, err := DeriveMuxLaneKeyV4(codec.secret, codec.sid, codec.direction, codec.clientID, codec.runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, codec.sid, codec.direction, sequence, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	first, last := muxBatchFrameSeqRange(frames)
	ids := muxBatchStreamIDs(frames)
	if len(ids) != 1 {
		first, last = 0, 0
	}
	name := muxObjectNameWithStreamIDs(codec.sid, codec.direction, codec.clientID, codec.runID, "public-epoch", frames[0].StreamID, ids, 0, sequence, len(frames), first, last, len(raw), true)
	r := fsHybridRecord{Sequence: sequence, Name: name, Sealed: sealed}
	r.ByteEnd = priorBytes + r.cost()
	return r
}

func TestFSHybridCodecIncludesOPENAndRejectsInnerOuterIdentityFailures(t *testing.T) {
	codec := fsHybridTestCodec(t, DirectionUp)
	open := fsHybridTestRecord(t, codec, 1, 0, muxFrame{Kind: muxFrameOpen, StreamID: 7, Payload: encodeMuxOpenPayload("public.test:443", []byte("hello"))})
	snapshot := codec.snapshot(1, 0, fsHybridQueueMax, fsHybridCacheMax, []fsHybridRecord{open})
	wire, err := codec.seal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.open(wire)
	if err != nil || len(got.Records) != 1 || !bytes.Equal(got.Records[0].Sealed, open.Sealed) {
		t.Fatal("actual OPEN was not preserved", err)
	}
	for _, change := range []func([]byte) []byte{
		func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		func(b []byte) []byte { return b[:8] },
		func(b []byte) []byte { return append(b, make([]byte, fsHybridWireMax)...) },
	} {
		if _, err := codec.open(change(bytes.Clone(wire))); err == nil {
			t.Fatal("invalid outer envelope accepted")
		}
	}
	wrongDirection := fsHybridTestCodec(t, DirectionDown)
	if _, err := wrongDirection.open(wire); err == nil {
		t.Fatal("cross-direction ciphertext accepted")
	}
	wrongEpoch, _ := newFSHybridCodec(codec.secret, codec.sid, [16]byte{3}, DirectionUp, "client", "run")
	if _, err := wrongEpoch.open(wire); err == nil {
		t.Fatal("prior process epoch accepted")
	}
	bad := open.clone()
	bad.Sealed[len(bad.Sealed)-1] ^= 1
	if _, err := codec.seal(codec.snapshot(2, 0, 32, fsHybridCacheMax, []fsHybridRecord{bad})); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("inner corruption accepted", err)
	}
	bad = open.clone()
	bad.Name = strings.Replace(bad.Name, "/client/run/", "/other/run/", 1)
	if _, err := codec.seal(codec.snapshot(2, 0, 32, fsHybridCacheMax, []fsHybridRecord{bad})); err == nil {
		t.Fatal("cross-client name accepted")
	}
}

func TestFSHybridCodecRequiresContiguousOffsetsAndCanonicalAuthenticatedJSON(t *testing.T) {
	codec := fsHybridTestCodec(t, DirectionUp)
	a := fsHybridTestRecord(t, codec, 1, 0, muxFrame{Kind: muxFrameData, StreamID: 7, Seq: 1, Payload: []byte("a")})
	b := fsHybridTestRecord(t, codec, 2, a.ByteEnd, muxFrame{Kind: muxFrameData, StreamID: 8, Seq: 1, Payload: []byte("b")})
	s := codec.snapshot(1, 0, 32, fsHybridCacheMax, []fsHybridRecord{a, b})
	if _, err := codec.seal(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fsHybridSnapshot){
		func(s *fsHybridSnapshot) { s.Records[1].Sequence++ },
		func(s *fsHybridSnapshot) { s.Records[1].ByteEnd++ },
		func(s *fsHybridSnapshot) { s.Records[1].Name = s.Records[0].Name },
		func(s *fsHybridSnapshot) { s.CreditSeq = fsHybridClaimsMax + 1 },
	} {
		candidate := s
		candidate.Records = append([]fsHybridRecord(nil), s.Records...)
		mutate(&candidate)
		if _, err := codec.seal(candidate); err == nil {
			t.Fatal("malformed bounded queue accepted")
		}
	}
	raw, _ := json.Marshal(s)
	// Authenticated duplicate fields must not acquire last-value-wins semantics.
	raw = append([]byte(`{"revision":99,`), raw[1:]...)
	nonce := bytes.Repeat([]byte{3}, codec.aead.NonceSize())
	wire := append([]byte(fsHybridMagic), nonce...)
	wire = codec.aead.Seal(wire, nonce, raw, []byte(fsHybridMagic))
	if _, err := codec.open(wire); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("duplicate authenticated JSON field accepted", err)
	}
}
