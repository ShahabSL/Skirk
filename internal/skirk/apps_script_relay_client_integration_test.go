package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func relayClientTestBroker(t *testing.T, cfg relayResearchConfig, wrap func(*relayResearchBroker) relayResearchExchange) (*relayResearchBroker, *relayResearchClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	key, epoch := bytes.Repeat([]byte{17}, 32), [16]byte{9, 1}
	b, err := newRelayResearchBroker(ctx, key, epoch, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	exchange := b.exchange
	if wrap != nil {
		exchange = wrap(b)
	}
	c, err := newRelayResearchClient(ctx, key, epoch, cfg, exchange)
	if err != nil {
		b.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); c.Close(); b.Close() })
	return b, c
}

func relayClientWait(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func relayClientHTTPExchange(t *testing.T, b *relayResearchBroker) relayResearchExchange {
	t.Helper()
	auth := "synthetic-research-auth-at-least32-characters"
	server := httptest.NewServer(b.handler(auth))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: time.Second * 30}
	t.Cleanup(client.CloseIdleConnections)
	return func(ctx context.Context, wire []byte) ([]byte, error) {
		raw, err := json.Marshal(relayResearchHTTPEnvelope{Auth: auth, Data: base64.StdEncoding.EncodeToString(wire)})
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/exchange", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		limit := base64.StdEncoding.EncodedLen(b.codec.maxWireBytes()) + 1024
		raw, err = io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
		if err != nil {
			return nil, err
		}
		if response.StatusCode != 200 || len(raw) > limit {
			return nil, errRelayResearchConflict
		}
		var envelope relayResearchHTTPEnvelope
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, errRelayResearchConflict
		}
		return base64.StdEncoding.Strict().DecodeString(envelope.Data)
	}
}

func TestRelayClientLostCommitReplyAndImmutableDeletedUpload(t *testing.T) {
	var lost atomic.Bool
	var mu sync.Mutex
	var last []byte
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, PollWait: 5 * time.Millisecond}, func(b *relayResearchBroker) relayResearchExchange {
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			reply, err := b.exchange(ctx, wire)
			if err != nil {
				return nil, err
			}
			p, err := b.codec.open(reply)
			if err == nil && len(p.Committed) > 0 {
				mu.Lock()
				last = bytes.Clone(wire)
				mu.Unlock()
				if lost.CompareAndSwap(false, true) {
					return nil, errors.New("injected response loss after commit")
				}
			}
			return reply, err
		}
	})
	data := bytes.Repeat([]byte("bounded-upload"), 24000)
	info, err := c.PutObject(context.Background(), "unit/p0/upload", data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.exitStore().GetByID(context.Background(), info.ID)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload mismatch", err)
	}
	if b.snapshot().AcceptedUp != 1 || b.snapshot().Replayed < 1 || c.snapshot()["retries"] != 1 {
		t.Fatal("ambiguous commit was not replayed exactly", b.snapshot(), c.snapshot())
	}
	if err = b.exitStore().DeleteID(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	// A replay after destination processing/deletion must return its old reply,
	// without restoring payload or invoking the destination again.
	mu.Lock()
	wire := bytes.Clone(last)
	mu.Unlock()
	if _, err = b.exchange(context.Background(), wire); err != nil {
		t.Fatal(err)
	}
	p, _ := b.codec.open(wire)
	p.Seq++
	p.Receipt = p.Seq - 1
	fresh, _ := b.codec.seal(p)
	response, err := b.exchange(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := b.codec.open(response)
	if len(r.Committed) != 1 || r.Committed[0] != info.ID || b.snapshot().AcceptedUp != 1 {
		t.Fatal("immutable tombstone did not suppress fresh-RPC replay", r, b.snapshot())
	}
	if _, err = b.exitStore().GetByID(context.Background(), info.ID); err == nil {
		t.Fatal("deleted payload resurrected")
	}
	if _, err = b.exchange(context.Background(), wire); !errors.Is(err, errRelayResearchSequence) {
		t.Fatal("ACKed RPC replay was accepted", err)
	}
	if _, err = c.PutObject(context.Background(), "unit/p0/upload", append(bytes.Clone(data), 1)); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("same-name mutation accepted", err)
	}
}

