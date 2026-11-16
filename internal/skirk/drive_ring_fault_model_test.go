package skirk

import (
	"errors"
	"fmt"
	"testing"
)

// This is a deterministic protocol model, not a Drive adapter or product
// transport. It deliberately gives a ring ideal instantaneous visibility and
// independent local processing, so any failure here also survives a slower
// real carrier. The inspected XDRIVE v3 PATCH path is unconditional, and the
// selected pinned-front route has not proved the CAS assumed below.
var (
	errRingNoCredit     = errors.New("ring has no safely reusable slot")
	errRingWrongFrame   = errors.New("ring slot contains a different sequence")
	errRingPrecondition = errors.New("server rejected stale slot version")
	errRingNoFrame      = errors.New("ring slot is empty")
)

type ringFaultFrame struct {
	sequence   int
	generation int
	payload    string
}

type ringFaultSlot struct {
	frame   ringFaultFrame
	version int
	used    bool
}

type ringFaultAttempt struct {
	slot     int
	expected int
	frame    ringFaultFrame
}

type ringFaultModel struct {
	slots     []ringFaultSlot
	processed int // contiguous durable-delivery watermark; -1 before seq 0
	volatile  map[int]string
	accepted  map[int]bool
	patches   int
	gets      int
}

func newRingFaultModel(depth int) *ringFaultModel {
	if depth < 1 {
		panic("ring depth must be positive")
	}
	r := &ringFaultModel{
		slots: make([]ringFaultSlot, depth), processed: -1,
		volatile: make(map[int]string), accepted: make(map[int]bool),
	}
	for i := range r.slots {
		// A pre-created Drive file has an initial strong version in the ideal
		// fenced model. The unconditional model ignores this value.
		r.slots[i].version = 1
	}
	return r
}

func (r *ringFaultModel) begin(sequence int, payload string) (ringFaultAttempt, error) {
	if sequence < 0 {
		return ringFaultAttempt{}, errors.New("negative sequence")
	}
	if sequence >= len(r.slots) && r.processed < sequence-len(r.slots) {
		return ringFaultAttempt{}, errRingNoCredit
	}
	index := sequence % len(r.slots)
	return ringFaultAttempt{
		slot: index, expected: r.slots[index].version,
		frame: ringFaultFrame{sequence: sequence, generation: sequence / len(r.slots), payload: payload},
	}, nil
}

// finish represents the server applying an already-sent PATCH. A timeout or
// local cancellation does not necessarily stop such a server-side mutation.
func (r *ringFaultModel) finish(a ringFaultAttempt, serverCAS bool) error {
	r.patches++
	slot := &r.slots[a.slot]
	if serverCAS && a.expected != slot.version {
		return errRingPrecondition
	}
	slot.version++
	slot.frame = a.frame
	slot.used = true
	return nil
}

func (r *ringFaultModel) publish(sequence int, payload string, serverCAS bool) error {
	a, err := r.begin(sequence, payload)
	if err != nil {
		return err
	}
	return r.finish(a, serverCAS)
}

func (r *ringFaultModel) fetch(sequence int) error {
	r.gets++
	slot := r.slots[sequence%len(r.slots)]
	if !slot.used {
		return errRingNoFrame
	}
	// An authenticated generation and sequence detect this corruption; they
	// do not restore the overwritten newer frame.
	if slot.frame.sequence != sequence || slot.frame.generation != sequence/len(r.slots) {
		return errRingWrongFrame
	}
	r.volatile[sequence] = slot.frame.payload
	return nil
}

func (r *ringFaultModel) acknowledge(sequence int) {
	r.accepted[sequence] = true
	for r.accepted[r.processed+1] {
		r.processed++
		delete(r.accepted, r.processed)
	}
}

func (r *ringFaultModel) deliver(sequence int) error {
	if _, ok := r.volatile[sequence]; !ok {
		return errors.New("no fetched bytes to deliver")
	}
	delete(r.volatile, sequence)
	r.acknowledge(sequence)
	return nil
}

func (r *ringFaultModel) crash() { r.volatile = make(map[int]string) }

