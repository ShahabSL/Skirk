package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rrFixture(t *testing.T, cfg relayResearchConfig) *relayResearchBroker {
	t.Helper()
	b, err := newRelayResearchBroker(context.Background(), bytes.Repeat([]byte{0x6d}, 32), [16]byte{1, 2, 3}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}
func rrPiece(id, name string, data []byte) relayResearchPiece {
	return relayResearchPiece{ID: id, Name: name, Total: len(data), Hash: sha256.Sum256(data), Data: data, Priority: rrPriorityName(name)}
}
func rrCall(t *testing.T, b *relayResearchBroker, p relayResearchPacket) relayResearchPacket {
	t.Helper()
	wire, err := b.codec.seal(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := b.exchange(ctx, wire)
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.codec.open(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != p.Kind|rrResponse || r.Seq != p.Seq {
		t.Fatal("response identity differs")
	}
	return r
}

func TestRelayResearchCodecAuthenticatesEpochAndBounds(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024})
	p := relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{rrPiece("id", "name", bytes.Repeat([]byte{0xd7}, 128*1024))}}
	wire, err := b.codec.seal(p)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := b.codec.open(wire)
	if err != nil || !bytes.Equal(decoded.Pieces[0].Data, p.Pieces[0].Data) {
		t.Fatal("codec changed bounded bytes")
	}
	wire[len(wire)-1] ^= 1
	if _, err := b.codec.open(wire); err == nil {
		t.Fatal("tampering accepted")
	}
	other, _ := newRelayResearchCodec(bytes.Repeat([]byte{0x6d}, 32), [16]byte{9}, 128*1024)
	wire, _ = b.codec.seal(p)
	if _, err := other.open(wire); err == nil {
		t.Fatal("different session epoch accepted")
	}
	p.Pieces[0] = rrPiece("id", "name", bytes.Repeat([]byte{1}, 128*1024+1))
	if _, err := b.codec.seal(p); err == nil {
		t.Fatal("over-frame payload accepted")
	}
	if _, err := newRelayResearchCodec(make([]byte, 16), [16]byte{}, 128*1024); err == nil {
		t.Fatal("unexpected key length accepted")
	}
}

func TestRelayResearchLostCommittedResponseIsExactAndNeverRepublishesDeletedUp(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024})
	entered, release := make(chan struct{}), make(chan struct{})
	var published atomic.Int32
	b.setUpHooks(func(ObjectInfo, []byte) error { close(entered); <-release; return nil }, func(ObjectInfo) { published.Add(1) })
	piece := rrPiece("up-a", "muxv4/research/p0/a", []byte("synthetic sealed object"))
	p := relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{piece}}
	wire, _ := b.codec.seal(p)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := b.exchange(ctx, wire); first <- err }()
	<-entered
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("HTTP cancellation did not return")
	}
	close(release)
	raw, err := b.exchange(context.Background(), wire)
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("ambiguous retry changed cached ciphertext")
	}
	r, _ := b.codec.open(raw)
	if len(r.Committed) != 1 || r.Committed[0] != piece.ID {
		t.Fatal("publication not explicitly acknowledged")
	}
	if err := b.exitStore().DeleteID(context.Background(), piece.ID); err != nil {
		t.Fatal(err)
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{piece}})
	if r.Status != 0 || len(r.Committed) != 1 || published.Load() != 1 || b.snapshot().AcceptedUp != 1 {
		t.Fatal("late retry republished consumed OPEN/data")
	}
	infos, _ := b.exitStore().List(context.Background(), "muxv4/")
	if len(infos) != 0 || b.snapshot().UpBytes != 0 {
		t.Fatal("consumed payload resurrected")
	}
	if _, err := b.exchange(context.Background(), wire); !errors.Is(err, errRelayResearchSequence) {
		t.Fatal("ACKed old RPC executed again")
	}
	changed, _ := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{piece}})
	if _, err := b.exchange(context.Background(), changed); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("same logical request accepted different sealed identity")
	}
}