func TestRelayClientDownReceiptDoesNotDeleteBeforeMuxDelete(t *testing.T) {
	var lost atomic.Bool
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, PollWait: 5 * time.Millisecond}, func(b *relayResearchBroker) relayResearchExchange {
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			reply, err := b.exchange(ctx, wire)
			if err != nil {
				return nil, err
			}
			p, _ := b.codec.open(reply)
			if p.Kind == rrPull|rrResponse && len(p.Pieces) > 0 && lost.CompareAndSwap(false, true) {
				return nil, errors.New("injected downstream response loss")
			}
			return reply, nil
		}
	})
	data := bytes.Repeat([]byte("download"), 40000)
	info, err := b.exitStore().PutObject(context.Background(), "unit/p1/download", data)
	if err != nil {
		t.Fatal(err)
	}
	relayClientWait(t, func() bool {
		got, e := c.GetByID(context.Background(), info.ID)
		return e == nil && bytes.Equal(got, data)
	}, "download assembly did not finish")
	// The next PULL receipt discards only the cached RPC response, not its
	// underlying ciphertext. The mux still owns destination processing.
	relayClientWait(t, func() bool { return b.snapshot().Requests > 5 }, "pull receipts did not advance")
	if b.snapshot().DownBytes != int64(len(data)) || b.snapshot().DeletedDown != 0 {
		t.Fatal("RPC receipt deleted unprocessed ciphertext", b.snapshot())
	}
	if err = c.DeleteID(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if b.snapshot().DownBytes != 0 || b.snapshot().DeletedDown != 1 || c.snapshot()["down_bytes"] != 0 {
		t.Fatal("processing deletion did not free payload", b.snapshot(), c.snapshot())
	}
	if err = c.DeleteID(context.Background(), info.ID); err != nil {
		t.Fatal("repeated deletion failed", err)
	}
	if _, err = c.GetByID(context.Background(), info.ID); err == nil {
		t.Fatal("deleted download still readable")
	}
}

func TestRelayClientBusyUploadCannotBlockDownDeletion(t *testing.T) {
	cfg := relayResearchConfig{FrameBytes: 128 << 10, MaxBytes: 128 << 10, PollWait: 5 * time.Millisecond}
	b, c := relayClientTestBroker(t, cfg, nil)
	data := bytes.Repeat([]byte{31}, 128<<10)
	first, err := c.PutObject(context.Background(), "up/p1/first", data)
	if err != nil {
		t.Fatal(err)
	}
	down, err := b.exitStore().PutObject(context.Background(), "down/p1/first", data)
	if err != nil {
		t.Fatal(err)
	}
	relayClientWait(t, func() bool { _, e := c.GetByID(context.Background(), down.ID); return e == nil }, "full DOWN missing")
	second := make(chan error, 1)
	go func() { _, e := c.PutObject(context.Background(), "up/p1/second", data); second <- e }()
	relayClientWait(t, func() bool { return b.snapshot().BusyResponses > 0 }, "UP did not expose bounded pressure")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.DeleteID(ctx, down.ID); err != nil {
		t.Fatal("DOWN deletion blocked behind busy UP", err)
	}
	if b.snapshot().DownBytes != 0 {
		t.Fatal("DOWN capacity not released")
	}
	if err = b.exitStore().DeleteID(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("UP did not resume after exit processing")
	}
	if b.snapshot().AcceptedUp != 2 || c.snapshot()["up_bytes"] != 0 {
		t.Fatal("pressure changed publication accounting", b.snapshot(), c.snapshot())
	}
}

func TestRelayClientLostDeleteReplyDoesNotOverflowSaturatedDownloadCache(t *testing.T) {
	deleted, release := make(chan struct{}), make(chan struct{})
	var drop atomic.Bool
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, MaxBytes: 128 << 10, PollWait: 5 * time.Millisecond}, func(b *relayResearchBroker) relayResearchExchange {
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			reply, err := b.exchange(ctx, wire)
			if err != nil {
				return nil, err
			}
			r, _ := b.codec.open(reply)
			if len(r.Deleted) > 0 && drop.CompareAndSwap(false, true) {
				close(deleted)
				select {
				case <-release:
					return nil, errors.New("injected committed-delete reply loss")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return reply, nil
		}
	})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	first, err := b.exitStore().PutObject(context.Background(), "down/p1/first", bytes.Repeat([]byte{1}, 128<<10))
	if err != nil {
		t.Fatal(err)
	}
	relayClientWait(t, func() bool { _, e := c.GetByID(context.Background(), first.ID); return e == nil }, "first download missing")
	done := make(chan error, 1)
	go func() { done <- c.DeleteID(context.Background(), first.ID) }()
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("delete did not reach broker")
	}
	data := bytes.Repeat([]byte{2}, 128<<10)
	second, err := b.exitStore().PutObject(context.Background(), "down/p1/second", data)
	if err != nil {
		t.Fatal(err)
	}
	relayClientWait(t, func() bool { return c.snapshot()["down_waits"] > 0 }, "new response did not exercise client cache pressure")
	if c.error() != nil || c.snapshot()["down_bytes"] != 128<<10 {
		t.Fatal("lost delete ACK failed or overallocated cache", c.error(), c.snapshot())
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete recovery stalled")
	}
	relayClientWait(t, func() bool {
		got, e := c.GetByID(context.Background(), second.ID)
		return e == nil && bytes.Equal(got, data)
	}, "download did not resume after recovered delete ACK")
	if c.error() != nil || c.snapshot()["down_bytes"] != 128<<10 {
		t.Fatal("cache accounting changed after recovery", c.error(), c.snapshot())
	}
}

