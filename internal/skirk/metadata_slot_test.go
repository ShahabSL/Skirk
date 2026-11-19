package skirk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func metadataSlotTestPair(t *testing.T, limits metadataSlotLimits, generation [16]byte) (*metadataSlotPeer, *metadataSlotPeer, [16]byte) {
	t.Helper()
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	up, err := newMetadataSlotPeer("slot-test-secret", sid, DirectionUp, "client1", "run1", generation, limits)
	if err != nil {
		t.Fatal(err)
	}
	down, err := newMetadataSlotPeer("slot-test-secret", sid, DirectionDown, "client1", "run1", generation, limits)
	if err != nil {
		t.Fatal(err)
	}
	return up, down, sid
}

func metadataSlotTestGeneration() [16]byte {
	return [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
}

func metadataSlotTestName(sid [16]byte, direction byte, muxSeq uint64) string {
	return muxObjectNameWithStreamIDs(sid, direction, "client1", "run1", "1234567890abcdef", 7, []uint64{7}, 0, muxSeq, 1, 1, 1, 24, true)
}

func metadataSlotTestRecord(t *testing.T, peer *metadataSlotPeer, sid [16]byte, direction byte, muxSeq uint64) metadataSlotRecord {
	t.Helper()
	record, err := peer.enqueue(metadataSlotTestName(sid, direction, muxSeq), []byte(fmt.Sprintf("mux-sealed-%d", muxSeq)))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestMetadataSlotSupportsConfiguredDottedClientAndRunIDs(t *testing.T) {
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := newMetadataSlotPeer("slot-test-secret", sid, DirectionUp, "client.one", "run.two", metadataSlotTestGeneration(), metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	name := muxObjectNameWithStreamIDs(sid, DirectionUp, "client.one", "run.two", "1234567890abcdef", 7, []uint64{7}, 0, 1, 1, 1, 1, 24, true)
	if _, err := peer.enqueue(name, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if _, err := newMetadataSlotPeer("slot-test-secret", sid, DirectionUp, " client.one", "run.two", metadataSlotTestGeneration(), metadataSlotLimits{}); err == nil {
		t.Fatal("slot accepted an ID that muxDirPrefix would trim")
	}
}

func TestMetadataSlotVersionSkipAndAckLoss(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	var records []metadataSlotRecord
	var descriptions []string
	for i := uint64(1); i <= 3; i++ {
		records = append(records, metadataSlotTestRecord(t, up, sid, DirectionUp, i))
		patch, err := up.prepare()
		if err != nil {
			t.Fatal(err)
		}
		descriptions = append(descriptions, patch.description)
		if err := up.published(patch); err != nil {
			t.Fatal(err)
		}
	}
	// Drive may expose only the newest description after multiple PATCHes.
	delivered, err := down.receive(descriptions[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 3 {
		t.Fatalf("version skip delivered %d records, want 3", len(delivered))
	}
	for i, record := range delivered {
		if record.SlotSeq != uint64(i+1) || record.MuxName != records[i].MuxName || !bytes.Equal(record.MuxSealed, records[i].MuxSealed) {
			t.Fatalf("record %d changed across slot: %+v", i, record)
		}
		if err := down.markProcessed(record.SlotSeq, record.MuxName); err != nil {
			t.Fatal(err)
		}
	}
	if got := down.acked(); got != 3 {
		t.Fatalf("processed ACK = %d, want 3", got)
	}
	// The first ACK description lands but its observation is lost.
	lostAck, err := down.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := down.published(lostAck); err != nil {
		t.Fatal(err)
	}
	resend, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(resend); err != nil {
		t.Fatal(err)
	}
	delivered, err = down.receive(resend.description)
	if err != nil || len(delivered) != 0 {
		t.Fatalf("ACK loss redelivered records: count=%d err=%v", len(delivered), err)
	}
	ack, err := down.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := down.published(ack); err != nil {
		t.Fatal(err)
	}
	if _, err := up.receive(ack.description); err != nil {
		t.Fatal(err)
	}
	if len(up.outbound.pending) != 0 || up.outbound.acked != 3 {
		t.Fatalf("authenticated ACK did not trim pending: %+v", up.outbound)
	}
	if _, err := down.receive(descriptions[2]); !errors.Is(err, errMetadataSlotReplay) {
		t.Fatalf("old description replay: got %v", err)
	}
}

func TestMetadataSlotAmbiguousPatchAndSameRecordFallback(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	record := metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.ambiguous(patch); err != nil {
		t.Fatal(err)
	}
	if _, err := up.prepare(); !errors.Is(err, errMetadataSlotBusy) {
		t.Fatalf("unresolved PATCH allowed a newer write: %v", err)
	}
	if err := up.resolveAmbiguous(patch, "different description"); !errors.Is(err, errMetadataSlotBusy) {
		t.Fatalf("mismatched readback allowed a newer write: %v", err)
	}
	fallback := up.fallbackAndClose()
	if len(fallback) != 1 || fallback[0].SlotSeq != record.SlotSeq || fallback[0].MuxName != record.MuxName || !bytes.Equal(fallback[0].MuxSealed, record.MuxSealed) {
		t.Fatalf("fallback changed immutable mux identity: %+v", fallback)
	}
	fallback[0].MuxSealed[0] ^= 1
	if bytes.Equal(fallback[0].MuxSealed, record.MuxSealed) {
		t.Fatal("fallback did not return an independent copy")
	}
	if _, err := up.prepare(); !errors.Is(err, errMetadataSlotClosed) {
		t.Fatalf("closed generation accepted a new PATCH: %v", err)
	}
	// If readback proves the exact PATCH landed, the same generation can continue.
	up2, _, sid2 := metadataSlotTestPair(t, metadataSlotLimits{}, [16]byte{2})
	metadataSlotTestRecord(t, up2, sid2, DirectionUp, 2)
	patch2, err := up2.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up2.ambiguous(patch2); err != nil {
		t.Fatal(err)
	}
	if err := up2.resolveAmbiguous(patch2, patch2.description); err != nil {
		t.Fatal(err)
	}
	if _, err := up2.prepare(); err != nil {
		t.Fatalf("matching readback did not unblock writer: %v", err)
	}
	// A processed, authenticated reverse ACK can also settle the ambiguity.
	up3, down3, sid3 := metadataSlotTestPair(t, metadataSlotLimits{}, [16]byte{3})
	metadataSlotTestRecord(t, up3, sid3, DirectionUp, 3)
	patch3, err := up3.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up3.ambiguous(patch3); err != nil {
		t.Fatal(err)
	}
	delivered, err := down3.receive(patch3.description)
	if err != nil || len(delivered) != 1 {
		t.Fatalf("ambiguous PATCH was not visible to receiver: %v, %d", err, len(delivered))
	}
	if err := down3.markProcessed(delivered[0].SlotSeq, delivered[0].MuxName); err != nil {
		t.Fatal(err)
	}
	ack, err := down3.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := down3.published(ack); err != nil {
		t.Fatal(err)
	}
	if _, err := up3.receive(ack.description); err != nil {
		t.Fatal(err)
	}
	if _, err := up3.prepare(); err != nil {
		t.Fatalf("authenticated ACK did not resolve ambiguous PATCH: %v", err)
	}
	_ = down
}

func TestMetadataSlotOverflowIsAtomic(t *testing.T) {
	limits := metadataSlotLimits{DescriptionBytes: 512, Records: 2}
	up, _, sid := metadataSlotTestPair(t, limits, metadataSlotTestGeneration())
	name1 := metadataSlotTestName(sid, DirectionUp, 1)
	first, err := up.enqueue(name1, bytes.Repeat([]byte{1}, 80))
	if err != nil {
		t.Fatal(err)
	}
	name2 := metadataSlotTestName(sid, DirectionUp, 2)
	if _, err := up.enqueue(name2, bytes.Repeat([]byte{2}, 80)); !errors.Is(err, errMetadataSlotOverflow) {
		t.Fatalf("overfull queue was accepted: %v", err)
	}
	if up.outbound.nextSeq != 1 || len(up.outbound.pending) != 1 {
		t.Fatal("overflow mutated queue or consumed sequence")
	}
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.description) > limits.DescriptionBytes {
		t.Fatalf("description length %d exceeds cap", len(patch.description))
	}
	if err := up.published(patch); err != nil {
		t.Fatal(err)
	}
	fallback := up.fallbackAndClose()
	if len(fallback) != 1 || fallback[0].MuxName != first.MuxName || !bytes.Equal(fallback[0].MuxSealed, first.MuxSealed) {
		t.Fatal("overflow fallback changed the queued mux record")
	}
}

func TestMetadataSlotFallbackDeduplicatesEitherArrivalOrder(t *testing.T) {
	for _, fallbackFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("fallbackFirst=%t", fallbackFirst), func(t *testing.T) {
			up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
			record := metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
			patch, err := up.prepare()
			if err != nil {
				t.Fatal(err)
			}
			if err := up.published(patch); err != nil {
				t.Fatal(err)
			}
			if fallbackFirst {
				if err := down.noteFallbackProcessed(record.MuxName, record.MuxSealed); err != nil {
					t.Fatal(err)
				}
			}
			delivered, err := down.receive(patch.description)
			if err != nil {
				t.Fatal(err)
			}
			if fallbackFirst && len(delivered) != 0 {
				t.Fatalf("fallback-first delivered duplicate slot record: %d", len(delivered))
			}
			if !fallbackFirst {
				if len(delivered) != 1 {
					t.Fatalf("slot-first delivered %d records, want 1", len(delivered))
				}
				if err := down.noteFallbackProcessed(record.MuxName, record.MuxSealed); err != nil {
					t.Fatal(err)
				}
			}
			if got := down.acked(); got != 1 {
				t.Fatalf("fallback did not advance contiguous ACK: %d", got)
			}
		})
	}
}

func TestMetadataSlotMalformedTamperAndRestartGeneration(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	valid := patch.description
	bad := []string{"", "%%%", valid[:10], valid + "A"}
	for _, description := range bad {
		if _, err := down.receive(description); err == nil {
			t.Fatalf("accepted malformed description %q", description)
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(valid)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, err := down.receive(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
	// Re-encrypt syntactically invalid plaintext with the known test key to
	// reach the parser after AEAD, including a false record count.
	raw, _ = base64.RawURLEncoding.DecodeString(valid)
	block, err := aes.NewCipher(up.outbound.codec.key[:])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := aead.Open(nil, raw[5:17], raw[17:], raw[:17])
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(plain[33:35], 2)
	copy(raw[5:17], []byte("other nonce!"))
	forged := aead.Seal(raw[:17], raw[5:17], plain, raw[:17])
	if _, err := down.receive(base64.RawURLEncoding.EncodeToString(forged)); err == nil {
		t.Fatal("accepted truncated second record")
	}
	newUp, newDown, _ := metadataSlotTestPair(t, metadataSlotLimits{}, [16]byte{99})
	if _, err := newDown.receive(valid); err == nil {
		t.Fatal("new generation accepted old ciphertext")
	}
	if _, err := newUp.receive(valid); err == nil {
		t.Fatal("wrong direction accepted old ciphertext")
	}
	if _, err := down.receive(valid); err != nil {
		t.Fatalf("malformed attempts changed receiver state: %v", err)
	}
}

func TestMetadataSlotKeyDomainIsSeparateFromMux(t *testing.T) {
	sid, err := ParseSessionID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	codec, err := newMetadataSlotCodec("slot-test-secret", sid, DirectionUp, "client1", "run1", metadataSlotTestGeneration(), metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	muxKey, err := DeriveMuxLaneKeyV4("slot-test-secret", sid, DirectionUp, "client1", "run1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(codec.key[:], muxKey) {
		t.Fatal("fixed metadata slot reused the mux lane AEAD key")
	}
	other, err := newMetadataSlotCodec("slot-test-secret", sid, DirectionUp, "client1", "run1", [16]byte{9}, metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(codec.key[:], other.key[:]) {
		t.Fatal("restart generation reused the old slot AEAD key")
	}
}

func TestMetadataSlotAuthenticatedAckBounds(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(patch); err != nil {
		t.Fatal(err)
	}
	// A valid AEAD message from the opposite side still cannot claim that an
	// unsent record was processed.
	forged, err := down.outbound.codec.seal(metadataSlotSnapshot{
		generation: metadataSlotTestGeneration(), direction: DirectionDown,
		revision: 1, ack: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.receive(forged); err == nil {
		t.Fatal("accepted ACK beyond offered sequence")
	}
	if up.outbound.acked != 0 || len(up.outbound.pending) != 1 {
		t.Fatal("invalid ACK trimmed outbound records")
	}
}

func TestMetadataSlotReplayedSnapshotWithOldACKDoesNotClosePeer(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(patch); err != nil {
		t.Fatal(err)
	}
	seal := func(revision, ack uint64) string {
		description, err := down.outbound.codec.seal(metadataSlotSnapshot{generation: metadataSlotTestGeneration(), direction: DirectionDown, revision: revision, ack: ack})
		if err != nil {
			t.Fatal(err)
		}
		return description
	}
	old := seal(1, 0)
	if _, err := up.receive(old); err != nil {
		t.Fatal(err)
	}
	if _, err := up.receive(seal(2, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := up.receive(old); !errors.Is(err, errMetadataSlotReplay) {
		t.Fatalf("cached old ACK treated as fatal: %v", err)
	}
	if up.outbound.acked != 1 || up.inbound.lastRevision != 2 {
		t.Fatal("replay mutated current state")
	}
	if _, err := up.receive(seal(3, 0)); err == nil || errors.Is(err, errMetadataSlotReplay) {
		t.Fatalf("fresh regressed ACK accepted: %v", err)
	}
	if up.outbound.acked != 1 || up.inbound.lastRevision != 2 {
		t.Fatal("fresh invalid ACK mutated current state")
	}
}

func TestMetadataSlotHigherRevisionCannotRewriteStagedRecord(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	record := metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	first, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(first); err != nil {
		t.Fatal(err)
	}
	if delivered, err := down.receive(first.description); err != nil || len(delivered) != 1 {
		t.Fatalf("initial snapshot: %v, %d records", err, len(delivered))
	}
	changed := record.clone()
	changed.MuxSealed = []byte("different-but-authenticated")
	forged, err := up.outbound.codec.seal(metadataSlotSnapshot{
		generation: metadataSlotTestGeneration(), direction: DirectionUp,
		revision: 2, records: []metadataSlotRecord{changed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.receive(forged); err == nil {
		t.Fatal("higher revision changed a staged mux record")
	}
	if down.inbound.lastRevision != 1 || down.acked() != 0 {
		t.Fatal("identity violation mutated receiver revision or ACK")
	}
	if err := down.markProcessed(record.SlotSeq, record.MuxName); err != nil {
		t.Fatal(err)
	}
	second, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(second); err != nil {
		t.Fatal(err)
	}
	if delivered, err := down.receive(second.description); err != nil || len(delivered) != 0 {
		t.Fatalf("valid retransmission after failed identity check: %v, %d records", err, len(delivered))
	}
}

func TestMetadataSlotFallbackIdentityFailureDoesNotMutateState(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	record := metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(patch); err != nil {
		t.Fatal(err)
	}
	if _, err := down.receive(patch.description); err != nil {
		t.Fatal(err)
	}
	if err := down.noteFallbackProcessed(record.MuxName, []byte("wrong ciphertext")); err == nil {
		t.Fatal("fallback with changed mux bytes accepted")
	}
	if down.acked() != 0 || len(down.inbound.fallbackProcessed) != 0 {
		t.Fatal("failed fallback identity check mutated receiver")
	}
	if err := down.noteFallbackProcessed(record.MuxName, record.MuxSealed); err != nil {
		t.Fatal(err)
	}
	if down.acked() != 1 {
		t.Fatal("correct fallback did not complete staged slot record")
	}
}

func TestMetadataSlotRejectsReusedMuxNameAfterAck(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	record := metadataSlotTestRecord(t, up, sid, DirectionUp, 1)
	first, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(first); err != nil {
		t.Fatal(err)
	}
	if _, err := down.receive(first.description); err != nil {
		t.Fatal(err)
	}
	if err := down.markProcessed(record.SlotSeq, record.MuxName); err != nil {
		t.Fatal(err)
	}
	ack, err := down.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := down.published(ack); err != nil {
		t.Fatal(err)
	}
	if _, err := up.receive(ack.description); err != nil {
		t.Fatal(err)
	}
	if _, err := up.enqueue(record.MuxName, record.MuxSealed); err != nil {
		t.Fatal(err)
	}
	second, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(second); err != nil {
		t.Fatal(err)
	}
	if _, err := down.receive(second.description); err == nil {
		t.Fatal("reused mux identity under a new slot sequence was delivered")
	}
}

func TestMetadataSlotConcurrentEnqueueAndPatch(t *testing.T) {
	up, down, sid := metadataSlotTestPair(t, metadataSlotLimits{}, metadataSlotTestGeneration())
	const count = 20
	var wg sync.WaitGroup
	for i := 1; i <= count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := up.enqueue(metadataSlotTestName(sid, DirectionUp, uint64(i)), []byte{byte(i)}); err != nil {
				t.Errorf("enqueue %d: %v", i, err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			patch, err := up.prepare()
			if err != nil {
				t.Errorf("concurrent prepare: %v", err)
				return
			}
			if err := up.published(patch); err != nil {
				t.Errorf("concurrent publish: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	patch, err := up.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := up.published(patch); err != nil {
		t.Fatal(err)
	}
	delivered, err := down.receive(patch.description)
	if err != nil {
		t.Fatal(err)
	}
	if len(delivered) != count {
		t.Fatalf("latest cumulative snapshot delivered %d records, want %d", len(delivered), count)
	}
	for i, r := range delivered {
		if r.SlotSeq != uint64(i+1) {
			t.Fatalf("record order at %d: seq=%d", i, r.SlotSeq)
		}
	}
}