func TestRelayResearchResponseReceiptDoesNotDeleteDownPayload(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024, PollWait: time.Millisecond})
	data := bytes.Repeat([]byte{0xa8}, 300*1024)
	if _, err := b.exitStore().PutObjectWithID(context.Background(), "down-a", "muxv4/research/p1/a", data); err != nil {
		t.Fatal(err)
	}
	wire, _ := b.codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	raw, err := b.exchange(context.Background(), wire)
	if err != nil {
		t.Fatal(err)
	}
	retry, _ := b.exchange(context.Background(), wire)
	if !bytes.Equal(raw, retry) {
		t.Fatal("PULL replay advanced cursor or resealed response")
	}
	first, _ := b.codec.open(raw)
	if len(first.Pieces) != 1 || first.Pieces[0].Offset != 0 {
		t.Fatal("first fragment missing")
	}
	second := rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 2, Receipt: 1})
	third := rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 3, Receipt: 2})
	if second.Pieces[0].Offset != 128*1024 || third.Pieces[0].Offset != 256*1024 {
		t.Fatal("fragment cursor wrong")
	}
	if b.snapshot().DownBytes != int64(len(data)) {
		t.Fatal("RPC receipt freed unprocessed object")
	}
	del := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Deletes: []string{"down-a"}})
	if len(del.Deleted) != 1 || b.snapshot().DownBytes != 0 {
		t.Fatal("mux deletion did not release retained ciphertext")
	}
	if _, err := b.exitStore().PutObjectWithID(context.Background(), "down-a", "muxv4/research/p1/a", data); err != nil {
		t.Fatal(err)
	}
	if b.snapshot().AcceptedDown != 1 || b.snapshot().DownBytes != 0 {
		t.Fatal("late exit write resurrected deleted object")
	}
	empty := rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 4, Receipt: 3})
	if len(empty.Pieces) != 0 {
		t.Fatal("deleted DOWN resent")
	}
}

func TestRelayResearchBusyPreflightAllowsDeletionToBreakFullDuplexPressure(t *testing.T) {
	frame := 128 * 1024
	b := rrFixture(t, relayResearchConfig{FrameBytes: frame, MaxBytes: int64(frame), PollWait: time.Millisecond})
	a := rrPiece("up-a", "a", bytes.Repeat([]byte{1}, frame))
	c := rrPiece("up-b", "b", bytes.Repeat([]byte{2}, frame))
	if r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{a}}); r.Status != 0 {
		t.Fatal("first UP failed")
	}
	if _, err := b.exitStore().PutObjectWithID(context.Background(), "down-a", "down", a.Data); err != nil {
		t.Fatal(err)
	}
	_ = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 1})
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{c}, Deletes: []string{"down-a"}})
	if r.Status != rrStatusBusy || len(r.Deleted) != 0 || len(r.Committed) != 0 || b.snapshot().DownBytes != int64(frame) || b.snapshot().UpObjects != 1 {
		t.Fatal("Busy request mutated payloads or blocked")
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 3, Receipt: 2, Deletes: []string{"down-a"}})
	if r.Status != 0 || len(r.Deleted) != 1 || b.snapshot().DownBytes != 0 {
		t.Fatal("deletion could not progress past busy UP")
	}
	if err := b.exitStore().DeleteID(context.Background(), "up-a"); err != nil {
		t.Fatal(err)
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 4, Receipt: 3, Pieces: []relayResearchPiece{c}})
	if r.Status != 0 || len(r.Committed) != 1 || b.snapshot().BusyResponses != 1 {
		t.Fatal("same immutable object did not resume after capacity freed")
	}
}

func TestRelayResearchDownCapacityAndCloseJoinAreBounded(t *testing.T) {
	frame := 128 * 1024
	b := rrFixture(t, relayResearchConfig{FrameBytes: frame, MaxBytes: int64(frame), PollWait: 20 * time.Second})
	data := bytes.Repeat([]byte{9}, frame)
	if _, err := b.exitStore().PutObjectWithID(context.Background(), "down-a", "a", data); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := b.exitStore().PutObjectWithID(ctx, "down-b", "b", data); waiting <- err }()
	cancel()
	if !errors.Is(<-waiting, context.Canceled) {
		t.Fatal("blocked DOWN writer ignored cancellation")
	}
	if b.snapshot().DownObjects != 1 || b.snapshot().DownBytes != int64(frame) {
		t.Fatal("rejected writer reserved excess payload")
	}
	_ = rrCall(t, b, relayResearchPacket{Kind: rrPull, Seq: 1})
	wire, _ := b.codec.seal(relayResearchPacket{Kind: rrPull, Seq: 2, Receipt: 1})
	pull := make(chan struct{})
	go func() { _, _ = b.exchange(context.Background(), wire); close(pull) }()
	deadline := time.Now().Add(time.Second)
	for b.snapshot().LiveWorkers == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{rrPiece("up", "up", []byte("small"))}})
	if r.Status != 0 {
		t.Fatal("independent long PULL blocked UP")
	}
	b.Close()
	select {
	case <-pull:
	case <-time.After(time.Second):
		t.Fatal("close did not join long PULL computation")
	}
	s := b.snapshot()
	if s.LiveWorkers != 0 || s.UpBytes != 0 || s.DownBytes != 0 || s.CachedReplies != 0 {
		t.Fatalf("close debt: %+v", s)
	}
}

func TestRelayResearchTombstoneCapacityNeverEvictsDedupe(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024, MaxObjects: 1})
	piece := rrPiece("up-a", "a", []byte("first"))
	_ = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{piece}})
	_ = b.exitStore().DeleteID(context.Background(), piece.ID)
	r := rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{rrPiece("up-b", "b", []byte("second"))}})
	if r.Status != rrStatusBounds || b.snapshot().UpObjects != 1 {
		t.Fatal("dedupe capacity silently evicted proof")
	}
	r = rrCall(t, b, relayResearchPacket{Kind: rrUp, Seq: 3, Receipt: 2, Pieces: []relayResearchPiece{piece}})
	if r.Status != 0 || len(r.Committed) != 1 || b.snapshot().AcceptedUp != 1 {
		t.Fatal("original tombstone no longer proves commit")
	}
}