func TestRelayClientReservedPriorityProgressWithBothNormalBuffersFull(t *testing.T) {
	cfg := relayResearchConfig{FrameBytes: 128 << 10, MaxBytes: 256 << 10, MaxObjects: 8, PriorityReserveBytes: 128 << 10, PriorityReserveObjects: 2, PollWait: 5 * time.Millisecond}
	b, c := relayClientTestBroker(t, cfg, nil)
	normal := bytes.Repeat([]byte{1}, 128<<10)
	priority := bytes.Repeat([]byte{2}, 64<<10)
	up, err := c.PutObject(context.Background(), "up/p1/first", normal)
	if err != nil {
		t.Fatal(err)
	}
	down, err := b.exitStore().PutObject(context.Background(), "down/p1/first", normal)
	if err != nil {
		t.Fatal(err)
	}
	relayClientWait(t, func() bool { _, e := c.GetByID(context.Background(), down.ID); return e == nil }, "first normal DOWN missing")
	secondUp := make(chan error, 1)
	go func() { secondUp <- c.Put(context.Background(), "up/p1/second", normal) }()
	secondDown := make(chan error, 1)
	go func() {
		_, e := b.exitStore().PutObject(context.Background(), "down/p1/second", normal)
		secondDown <- e
	}()
	relayClientWait(t, func() bool { return b.snapshot().BusyResponses > 0 }, "normal UP did not reach reserved boundary")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.Put(ctx, "up/p0/interactive", priority); err != nil {
		t.Fatal("priority UP blocked behind normal reservation", err)
	}
	control, err := b.exitStore().PutObject(ctx, "down/p0/interactive", priority)
	if err != nil {
		t.Fatal("priority DOWN blocked behind normal reservation", err)
	}
	relayClientWait(t, func() bool { got, e := c.GetByID(ctx, control.ID); return e == nil && bytes.Equal(got, priority) }, "priority DOWN did not enter reserved client cache")
	select {
	case <-secondUp:
		t.Fatal("normal UP consumed priority headroom")
	default:
	}
	select {
	case <-secondDown:
		t.Fatal("normal DOWN consumed priority headroom")
	default:
	}
	if c.snapshot()["down_bytes"] > cfg.MaxBytes || c.snapshot()["normal_down_bytes"] > cfg.MaxBytes-cfg.PriorityReserveBytes {
		t.Fatal("receive budget overrun", c.snapshot())
	}
	if err = b.exitStore().DeleteID(ctx, up.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteID(ctx, down.ID); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan error{secondUp, secondDown} {
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("normal work did not resume after processing")
		}
	}
}

