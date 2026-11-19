package skirk

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fsHybridStatePair(t *testing.T) (*fsHybridState, *fsHybridState) {
	t.Helper()
	up, down := fsHybridTestCodec(t, DirectionUp), fsHybridTestCodec(t, DirectionDown)
	client, exit := newFSHybridState(up, down), newFSHybridState(down, up)
	for _, pair := range [][2]*fsHybridState{{client, exit}, {exit, client}} {
		wire, _, err := pair[0].prepare()
		if err != nil {
			t.Fatal(err)
		}
		if _, advanced, err := pair[1].receive(wire, time.Now()); err != nil || advanced {
			t.Fatal("initial credit exchange", err)
		}
	}
	return client, exit
}

func fsHybridStateAdd(t *testing.T, s *fsHybridState, muxSeq uint64, stream uint64, kind byte) *fsHybridOutbound {
	t.Helper()
	payload := []byte("public data")
	frame := muxFrame{Kind: kind, StreamID: stream, Seq: 1, Payload: payload}
	if kind == muxFrameOpen {
		frame.Seq = 0
		frame.Payload = encodeMuxOpenPayload("public.test:443", payload)
	}
	record := fsHybridTestRecord(t, s.outgoing, muxSeq, 0, frame)
	r, fresh, err := s.admission(record.Name, record.Sealed, time.Now().Add(time.Second))
	if err != nil || !fresh || !s.enqueue(r) {
		t.Fatal("valid outgoing admission failed", fresh, err)
	}
	return r
}

func TestFSHybridCoalescedLatestSnapshotRetainsAllRecordsUntilProcessing(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	a := fsHybridStateAdd(t, client, 1, 7, muxFrameData)
	lost, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high) // The provider stores it, but its notification is lost.
	b := fsHybridStateAdd(t, client, 2, 8, muxFrameData)
	latest, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high)
	ready, advanced, err := exit.receive(latest, time.Now())
	if err != nil || advanced || len(ready) != 2 {
		t.Fatal("coalesced state lost an unprocessed identity", len(ready), advanced, err)
	}
	if len(client.outQueue) != 2 || client.acked != 0 {
		t.Fatal("publication was confused with processing")
	}
	if _, _, advanced, err := exit.markProcessed(ready[1].info.ID); err != nil || advanced {
		t.Fatal("out-of-order completion advanced cumulative ACK", err)
	}
	ack0, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, advanced, err = client.receive(ack0, time.Now()); err != nil || advanced || len(client.outQueue) != 2 {
		t.Fatal("unprocessed head was ACKed", err)
	}
	if _, _, advanced, err = exit.markProcessed(ready[0].info.ID); err != nil || !advanced || exit.rxProcessed != 2 {
		t.Fatal("contiguous completion did not advance", err)
	}
	ack2, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, advanced, err = client.receive(ack2, time.Now()); err != nil || advanced {
		t.Fatal("ACK-only receive requested ACK-of-ACK", err)
	}
	if len(client.outQueue) != 0 || client.outRetained != 0 || !a.processed || !b.processed {
		t.Fatal("processed payloads were not released")
	}
	if _, _, err = exit.receive(lost, time.Now()); !errors.Is(err, errFSHybridReplay) {
		t.Fatal("older revision was not safely ignored", err)
	}
	if exit.rxProcessed != 2 || len(exit.rxNames) != 2 || exit.rxBytes != 0 {
		t.Fatal("replay rewound ACK or evicted lifetime claims")
	}
}

