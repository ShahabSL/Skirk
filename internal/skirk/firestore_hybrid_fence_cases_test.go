package skirk

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestFSHybridDocumentFencesKeepFirestoreStrictAndV2Opaque(t *testing.T) {
	ts := fsPriorityDocument{UpdateTime: fsHybridPublicTime}
	etag := fsPriorityDocument{FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"first"`}
	if !ts.valid() || !etag.valid() {
		t.Fatal("known carrierfence rejected")
	}
	for _, bad := range []fsPriorityDocument{{FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `W/"weak"`}, {FenceKind: fsPriorityDriveV2Fence, OpaqueFence: "unquoted"}, {FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `""`}, {FenceKind: fsPriorityDriveV2Fence, OpaqueFence: "\"bad\nline\""}, {FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"valid"`, UpdateTime: fsHybridPublicTime}, {FenceKind: fsPriorityFirestoreFence, OpaqueFence: `"extra"`, UpdateTime: fsHybridPublicTime}, {FenceKind: 99, UpdateTime: fsHybridPublicTime}} {
		if bad.valid() {
			t.Fatal("malformed/mixedcarrierfence admitted")
		}
	}
	wire := []byte("identical prepared ciphertext")
	next := etag
	next.OpaqueFence = `"second"`
	next.Ciphertext = bytes.Clone(wire)
	if !fsHybridDocumentAfter(next, etag, wire) || fsHybridDocumentAfter(etag, etag, wire) {
		t.Fatal("opaque transition failed exactidentity")
	}
	if next.sameFence(etag) || ts.sameFence(etag) || etag.fence() != `"first"` {
		t.Fatal("carrier fences conflated")
	}
	next.FenceKind = fsPriorityFirestoreFence
	next.OpaqueFence = ""
	next.UpdateTime = fsHybridPublicTime
	if fsHybridDocumentAfter(next, etag, wire) {
		t.Fatal("write response changed carrierkind")
	}
}

func TestFSHybridCarrierSpecificSnapshotBoundFallsBackBeforeSeal(t *testing.T) {
	out, in := fsHybridTestCodec(t, DirectionUp), fsHybridTestCodec(t, DirectionDown)
	out.wireMax = 2048
	in.wireMax = 2048
	s := newFSHybridState(out, in)
	s.peerCreditSeq = fsHybridQueueMax
	s.peerCreditBytes = 2048
	accepted, fallback := 0, 0
	for i := 1; i <= 8; i++ {
		r := fsHybridTestRecord(t, out, uint64(i), 0, muxFrame{Kind: muxFrameData, StreamID: uint64(i), Seq: 1, Payload: bytes.Repeat([]byte{byte(i)}, 128)})
		entry, _, e := s.admission(r.Name, r.Sealed, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if s.enqueue(entry) {
			accepted++
		} else {
			fallback++
			if entry.record.Sequence != 0 {
				t.Fatal("oversized snapshot consumedqueueidentity")
			}
		}
	}
	if accepted == 0 || fallback == 0 {
		t.Fatal("fixture didnotexercise snapshot-bound fallback", accepted, fallback)
	}
	wire, _, e := s.prepare()
	if e != nil || len(wire) > 2048 {
		t.Fatal("bounded snapshot failed ratherthanroutingfallback", len(wire), e)
	}
	if _, e = out.open(wire); e != nil {
		t.Fatal(e)
	}
	out.wireMax = 1024
	if len(wire) <= 1024 {
		t.Fatal("fixture didnotfill snapshot beyondsmallerbound")
	}
	if _, e = out.open(wire); !errors.Is(e, errFSHybridBounds) {
		t.Fatal("oversizedwire reacheddecryptallocation", e)
	}
	defaults, e := (fsHybridStoreConfig{}).normalized()
	if e != nil || defaults.MaxSnapshotBytes != fsHybridWireMax {
		t.Fatal("Firestoredefaultchanged")
	}
	if _, e = (fsHybridStoreConfig{MaxSnapshotBytes: 1023}).normalized(); e == nil {
		t.Fatal("invalidsnapshotboundadmitted")
	}
}

type fsHybridOpaqueFixture struct {
	doc        fsPriorityDocument
	commitLost bool
	calls      int
	expected   []string
	wires      [][]byte
}

func (c *fsHybridOpaqueFixture) WriteOwn(_ context.Context, expected string, wire []byte) (fsPriorityDocument, error) {
	c.calls++
	c.expected = append(c.expected, expected)
	c.wires = append(c.wires, bytes.Clone(wire))
	if expected != c.doc.fence() {
		return fsPriorityDocument{}, &GoogleAPIError{Status: 412}
	}
	if c.calls == 1 {
		if c.commitLost {
			c.doc = fsPriorityDocument{Ciphertext: bytes.Clone(wire), FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"second"`}
		}
		return fsPriorityDocument{}, context.DeadlineExceeded
	}
	c.doc = fsPriorityDocument{Ciphertext: bytes.Clone(wire), FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"second"`}
	return c.doc, nil
}
func (c *fsHybridOpaqueFixture) ReadOwn(context.Context) (fsPriorityDocument, error) {
	return c.doc, nil
}
func (c *fsHybridOpaqueFixture) ReadPeer(context.Context) (fsPriorityDocument, error) {
	return c.doc, nil
}
func (c *fsHybridOpaqueFixture) WatchPeer(ctx context.Context, _ func(fsPriorityDocument) error) error {
	return ctx.Err()
}
func (c *fsHybridOpaqueFixture) Close() error { return nil }
func TestFSHybridOpaqueAmbiguityUsesExactSameFenceAndPreparedCiphertext(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "oldreadback_retry", true: "committed_readback"}[lost], func(t *testing.T) {
			previous := fsPriorityDocument{FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"first"`}
			carrier := &fsHybridOpaqueFixture{doc: previous, commitLost: lost}
			store := &fsHybridPriorityStore{ctx: context.Background(), carrier: carrier, cfg: fsHybridStoreConfig{OperationTimeout: time.Second}, counters: make(map[string]int64)}
			wire := []byte("one prepared ciphertext")
			next, e := store.writePrepared(previous, wire)
			if e != nil || !fsHybridDocumentAfter(next, previous, wire) {
				t.Fatal("opaque exactwrite/readback failed", e)
			}
			want := 2
			if lost {
				want = 1
			}
			if carrier.calls != want {
				t.Fatal("unexpectedphysicalwritecount", carrier.calls)
			}
			for i := range carrier.expected {
				if carrier.expected[i] != previous.OpaqueFence || !bytes.Equal(carrier.wires[i], wire) {
					t.Fatal("ambiguousretry advancedfence orchangedciphertext")
				}
			}
		})
	}
}

func TestFSHybridIngestCannotSwitchCarrierFenceKind(t *testing.T) {
	client, exit, _, _ := fsHybridMemoryStores(t)
	exit.mu.Lock()
	wire, _, e := exit.state.prepare()
	exit.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	client.mu.Lock()
	revision := client.state.peerRevision
	client.mu.Unlock()
	e = client.ingest(fsPriorityDocument{Ciphertext: wire, FenceKind: fsPriorityDriveV2Fence, OpaqueFence: `"other"`})
	if !errors.Is(e, errFSHybridIntegrity) {
		t.Fatal("peer changedcarrierfencekind", e)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.state.peerRevision != revision {
		t.Fatal("rejectedcarrierkindmutatedstate")
	}
}
