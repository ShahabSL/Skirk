package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func roundtripFixture() *roundtripProbe {
	b := &roundtripProbe{probe: &probe{}, generation: "test-generation"}
	b.key[0] = 42
	for index := range b.state.requests {
		b.state.requests[index] = bytes.Repeat([]byte{byte(index + 1)}, frameBytes)
		b.state.scheduled[index] = time.Now().Add(-time.Second)
	}
	return b
}

func roundtripAccept(t *testing.T, b *roundtripProbe, up bool, sequence, ack, first, last int, version int64) {
	t.Helper()
	description, err := b.encodeSnapshot(up, sequence, ack, first, last)
	if err != nil {
		t.Fatal(err)
	}
	value, err := b.decodeSnapshot(up, description)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.accept(up, value, version, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRoundtripCumulativeSnapshotsSurviveSkippedVersions(t *testing.T) {
	b := roundtripFixture()
	b.state.up.produced = 10
	roundtripAccept(t, b, true, 1, 0, 1, 10, 2)
	if b.state.up.received != 10 || b.state.down.produced != 10 {
		t.Fatal("server did not produce replies after receiving requests")
	}
	roundtripAccept(t, b, false, 1, 10, 1, 10, 1)
	if b.state.up.acked != 10 || b.state.down.received != 10 || len(b.state.rtts) != 10 {
		t.Fatal("client did not receive and acknowledge first replies")
	}
	b.state.up.produced = frameCount
	roundtripAccept(t, b, true, 2, 10, 11, frameCount, 7)
	if b.state.up.skipped != 4 || b.state.down.acked != 10 || b.state.down.produced != frameCount {
		t.Fatal("skipped revisions lost an unacknowledged request or reply")
	}
	roundtripAccept(t, b, false, 2, frameCount, 11, frameCount, 5)
	if b.state.down.skipped != 3 || b.state.up.acked != frameCount ||
		b.state.down.received != frameCount || len(b.state.rtts) != frameCount {
		t.Fatal("skipped revisions lost a round trip")
	}
	roundtripAccept(t, b, true, 3, frameCount, frameCount+1, frameCount, 8)
	if b.state.down.acked != frameCount {
		t.Fatal("ACK-only update did not release the final reply")
	}
	// A repeated version must not deliver the same reply twice.
	roundtripAccept(t, b, false, 2, frameCount, 11, frameCount, 5)
	if len(b.state.rtts) != frameCount || b.state.down.staleGets != 1 {
		t.Fatal("duplicate snapshot was not idempotent")
	}
}

func TestRoundtripRejectsTamperGapAndOversizedSnapshot(t *testing.T) {
	b := roundtripFixture()
	b.state.up.produced = frameCount
	description, err := b.encodeSnapshot(true, 1, 0, 1, frameCount)
	if err != nil || len(description) > roundtripCap {
		t.Fatalf("20-frame description did not fit: bytes=%d err=%v", len(description), err)
	}
	if _, err := b.decodeSnapshot(false, description); err == nil {
		t.Fatal("accepted a snapshot on the wrong direction")
	}
	if _, err := b.decodeSnapshot(true, description+"x"); err == nil {
		t.Fatal("accepted tampered snapshot")
	}
	wire, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(description, roundtripMarker))
	if err != nil {
		t.Fatal(err)
	}
	wire[0] = 'x'
	if _, err := b.decodeSnapshot(true, roundtripMarker+base64.RawURLEncoding.EncodeToString(wire)); err == nil {
		t.Fatal("accepted changed signed JSON")
	}
	b.state.up.received = 10
	b.state.down.produced = 10
	missing, err := b.encodeSnapshot(true, 2, 0, 12, frameCount)
	if err != nil {
		t.Fatal(err)
	}
	value, err := b.decodeSnapshot(true, missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.accept(true, value, 3, time.Now()); err == nil {
		t.Fatal("accepted a missing request sequence")
	}
	if _, err := b.encodeSnapshot(true, 1, 0, 1, frameCount+1); err == nil {
		t.Fatal("accepted a frame beyond the bounded batch")
	}
}

func TestRoundtripBudgetRaisesCapsWithoutChangingDefault(t *testing.T) {
	p := &probe{roundtrip: true}
	p.gets = 259
	if err := p.reserve("get", true); err != nil {
		t.Fatal(err)
	}
	if err := p.reserve("get", true); err == nil {
		t.Fatal("roundtrip GET cap was not enforced")
	}
	p = &probe{roundtrip: true, dataWrites: 70}
	if err := p.reserve("data", false); err == nil {
		t.Fatal("roundtrip up PATCH cap was not enforced")
	}
	p = &probe{roundtrip: true, ackWrites: 70}
	if err := p.reserve("ack", false); err == nil {
		t.Fatal("roundtrip down PATCH cap was not enforced")
	}
}
