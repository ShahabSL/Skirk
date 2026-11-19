package skirk

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"
)

func driveV2WindowPair(t *testing.T, selective bool) (*fsHybridState, *fsHybridState) {
	t.Helper()
	up, down := fsHybridTestCodec(t, DirectionUp), fsHybridTestCodec(t, DirectionDown)
	up.selective, down.selective = selective, selective
	up.wireMax, down.wireMax = driveV2PriorityCipherMax, driveV2PriorityCipherMax
	client, exit := newFSHybridState(up, down), newFSHybridState(down, up)
	client.outPerStreamMax, exit.outPerStreamMax = 8, 8
	for _, pair := range [][2]*fsHybridState{{client, exit}, {exit, client}} {
		wire, _, err := pair[0].prepare()
		if err != nil {
			t.Fatal(err)
		}
		if _, advanced, err := pair[1].receive(wire, time.Now()); err != nil || advanced {
			t.Fatal("initial selective credit exchange", err)
		}
	}
	return client, exit
}

func driveV2SelectivePair(t *testing.T) (*fsHybridState, *fsHybridState) {
	return driveV2WindowPair(t, true)
}

func driveV2SelectiveFill(t *testing.T, client *fsHybridState, count uint64) {
	t.Helper()
	for i := uint64(1); i <= count; i++ {
		stream := uint64(100 + (i-1)/8)
		if !client.enqueue(driveV2WindowClaim(t, client, i, stream, []byte{byte(i)})) {
			t.Fatal("bounded selective queue rejected its admitted claim", i)
		}
	}
}

func TestDriveV2SelectiveReceiptReleasesOtherStreamsWithoutOldestACK(t *testing.T) {
	client, exit := driveV2SelectivePair(t)
	driveV2SelectiveFill(t, client, fsHybridQueueMax)
	if client.enqueue(driveV2WindowClaim(t, client, 33, 104, []byte("fifth"))) {
		t.Fatal("fixture did not saturate the initial queue")
	}
	wire, high, err := client.prepare()
	if err != nil || high != 32 || len(wire) > driveV2PriorityCipherMax {
		t.Fatal("initial selective snapshot failed", high, len(wire), err)
	}
	client.published(high)
	ready, advanced, err := exit.receive(wire, time.Now())
	if err != nil || advanced || len(ready) != 32 {
		t.Fatal("initial selective snapshot lost claims", len(ready), advanced, err)
	}
	for _, entry := range ready[1:] {
		if _, _, changed, err := exit.markProcessed(entry.info.ID); err != nil || !changed {
			t.Fatal("selective completion did not mark a receipt", entry.sequence, err)
		}
	}
	if exit.rxProcessed != 0 {
		t.Fatal("out-of-order processing advanced cumulative ACK")
	}
	ack, _, err := exit.prepare()
	if err != nil || len(ack) > driveV2PriorityCipherMax {
		t.Fatal("selective receipt exceeded wire cap", len(ack), err)
	}
	if _, changed, err := client.receive(ack, time.Now()); err != nil || changed {
		t.Fatal("selective reply failed", changed, err)
	}
	if len(client.outQueue) != 1 || client.outQueue[0].record.Sequence != 1 || client.outByStream[100] != 1 || client.outByStream[101] != 0 || client.outByStream[102] != 0 || client.outByStream[103] != 0 {
		t.Fatal("processed streams still occupy inline slots", len(client.outQueue), client.outByStream)
	}
	if client.peerCreditSeq != 63 {
		t.Fatal("receiver did not replenish processed occupancy", client.peerCreditSeq)
	}
	for i := uint64(33); i <= 63; i++ {
		stream := uint64(104 + (i-33)/8)
		if !client.enqueue(driveV2WindowClaim(t, client, i, stream, []byte{byte(i)})) {
			t.Fatal("new stream blocked behind an already processed claim", i)
		}
	}
	if len(client.outQueue) != 32 || client.outQueue[0].record.Sequence != 1 || client.outQueue[1].record.Sequence != 33 {
		t.Fatal("selective queue did not retain only live claims")
	}
	latest, high, err := client.prepare()
	if err != nil || high != 63 || len(latest) > driveV2PriorityCipherMax {
		t.Fatal("sparse current snapshot failed", high, len(latest), err)
	}
	client.published(high)
	ready, advanced, err = exit.receive(latest, time.Now())
	if err != nil || advanced || len(ready) != 32 || exit.rxHigh != 63 {
		t.Fatal("receiver rejected authenticated sparse snapshot", len(ready), advanced, exit.rxHigh, err)
	}
	if _, _, advanced, err = exit.markProcessed(ready[0].info.ID); err != nil || !advanced || exit.rxProcessed != 32 {
		t.Fatal("stalled head did not release prior contiguous identities", exit.rxProcessed, err)
	}
	finalACK, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.receive(finalACK, time.Now()); err != nil || len(client.outQueue) != 31 {
		t.Fatal("head receipt did not leave newer claims intact", len(client.outQueue), err)
	}
	before := len(client.outQueue)
	if _, _, err := client.receive(ack, time.Now()); !errors.Is(err, errFSHybridReplay) || len(client.outQueue) != before {
		t.Fatal("old selective reply rewound sender state", err)
	}
}

