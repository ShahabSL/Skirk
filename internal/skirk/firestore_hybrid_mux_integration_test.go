package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All carriers and destinations in this file are owned loopback/memory
// fixtures. Timed pauses enforce simultaneous text and bulk correctness;
// their durations are not measurements of Google or transport performance.
type fsHybridMuxFixture struct {
	ctx          context.Context
	cancel       context.CancelFunc
	client, exit *fsHybridPriorityStore
	cloud        *fsHybridMemoryCloud
	base         *MemoryStore
	cm, em       *driveMux
	http         *http.Client
	workers      sync.WaitGroup
	closeOnce    sync.Once
}

// Track the production start() loops individually, including the urgent
// batch loop which runBatchLoop otherwise starts internally. Stream sockets
// are closed before this join; Store.Close separately joins adapter workers.
func (f *fsHybridMuxFixture) startMux(m *driveMux) {
	launch := func(fn func()) { f.workers.Add(1); go func() { defer f.workers.Done(); fn() }() }
	for _, lane := range m.lanes {
		launch(func() { lane.runUrgentBatchLoop(f.ctx) })
		launch(func() { lane.runFairNormalBatchLoop(f.ctx) })
		for i := 0; i < m.priorityUploadWorkersPerLane(); i++ {
			launch(func() { lane.runUploadLoop(f.ctx, true) })
		}
		for i := 0; i < m.uploadWorkersPerLane(); i++ {
			launch(func() { lane.runUploadLoop(f.ctx, false) })
		}
	}
	p, n := m.receiveWorkerCounts()
	for i := 0; i < p; i++ {
		launch(func() { m.runReceiveWorker(f.ctx, true) })
	}
	for i := 0; i < n; i++ {
		launch(func() { m.runReceiveWorker(f.ctx, false) })
	}
	launch(func() { m.runCleanupLoop(f.ctx) })
	launch(func() { m.runReceiveLoop(f.ctx) })
}

func fsHybridFullMuxFixture(t *testing.T, socks bool) *fsHybridMuxFixture {
	t.Helper()
	c, e, cloud, base := fsHybridMemoryStores(t)
	f := fsHybridFullMuxFixtureWithStores(t, c, e, base, socks)
	f.cloud = cloud
	return f
}