func TestRelayClientReservedPriorityClaimsSurviveNormalTombstones(t *testing.T) {
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, MaxObjects: 3, PriorityReserveObjects: 1, PollWait: 5 * time.Millisecond}, nil)
	for i := 0; i < 2; i++ {
		info, err := c.PutObject(context.Background(), fmt.Sprintf("up/p1/%d", i), []byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		if err = b.exitStore().DeleteID(context.Background(), info.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Put(context.Background(), "up/p1/no-reserved-claim", []byte{3}); !errors.Is(err, errRelayResearchBounds) {
		t.Fatal("normal tombstone consumed reserved claim", err)
	}
	if err := c.Put(context.Background(), "up/p0/control", []byte{4}); err != nil {
		t.Fatal("priority could not use reserved immutable claim", err)
	}
	if b.snapshot().UpObjects != 3 || c.snapshot()["normal_up_objects"] != 2 {
		t.Fatal("normal tombstones were evicted", b.snapshot(), c.snapshot())
	}
}

func TestRelayClientAdmissionCancellationBoundsAndJoinedPumps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	key, epoch := bytes.Repeat([]byte{2}, 32), [16]byte{8}
	var active atomic.Int64
	entered := make(chan struct{}, 4)
	exchange := func(ctx context.Context, _ []byte) ([]byte, error) {
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c, err := newRelayResearchClient(ctx, key, epoch, relayResearchConfig{FrameBytes: 128 << 10, MaxBytes: 128 << 10}, exchange)
	if err != nil {
		t.Fatal(err)
	}
	put := make(chan error, 1)
	go func() { put <- c.Put(context.Background(), "up/p1/pending", make([]byte, 128<<10)) }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("pumps not started")
		}
	}
	wait, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelWait()
	if err = c.Put(wait, "up/p0/capacity-wait", []byte{1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("admission did not respect cancellation", err)
	}
	if c.snapshot()["up_objects"] != 1 || c.snapshot()["up_bytes"] != 128<<10 {
		t.Fatal("waiting admission allocated a second object", c.snapshot())
	}
	cancel()
	c.Close()
	select {
	case err = <-put:
		if err == nil {
			t.Fatal("pending upload falsely succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending waiter leaked")
	}
	if active.Load() != 0 || c.snapshot()["up_bytes"] != 0 || c.snapshot()["down_bytes"] != 0 {
		t.Fatal("Close did not join/free session", active.Load(), c.snapshot())
	}
}

func TestRelayClientRejectsGapsChangedFragmentsAndUnsolicitedCommit(t *testing.T) {
	// Direct calls isolate receive-side admission from the exchange scheduler.
	s := &relayResearchClient{cfg: relayResearchConfig{MaxObjects: 2, MaxBytes: 256 << 10}, down: map[string]*relayResearchDownload{}, downNames: map[string]string{}}
	data := []byte("abcd")
	p := relayResearchPiece{ID: "d1", Name: "down", Total: 4, Hash: sha256.Sum256(data), Data: []byte("ab")}
	if err := s.acceptDownPiece(p); err != nil {
		t.Fatal(err)
	}
	gap := p
	gap.Offset = 3
	gap.Data = []byte("d")
	if err := s.acceptDownPiece(gap); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("fragment gap accepted", err)
	}
	changed := p
	changed.Data = []byte("zz")
	if err := s.acceptDownPiece(changed); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("changed duplicate fragment accepted", err)
	}
	p.Offset = 2
	p.Data = []byte("cd")
	if err := s.acceptDownPiece(p); err != nil {
		t.Fatal(err)
	}
	bad := relayResearchPacket{Committed: []string{"foreign"}}
	if err := s.acceptUpReply(relayResearchPacket{}, bad, nil); !errors.Is(err, errRelayResearchConflict) {
		t.Fatal("unsolicited publication accepted", err)
	}
	if err := s.acceptDownPiece(relayResearchPiece{ID: "over", Name: "over", Total: rrMaxObject + 1, Data: []byte{1}}); !errors.Is(err, errRelayResearchBounds) {
		t.Fatal("oversized assembly accepted", err)
	}
}

type relayResearchSOCKSFixture struct {
	ctx          context.Context
	cancel       context.CancelFunc
	broker       *relayResearchBroker
	client       *relayResearchClient
	cm, em       *driveMux
	http         *http.Client
	socksAddress string
	close        func()
}

