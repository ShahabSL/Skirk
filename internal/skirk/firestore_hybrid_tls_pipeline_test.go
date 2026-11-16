package skirk

// A separate research workload: pipelined frames on one owned TLS connection.
// HTTP/1.1 request serialization is not part of the framed echo path.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fsHybridPipelineFrame(seed []byte, sequence int) []byte {
	frame := make([]byte, 64)
	binary.BigEndian.PutUint64(frame, uint64(sequence))
	for block := 0; block < 2; block++ {
		mac := hmac.New(sha256.New, seed)
		_, _ = mac.Write([]byte("skirk-owned-pipelined-echo-v1"))
		_, _ = mac.Write(frame[:8])
		_, _ = mac.Write([]byte{byte(block)})
		copy(frame[8+block*32:], mac.Sum(nil))
	}
	return frame
}

type fsHybridPipelineSlot struct {
	row               fsHybridLiveRow
	started, planned  time.Time
	offered, complete bool
	done              chan struct{}
}

func (r *fsHybridLiveRun) pipelinedWorkload(holdBulkForProof bool) error {
	return r.pipelinedWorkloadWithResponseHook(holdBulkForProof, nil)
}

func (r *fsHybridLiveRun) pipelinedWorkloadWithResponseHook(holdBulkForProof bool, responseHook func(int, []byte)) (result error) {
	seed, _ := hex.DecodeString(r.settings.PayloadSeedHex)
	if len(seed) != 32 {
		return errFSHybridBounds
	}
	down, up := fsHybridLivePayload(seed, 1), fsHybridLivePayload(seed, 2)
	upHash := sha256.Sum256(up)
	r.downloadStarted, r.uploadStarted = make(chan struct{}), make(chan struct{})
	downRelease, upRelease := make(chan struct{}), make(chan struct{})
	var downReleaseOnce, upReleaseOnce sync.Once
	defer func() {
		downReleaseOnce.Do(func() { close(downRelease) })
		upReleaseOnce.Do(func() { close(upRelease) })
	}()
	var streamMu sync.Mutex
	streamTaken := false
	streamClosing := false
	var streamConn net.Conn
	var streamOwners sync.WaitGroup
	defer func() {
		streamMu.Lock()
		streamClosing = true
		conn := streamConn
		streamMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		streamOwners.Wait()
	}()
	r.origin = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		switch req.URL.Path {
		case "/stream":
			streamMu.Lock()
			already := streamTaken || streamClosing || r.ctx.Err() != nil
			streamTaken = true
			if !already {
				streamOwners.Add(1)
			}
			streamMu.Unlock()
			if already || req.Method != http.MethodGet || req.Header.Get("Upgrade") != "skirk-research-echo-v1" {
				if !already {
					streamOwners.Done()
				}
				r.bad.Add(1)
				http.Error(w, "stream", 400)
				return
			}
			defer streamOwners.Done()
			hijack, ok := w.(http.Hijacker)
			if !ok {
				r.bad.Add(1)
				return
			}
			conn, rw, e := hijack.Hijack()
			if e != nil {
				r.bad.Add(1)
				return
			}
			defer conn.Close()
			streamMu.Lock()
			streamConn = conn
			streamMu.Unlock()
			if deadline, ok := r.ctx.Deadline(); ok {
				_ = conn.SetDeadline(deadline)
			}
			stop := context.AfterFunc(r.ctx, func() { _ = conn.Close() })
			defer stop()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: skirk-research-echo-v1\r\n\r\n")
			if e = rw.Flush(); e != nil {
				return
			}
			for sequence := 0; sequence < 41; sequence++ {
				frame := make([]byte, 64)
				if _, e = io.ReadFull(rw, frame); e != nil {
					return
				}
				if !bytes.Equal(frame, fsHybridPipelineFrame(seed, sequence)) {
					r.bad.Add(1)
					return
				}
				r.texts.Add(1)
				reply := frame
				if responseHook != nil {
					reply = bytes.Clone(frame)
					responseHook(sequence, reply)
				}
				if _, e = rw.Write(reply); e != nil {
					return
				}
				if e = rw.Flush(); e != nil {
					return
				}
			}
		case "/download":
			if req.Method != http.MethodGet {
				r.bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			r.gets.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(down)))
			for offset := 0; offset < len(down); offset += 64 << 10 {
				if _, e := w.Write(down[offset : offset+(64<<10)]); e != nil {
					return
				}
				if offset >= 256<<10 {
					r.downloadOnce.Do(func() { close(r.downloadStarted) })
				}
				if holdBulkForProof && offset == 8<<20 {
					select {
					case <-downRelease:
					case <-r.ctx.Done():
						return
					}
				}
			}
		case "/upload":
			if req.Method != http.MethodPost {
				r.bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			r.posts.Add(1)
			hash := sha256.New()
			total := 0
			buf := make([]byte, 64<<10)
			for {
				n, e := req.Body.Read(buf)
				total += n
				_, _ = hash.Write(buf[:n])
				if total >= 256<<10 {
					r.uploadOnce.Do(func() { close(r.uploadStarted) })
				}
				if holdBulkForProof && total >= 8<<20 {
					select {
					case <-upRelease:
					case <-r.ctx.Done():
						return
					}
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					return
				}
				if total > len(up) {
					r.bad.Add(1)
					http.Error(w, "size", 400)
					return
				}
			}
			if total != len(up) || !bytes.Equal(hash.Sum(nil), upHash[:]) {
				r.bad.Add(1)
				http.Error(w, "hash", 400)
				return
			}
			_, _ = w.Write([]byte(hex.EncodeToString(upHash[:])))
		default:
			r.bad.Add(1)
			http.NotFound(w, req)
		}
	}))
	owned := r.origin.Listener.Addr().String()
	r.em.dialExit = func(ctx context.Context, target string) (net.Conn, error) {
		if target != owned {
			return nil, errFSHybridBounds
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	r.startMux(r.cm)
	r.startMux(r.em)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	address := l.Addr().String()
	_ = l.Close()
	socks := SOCKSServer{Listen: address, Logger: r.logger, Handler: func(ctx context.Context, target string, conn net.Conn) { _ = r.cm.openClientStream(ctx, target, conn) }}
	r.launch(func() { _ = socks.Serve(r.ctx) })
	ready := false
	for i := 0; i < 100; i++ {
		conn, e := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if e == nil {
			conn.Close()
			ready = true
			break
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !ready {
		return errFSHybridClosed
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.origin.Certificate())
	tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	dial := func(ctx context.Context) (net.Conn, error) { return dialViaSOCKS5(ctx, "socks5h://"+address, owned) }
	r.bulkHTTP = &http.Client{Timeout: 180 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, MaxConnsPerHost: 1, DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
		if target != owned {
			return nil, errFSHybridBounds
		}
		return dial(ctx)
	}}}
	coldStarted := time.Now()
	raw, e := dial(r.ctx)
	if e != nil {
		return e
	}
	tlsConnConfig := tlsConfig.Clone()
	tlsConnConfig.ServerName = "127.0.0.1"
	conn := tls.Client(raw, tlsConnConfig)
	defer conn.Close()
	stopRaw := context.AfterFunc(r.ctx, func() { _ = conn.Close() })
	defer stopRaw()
	if deadline, ok := r.ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if e = conn.HandshakeContext(r.ctx); e != nil {
		return e
	}
	if _, e = io.WriteString(conn, "GET /stream HTTP/1.1\r\nHost: "+owned+"\r\nConnection: Upgrade\r\nUpgrade: skirk-research-echo-v1\r\n\r\n"); e != nil {
		return e
	}
	reader := bufio.NewReader(conn)
	response, e := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if e != nil {
		return e
	}
	if response.StatusCode != 101 || response.Header.Get("Upgrade") != "skirk-research-echo-v1" {
		return errFSHybridIntegrity
	}
	_ = response.Body.Close()
	r.journal.write(map[string]any{"phase": "pipelined_tls_ready", "cold_connect_handshake_upgrade_ms": float64(time.Since(coldStarted).Microseconds()) / 1000})
	var slotsMu sync.Mutex
	var slots [41]fsHybridPipelineSlot
	for i := range slots {
		slots[i].done = make(chan struct{})
	}
	readDone := make(chan struct{})
	var readErr error
	r.launch(func() {
		defer close(readDone)
		for sequence := 0; sequence < 41; sequence++ {
			frame := make([]byte, 64)
			_, err := io.ReadFull(reader, frame[:1])
			firstByte := time.Now()
			if err == nil {
				_, err = io.ReadFull(reader, frame[1:])
			}
			complete := time.Now()
			if err != nil {
				slotsMu.Lock()
				readErr = err
				slotsMu.Unlock()
				r.journal.write(map[string]any{"phase": "pipelined_frame_read_failed", "sequence": sequence, "category": hybridHTTPFailure(err)})
				r.cancel()
				return
			}
			slotsMu.Lock()
			slot := &slots[sequence]
			if !slot.offered || !bytes.Equal(frame, fsHybridPipelineFrame(seed, sequence)) {
				readErr = errFSHybridIntegrity
				slotsMu.Unlock()
				r.journal.write(map[string]any{"phase": "pipelined_frame_invalid", "sequence": sequence, "category": "sequence_or_payload"})
				r.cancel()
				return
			}
			slot.complete = true
			row := slot.row
			row.OK = true
			row.Bytes = 64
			hash := sha256.Sum256(frame)
			row.SHA256 = hex.EncodeToString(hash[:])
			row.TTFBMS = float64(firstByte.Sub(slot.started).Microseconds()) / 1000
			row.RTTMS = float64(complete.Sub(slot.started).Microseconds()) / 1000
			row.ScheduledMS = float64(complete.Sub(slot.planned).Microseconds()) / 1000
			row.CompletedDuringBulk = r.bulkActive.Load()
			close(slot.done)
			slotsMu.Unlock()
			r.journal.row(row)
		}
	})
	defer func() {
		_ = conn.Close()
		<-readDone
		slotsMu.Lock()
		if readErr != nil {
			result = errors.Join(result, readErr)
		}
		for i := range slots {
			slot := &slots[i]
			if slot.offered && !slot.complete {
				row := slot.row
				row.Failure = "pipelined_frame_aborted"
				if errors.Is(readErr, errFSHybridIntegrity) {
					row.Failure = "pipelined_frame_integrity"
				}
				row.RTTMS = float64(time.Since(slot.started).Microseconds()) / 1000
				row.ScheduledMS = float64(time.Since(slot.planned).Microseconds()) / 1000
				r.journal.row(row)
			}
		}
		slotsMu.Unlock()
	}()
	send := func(sequence int, kind string, planned time.Time) error {
		r.journal.write(map[string]any{"phase": "pipelined_frame_offered", "sequence": sequence, "kind": kind})
		slotsMu.Lock()
		slots[sequence].offered = true
		slots[sequence].planned = planned
		slots[sequence].started = time.Now()
		slots[sequence].row = fsHybridLiveRow{Kind: kind, Sequence: sequence, OfferedDuringBulk: r.bulkActive.Load()}
		slotsMu.Unlock()
		if n, e := conn.Write(fsHybridPipelineFrame(seed, sequence)); e != nil || n != 64 {
			r.cancel()
			if e != nil {
				return e
			}
			return io.ErrShortWrite
		}
		return nil
	}
	await := func(sequence int) error {
		slotsMu.Lock()
		complete := slots[sequence].complete
		slotsMu.Unlock()
		if complete {
			return nil
		}
		select {
		case <-slots[sequence].done:
			return nil
		case <-readDone:
			slotsMu.Lock()
			defer slotsMu.Unlock()
			if slots[sequence].complete {
				return nil
			}
			return errors.Join(errFSHybridClosed, readErr)
		case <-r.ctx.Done():
			return r.ctx.Err()
		}
	}
	if e = send(0, "warm_tls_frame", time.Now()); e != nil {
		return e
	}
	if e = await(0); e != nil {
		return e
	}
	bulkRequest := func(direction string) error {
		started := time.Now()
		row := fsHybridLiveRow{Kind: "pipeline_bulk_" + direction}
		method, path := http.MethodGet, "/download"
		var body io.Reader
		want := down
		if direction == "upload" {
			method, path = http.MethodPost, "/upload"
			body = bytes.NewReader(up)
			want = []byte(hex.EncodeToString(upHash[:]))
		}
		req, e := http.NewRequestWithContext(r.ctx, method, r.origin.URL+path, body)
		if e != nil {
			return e
		}
		resp, e := r.bulkHTTP.Do(req)
		row.TTFBMS = float64(time.Since(started).Microseconds()) / 1000
		if e != nil {
			row.Failure = hybridHTTPFailure(e)
			row.RTTMS = row.TTFBMS
			r.journal.row(row)
			r.cancel()
			return e
		}
		got, e := io.ReadAll(io.LimitReader(resp.Body, int64(len(want))+1))
		_ = resp.Body.Close()
		row.RTTMS = float64(time.Since(started).Microseconds()) / 1000
		row.ScheduledMS = row.RTTMS
		row.Bytes = len(got)
		sum := sha256.Sum256(got)
		row.SHA256 = hex.EncodeToString(sum[:])
		row.OK = e == nil && resp.StatusCode == 200 && bytes.Equal(got, want)
		if direction == "upload" && row.OK {
			row.Bytes = len(up)
		}
		if !row.OK {
			row.Failure = "pipeline_bulk_exact_body"
		}
		r.journal.row(row)
		if !row.OK {
			r.cancel()
			return errFSHybridIntegrity
		}
		return nil
	}
	for index, direction := range []string{"download", "upload"} {
		r.bulkActive.Store(true)
		bulkDone := make(chan error, 1)
		go func() { e := bulkRequest(direction); r.bulkActive.Store(false); bulkDone <- e }()
		started := r.downloadStarted
		if direction == "upload" {
			started = r.uploadStarted
		}
		select {
		case <-started:
		case e = <-bulkDone:
			if e != nil {
				return e
			}
			return errFSHybridIntegrity
		case <-r.ctx.Done():
			<-bulkDone
			return r.ctx.Err()
		}
		base := time.Now()
		var sendingErr error
		for i := 0; i < 20; i++ {
			planned := base.Add(time.Duration(i) * 250 * time.Millisecond)
			timer := time.NewTimer(time.Until(planned))
			select {
			case <-timer.C:
			case <-r.ctx.Done():
				timer.Stop()
				sendingErr = r.ctx.Err()
			}
			if sendingErr != nil {
				break
			}
			if sendingErr = send(1+index*20+i, direction+"_tls_frame", planned); sendingErr != nil {
				break
			}
		}
		if sendingErr == nil {
			sendingErr = await(20 + index*20)
		}
		if sendingErr != nil {
			r.cancel()
		}
		if index == 0 {
			downReleaseOnce.Do(func() { close(downRelease) })
		} else {
			upReleaseOnce.Do(func() { close(upRelease) })
		}
		bulkErr := <-bulkDone
		if sendingErr != nil {
			return sendingErr
		}
		if bulkErr != nil {
			return bulkErr
		}
	}
	if e = await(40); e != nil {
		return e
	}
	if r.gets.Load() != 1 || r.posts.Load() != 1 || r.texts.Load() != 41 || r.bad.Load() != 0 {
		return errFSHybridIntegrity
	}
	return nil
}

func TestFSHybridPipelinedTLS64ByteFramesWithExact16MiBBulk(t *testing.T) {
	for _, mode := range []string{"hybrid", "drive"} {
		t.Run(mode, func(t *testing.T) {
			f := fsHybridFullMuxFixture(t, false)
			if mode == "drive" {
				f.cm.t.Data = f.base
				f.em.t.Data = f.base
			}
			file, e := os.OpenFile(filepath.Join(t.TempDir(), "pipeline.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			j := &fsHybridLiveJournal{file: file, cancel: f.cancel}
			r := &fsHybridLiveRun{ctx: f.ctx, cancel: f.cancel, cm: f.cm, em: f.em, stores: []*fsHybridPriorityStore{f.client, f.exit}, journal: j, logger: f.cm.t.Logger, settings: fsHybridLiveSettings{Mode: "hybrid", PayloadSeedHex: strings.Repeat("31", 32)}}
			t.Cleanup(func() {
				if e := r.stop(); e != nil {
					t.Error(e)
				}
			})
			if e = r.pipelinedWorkload(true); e != nil {
				t.Fatal("owned pipelined TLS workload failed", e, f.client.Error(), f.exit.Error())
			}
			if e = r.stop(); e != nil {
				t.Fatal(e)
			}
			if len(j.rows) != 43 || r.texts.Load() != 41 || r.gets.Load() != 1 || r.posts.Load() != 1 || !r.joined {
				t.Fatal("pipeline count/join mismatch", len(j.rows))
			}
			frames := 0
			for _, row := range j.rows {
				if !row.OK {
					t.Fatal("pipeline row failed", row)
				}
				if strings.HasSuffix(row.Kind, "_tls_frame") && row.Kind != "warm_tls_frame" {
					frames++
					if !row.OfferedDuringBulk || !row.CompletedDuringBulk {
						t.Fatal("frame didnotcompletewhilebulkactive", row)
					}
				}
			}
			if frames != 40 {
				t.Fatal("missing pipelined frame rows", frames)
			}
		})
	}
}

func TestFSHybridPipelinedTLSIntegrityFailureCancelsAndJoins(t *testing.T) {
	f := fsHybridFullMuxFixture(t, false)
	file, e := os.OpenFile(filepath.Join(t.TempDir(), "pipeline-failed.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	j := &fsHybridLiveJournal{file: file, cancel: f.cancel}
	r := &fsHybridLiveRun{ctx: f.ctx, cancel: f.cancel, cm: f.cm, em: f.em, stores: []*fsHybridPriorityStore{f.client, f.exit}, journal: j, logger: f.cm.t.Logger, settings: fsHybridLiveSettings{Mode: "hybrid", PayloadSeedHex: strings.Repeat("31", 32)}}
	t.Cleanup(func() { _ = r.stop() })
	e = r.pipelinedWorkloadWithResponseHook(true, func(sequence int, reply []byte) {
		if sequence == 1 {
			reply[9] ^= 1
		}
	})
	if !errors.Is(e, errFSHybridIntegrity) || r.ctx.Err() == nil {
		t.Fatal("corrupt frame didnotterminateworkloadwithintegritycause", e)
	}
	if e = r.stop(); e != nil || !r.joined {
		t.Fatal("failed framed workload leftownedwriters", e, r.joined)
	}
	failed := 0
	for _, row := range j.rows {
		if row.Failure == "pipelined_frame_integrity" {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("corrupt scheduledframe omittedfromdurablefailedrows")
	}
}