func fsHybridFullMuxFixtureWithStores(t *testing.T, c, e *fsHybridPriorityStore, base *MemoryStore, socks bool) *fsHybridMuxFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	f := &fsHybridMuxFixture{ctx: ctx, cancel: cancel, client: c, exit: e, base: base}
	t.Cleanup(func() { f.close(t) })
	cfg := &Config{Secret: c.cfg.Secret, SessionID: hex.EncodeToString(c.cfg.Session[:]), Client: ClientConfig{ID: c.cfg.ClientID, RunID: c.cfg.RunID}, Tunnel: TunnelConfig{Transport: "muxv4", Profile: "fixed", PollIntervalMS: 5, ChunkSize: 4 << 20, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true}}
	cfg.ApplyDefaults()
	ct, err := NewTunnel(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	et, err := NewTunnel(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ct.Logger = log.New(io.Discard, "", 0)
	et.Logger = ct.Logger
	ct.setRole("client")
	et.setRole("exit")
	f.cm, err = newDriveMux(ct, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	f.em, err = newDriveMux(et, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.bindMux(f.cm, ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.bindMux(f.em, ctx); err != nil {
		t.Fatal(err)
	}
	// Store failure terminates the actual workload too, rather than just
	// fencing its callback while sockets remain alive until a test timeout.
	for _, s := range []*fsHybridPriorityStore{c, e} {
		f.workers.Add(1)
		go func() {
			defer f.workers.Done()
			select {
			case <-ctx.Done():
			case <-s.Done():
				cancel()
			}
		}()
	}
	if !socks {
		return f
	}
	f.startMux(f.cm)
	f.startMux(f.em)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	server := SOCKSServer{Listen: addr, Logger: ct.Logger, Handler: func(ctx context.Context, target string, conn net.Conn) { _ = f.cm.openClientStream(ctx, target, conn) }}
	f.workers.Add(1)
	go func() { defer f.workers.Done(); _ = server.Serve(ctx) }()
	fsHybridEventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Millisecond*10)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	transport := &http.Transport{MaxConnsPerHost: 32, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
		return dialViaSOCKS5(ctx, "socks5h://"+addr, target)
	}}
	f.http = &http.Client{Transport: transport, Timeout: 15 * time.Second}
	return f
}

func (f *fsHybridMuxFixture) close(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		if f.http != nil {
			f.http.CloseIdleConnections()
		}
		f.cancel()
		if f.cm != nil {
			f.cm.closeAll()
		}
		if f.em != nil {
			f.em.closeAll()
		}
		for _, s := range []*fsHybridPriorityStore{f.client, f.exit} {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
		joined := make(chan struct{})
		go func() { f.workers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned mux loops failed to join")
			return
		}
		if f.cm != nil {
			f.cm.closeAll()
		}
		if f.em != nil {
			f.em.closeAll()
		}
		for _, m := range []*driveMux{f.cm, f.em} {
			if m != nil && (m.active.Load() != 0 || m.t.activeStreams.Load() != 0) {
				t.Errorf("owned mux streams remain: %d/%d", m.active.Load(), m.t.activeStreams.Load())
			}
		}
		for _, s := range []*fsHybridPriorityStore{f.client, f.exit} {
			if snap := s.Snapshot(); snap["joined"] != 1 || snap["send_payload_bytes"] != 0 || snap["receive_payload_bytes"] != 0 || snap["pending_records"] != 0 || snap["pending_prefetch"] != 0 || snap["queued_prefetch"] != 0 || snap["queued_fallback"] != 0 {
				t.Errorf("adapter workers/payload retained: %v", snap)
			}
			targets, err := s.CleanupTargets()
			if err != nil {
				t.Error(err)
				continue
			}
			for _, target := range targets {
				if err = f.base.DeleteID(context.Background(), target.ID); err != nil {
					t.Error(err)
				}
			}
		}
		// Priority target IDs do not include ordinary p1 bulk files. Delete
		// both exact fresh run prefixes, retaining an unrelated sentinel.
		for _, d := range []byte{DirectionUp, DirectionDown} {
			prefix := muxDirPrefix(f.client.cfg.Session, d, f.client.cfg.ClientID, f.client.cfg.RunID)
			objects, err := f.base.List(context.Background(), prefix)
			if err != nil {
				t.Error(err)
				continue
			}
			for _, obj := range objects {
				if !strings.HasPrefix(obj.Name, prefix) {
					t.Error("foreign cleanup name")
					continue
				}
				if obj.ID != "" {
					err = f.base.DeleteID(context.Background(), obj.ID)
				} else {
					err = f.base.Delete(context.Background(), obj.Name)
				}
				if err != nil {
					t.Error(err)
				}
			}
			objects, _ = f.base.List(context.Background(), prefix)
			if len(objects) != 0 {
				t.Error("owned run prefix not empty")
			}
		}
	})
}

func TestFSHybridRealSOCKS16MiBEachDirectionMixedTextFallbackReconnect(t *testing.T) {
	f := fsHybridFullMuxFixture(t, true)
	runFSHybridTLSMixedWorkload(t, f, nil, nil, nil)
}

func runFSHybridTLSMixedWorkload(t *testing.T, f *fsHybridMuxFixture, before, afterGET, afterStop func()) {
	t.Helper()
	const size = 16 << 20
	down := make([]byte, size)
	up := make([]byte, size)
	if _, err := rand.Read(down); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(up); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(down) == sha256.Sum256(up) {
		t.Fatal("independent file fixtures collided")
	}
	var gets, posts, texts, bad atomic.Int64
	var downloaded, uploaded atomic.Int64
	getStarted, postStarted := make(chan struct{}), make(chan struct{})
	getRelease, postRelease := make(chan struct{}), make(chan struct{})
	var getOnce, postOnce sync.Once
	t.Cleanup(func() { getOnce.Do(func() { close(getRelease) }); postOnce.Do(func() { close(postRelease) }) })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		switch r.URL.Path {
		case "/download":
			if r.Method != http.MethodGet {
				bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			gets.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(size))
			for offset := 0; offset < size; offset += 64 << 10 {
				n, err := w.Write(down[offset : offset+(64<<10)])
				downloaded.Add(int64(n))
				if err != nil {
					bad.Add(1)
					return
				}
				if offset == 8<<20 {
					close(getStarted)
					select {
					case <-getRelease:
					case <-f.ctx.Done():
						return
					}
				}
			}
		case "/upload":
			if r.Method != http.MethodPost {
				bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			posts.Add(1)
			h := sha256.New()
			buf := make([]byte, 64<<10)
			for {
				n, err := r.Body.Read(buf)
				if n > 0 {
					h.Write(buf[:n])
					v := uploaded.Add(int64(n))
					if v >= 8<<20 && v-int64(n) < 8<<20 {
						close(postStarted)
						select {
						case <-postRelease:
						case <-f.ctx.Done():
							return
						}
					}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					bad.Add(1)
					return
				}
			}
			if uploaded.Load() != size || !bytes.Equal(h.Sum(nil), func() []byte { s := sha256.Sum256(up); return s[:] }()) {
				bad.Add(1)
				http.Error(w, "hash", 400)
				return
			}
			_, _ = w.Write([]byte("verified"))
		case "/echo":
			b, err := io.ReadAll(io.LimitReader(r.Body, 65))
			if err != nil || r.Method != http.MethodPost || len(b) != 64 {
				bad.Add(1)
				http.Error(w, "echo", 400)
				return
			}
			texts.Add(1)
			_, _ = w.Write(b)
		default:
			bad.Add(1)
			http.NotFound(w, r)
		}
	}))
	// Trust only this owned TLS origin's certificate. SOCKS still carries
	// the complete TLS handshake and binary records; verification stays on.
	root := x509.NewCertPool()
	root.AddCert(origin.Certificate())
	f.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: root, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() {
		getOnce.Do(func() { close(getRelease) })
		postOnce.Do(func() { close(postRelease) })
		f.close(t)
		origin.Close()
	})
	ownedTarget := origin.Listener.Addr().String()
	f.em.dialExit = func(ctx context.Context, target string) (net.Conn, error) {
		if target != ownedTarget {
			return nil, fmt.Errorf("unowned loopback destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	if err := f.base.Put(f.ctx, "unrelated/sentinel", []byte("retain")); err != nil {
		t.Fatal(err)
	}
	// A dropped/coalesced cumulative notification must yield a normal Drive
	// fallback of the exact prepared priority record, not a different OPEN.
	if f.cloud != nil {
		f.cloud.setDrop(0, true)
	}
	if before != nil {
		before()
	}
	type result struct {
		body []byte
		err  error
	}
	getDone := make(chan result, 1)
	go func() {
		resp, err := f.http.Get(origin.URL + "/download")
		if err != nil {
			getDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			err = fmt.Errorf("GET status %d", resp.StatusCode)
		}
		getDone <- result{body, err}
	}()
	select {
	case <-getStarted:
	case <-f.ctx.Done():
		t.Fatal("actual GET bytes never reached origin", f.client.Error(), f.exit.Error())
	}
	assertPhysicalNormal := func(direction byte) {
		t.Helper()
		// TCP read boundaries and fair batching differ across platforms. An
		// actual p1 object proves the bulk path; requiring any one object to
		// exceed the p0 carrier bound makes this test depend on socket timing.
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			objects, err := f.base.List(f.ctx, muxDirPrefix(f.client.cfg.Session, direction, f.client.cfg.ClientID, f.client.cfg.RunID))
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range objects {
				if meta, ok := parseMuxObjectInfo(obj); ok && !meta.Priority && obj.Size > 0 {
					return
				}
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("no physical normal object during bulk transfer", direction)
			case <-f.ctx.Done():
				t.Fatal("bulk transfer ended before a physical normal object", direction, f.ctx.Err())
			}
		}
	}
	assertPhysicalNormal(DirectionDown)
	fsHybridEventually(t, func() bool { return f.client.Snapshot()["fallback_published"] > 0 })
	if f.cloud != nil {
		f.cloud.setDrop(0, false)
		f.cloud.failWatch(1, context.DeadlineExceeded)
		fsHybridEventually(t, func() bool { return f.exit.Snapshot()["watch_reconnects"] == 1 })
		f.cloud.deliver(0)
	}
	if afterGET != nil {
		afterGET()
	}
	textBatch := func(seed int, bulkBytes *atomic.Int64) {
		t.Helper()
		for i := 0; i < 20; i++ {
			payload := bytes.Repeat([]byte{byte(seed + i)}, 64)
			req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, origin.URL+"/echo", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := f.http.Do(req)
			if err != nil {
				t.Fatal("text while bulk", err, f.client.Error(), f.exit.Error())
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, payload) {
				t.Fatal("text mismatch", i, err)
			}
			if n := bulkBytes.Load(); n <= 0 || n >= size {
				t.Fatal("text did not complete between actual bulk start and completion", n)
			}
		}
	}
	textBatch(1, &downloaded)
	getOnce.Do(func() { close(getRelease) })
	get := <-getDone
	if get.err != nil || len(get.body) != size || sha256.Sum256(get.body) != sha256.Sum256(down) {
		t.Fatal("GET exact hash/size", len(get.body), get.err)
	}
	postDone := make(chan result, 1)
	go func() {
		resp, err := f.http.Post(origin.URL+"/upload", "application/octet-stream", bytes.NewReader(up))
		if err != nil {
			postDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			err = fmt.Errorf("POST status %d", resp.StatusCode)
		}
		postDone <- result{body, err}
	}()
	select {
	case <-postStarted:
	case <-f.ctx.Done():
		t.Fatal("actual POST bytes never reached origin")
	}
	assertPhysicalNormal(DirectionUp)
	textBatch(40, &uploaded)
	postOnce.Do(func() { close(postRelease) })
	post := <-postDone
	if post.err != nil || string(post.body) != "verified" {
		t.Fatal("POST exact hash/size", post.err, string(post.body))
	}
	if gets.Load() != 1 || posts.Load() != 1 || texts.Load() != 40 || bad.Load() != 0 {
		t.Fatal("unexpected origin side effects", gets.Load(), posts.Load(), texts.Load(), bad.Load())
	}
	for _, s := range []*fsHybridPriorityStore{f.client, f.exit} {
		fsHybridEventually(t, func() bool {
			return s.Snapshot()["processed_receipts"] > 0 && s.Snapshot()["peer_processed_prefix"] > 0
		})
		if err := s.Error(); err != nil {
			t.Fatal(err)
		}
	}
	beforeC, beforeE := f.client.Snapshot(), f.exit.Snapshot()
	f.close(t)
	origin.Close()
	if gets.Load() != 1 || posts.Load() != 1 || texts.Load() != 40 || bad.Load() != 0 {
		t.Fatal("late origin side effect after joined teardown", gets.Load(), posts.Load(), texts.Load(), bad.Load())
	}
	if got, err := f.base.Get(context.Background(), "unrelated/sentinel"); err != nil || string(got) != "retain" {
		t.Fatal("unrelated object changed", err)
	}
	if remaining, _ := f.base.List(context.Background(), ""); len(remaining) != 1 {
		t.Fatal("normal/priority cleanup left objects", remaining)
	}
	if afterStop != nil {
		afterStop()
	}
	t.Logf("loopback only: exact independent16MiB GET+POST,40 overlapping text echoes; processing receipts client=%d exit=%d; fallback=%d reconnect=%d; no live speed claim", beforeC["processed_receipts"], beforeE["processed_receipts"], beforeC["fallback_published"], beforeE["watch_reconnects"])
}

