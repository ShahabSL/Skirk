package skirk

import (
	"bytes"
	"testing"
)

func TestSealOpenEnvelope(t *testing.T) {
	key, err := DeriveKey("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("hello skirk")
	sealed, err := Seal(key, sid, DirectionUp, 7, plaintext, true)
	if err != nil {
		t.Fatal(err)
	}
	env, opened, err := OpenEnvelope(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("plaintext mismatch: got %q", opened)
	}
	if env.SessionID != sid || env.Direction != DirectionUp || env.Sequence != 7 || env.Flags != FlagFinal {
		t.Fatalf("metadata mismatch: %+v", env)
	}
}

func TestOpenEnvelopeRejectsTamper(t *testing.T) {
	key, err := DeriveKey("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := ParseSessionID("00112233445566778899aabbccddeeff")
	sealed, err := Seal(key, sid, DirectionUp, 1, []byte("payload"), false)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, _, err := OpenEnvelope(key, sealed); err == nil {
		t.Fatal("expected tampered envelope to fail authentication")
	}
}

func TestDeriveStreamKeySeparatesConnectionsAndDirections(t *testing.T) {
	sid, _ := ParseSessionID("00112233445566778899aabbccddeeff")
	upA, err := DeriveStreamKey("test-secret", sid, DirectionUp, "conn-a")
	if err != nil {
		t.Fatal(err)
	}
	upA2, err := DeriveStreamKey("test-secret", sid, DirectionUp, "conn-a")
	if err != nil {
		t.Fatal(err)
	}
	upB, err := DeriveStreamKey("test-secret", sid, DirectionUp, "conn-b")
	if err != nil {
		t.Fatal(err)
	}
	downA, err := DeriveStreamKey("test-secret", sid, DirectionDown, "conn-a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(upA, upA2) {
		t.Fatal("same stream inputs must derive the same key")
	}
	if bytes.Equal(upA, upB) {
		t.Fatal("different connection IDs must derive different stream keys")
	}
	if bytes.Equal(upA, downA) {
		t.Fatal("different directions must derive different stream keys")
	}
}

func TestDeriveMuxLaneKeyV4SeparatesClientsRunsAndDirections(t *testing.T) {
	sid, _ := ParseSessionID("00112233445566778899aabbccddeeff")
	upA, err := DeriveMuxLaneKeyV4("test-secret", sid, DirectionUp, "client-a", "run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	upA2, err := DeriveMuxLaneKeyV4("test-secret", sid, DirectionUp, "client-a", "run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	upB, err := DeriveMuxLaneKeyV4("test-secret", sid, DirectionUp, "client-b", "run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	upRunB, err := DeriveMuxLaneKeyV4("test-secret", sid, DirectionUp, "client-a", "run-b", 0)
	if err != nil {
		t.Fatal(err)
	}
	downA, err := DeriveMuxLaneKeyV4("test-secret", sid, DirectionDown, "client-a", "run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(upA, upA2) {
		t.Fatal("same mux v4 inputs must derive the same key")
	}
	if bytes.Equal(upA, upB) || bytes.Equal(upA, upRunB) || bytes.Equal(upA, downA) {
		t.Fatal("mux v4 keys must differ by client id, run id, and direction")
	}
}