func relayClientSOCKSFixture(t *testing.T, rc relayResearchConfig, muxChunk int, wrap func(*relayResearchBroker) relayResearchExchange) *relayResearchSOCKSFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	key, epoch := bytes.Repeat([]byte{3}, 32), [16]byte{7, 2}
	b, err := newRelayResearchBroker(ctx, key, epoch, rc)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	x := b.exchange
	if wrap != nil {
		x = wrap(b)
	}
	c, err := newRelayResearchClient(ctx, key, epoch, rc, x)
	if err != nil {
		b.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); c.Close(); b.Close() })
	sid, _ := NewSessionID()
	cfg := &Config{Secret: "relay-full-local", SessionID: hex.EncodeToString(sid[:]), Client: ClientConfig{ID: "client-a", RunID: "run-a"}, Tunnel: TunnelConfig{Profile: "fixed", PollIntervalMS: 10, ChunkSize: muxChunk, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
	cfg.ApplyDefaults()
	ct, err := NewTunnel(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	et, err := NewTunnel(b.exitStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ct.Logger = log.New(io.Discard, "", 0)
	et.Logger = ct.Logger
	ct.setRole("client")
	et.setRole("exit")
	cm, err := newDriveMux(ct, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	em, err := newDriveMux(et, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	validate, notify := relayResearchMuxHooks(cm, ctx)
	c.setDownHooks(validate, notify)
	validate, notify = relayResearchMuxHooks(em, ctx)
	b.setUpHooks(validate, func(info ObjectInfo) { notify(info) })
	cm.start(ctx)
	em.start(ctx)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	socks := SOCKSServer{Listen: address, Logger: ct.Logger, Handler: func(ctx context.Context, target string, conn net.Conn) { _ = cm.openClientStream(ctx, target, conn) }}
	serverDone := make(chan error, 1)
	go func() { serverDone <- socks.Serve(ctx) }()
	ready := false
	for i := 0; i < 100; i++ {
		conn, e := net.DialTimeout("tcp", address, 10*time.Millisecond)
		if e == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !ready {
		cancel()
		c.Close()
		b.Close()
		t.Fatal("SOCKS listener unavailable")
	}
	transport := &http.Transport{MaxConnsPerHost: 32, DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
		return dialViaSOCKS5(ctx, "socks5h://"+address, target)
	}}
	f := &relayResearchSOCKSFixture{ctx: ctx, cancel: cancel, broker: b, client: c, cm: cm, em: em, socksAddress: address, http: &http.Client{Transport: transport, Timeout: 60 * time.Second}}
	var once sync.Once
	f.close = func() {
		once.Do(func() {
			transport.CloseIdleConnections()
			cancel()
			cm.closeAll()
			em.closeAll()
			c.Close()
			b.Close()
			<-serverDone
		})
	}
	t.Cleanup(f.close)
	return f
}

func TestRelayClientRealSOCKS16MiBEachDirectionMixedTextLostReplies(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		name := "baseline"
		cfg := relayResearchConfig{FrameBytes: 128 << 10, PollWait: 10 * time.Millisecond}
		if reserved {
			name = "reserved-priority"
			cfg.PriorityReserveBytes = 1 << 20
			cfg.PriorityReserveObjects = 16
		}
		t.Run(name, func(t *testing.T) { relayClientSOCKSExercise(t, cfg, 256<<10, 0) })
	}
	t.Run("reserved-4m-objects-20ms-simulated-rpc", func(t *testing.T) {
		relayClientSOCKSExercise(t, relayResearchConfig{FrameBytes: 768 << 10, PollWait: 10 * time.Millisecond, PriorityReserveBytes: 1 << 20, PriorityReserveObjects: 16}, 4<<20, 20*time.Millisecond)
	})
	t.Run("packed-priority-4m-objects-20ms-simulated-rpc", func(t *testing.T) {
		relayClientSOCKSExercise(t, relayResearchConfig{FrameBytes: 768 << 10, MaxBytes: 32 << 20, PollWait: 10 * time.Millisecond, PriorityReserveBytes: 1 << 20, PriorityReserveObjects: 16, PackPriority: true}, 4<<20, 20*time.Millisecond)
	})
}

func relayClientDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func relayClientSOCKSExercise(t *testing.T, cfg relayResearchConfig, muxChunk int, rpcDelay time.Duration) {
	var upLost, downLost atomic.Bool
	var largestNormal atomic.Int64
	var fragmented atomic.Bool
	f := relayClientSOCKSFixture(t, cfg, muxChunk, func(b *relayResearchBroker) relayResearchExchange {
		httpExchange := relayClientHTTPExchange(t, b)
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			// A small simulated RTT makes the larger-object case long enough to
			// observe text interleaving rather than merely local CPU copy speed.
			delay := rpcDelay / 2
			if err := relayClientDelay(ctx, delay); err != nil {
				return nil, err
			}
			p, _ := b.codec.open(wire)
			for _, piece := range p.Pieces {
				if !piece.Priority {
					for old := largestNormal.Load(); int64(piece.Total) > old; old = largestNormal.Load() {
						if largestNormal.CompareAndSwap(old, int64(piece.Total)) {
							break
						}
					}
					if piece.Offset > 0 {
						fragmented.Store(true)
					}
				}
			}
			response, err := httpExchange(ctx, wire)
			if err != nil {
				return nil, err
			}
			if err = relayClientDelay(ctx, delay); err != nil {
				return nil, err
			}
			r, _ := b.codec.open(response)
			for _, piece := range r.Pieces {
				if !piece.Priority {
					for old := largestNormal.Load(); int64(piece.Total) > old; old = largestNormal.Load() {
						if largestNormal.CompareAndSwap(old, int64(piece.Total)) {
							break
						}
					}
					if piece.Offset > 0 {
						fragmented.Store(true)
					}
				}
			}
			if len(r.Committed) > 0 && upLost.CompareAndSwap(false, true) {
				return nil, errors.New("injected committed-UP reply loss")
			}
			if len(r.Pieces) > 0 && downLost.CompareAndSwap(false, true) {
				return nil, errors.New("injected PULL reply loss")
			}
			return response, nil
		}
	})
	payload := make([]byte, 16<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	var uploadSideEffects atomic.Int64
	bulkReached := make(chan string, 4)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			bulkReached <- r.URL.Path
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		case "/upload":
			bulkReached <- r.URL.Path
			h := sha256.New()
			n, e := io.Copy(h, io.LimitReader(r.Body, int64(len(payload))+1))
			if e != nil || n != int64(len(payload)) || !bytes.Equal(h.Sum(nil), want[:]) {
				http.Error(w, "mismatch", 400)
				return
			}
			uploadSideEffects.Add(1)
			_, _ = io.WriteString(w, hex.EncodeToString(h.Sum(nil)))
		case "/text":
			w.Header().Set("Content-Length", "64")
			_, _ = w.Write(bytes.Repeat([]byte("t"), 64))
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { f.close(); origin.Close() }()
	request := func(path string, body []byte) error {
		method := "GET"
		if body != nil {
			method = "POST"
		}
		req, e := http.NewRequestWithContext(f.ctx, method, origin.URL+path, bytes.NewReader(body))
		if e != nil {
			return e
		}
		response, e := f.http.Do(req)
		if e != nil {
			return e
		}
		defer response.Body.Close()
		got, e := io.ReadAll(io.LimitReader(response.Body, int64(len(payload))+1))
		if e != nil {
			return e
		}
		if response.StatusCode != 200 {
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		switch path {
		case "/download":
			if len(got) != len(payload) || sha256.Sum256(got) != want {
				return errors.New("download exact-byte/hash mismatch")
			}
		case "/upload":
			if string(got) != hex.EncodeToString(want[:]) {
				return errors.New("upload receipt mismatch")
			}
		case "/text":
			if !bytes.Equal(got, bytes.Repeat([]byte("t"), 64)) {
				return errors.New("text exact-byte mismatch")
			}
		}
		return nil
	}
	if err := request("/text", nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/download", "/upload"} {
		bulkDone := make(chan error, 1)
		var finished atomic.Bool
		go func() {
			var body []byte
			if path == "/upload" {
				body = payload
			}
			err := request(path, body)
			finished.Store(true)
			bulkDone <- err
		}()
		select {
		case started := <-bulkReached:
			if started != path {
				t.Fatal("wrong bulk phase reached origin")
			}
		case <-f.ctx.Done():
			t.Fatal("bulk never reached origin", path)
		}
		completedWhileBulkActive := 0
		for i := 0; i < 8; i++ {
			if err := request("/text", nil); err != nil {
				t.Fatal(path, "mixed text", i, err)
			}
			if !finished.Load() {
				completedWhileBulkActive++
			}
		}
		if err := <-bulkDone; err != nil {
			t.Fatal(path, err, f.client.snapshot(), f.broker.snapshot())
		}
		if completedWhileBulkActive == 0 {
			t.Fatal("no text completed during active bulk", path)
		}
	}
	if !upLost.Load() || !downLost.Load() || uploadSideEffects.Load() != 1 {
		t.Fatal("faults/side effects not exercised", upLost.Load(), downLost.Load(), uploadSideEffects.Load())
	}
	if muxChunk >= 4<<20 && (largestNormal.Load() < 3<<20 || !fragmented.Load()) {
		t.Fatal("4MiB-class normal object/fragment condition not exercised", largestNormal.Load(), fragmented.Load())
	}
	// Measurement work has finished. Flush already-processed objects through
	// the mux's existing emergency cleanup path rather than waiting its normal
	// 10–15 second foreground quiet timer in this loopback correctness test.
	f.http.CloseIdleConnections()
	relayClientWait(t, func() bool {
		s := f.broker.snapshot()
		if s.UpBytes == 0 && s.DownBytes == 0 {
			return true
		}
		f.cm.requestCleanupKick()
		f.em.requestCleanupKick()
		return false
	}, "processed ciphertext cleanup incomplete")
	before := f.broker.snapshot()
	f.close()
	if f.broker.snapshot().LiveWorkers != 0 || f.client.snapshot()["up_bytes"] != 0 || f.client.snapshot()["down_bytes"] != 0 {
		t.Fatal("session leaked after cancellation")
	}
	t.Logf("loopback-only exact16MiB GET+POST and17 text replies; simulated_rpc=%s max_normal_sealed=%d fragmented=%t; no live-speed claim; broker=%+v", rpcDelay, largestNormal.Load(), fragmented.Load(), before)
}

func TestRelayClientActualOPENRetryAfterMuxClosedTTLDoesNotRepeatSideEffect(t *testing.T) {
	b, c := relayClientTestBroker(t, relayResearchConfig{FrameBytes: 128 << 10, PollWait: 5 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sid, _ := NewSessionID()
	secret := "relay-open-replay"
	tunnel := &Tunnel{Data: b.exitStore(), Secret: secret, SessionID: sid, ClientID: "client-a", RunID: "run-a", Logger: log.New(io.Discard, "", 0)}
	m, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.closeAll()
	initial := []byte("one destination side effect")
	var dials, writes atomic.Int64
	written := make(chan struct{}, 2)
	var observers sync.WaitGroup
	m.dialExit = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		a, z := net.Pipe()
		observers.Add(1)
		go func() {
			defer observers.Done()
			defer z.Close()
			got := make([]byte, len(initial))
			if _, e := io.ReadFull(z, got); e == nil && bytes.Equal(got, initial) {
				writes.Add(1)
				written <- struct{}{}
			}
			<-ctx.Done()
		}()
		return a, nil
	}
	validate, _ := relayResearchMuxHooks(m, ctx)
	var processed atomic.Int64
	b.setUpHooks(validate, func(info ObjectInfo) {
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			return
		}
		if e := m.processMuxObject(ctx, meta); e == nil {
			processed.Add(1)
		}
	})
	raw, _ := encodeMuxBatch([]muxFrame{{Kind: muxFrameOpen, StreamID: 99, Payload: encodeMuxOpenPayload("example.test:443", initial)}})
	key, _ := DeriveMuxLaneKeyV4(secret, sid, DirectionUp, "client-a", "run-a", 0)
	sealed, _ := Seal(key, sid, DirectionUp, 42, raw, false)
	name := muxObjectNameWithStreamIDs(sid, DirectionUp, "client-a", "run-a", "relay-open", 99, []uint64{99}, 0, 42, 1, 0, 0, len(raw), true)
	info, err := c.PutObject(ctx, name, sealed)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("OPEN initial bytes missing")
	}
	if err = b.exitStore().DeleteID(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	streamKey := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 99}
	stream := m.streamByKey(streamKey)
	if stream == nil {
		t.Fatal("destination stream missing")
	}
	stream.close()
	m.streamsMu.Lock()
	m.closed[streamKey] = time.Now().Add(-time.Second)
	m.streamsMu.Unlock()
	if m.isClosedStream(streamKey) {
		t.Fatal("closed mux claim did not expire")
	}
	// Replay the same immutable OPEN at a new authenticated RPC sequence,
	// bypassing the client's own duplicate cache to exercise the broker ledger.
	p := relayResearchPacket{Kind: rrUp, Seq: 2, Receipt: 1, Pieces: []relayResearchPiece{{ID: info.ID, Name: name, Total: len(sealed), Hash: sha256.Sum256(sealed), Data: sealed, Priority: true}}}
	wire, _ := b.codec.seal(p)
	response, err := b.exchange(ctx, wire)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := b.codec.open(response)
	if reply.Status != rrStatusOK || len(reply.Committed) != 1 || dials.Load() != 1 || writes.Load() != 1 || processed.Load() != 1 {
		t.Fatal("duplicate OPEN repeated a destination effect", reply, dials.Load(), writes.Load(), processed.Load())
	}
	cancel()
	m.closeAll()
	observers.Wait()
}