func mustBeginRing(t *testing.T, r *ringFaultModel, sequence int, payload string) ringFaultAttempt {
	t.Helper()
	a, err := r.begin(sequence, payload)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustRing(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func runDelayedPatchSchedule(t *testing.T, serverCAS bool) (*ringFaultModel, error) {
	t.Helper()
	r := newRingFaultModel(2)
	late := mustBeginRing(t, r, 0, "text-0")
	retry := mustBeginRing(t, r, 0, "text-0")
	mustRing(t, r.finish(retry, serverCAS))
	mustRing(t, r.fetch(0))
	mustRing(t, r.deliver(0))
	mustRing(t, r.publish(1, "bulk-1", serverCAS))
	mustRing(t, r.fetch(1))
	mustRing(t, r.deliver(1))
	mustRing(t, r.publish(2, "text-2", serverCAS)) // reuses slot 0
	return r, r.finish(late, serverCAS)            // old in-flight seq 0 arrives last
}

func TestDriveRingFaultModelDelayedOldPatchClobbersReusedSlot(t *testing.T) {
	r, err := runDelayedPatchSchedule(t, false)
	mustRing(t, err)
	if err := r.fetch(2); !errors.Is(err, errRingWrongFrame) {
		t.Fatalf("unconditional Drive PATCH unexpectedly retained seq 2: %v", err)
	}
	if got := r.slots[0].frame; got.sequence != 0 || got.generation != 0 {
		t.Fatalf("the old request did not overwrite the newer slot: %+v", got)
	}
	if r.processed != 1 || r.patches != 4 {
		t.Fatalf("wrong completed schedule: watermark=%d patches=%d", r.processed, r.patches)
	}
}

func TestDriveRingFaultModelIdealCASRejectsDelayedOldPatch(t *testing.T) {
	r, err := runDelayedPatchSchedule(t, true)
	if !errors.Is(err, errRingPrecondition) {
		t.Fatalf("server CAS did not fence delayed old attempt: %v", err)
	}
	mustRing(t, r.fetch(2))
	if got := r.volatile[2]; got != "text-2" {
		t.Fatalf("new sequence did not survive: %q", got)
	}
}

func TestDriveRingFaultModelEarlyReadWatermarkLosesUnprocessedBytes(t *testing.T) {
	unsafe := newRingFaultModel(2)
	mustRing(t, unsafe.publish(0, "undelivered", false))
	mustRing(t, unsafe.fetch(0))
	unsafe.acknowledge(0) // optimistic watermark on fetch: unsafe
	mustRing(t, unsafe.publish(1, "other", false))
	mustRing(t, unsafe.publish(2, "reuse", false))
	unsafe.crash()
	if err := unsafe.fetch(0); !errors.Is(err, errRingWrongFrame) {
		t.Fatalf("early ACK did not expose lost undelivered bytes: %v", err)
	}

	safe := newRingFaultModel(2)
	mustRing(t, safe.publish(0, "recoverable", false))
	mustRing(t, safe.fetch(0))
	mustRing(t, safe.publish(1, "other", false))
	if err := safe.publish(2, "reuse", false); !errors.Is(err, errRingNoCredit) {
		t.Fatalf("unprocessed slot was reused: %v", err)
	}
	safe.crash()
	mustRing(t, safe.fetch(0))
	if got := safe.volatile[0]; got != "recoverable" {
		t.Fatalf("unacknowledged bytes could not be recovered: %q", got)
	}
	mustRing(t, safe.deliver(0))
	mustRing(t, safe.publish(2, "reuse", false))
}

func TestDriveRingFaultModelContiguousAckBlocksUnrelatedStream(t *testing.T) {
	r := newRingFaultModel(2)
	mustRing(t, r.publish(0, "stream-A-blocked", false))
	mustRing(t, r.publish(1, "stream-B-ready", false))
	mustRing(t, r.fetch(1))
	mustRing(t, r.deliver(1))
	if r.processed != -1 {
		t.Fatalf("out-of-order local acceptance advanced watermark to %d", r.processed)
	}
	if err := r.publish(2, "stream-C-priority", false); !errors.Is(err, errRingNoCredit) {
		t.Fatalf("unrelated stream bypassed the necessary contiguous fence: %v", err)
	}
	mustRing(t, r.fetch(0))
	mustRing(t, r.deliver(0))
	if r.processed != 1 {
		t.Fatalf("contiguous receipt did not release credit: %d", r.processed)
	}
	mustRing(t, r.publish(2, "stream-C-priority", false))
}

type ringFaultImmutable struct{ objects map[int]string }

func (s *ringFaultImmutable) create(sequence int, payload string) error {
	if _, exists := s.objects[sequence]; exists {
		return errRingPrecondition
	}
	s.objects[sequence] = payload
	return nil
}

func TestDriveRingFaultModelImmutableIdentityFencesLateCreate(t *testing.T) {
	s := ringFaultImmutable{objects: make(map[int]string)}
	// A retry can win after the first create's response is lost. The old
	// request, arriving later, cannot change that immutable sequence ID.
	mustRing(t, s.create(0, "text-0"))
	mustRing(t, s.create(1, "bulk-1"))
	mustRing(t, s.create(2, "text-2"))
	if err := s.create(0, "text-0"); !errors.Is(err, errRingPrecondition) {
		t.Fatalf("late duplicate create mutated an existing ID: %v", err)
	}
	if got := s.objects[2]; got != "text-2" {
		t.Fatalf("newer object was overwritten: %q", got)
	}
}

func ringFaultColdUnits(depth int) int { return (2*depth + 2) * 50 }

func ringFaultTransferUnits(messages, ackBatch int) int {
	if messages == 0 {
		return 0
	}
	return messages*(50+5) + ((messages+ackBatch-1)/ackBatch)*50
}

func TestDriveRingFaultModelQuotaEnvelope(t *testing.T) {
	for _, row := range []struct{ depth, want int }{{2, 300}, {4, 500}, {64, 6500}} {
		if got := ringFaultColdUnits(row.depth); got != row.want {
			t.Fatalf("depth %d cold request units: got %d want %d", row.depth, got, row.want)
		}
	}
	if got := ringFaultTransferUnits(32, 32); got != 1810 {
		t.Fatal(fmt.Sprintf("batch-ACK lower bound: got %d want 1810", got))
	}
	if got := ringFaultTransferUnits(32, 1); got != 3360 {
		t.Fatal(fmt.Sprintf("per-message ACK lower bound: got %d want 3360", got))
	}
	// One five-unit GET/s across 200k always-active users already exceeds
	// the shared-project minute cap by 60x before any transfer or write.
	if unitsPerMinute := 200_000 * 60 * 5; unitsPerMinute != 60_000_000 {
		t.Fatalf("idle poll arithmetic changed: %d", unitsPerMinute)
	}
}
