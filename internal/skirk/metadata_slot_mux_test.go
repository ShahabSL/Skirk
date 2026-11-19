package skirk

import (
	"bytes"
	"testing"
)

func TestMuxSlotCapabilityRoundTripAndLegacyIgnore(t *testing.T) {
	var generation [16]byte
	generation[0] = 7
	offer := muxSlotCapability{kind: muxSlotCapabilityOffer, generation: generation, limit: 8192, upID: "up-id", downID: "down-id"}
	encoded, err := encodeMuxSlotCapability(offer)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == muxMetadataCapabilityPayloadBytes {
		t.Fatal("extended offer could be misread as a legacy capability")
	}
	decoded, err := decodeMuxSlotCapability(encoded)
	if err != nil || decoded != offer {
		t.Fatalf("offer decode = %#v, %v", decoded, err)
	}
	open := muxFrame{Kind: muxFrameOpen, StreamID: 3, Payload: encodeMuxOpenPayload("example.org:443", []byte("hello"))}
	raw, err := encodeMuxBatch([]muxFrame{open, {Kind: muxFrameMetadataCapability, StreamID: 3, Payload: encoded}})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMuxBatch(raw)
	if err != nil || len(frames) != 2 || !bytes.Equal(frames[0].Payload, open.Payload) {
		t.Fatalf("legacy mux OPEN lost: frames=%d err=%v", len(frames), err)
	}
	accept := muxSlotCapability{kind: muxSlotCapabilityAccept, generation: generation, limit: 4096}
	encoded, err = encodeMuxSlotCapability(accept)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decodeMuxSlotCapability(encoded)
	if err != nil || decoded != accept {
		t.Fatalf("accept decode = %#v, %v", decoded, err)
	}
}

func TestMuxSlotCapabilityRejectsMalformed(t *testing.T) {
	var generation [16]byte
	generation[0] = 1
	valid, err := encodeMuxSlotCapability(muxSlotCapability{kind: muxSlotCapabilityOffer, generation: generation, limit: 8192, upID: "up", downID: "down"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, valid[:17], append(bytes.Clone(valid), 0), append(bytes.Clone(valid[:26]), []byte("up")...)} {
		if _, err := decodeMuxSlotCapability(bad); err == nil {
			t.Fatalf("accepted malformed capability of length %d", len(bad))
		}
	}
	if _, err := encodeMuxSlotCapability(muxSlotCapability{kind: muxSlotCapabilityOffer, generation: generation, limit: 8192, upID: "same", downID: "same"}); err == nil {
		t.Fatal("accepted reused slot ID")
	}
}