func TestDriveV2SelectiveReceiptSurvivesLostReplyAndRejectsOmission(t *testing.T) {
	client, exit := driveV2SelectivePair(t)
	driveV2SelectiveFill(t, client, 2)
	wire, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high)
	ready, _, err := exit.receive(wire, time.Now())
	if err != nil || len(ready) != 2 {
		t.Fatal(err)
	}
	if _, _, _, err := exit.markProcessed(ready[1].info.ID); err != nil {
		t.Fatal(err)
	}
	lost, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	// The peer stored a newer cumulative document; the sender never saw its
	// selective receipt and must continue advertising both identities.
	retry, _, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err = exit.receive(retry, time.Now())
	if err != nil || len(ready) != 1 || ready[0].sequence != 1 {
		t.Fatal("lost reply duplicated a processed claim", len(ready), err)
	}
	ack, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.receive(ack, time.Now()); err != nil || len(client.outQueue) != 1 || client.outQueue[0].record.Sequence != 1 {
		t.Fatal("new receipt did not recover after lost response", err)
	}
	if _, _, err := client.receive(lost, time.Now()); !errors.Is(err, errFSHybridReplay) {
		t.Fatal("reordered old receipt was not rejected", err)
	}
	bad := client.snapshot(client.revision+1, client.rxProcessed, client.grantedSeq, client.grantedBytes, nil)
	badWire, err := client.outgoing.seal(bad)
	if err != nil {
		t.Fatal(err)
	}
	before := exit.peerRevision
	if _, _, err := exit.receive(badWire, time.Now()); !errors.Is(err, errFSHybridIntegrity) || exit.peerRevision != before {
		t.Fatal("omitted unprocessed head changed receiver state", err)
	}
}

func TestDriveV2SelectiveReceiptRejectsForgedOrRegressedClaims(t *testing.T) {
	client, exit := driveV2SelectivePair(t)
	shortBits := exit.snapshot(exit.revision+1, 0, exit.grantedSeq, exit.grantedBytes, nil)
	shortBits.SelectiveDone = shortBits.SelectiveDone[:fsHybridReceiptBytes-1]
	if _, err := exit.outgoing.seal(shortBits); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("malformed receipt bitmap encoded", err)
	}
	driveV2SelectiveFill(t, client, 1)
	wire, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high)
	ready, _, err := exit.receive(wire, time.Now())
	if err != nil || len(ready) != 1 {
		t.Fatal(err)
	}
	bad := exit.snapshot(exit.revision+1, 0, exit.grantedSeq, exit.grantedBytes, nil)
	fsHybridReceiptSet(bad.SelectiveDone, 2) // Sender offered only claim 1.
	badWire, err := exit.outgoing.seal(bad)
	if err != nil {
		t.Fatal(err)
	}
	before := client.peerRevision
	if _, _, err := client.receive(badWire, time.Now()); !errors.Is(err, errFSHybridIntegrity) || client.peerRevision != before || len(client.outQueue) != 1 {
		t.Fatal("future selective receipt changed sender state", err)
	}
	if _, _, _, err := exit.markProcessed(ready[0].info.ID); err != nil {
		t.Fatal(err)
	}
	good, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.receive(good, time.Now()); err != nil || len(client.outQueue) != 0 {
		t.Fatal("legitimate receipt failed", err)
	}
	regressed := exit.snapshot(exit.revision+1, exit.rxProcessed, exit.grantedSeq, exit.grantedBytes, nil)
	regressed.SelectiveDone[0] = 0
	badWire, err = exit.outgoing.seal(regressed)
	if err != nil {
		t.Fatal(err)
	}
	before = client.peerRevision
	if _, _, err := client.receive(badWire, time.Now()); !errors.Is(err, errFSHybridIntegrity) || client.peerRevision != before {
		t.Fatal("regressed receipt changed sender state", err)
	}
}