func TestFSHybridActualOPENLifetimeClaimAcrossBothArrivalOrders(t *testing.T) {
	for _, physicalFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("physical-first=", physicalFirst), func(t *testing.T) {
			f := fsHybridFullMuxFixture(t, false)
			initial := []byte("one owned destination side effect")
			var dials, writes atomic.Int64
			var dispatched atomic.Int64
			f.exit.mu.Lock()
			receive := f.exit.receive
			f.exit.receive = func(info ObjectInfo) bool { dispatched.Add(1); return receive(info) }
			f.exit.mu.Unlock()
			var remoteWG sync.WaitGroup
			t.Cleanup(func() { f.close(t); remoteWG.Wait() })
			f.em.dialExit = func(context.Context, string) (net.Conn, error) {
				dials.Add(1)
				a, z := net.Pipe()
				remoteWG.Add(1)
				go func() {
					defer remoteWG.Done()
					defer z.Close()
					got := make([]byte, len(initial))
					if _, err := io.ReadFull(z, got); err == nil && bytes.Equal(got, initial) {
						writes.Add(1)
					}
					<-f.ctx.Done()
				}()
				return a, nil
			}
			f.startMux(f.em)
			r := fsHybridTestRecord(t, f.client.state.outgoing, 1, 0, muxFrame{Kind: muxFrameOpen, StreamID: 99, Payload: encodeMuxOpenPayload("owned.invalid:443", initial)})
			if physicalFirst {
				f.cloud.setDrop(0, true)
			}
			if _, err := f.client.PutObject(f.ctx, r.Name, r.Sealed); err != nil {
				t.Fatal(err)
			}
			if physicalFirst {
				f.client.forceFallback(r.Name)
			}
			fsHybridEventually(t, func() bool { return writes.Load() == 1 && f.exit.Snapshot()["processed_receipts"] == 1 })
			if physicalFirst {
				f.client.mu.Lock()
				out := f.client.state.outNames[r.Name]
				id, digest := out.physicalID, out.digest
				f.client.mu.Unlock()
				got, err := f.base.GetByID(f.ctx, id)
				if err != nil || !bytes.Equal(got, r.Sealed) || fsHybridIdentity(r.Name, got) != digest {
					t.Fatal("fallback did not preserve exact prepared ciphertext", err)
				}
			}
			key := muxStreamKey{ClientID: f.client.cfg.ClientID, RunID: f.client.cfg.RunID, StreamID: 99}
			stream := f.em.streamByKey(key)
			if stream == nil {
				t.Fatal("actual destination stream missing")
			}
			stream.close()
			f.em.streamsMu.Lock()
			delete(f.em.closed, key)
			f.em.streamsMu.Unlock()
			f.em.seenMu.Lock()
			delete(f.em.seen, r.Name)
			delete(f.em.processed, r.Name)
			delete(f.em.queued, r.Name)
			delete(f.em.knownIDs, r.Name)
			delete(f.em.primaryIDs, r.Name)
			f.em.seenMu.Unlock()
			if physicalFirst {
				f.cloud.setDrop(0, false)
				f.cloud.deliver(0)
				fsHybridEventually(t, func() bool { return f.exit.Snapshot()["processed_prefix"] == 1 })
			} else {
				ids, err := f.base.GenerateObjectIDs(f.ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.base.PutObjectWithID(f.ctx, ids[0], r.Name, r.Sealed); err != nil {
					t.Fatal(err)
				}
				// Trigger exact namespace discovery, which must attach a physical
				// alias to the lifetime processed claim instead of redispatching it.
				f.exit.signal(f.exit.discoverWake)
				fsHybridEventually(t, func() bool {
					f.exit.mu.Lock()
					defer f.exit.mu.Unlock()
					return f.exit.state.rxPhysical[ids[0]] != nil && len(f.exit.prefetchPending) == 0
				})
			}
			// A synchronous cumulative ingest proves replay handling completed;
			// elapsed sleep alone would not prove the duplicate was examined.
			f.cloud.mu.Lock()
			doc := fsHybridMemoryClone(f.cloud.docs[0])
			f.cloud.mu.Unlock()
			if err := f.exit.ingest(doc); err != nil {
				t.Fatal(err)
			}
			if dials.Load() != 1 || writes.Load() != 1 || dispatched.Load() != 1 || f.exit.Snapshot()["processed_receipts"] != 1 {
				t.Fatal("OPEN replay repeated a destination effect", dials.Load(), writes.Load(), f.exit.Snapshot())
			}
			f.close(t)
			remoteWG.Wait()
			if dials.Load() != 1 || writes.Load() != 1 || dispatched.Load() != 1 {
				t.Fatal("late duplicate OPEN side effect or enqueue after stop", dials.Load(), writes.Load(), dispatched.Load())
			}
		})
	}
}

func TestFSHybridFullMuxWorkloadCanceledOnStoreFatalError(t *testing.T) {
	f := fsHybridFullMuxFixture(t, true)
	f.client.fail(&GoogleAPIError{Op: "owned_fixture", Status: 403, Reason: "PERMISSION_DENIED"})
	select {
	case <-f.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("store fatal error did not cancel actual mux/workload context")
	}
	if f.client.Error() == nil {
		t.Fatal("fatal store error lost")
	}
	f.close(t)
}
