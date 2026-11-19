package skirk

import (
	"bytes"
	"testing"
)

func testMuxDriveV2Offer() muxDriveV2OfferValue {
	return muxDriveV2OfferValue{
		SessionID:      [16]byte{1},
		Generation:     [16]byte{2},
		MaxCipherBytes: muxDriveV2MaxCipher,
		UpID:           "owned-up-id-0123456789",
		DownID:         "owned-down-id-0123456789",
	}
}

func TestMuxDriveV2CapabilityRoundTripAndBinding(t *testing.T) {
	offer := testMuxDriveV2Offer()
	raw, err := encodeMuxDriveV2Offer(offer)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMuxDriveV2Offer(raw)
	if err != nil || decoded != offer {
		t.Fatal("offer changed across wire", decoded, err)
	}
	up, down := decoded.names()
	if up != "skirk-v2q-02000000000000000000000000000000-up" ||
		down != "skirk-v2q-02000000000000000000000000000000-down" {
		t.Fatal("owned generation names changed", up, down)
	}
	accept, err := encodeMuxDriveV2Reply(muxDriveV2Accept, raw)
	if err != nil || checkMuxDriveV2Reply(accept, muxDriveV2Accept, raw) != nil {
		t.Fatal("accept did not bind exact offer", err)
	}
	ready, err := encodeMuxDriveV2Reply(muxDriveV2Ready, raw)
	if err != nil || checkMuxDriveV2Reply(ready, muxDriveV2Ready, raw) != nil ||
		checkMuxDriveV2Reply(ready, muxDriveV2Accept, raw) == nil {
		t.Fatal("ready kind or offer binding lost", err)
	}
	other := bytes.Clone(raw)
	other[len(other)-1] = 'x'
	if checkMuxDriveV2Reply(accept, muxDriveV2Accept, other) == nil {
		t.Fatal("accept authorized another owned file")
	}
	other = bytes.Clone(raw)
	other[6] = 3
	if checkMuxDriveV2Reply(accept, muxDriveV2Accept, other) == nil {
		t.Fatal("accept authorized another session")
	}
	other = bytes.Clone(raw)
	other[22] = 4
	if checkMuxDriveV2Reply(accept, muxDriveV2Accept, other) == nil {
		t.Fatal("accept authorized another generation")
	}
	for _, changed := range [][]byte{
		accept[:len(accept)-1],
		append(bytes.Clone(accept), 0),
		func() []byte { b := bytes.Clone(accept); b[7] ^= 1; return b }(),
	} {
		if checkMuxDriveV2Reply(changed, muxDriveV2Accept, raw) == nil {
			t.Fatal("malformed reply accepted")
		}
	}
}

func TestMuxDriveV2CapabilityRejectsMalformedOffer(t *testing.T) {
	valid, err := encodeMuxDriveV2Offer(testMuxDriveV2Offer())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"empty", func([]byte) []byte { return nil }},
		{"short", func(b []byte) []byte { return b[:41] }},
		{"trailing", func(b []byte) []byte { return append(b, 0) }},
		{"magic", func(b []byte) []byte { b[0] = 'X'; return b }},
		{"version", func(b []byte) []byte { b[4]++; return b }},
		{"kind", func(b []byte) []byte { b[5] = muxDriveV2Accept; return b }},
		{"zero session", func(b []byte) []byte { clear(b[6:22]); return b }},
		{"zero generation", func(b []byte) []byte { clear(b[22:38]); return b }},
		{"oversized cipher", func(b []byte) []byte { b[38], b[39] = 0x41, 0; return b }},
		{"wrong id length", func(b []byte) []byte { b[40]++; return b }},
		{"unsafe id", func(b []byte) []byte { b[42] = '/'; return b }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeMuxDriveV2Offer(tc.edit(bytes.Clone(valid))); err == nil {
				t.Fatal("malformed capability accepted")
			}
		})
	}
	for _, mutate := range []func(*muxDriveV2OfferValue){
		func(o *muxDriveV2OfferValue) { o.UpID = o.DownID },
		func(o *muxDriveV2OfferValue) { o.UpID = "abc" },
		func(o *muxDriveV2OfferValue) { o.UpID = " owned-up-id-0123456789" },
		func(o *muxDriveV2OfferValue) { o.UpID = "owned-up-id-0123456789/" },
		func(o *muxDriveV2OfferValue) { o.MaxCipherBytes = 0 },
		func(o *muxDriveV2OfferValue) { o.MaxCipherBytes = muxDriveV2MaxCipher + 1 },
	} {
		o := testMuxDriveV2Offer()
		mutate(&o)
		if _, err := encodeMuxDriveV2Offer(o); err == nil {
			t.Fatal("invalid typed offer encoded", o)
		}
	}
}

func TestMuxDriveV2CapabilityOlderPeerIgnoresMarker(t *testing.T) {
	offer, err := encodeMuxDriveV2Offer(testMuxDriveV2Offer())
	if err != nil {
		t.Fatal(err)
	}
	frame := muxFrame{Kind: muxFrameMetadataCapability, ClientID: "client-a", RunID: "run-a", StreamID: 1, Payload: offer}
	batch, err := encodeMuxBatch([]muxFrame{
		{Kind: muxFrameOpen, StreamID: 1, Payload: encodeMuxOpenPayload("example.test:443", nil)},
		frame,
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMuxBatch(batch)
	if err != nil || len(frames) != 2 || !bytes.Equal(frames[1].Payload, offer) {
		t.Fatal("existing mux wire rejected a distinct capability", err)
	}
	store := &metadataPriorityTestStore{MemoryStore: NewMemoryStore(), enabled: true}
	legacy := metadataPriorityTestMux(t, "exit", store, true)
	legacy.handleMetadataCapability(frame)
	legacy.handleSlotCapability(t.Context(), frame)
	if len(legacy.metadataCaps) != 0 || len(legacy.slots) != 0 {
		t.Fatal("older mux changed state for unknown capability")
	}
}