func TestFSHybridPhysicalFirstOPENNeverReentersAfterClosedTTL(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	open := fsHybridStateAdd(t, client, 1, 99, muxFrameOpen)
	info := ObjectInfo{Name: open.name, ID: "physical-owned-id", Size: int64(len(open.sealed))}
	entry, err := exit.normalizePhysical(info, open.sealed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entry.sequence != 0 {
		t.Fatal("physical publication fabricated a Firestore processing sequence")
	}
	if _, _, advanced, err := exit.markProcessed(entry.info.ID); err != nil || advanced {
		t.Fatal("unseen watch sequence was prematurely ACKed", err)
	}
	wire, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high)
	ready, advanced, err := exit.receive(wire, time.Now().Add(muxClosedStreamTTL+time.Second))
	if err != nil || len(ready) != 0 || !advanced || exit.rxProcessed != 1 {
		t.Fatal("delayed OPEN was redelivered or not receipted", len(ready), advanced, err)
	}
	// A fresh document revision carrying the original immutable OPEN is also
	// a duplicate after the mux's much shorter replay cache has expired.
	again, _, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if ready, advanced, err = exit.receive(again, time.Now().Add(2*muxClosedStreamTTL)); err != nil || len(ready) != 0 || advanced {
		t.Fatal("lifetime tombstone was lost", err)
	}
	if !entry.done || entry.sealed != nil || len(exit.rxNames) != 1 {
		t.Fatal("processed identity was evicted or payload retained")
	}
	changed := bytes.Clone(open.sealed)
	changed[len(changed)-1] ^= 1
	if _, err = exit.normalizePhysical(ObjectInfo{Name: open.name, ID: "other-physical-id"}, changed, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("changed physical ciphertext under a completed name accepted", err)
	}
}

func TestFSHybridReceiveRejectsOmissionAndOffsetGapWithoutPartialACKMutation(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	fsHybridStateAdd(t, client, 1, 7, muxFrameData)
	wire, _, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = exit.receive(wire, time.Now()); err != nil {
		t.Fatal(err)
	}
	beforeRevision, beforeClaims := exit.peerRevision, len(exit.rxNames)
	empty := client.outgoing.snapshot(client.revision+1, 0, 32, fsHybridWireMax, nil)
	bad, err := client.outgoing.seal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = exit.receive(bad, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("unprocessed cumulative identity omitted", err)
	}
	if exit.peerRevision != beforeRevision || len(exit.rxNames) != beforeClaims {
		t.Fatal("rejected snapshot mutated state")
	}
	if _, _, advanced, err := exit.markProcessed(exit.rxSeq[1].info.ID); err != nil || !advanced {
		t.Fatal("fixture head did not complete", err)
	}
	second := fsHybridStateAdd(t, client, 2, 8, muxFrameData)
	snapshot := client.outgoing.snapshot(client.revision+1, 0, 32, fsHybridWireMax, []fsHybridRecord{second.record})
	snapshot.Records[0].ByteEnd++ // Cross-snapshot offset is not an intra-packet check.
	bad, err = client.outgoing.seal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = exit.receive(bad, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("new record skipped the known cumulative byte offset", err)
	}
	if exit.peerRevision != beforeRevision || exit.rxHigh != 1 {
		t.Fatal("invalid suffix partially advanced the receiver")
	}
}

func TestFSHybridClaimCapClosesWithoutEvictionAndPreservesReservedCredit(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	for i := 0; i < fsHybridClaimsMax; i++ {
		client.outNames[fmt.Sprintf("past-%d", i)] = &fsHybridOutbound{processed: true}
	}
	r := fsHybridTestRecord(t, client.outgoing, 1, 0, muxFrame{Kind: muxFrameOpen, StreamID: 7, Payload: encodeMuxOpenPayload("public.test:443", nil)})
	if _, _, err := client.admission(r.Name, r.Sealed, time.Now()); !errors.Is(err, errFSHybridClosed) || len(client.outNames) != fsHybridClaimsMax {
		t.Fatal("claim cap evicted replay protection", err)
	}
	// Leave exactly the already advertised32 unseen identity claims reserved.
	for i := 0; i < fsHybridClaimsMax-fsHybridQueueMax; i++ {
		exit.rxNames[fmt.Sprintf("past-%d", i)] = &fsHybridInbound{done: true}
	}
	credit, _ := exit.credit()
	if credit != fsHybridQueueMax {
		t.Fatal("credit was granted without lifetime capacity", credit)
	}
	if _, err := exit.normalizePhysical(ObjectInfo{Name: r.Name, ID: "physical"}, r.Sealed, time.Now()); !errors.Is(err, errFSHybridClosed) {
		t.Fatal("physical admission stole outstanding Firestore credit", err)
	}
	if len(exit.rxNames) != fsHybridClaimsMax-fsHybridQueueMax {
		t.Fatal("failed admission evicted claims")
	}
}

