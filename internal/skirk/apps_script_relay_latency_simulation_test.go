package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fixed application-level delay is a simulated service budget, not a measured
// Google RTT. Twelve messages are offered at4/s without waiting for echoes.
func TestRelayClientSimulated600msRPCOfferedTextWith4MiBMuxLimit(t *testing.T) {
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	for _, packed := range []bool{false, true} {
		name := "scalar"
		if packed {
			name = "packed-priority"
		}
		t.Run(name, func(t *testing.T) { relayClientSimulatedOfferedWorkload(t, packed, data, false) })
	}
}

func TestRelayClientSimulated600msRPCUploadTextWith4MiBMuxLimit(t *testing.T) {
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	for _, packed := range []bool{false, true} {
		name := "scalar"
		if packed {
			name = "packed-priority"
		}
		t.Run(name, func(t *testing.T) { relayClientSimulatedOfferedWorkload(t, packed, data, true) })
	}
}

func relayClientSimulatedOfferedWorkload(t *testing.T, packed bool, data []byte, upload bool) {
	var largestNormal, upCalls, pullCalls atomic.Int64
	f := relayClientSOCKSFixture(t, relayResearchConfig{FrameBytes: 768 << 10, MaxBytes: 32 << 20, PollWait: 10 * time.Millisecond, PriorityReserveBytes: 1 << 20, PriorityReserveObjects: 16, PackPriority: packed}, 4<<20, func(b *relayResearchBroker) relayResearchExchange {
		x := relayClientHTTPExchange(t, b)
		return func(ctx context.Context, wire []byte) ([]byte, error) {
			p, err := b.codec.open(wire)
			if err != nil {
				return nil, err
			}
			if p.Kind == rrUp {
				upCalls.Add(1)
			} else {
				pullCalls.Add(1)
			}
			for _, piece := range p.Pieces {
				if !piece.Priority {
					for old := largestNormal.Load(); int64(piece.Total) > old; old = largestNormal.Load() {
						if largestNormal.CompareAndSwap(old, int64(piece.Total)) {
							break
						}
					}
				}
			}
			if err = relayClientDelay(ctx, 300*time.Millisecond); err != nil {
				return nil, err
			}
			raw, err := x(ctx, wire)
			if err != nil {
				return nil, err
			}
			r, err := b.codec.open(raw)
			if err != nil {
				return nil, err
			}
			for _, piece := range r.Pieces {
				if !piece.Priority {
					for old := largestNormal.Load(); int64(piece.Total) > old; old = largestNormal.Load() {
						if largestNormal.CompareAndSwap(old, int64(piece.Total)) {
							break
						}
					}
				}
			}
			if err = relayClientDelay(ctx, 300*time.Millisecond); err != nil {
				return nil, err
			}
			return raw, nil
		}
	})
	want := sha256.Sum256(data)
	bulkReached := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bulkReached <- struct{}{}
		if upload {
			h := sha256.New()
			n, e := io.Copy(h, io.LimitReader(r.Body, int64(len(data))+1))
			if e != nil || n != int64(len(data)) || !bytes.Equal(h.Sum(nil), want[:]) {
				http.Error(w, "exact upload mismatch", 400)
				return
			}
			w.Header().Set("Content-Length", "64")
			_, _ = io.WriteString(w, hex.EncodeToString(want[:]))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		conn, e := echoListener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		stop := context.AfterFunc(f.ctx, func() { _ = conn.Close() })
		defer stop()
		_, _ = io.Copy(conn, conn)
	}()
	defer func() { f.close(); echoListener.Close(); origin.Close(); <-echoDone }()
	echo, err := dialViaSOCKS5(f.ctx, "socks5h://"+f.socksAddress, echoListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	_ = echo.SetDeadline(time.Now().Add(60 * time.Second))
	warm := bytes.Repeat([]byte{0x3d}, 64)
	if _, err = echo.Write(warm); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 64)
	if _, err = io.ReadFull(echo, got); err != nil || !bytes.Equal(got, warm) {
		t.Fatal("warm persistent echo failed", err)
	}
	baseUp, basePull := upCalls.Load(), pullCalls.Load()
	bulkDone := make(chan error, 1)
	var bulkFinished atomic.Bool
	var bulkElapsedUS, bulkFirstByteUS atomic.Int64
	go func() {
		begun := time.Now()
		trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { bulkFirstByteUS.CompareAndSwap(0, time.Since(begun).Microseconds()) }}
		method := http.MethodGet
		var body io.Reader
		if upload {
			method = http.MethodPost
			body = bytes.NewReader(data)
		}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(f.ctx, trace), method, origin.URL, body)
		response, e := f.http.Do(req)
		if e == nil {
			if upload {
				ack, readErr := io.ReadAll(io.LimitReader(response.Body, 65))
				if readErr != nil || response.StatusCode != 200 || string(ack) != hex.EncodeToString(want[:]) {
					e = errors.New("exact simulated upload receipt mismatch")
				}
			} else {
				h := sha256.New()
				n, readErr := io.Copy(h, io.LimitReader(response.Body, int64(len(data))+1))
				if readErr != nil || response.StatusCode != 200 || n != int64(len(data)) || !bytes.Equal(h.Sum(nil), want[:]) {
					e = fmt.Errorf("exact simulated bulk mismatch: bytes=%d status=%d", n, response.StatusCode)
				}
			}
			response.Body.Close()
		}
		bulkElapsedUS.Store(time.Since(begun).Microseconds())
		bulkFinished.Store(true)
		bulkDone <- e
	}()
	select {
	case <-bulkReached:
	case <-f.ctx.Done():
		t.Fatal("bulk never reached origin")
	}
	const count = 12
	var timesMu sync.Mutex
	sent := make([]time.Time, count)
	writerDone := make(chan error, 1)
	start := time.Now()
	go func() {
		for i := 0; i < count; i++ {
			if e := relayClientDelay(f.ctx, time.Until(start.Add(time.Duration(i)*250*time.Millisecond))); e != nil {
				writerDone <- e
				return
			}
			message := bytes.Repeat([]byte{byte(i + 1)}, 64)
			binary.BigEndian.PutUint64(message, uint64(i+1))
			timesMu.Lock()
			sent[i] = time.Now()
			timesMu.Unlock()
			if _, e := echo.Write(message); e != nil {
				writerDone <- e
				return
			}
		}
		writerDone <- nil
	}()
	samples := make([]float64, 0, count)
	whileBulk := 0
	for i := 0; i < count; i++ {
		message := make([]byte, 64)
		if _, err = io.ReadFull(echo, message); err != nil {
			t.Fatal("offered echo missing", i, err, f.client.snapshot(), f.broker.snapshot())
		}
		wantMessage := bytes.Repeat([]byte{byte(i + 1)}, 64)
		binary.BigEndian.PutUint64(wantMessage, uint64(i+1))
		if !bytes.Equal(message, wantMessage) {
			t.Fatal("offered echo reordered or changed", i)
		}
		timesMu.Lock()
		at := sent[i]
		timesMu.Unlock()
		if at.IsZero() {
			t.Fatal("echo precedes offer")
		}
		samples = append(samples, float64(time.Since(at).Microseconds())/1000)
		if !bulkFinished.Load() {
			whileBulk++
		}
	}
	if err = <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err = <-bulkDone; err != nil {
		t.Fatal(err)
	}
	workUp, workPull := upCalls.Load()-baseUp, pullCalls.Load()-basePull
	timesMu.Lock()
	offerSpan := sent[count-1].Sub(sent[0])
	timesMu.Unlock()
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	snapshot := f.client.snapshot()
	bulkMS, firstByteMS := float64(bulkElapsedUS.Load())/1000, float64(bulkFirstByteUS.Load())/1000
	bulkMbps := float64(len(data)) * 8 / float64(bulkElapsedUS.Load())
	method := http.MethodGet
	if upload {
		method = http.MethodPost
	}
	t.Logf("SIMULATED600ms RPC, oneUP+onePULL, method=%s pack_priority=%t,32MiBdirection, 12x64B offered250ms; span=%s echo_rtt_ms=%v median=%.3f max=%.3f first_to_last_growth_ms=%.3f text_finished_during_bulk=%d bulk_ttfb_ms=%.3f bulk_complete_ms=%.3f bulk_mbps=%.6f up_calls=%d pull_calls=%d packed_up_rpcs=%d packed_down_rpcs=%d max_normal_sealed=%d", method, packed, offerSpan, samples, (sorted[5]+sorted[6])/2, sorted[len(sorted)-1], samples[len(samples)-1]-samples[0], whileBulk, firstByteMS, bulkMS, bulkMbps, workUp, workPull, snapshot["packed_up_rpcs"], snapshot["packed_down_rpcs"], largestNormal.Load())
	if firstByteMS <= 0 || bulkMS < firstByteMS {
		t.Fatal("bulk timing observer missing or inconsistent", firstByteMS, bulkMS)
	}
	if whileBulk == 0 || largestNormal.Load() <= 768<<10 {
		t.Fatal("mixed/fragmented-object conditions not exercised", whileBulk, largestNormal.Load())
	}
	if packed && snapshot["packed_up_rpcs"]+snapshot["packed_down_rpcs"] == 0 {
		t.Fatal("offered load did not exercise priority packing")
	}
	// Cleanup remains a separate phase; the simulated wire also slows deletion.
	echo.Close()
	f.http.CloseIdleConnections()
	cleanupUntil := time.Now().Add(30 * time.Second)
	for {
		s := f.broker.snapshot()
		if s.UpBytes == 0 && s.DownBytes == 0 {
			break
		}
		if time.Now().After(cleanupUntil) {
			t.Fatal("simulated processing cleanup incomplete", s)
		}
		f.cm.requestCleanupKick()
		f.em.requestCleanupKick()
		time.Sleep(10 * time.Millisecond)
	}
	f.close()
	if f.broker.snapshot().LiveWorkers != 0 {
		t.Fatal("simulated workers leaked")
	}
}
