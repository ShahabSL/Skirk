package skirk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type muxReceiptTestStore struct {
	*MemoryStore
	mu       sync.Mutex
	receipts int
	hook     func(string, string) error
	noted    chan struct{}
	deletes  atomic.Int64
}

func (s *muxReceiptTestStore) NoteMuxProcessed(name, id string) error {
	s.mu.Lock()
	s.receipts++
	s.mu.Unlock()
	var err error
	if s.hook != nil {
		err = s.hook(name, id)
	}
	select {
	case s.noted <- struct{}{}:
	default:
	}
	return err
}
func (s *muxReceiptTestStore) DeleteID(ctx context.Context, id string) error {
	s.deletes.Add(1)
	return s.MemoryStore.DeleteID(ctx, id)
}
func (s *muxReceiptTestStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.receipts }

func muxReceiptFixture(t *testing.T, store BlobStore, bad bool) (*driveMux, *muxStream, muxObjectMeta, []byte) {
	t.Helper()
	sid := [16]byte{1}
	m, err := newDriveMux(&Tunnel{Data: store, Secret: "public-receipt-fixture", SessionID: sid, ClientID: "client", RunID: "run", Concurrency: 4, PollInterval: time.Second, CleanupProcessed: true, Logger: log.New(io.Discard, "", 0)}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	t.Cleanup(func() { remote.Close(); m.closeAll() })
	stream := m.registerStream(7, "client", "run", local)
	payload := []byte("mux owns this payload")
	raw, _ := encodeMuxBatch([]muxFrame{{Kind: muxFrameData, StreamID: 7, Seq: 1, Payload: payload}})
	key, _ := DeriveMuxLaneKeyV4(m.t.Secret, sid, DirectionUp, "client", "run", 0)
	sealed, err := Seal(key, sid, DirectionUp, 1, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if bad {
		sealed[len(sealed)-1] ^= 1
	}
	name := muxObjectNameWithStreamIDs(sid, DirectionUp, "client", "run", "receipt", 7, []uint64{7}, 0, 1, 1, 1, 1, len(raw), true)
	reserve := store.(ObjectIDReserveStore)
	ids, err := reserve.GenerateObjectIDs(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.(ObjectPutIDStore).PutObjectWithID(context.Background(), ids[0], name, sealed)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := parseMuxObjectInfo(info)
	if !ok {
		t.Fatal("invalid public fixture metadata")
	}
	if bad {
		meta.Attempts = muxProcessMaxRetries
	}
	return m, stream, meta, payload
}

func muxReceiptWorker(t *testing.T, m *driveMux, meta muxObjectMeta) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if !m.enqueueMuxObject(ctx, meta) {
		cancel()
		t.Fatal("fixture was not enqueued")
	}
	done := make(chan struct{})
	go func() { defer close(done); m.runReceiveWorker(ctx, true) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("receipt worker did not join")
		}
	})
	return cancel, done
}