func TestFSHybridRejectsRenamedCipherAndCrossNamePhysicalIDReuse(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	a := fsHybridStateAdd(t, client, 1, 7, muxFrameOpen)
	original := ObjectInfo{Name: a.name, ID: "physical-owned"}
	entry, err := exit.normalizePhysical(original, a.sealed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(a.name, "public-epoch", "renamed-epoch", 1)
	if renamed == a.name {
		t.Fatal("fixture epoch was not replaced")
	}
	if _, err = exit.normalizePhysical(ObjectInfo{Name: renamed, ID: "new-physical"}, a.sealed, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("cipher replay under another name/ID accepted", err)
	}
	if _, _, err = client.admission(renamed, a.sealed, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("sender republished one ciphertext under another name", err)
	}
	b := fsHybridTestRecord(t, client.outgoing, 2, 0, muxFrame{Kind: muxFrameData, StreamID: 8, Seq: 1, Payload: []byte("different")})
	if _, err = exit.normalizePhysical(ObjectInfo{Name: b.Name, ID: original.ID}, b.Sealed, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("one physical ID claimed two identities", err)
	}
	for _, bad := range []string{strings.Repeat("x", 129), "unsafe/id", ""} {
		if _, err = exit.normalizePhysical(ObjectInfo{Name: a.name, ID: bad}, a.sealed, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
			t.Fatal("unbounded or unsafe ID retained", err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err = exit.normalizePhysical(ObjectInfo{Name: a.name, ID: fmt.Sprintf("alias-%d", i)}, a.sealed, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = exit.normalizePhysical(ObjectInfo{Name: a.name, ID: "fifth-alias"}, a.sealed, time.Now()); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("physical alias bound exceeded", err)
	}
	if len(exit.rxNames) != 1 || len(exit.rxPhysical) != 4 || len(entry.physicalIDs) != 4 || entry.done {
		t.Fatal("rejected conflicts changed processing or identity claims")
	}
}

func TestFSHybridRejectsTwoFreshRenamedOPENAliasesBeforeAnySnapshotMutation(t *testing.T) {
	client, exit := fsHybridStatePair(t)
	a := fsHybridTestRecord(t, client.outgoing, 1, 0, muxFrame{Kind: muxFrameOpen, StreamID: 99, Payload: encodeMuxOpenPayload("public.test:443", []byte("one write"))})
	b := a.clone()
	b.Sequence = 2
	b.Name = strings.Replace(a.Name, "public-epoch", "renamed-epoch", 1)
	b.ByteEnd = a.ByteEnd + b.cost()
	snapshot := client.outgoing.snapshot(client.revision+1, 0, 32, fsHybridWireMax, []fsHybridRecord{a, b})
	// Deliberately construct a valid outer AEAD without using seal's semantic
	// checks: both inner ciphers authenticate, but there is only one OPEN.
	raw, _ := json.Marshal(snapshot)
	nonce := make([]byte, client.outgoing.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(fsHybridMagic), nonce...)
	wire = client.outgoing.aead.Seal(wire, nonce, raw, []byte(fsHybridMagic))
	beforeRevision, beforeCredit := exit.peerRevision, exit.peerCreditSeq
	if _, _, err := exit.receive(wire, time.Now()); !errors.Is(err, errFSHybridIntegrity) {
		t.Fatal("samecipher two fresh names admitted", err)
	}
	if exit.peerRevision != beforeRevision || exit.peerCreditSeq != beforeCredit || exit.rxHigh != 0 || len(exit.rxNames) != 0 || len(exit.rxCipher) != 0 || exit.rxBytes != 0 {
		t.Fatal("invalid snapshot partially published claims, data or credit")
	}
}