func TestRelayResearchRejectedSequenceCannotConsumeResponseReceipts(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 << 10})
	wires := make([][]byte, 2)
	replies := make([][]byte, 2)
	for i := range wires {
		var err error
		wires[i], err = b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: uint64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		replies[i], err = b.exchange(context.Background(), wires[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	badSequence, _ := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 99, Receipt: 2})
	if _, err := b.exchange(context.Background(), badSequence); !errors.Is(err, errRelayResearchSequence) {
		t.Fatal("unexpected future sequence error", err)
	}
	changedWire, _ := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1})
	if _, err := b.exchange(context.Background(), changedWire); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("changed exact request identity accepted", err)
	}
	for i := range wires {
		reply, err := b.exchange(context.Background(), wires[i])
		if err != nil || !bytes.Equal(reply, replies[i]) {
			t.Fatal("rejected request consumed recovery response", i, err)
		}
	}
	if b.snapshot().CachedReplies != 2 {
		t.Fatal("invalid request changed exact recovery cache", b.snapshot())
	}
}

func TestRelayResearchHTTPRejectsUnauthorizedBeforeRPCState(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/exchange", bytes.NewBufferString(`{"auth":"wrong","data":"AAAA"}`))
	b.handler("synthetic-auth-secret-at-least-32-characters").ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || b.snapshot().Requests != 0 {
		t.Fatal("unauthorized request reached RPC state")
	}
}

type rrBlockedBody struct {
	entered, closed, finished chan struct{}
	once                      sync.Once
}

func (b *rrBlockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *rrBlockedBody) Close() error {
	close(b.closed)
	time.Sleep(time.Millisecond)
	close(b.finished)
	return nil
}

func TestRelayResearchHTTPConcurrencyAndCloseJoinBodyCancellation(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024})
	handler := b.handler("synthetic-auth-secret-at-least-32-characters")
	var bodies []*rrBlockedBody
	var done []chan struct{}
	for range 4 {
		body := &rrBlockedBody{entered: make(chan struct{}), closed: make(chan struct{}), finished: make(chan struct{})}
		bodies = append(bodies, body)
		r := httptest.NewRequest(http.MethodPost, "/exchange", nil)
		r.Body = body
		ended := make(chan struct{})
		done = append(done, ended)
		go func() { handler.ServeHTTP(httptest.NewRecorder(), r); close(ended) }()
		<-body.entered
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/exchange", bytes.NewBufferString(`{}`)))
	if w.Code != http.StatusTooManyRequests {
		t.Fatal("HTTP handler admission unbounded")
	}
	b.Close()
	for i, ended := range done {
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("body reader not joined")
		}
		select {
		case <-bodies[i].finished:
		default:
			t.Fatal("Close returned before cancellation callback joined")
		}
	}
	if len(b.httpSlots) != 0 {
		t.Fatal("HTTP slot leaked")
	}
}

type rrBlockedWriter struct {
	header                  http.Header
	entered, deadline       chan struct{}
	enterOnce, deadlineOnce sync.Once
}

func (w *rrBlockedWriter) Header() http.Header { return w.header }
func (w *rrBlockedWriter) WriteHeader(int)     {}
func (w *rrBlockedWriter) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.deadline
	return 0, context.DeadlineExceeded
}
func (w *rrBlockedWriter) SetReadDeadline(time.Time) error { return nil }
func (w *rrBlockedWriter) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		w.deadlineOnce.Do(func() { close(w.deadline) })
	}
	return nil
}

func TestRelayResearchCloseInterruptsAndJoinsSlowHTTPResponseWriter(t *testing.T) {
	b := rrFixture(t, relayResearchConfig{FrameBytes: 128 * 1024})
	auth := "synthetic-auth-secret-at-least-32-characters"
	wire, _ := b.codec.seal(relayResearchPacket{Kind: rrUp, Seq: 1, Pieces: []relayResearchPiece{rrPiece("up", "up", []byte("synthetic"))}})
	body, _ := json.Marshal(relayResearchHTTPEnvelope{Auth: auth, Data: base64.StdEncoding.EncodeToString(wire)})
	w := &rrBlockedWriter{header: http.Header{}, entered: make(chan struct{}), deadline: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		b.handler(auth).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/exchange", bytes.NewReader(body)))
		close(done)
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("response writer never blocked")
	}
	closed := make(chan struct{})
	go func() { b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt response writing")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("response handler did not finish after Close joined its resources")
	}
	if len(b.httpSlots) != 0 {
		t.Fatal("slow writer retained HTTP slot")
	}
}