func TestDriveV2SelectiveWireIsDistinctFromLegacy(t *testing.T) {
	client, _ := driveV2SelectivePair(t)
	wire, _, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	legacy := fsHybridTestCodec(t, DirectionUp)
	legacy.wireMax = driveV2PriorityCipherMax
	if _, err := legacy.open(wire); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("old peer accepted selective wire", err)
	}
	old, err := legacy.seal(legacy.snapshot(1, 0, 32, fsHybridWireMax, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.outgoing.open(old); !errors.Is(err, errFSHybridBounds) {
		t.Fatal("selective peer silently accepted old wire", err)
	}
}

func TestDriveV2SelectiveReceiptWireOverheadBound(t *testing.T) {
	legacy, _ := driveV2WindowPair(t, false)
	selective, _ := driveV2SelectivePair(t)
	baseEmpty, _, err := legacy.prepare()
	if err != nil {
		t.Fatal(err)
	}
	newEmpty, _, err := selective.prepare()
	if err != nil {
		t.Fatal(err)
	}
	driveV2SelectiveFill(t, legacy, 32)
	driveV2SelectiveFill(t, selective, 32)
	baseFull, _, err := legacy.prepare()
	if err != nil {
		t.Fatal(err)
	}
	newFull, _, err := selective.prepare()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy/selective empty=%d/%d bytes, 32-record=%d/%d bytes", len(baseEmpty), len(newEmpty), len(baseFull), len(newFull))
	if overhead := len(newEmpty) - len(baseEmpty); overhead <= 0 || overhead > 1024 ||
		len(newFull) > driveV2PriorityCipherMax || len(newFull)-len(baseFull) != overhead {
		t.Fatal("selective bitmap exceeded its fixed wire bound", overhead, len(baseFull), len(newFull))
	}
}

func TestDriveV2SelectiveReceiptContinuesAcrossStalledHeadWaves(t *testing.T) {
	client, exit := driveV2SelectivePair(t)
	driveV2SelectiveFill(t, client, 32)
	var first *fsHybridInbound
	for wave := uint64(0); wave < 4; wave++ {
		wire, high, err := client.prepare()
		if err != nil || len(wire) > driveV2PriorityCipherMax {
			t.Fatal("bounded wave could not publish", wave, len(wire), err)
		}
		client.published(high)
		ready, _, err := exit.receive(wire, time.Now())
		if err != nil || len(ready) != 32 {
			t.Fatal("wave lost unprocessed identities", wave, len(ready), err)
		}
		if first == nil {
			first = ready[0]
		} else if ready[0] != first {
			t.Fatal("stalled head identity changed")
		}
		for _, entry := range ready[1:] {
			if _, _, _, err := exit.markProcessed(entry.info.ID); err != nil {
				t.Fatal(err)
			}
		}
		ack, _, err := exit.prepare()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.receive(ack, time.Now()); err != nil || len(client.outQueue) != 1 || exit.rxProcessed != 0 {
			t.Fatal("wave did not selectively release processed claims", wave, len(client.outQueue), exit.rxProcessed, err)
		}
		if wave == 3 {
			break
		}
		start := 33 + wave*31
		for seq := start; seq < start+31; seq++ {
			stream := uint64(100 + (seq-1)/8)
			if !client.enqueue(driveV2WindowClaim(t, client, seq, stream, []byte{byte(seq)})) {
				t.Fatal("stalled head prevented bounded replenishment", wave, seq)
			}
		}
	}
	if exit.rxHigh != 125 || len(exit.rxNames) != 125 {
		t.Fatal("finite lifetime identities were lost", exit.rxHigh, len(exit.rxNames))
	}
	if _, _, advanced, err := exit.markProcessed(first.info.ID); err != nil || !advanced || exit.rxProcessed != 125 {
		t.Fatal("stalled head did not release all completed waves", exit.rxProcessed, err)
	}
	ack, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.receive(ack, time.Now()); err != nil || len(client.outQueue) != 0 || client.outRetained != 0 {
		t.Fatal("final head receipt leaked sender queue", len(client.outQueue), client.outRetained, err)
	}
}

func TestDriveV2SelectivePhysicalFirstCompletionReceiptsLaterSequence(t *testing.T) {
	client, exit := driveV2SelectivePair(t)
	claim := driveV2WindowClaim(t, client, 1, 77, []byte("physical-first"))
	if !client.enqueue(claim) {
		t.Fatal("fixture did not admit inline identity")
	}
	physical, err := exit.normalizePhysical(ObjectInfo{Name: claim.name, ID: "owned-physical-id-0123", Size: int64(len(claim.sealed))}, claim.sealed, time.Now())
	if err != nil || physical.sequence != 0 {
		t.Fatal("physical-first identity failed", err)
	}
	if _, _, advanced, err := exit.markProcessed(physical.info.ID); err != nil || advanced {
		t.Fatal("physical-first processing invented unseen sequence", err)
	}
	wire, high, err := client.prepare()
	if err != nil {
		t.Fatal(err)
	}
	client.published(high)
	ready, advanced, err := exit.receive(wire, time.Now())
	if err != nil || len(ready) != 0 || !advanced || exit.rxProcessed != 1 || !fsHybridReceiptHas(exit.rxDone[:], 1) {
		t.Fatal("late inline association redelivered or missed processed identity", len(ready), advanced, err)
	}
	ack, _, err := exit.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.receive(ack, time.Now()); err != nil || len(client.outQueue) != 0 || !claim.processed {
		t.Fatal("physical-first selective receipt did not release sender", err)
	}
}

func TestDriveV2SelectiveEncodedCapTradeoff(t *testing.T) {
	legacy, _ := driveV2WindowPair(t, false)
	selective, _ := driveV2SelectivePair(t)
	counts := [2]int{}
	for index, state := range []*fsHybridState{legacy, selective} {
		for i := uint64(1); i <= 8; i++ {
			claim := driveV2WindowClaim(t, state, i, 77, bytes.Repeat([]byte{byte(i)}, 2400))
			if !state.enqueue(claim) {
				break
			}
			counts[index]++
		}
		wire, _, err := state.prepare()
		if err != nil || len(wire) > driveV2PriorityCipherMax {
			t.Fatal("encoded cap escaped", index, len(wire), err)
		}
	}
	t.Logf("2400-byte data frames admitted before 16 KiB encoded cap, legacy/selective=%d/%d", counts[0], counts[1])
	if counts[1] <= 0 || counts[1] > counts[0] {
		t.Fatal("selective envelope gained impossible encoded capacity", counts)
	}
}

func TestDriveV2SelectiveAndLegacyStoreRespect16KiBCapAndExactFallback(t *testing.T) {
	for _, selective := range []bool{false, true} {
		t.Run(map[bool]string{false: "FSH1", true: "FSH2"}[selective], func(t *testing.T) {
			client, _, backend, up, _, base := driveV2PriorityStoresVersion(t, 8, selective)
			if client.cfg.MaxSnapshotBytes != driveV2PriorityCipherMax || client.state.outgoing.wireLimit() != driveV2PriorityCipherMax {
				t.Fatal("store admission and v2 carrier caps disagree")
			}
			backend.mu.Lock()
			backend.frozenReadID = up.cfg.OwnID
			backend.mu.Unlock()
			expected := make(map[string][]byte)
			for i := uint64(1); i <= 8; i++ {
				record := fsHybridTestRecord(t, client.state.outgoing, i, 0, muxFrame{
					Kind: muxFrameData, StreamID: 77, Seq: i,
					Payload: bytes.Repeat([]byte{byte(i)}, 2400),
				})
				expected[record.Name] = record.Sealed
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, err := client.PutObject(ctx, record.Name, record.Sealed)
				cancel()
				if err != nil {
					t.Fatal("priority publication failed", i, err)
				}
			}
			stats := client.Snapshot()
			if stats["priority_enqueued"] < 4 || stats["priority_immutable_only"] < 1 || stats["fallback_published"] < 1 {
				t.Fatal("encoded cap did not use exact physical fallback", stats)
			}
			backend.mu.Lock()
			length := len(backend.docs[up.cfg.OwnID].wire)
			backend.frozenReadID = ""
			backend.mu.Unlock()
			if length > driveV2PriorityCipherMax {
				t.Fatal("metadata advertisement exceeded selected cap", length)
			}
			objects, err := base.List(context.Background(), "")
			if err != nil || len(objects) == 0 {
				t.Fatal("physical fallback was not published", err)
			}
			for _, object := range objects {
				body, getErr := base.Get(context.Background(), object.Name)
				if getErr != nil || !bytes.Equal(body, expected[object.Name]) {
					t.Fatal("physical fallback changed prepared ciphertext", object.Name, getErr)
				}
			}
		})
	}
}

func TestDriveV2SelectiveReceiptRandomOrderAndLostReplies(t *testing.T) {
	for seed := int64(1); seed <= 64; seed++ {
		client, exit := driveV2SelectivePair(t)
		driveV2SelectiveFill(t, client, 32)
		wire, high, err := client.prepare()
		if err != nil {
			t.Fatal(seed, err)
		}
		client.published(high)
		ready, _, err := exit.receive(wire, time.Now())
		if err != nil || len(ready) != 32 {
			t.Fatal(seed, err)
		}
		rng := rand.New(rand.NewSource(seed))
		var delayed [][]byte
		for index, choice := range rng.Perm(32) {
			if _, _, _, err := exit.markProcessed(ready[choice].info.ID); err != nil {
				t.Fatal(seed, choice, err)
			}
			if index%5 != 0 {
				continue
			}
			ack, _, err := exit.prepare()
			if err != nil {
				t.Fatal(seed, err)
			}
			if rng.Intn(2) == 0 {
				delayed = append(delayed, ack)
			} else if _, _, err := client.receive(ack, time.Now()); err != nil {
				t.Fatal(seed, "intermediate selective reply", err)
			}
		}
		final, _, err := exit.prepare()
		if err != nil {
			t.Fatal(seed, err)
		}
		if _, _, err := client.receive(final, time.Now()); err != nil || len(client.outQueue) != 0 || client.outRetained != 0 || client.acked != 32 || exit.rxProcessed != 32 {
			t.Fatal(seed, "final receipt did not drain exact claims", len(client.outQueue), client.outRetained, client.acked, exit.rxProcessed, err)
		}
		for _, old := range delayed {
			if _, _, err := client.receive(old, time.Now()); !errors.Is(err, errFSHybridReplay) {
				t.Fatal(seed, "delayed reply changed completed state", err)
			}
		}
	}
}

type driveV2SelectiveLocalCounts struct {
	visible, unitsUpper, fallbacks int64
}

func driveV2SelectiveRealTLSMixedArm(t *testing.T, selective bool) driveV2SelectiveLocalCounts {
	t.Helper()
	client, exit, backend, up, down, base := driveV2PriorityStoresVersion(t, 8, selective)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fake Drive diagnostic: selective=%t client=%v exit=%v up=%+v down=%+v errors=%v/%v", selective, client.Snapshot(), exit.Snapshot(), up.Snapshot(), down.Snapshot(), client.Error(), exit.Error())
		}
	})
	f := fsHybridFullMuxFixtureWithStores(t, client, exit, base, true)
	before := func() {
		backend.mu.Lock()
		backend.faultWriteID = up.cfg.OwnID
		backend.nextWriteAfter = context.DeadlineExceeded
		backend.frozenReadID = up.cfg.OwnID
		backend.mu.Unlock()
	}
	afterGET := func() {
		backend.mu.Lock()
		backend.frozenReadID = ""
		backend.faultReadID = up.cfg.OwnID
		backend.nextRead = context.DeadlineExceeded
		backend.mu.Unlock()
		select {
		case down.wake <- struct{}{}:
		default:
		}
		fsHybridEventually(t, func() bool { return exit.Snapshot()["watch_reconnects"] == 1 })
	}
	var counts driveV2SelectiveLocalCounts
	runFSHybridTLSMixedWorkload(t, f, before, afterGET, func() {
		for _, carrier := range []*driveV2PriorityCarrier{up, down} {
			s := carrier.Snapshot()
			if s.Joined != 1 || s.ProbePassed != 1 || s.GuardDenied != 0 || s.QuotaUnitsUpper > carrier.cfg.MaxQuotaUnitsUpper {
				t.Fatal("run violated fake Drive ownership or guard", s)
			}
			counts.visible += s.Visible
			counts.unitsUpper += s.QuotaUnitsUpper
		}
		counts.fallbacks = client.Snapshot()["fallback_published"] + exit.Snapshot()["fallback_published"]
	})
	return counts
}

func TestDriveV2SelectiveRealTLSMixedWorkloadOnFakeGoogleRoute(t *testing.T) {
	var baseline, candidate driveV2SelectiveLocalCounts
	t.Run("FSH1", func(t *testing.T) { baseline = driveV2SelectiveRealTLSMixedArm(t, false) })
	t.Run("FSH2", func(t *testing.T) { candidate = driveV2SelectiveRealTLSMixedArm(t, true) })
	t.Logf("loopback only, same test binary: FSH1/FSH2 visible fake Drive calls=%d/%d conservative upper units=%d/%d fallbacks=%d/%d; no Google latency or quota claim", baseline.visible, candidate.visible, baseline.unitsUpper, candidate.unitsUpper, baseline.fallbacks, candidate.fallbacks)
}