func TestMuxProcessingReceiptPrecedesDelayedPhysicalCleanup(t *testing.T) {
	store := &muxReceiptTestStore{MemoryStore: NewMemoryStore(), noted: make(chan struct{}, 2)}
	m, stream, meta, want := muxReceiptFixture(t, store, false)
	store.hook = func(name, id string) error {
		m.seenMu.Lock()
		_, processed := m.processed[name]
		m.seenMu.Unlock()
		if !processed || name != meta.Name || id != meta.ID || len(stream.inbound) != 1 {
			t.Error("receipt preceded authenticated mux ownership")
		}
		return nil
	}
	cancel, done := muxReceiptWorker(t, m, meta)
	select {
	case <-store.noted:
	case <-time.After(time.Second):
		t.Fatal("processing receipt awaited deferred cleanup")
	}
	if store.deletes.Load() != 0 {
		t.Fatal("receipt invoked physical I/O")
	}
	if _, err := store.GetByID(context.Background(), meta.ID); err != nil {
		t.Fatal("receipt deleted physical payload", err)
	}
	if got := <-stream.inbound; !bytes.Equal(got, want) {
		t.Fatal("wrong application bytes")
	}
	select {
	case task := <-m.cleanupQueue:
		if task.id != meta.ID {
			t.Fatal("wrong cleanup ownership")
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary deferred cleanup omitted")
	}
	if m.enqueueMuxObject(context.Background(), meta) {
		t.Fatal("successful object was re-enqueued")
	}
	if err := store.DeleteID(context.Background(), meta.ID); err != nil {
		t.Fatal(err)
	}
	if store.count() != 1 {
		t.Fatal("physical deletion fabricated a second processing receipt")
	}
	cancel()
	<-done
}

func TestMuxProcessingReceiptErrorDoesNotReplayAcceptedBytes(t *testing.T) {
	store := &muxReceiptTestStore{MemoryStore: NewMemoryStore(), noted: make(chan struct{}, 2), hook: func(string, string) error { return errors.New("untrusted raw receipt details") }}
	m, stream, meta, want := muxReceiptFixture(t, store, false)
	var output bytes.Buffer
	m.t.Logger = log.New(&output, "", 0)
	cancel, done := muxReceiptWorker(t, m, meta)
	select {
	case <-store.noted:
	case <-time.After(time.Second):
		t.Fatal("callback omitted")
	}
	if got := <-stream.inbound; !bytes.Equal(got, want) {
		t.Fatal("wrong accepted bytes")
	}
	if m.enqueueMuxObject(context.Background(), meta) {
		t.Fatal("receipt error retried side effects")
	}
	cancel()
	<-done
	if store.count() != 1 || len(stream.inbound) != 0 {
		t.Fatal("receipt failure repeated processing")
	}
	if bytes.Contains(output.Bytes(), []byte("untrusted raw receipt details")) {
		t.Fatal("raw callback details leaked")
	}
}

func TestMuxProcessingReceiptNeverAcknowledgesBadCiphertext(t *testing.T) {
	store := &muxReceiptTestStore{MemoryStore: NewMemoryStore(), noted: make(chan struct{}, 2)}
	m, stream, meta, _ := muxReceiptFixture(t, store, true)
	cancel, done := muxReceiptWorker(t, m, meta)
	select {
	case <-m.cleanupQueue:
	case <-time.After(time.Second):
		t.Fatal("bad object was not handled by actual failure path")
	}
	cancel()
	<-done
	if store.count() != 0 || len(stream.inbound) != 0 {
		t.Fatal("failed processing was receipted")
	}
	m.seenMu.Lock()
	_, processed := m.processed[meta.Name]
	m.seenMu.Unlock()
	if processed {
		t.Fatal("bad object marked successful")
	}
}

func TestMuxProcessingReceiptCanceledAfterOwnershipDoesNotAcknowledgeOrReexecute(t *testing.T) {
	store := &muxReceiptTestStore{MemoryStore: NewMemoryStore(), noted: make(chan struct{}, 2)}
	m, stream, meta, want := muxReceiptFixture(t, store, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !m.enqueueMuxObject(ctx, meta) {
		t.Fatal("fixture not enqueued")
	}
	// Hold only the successful-ownership marker. The actual DATA can enter
	// the stream, but the receipt cannot run until this lock is released.
	m.seenMu.Lock()
	done := make(chan struct{})
	go func() { defer close(done); m.runReceiveWorker(ctx, true) }()
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, want) {
			t.Error("wrong bytes")
		}
	case <-time.After(time.Second):
		m.seenMu.Unlock()
		cancel()
		<-done
		t.Fatal("DATA did not reach bounded mux ownership")
	}
	cancel()
	m.seenMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled receiver did not join")
	}
	m.seenMu.Lock()
	_, processed := m.processed[meta.Name]
	m.seenMu.Unlock()
	if !processed || store.count() != 0 || len(stream.inbound) != 0 {
		t.Fatal("cancellation fabricated receipt or replayed owned bytes")
	}
}

func TestMuxProcessingReceiptOptionalForOrdinaryMemoryStore(t *testing.T) {
	store := NewMemoryStore()
	if _, ok := any(store).(MuxProcessingReceiptStore); ok {
		t.Fatal("ordinary store unexpectedly opted in")
	}
	m, stream, meta, want := muxReceiptFixture(t, store, false)
	cancel, done := muxReceiptWorker(t, m, meta)
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, want) {
			t.Fatal("wrong ordinary bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary store path changed")
	}
	select {
	case task := <-m.cleanupQueue:
		if task.id != meta.ID {
			t.Fatal("ordinary cleanup identity changed")
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary cleanup missing")
	}
	cancel()
	<-done
}
