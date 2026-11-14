// Run with: go run ./tools/skirk_memory_bench.go -small=24 -bulk-mib=32
// This is a synthetic end-to-end mux benchmark. It does not call Google Drive.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type countingStore struct {
	inner                         *skirk.MemoryStore
	putDelay, getDelay, listDelay time.Duration
	put, get, list, del, reserve  atomic.Int64
}

func sleep(ctx context.Context, d time.Duration) error {
	if d == 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *countingStore) Put(ctx context.Context, n string, b []byte) error {
	s.put.Add(1)
	if err := sleep(ctx, s.putDelay); err != nil {
		return err
	}
	return s.inner.Put(ctx, n, b)
}
func (s *countingStore) PutObject(ctx context.Context, n string, b []byte) (skirk.ObjectInfo, error) {
	s.put.Add(1)
	if err := sleep(ctx, s.putDelay); err != nil {
		return skirk.ObjectInfo{}, err
	}
	return s.inner.PutObject(ctx, n, b)
}
func (s *countingStore) PutObjectWithID(ctx context.Context, id, n string, b []byte) (skirk.ObjectInfo, error) {
	s.put.Add(1)
	if err := sleep(ctx, s.putDelay); err != nil {
		return skirk.ObjectInfo{}, err
	}
	return s.inner.PutObjectWithID(ctx, id, n, b)
}
func (s *countingStore) GenerateObjectIDs(ctx context.Context, n int) ([]string, error) {
	s.reserve.Add(1)
	return s.inner.GenerateObjectIDs(ctx, n)
}
func (s *countingStore) Get(ctx context.Context, n string) ([]byte, error) {
	s.get.Add(1)
	if err := sleep(ctx, s.getDelay); err != nil {
		return nil, err
	}
	return s.inner.Get(ctx, n)
}
func (s *countingStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	s.get.Add(1)
	if err := sleep(ctx, s.getDelay); err != nil {
		return nil, err
	}
	return s.inner.GetByID(ctx, id)
}
func (s *countingStore) List(ctx context.Context, prefix string) ([]skirk.ObjectInfo, error) {
	s.list.Add(1)
	if err := sleep(ctx, s.listDelay); err != nil {
		return nil, err
	}
	return s.inner.List(ctx, prefix)
}
func (s *countingStore) Delete(ctx context.Context, n string) error {
	s.del.Add(1)
	return s.inner.Delete(ctx, n)
}
func (s *countingStore) DeleteID(ctx context.Context, id string) error {
	s.del.Add(1)
	return s.inner.DeleteID(ctx, id)
}

type result struct {
	TTFBMS  float64 `json:"ttfb_ms"`
	TotalMS float64 `json:"total_ms"`
	Bytes   int64   `json:"bytes"`
	Error   string  `json:"error,omitempty"`
}

func freeAddr() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().String()
}
func pct(rs []result, p float64, which string) float64 {
	vals := make([]float64, 0, len(rs))
	for _, r := range rs {
		if r.Error == "" {
			if which == "ttfb" {
				vals = append(vals, r.TTFBMS)
			} else {
				vals = append(vals, r.TotalMS)
			}
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	i := max(0, int(math.Ceil(float64(len(vals))*p/100))-1)
	return vals[i]
}
func req(client *http.Client, rawURL string, expected []byte) result {
	start := time.Now()
	resp, err := client.Get(rawURL)
	if err != nil {
		return result{Error: err.Error(), TotalMS: float64(time.Since(start).Microseconds()) / 1000}
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	r := result{TTFBMS: float64(ttfb.Microseconds()) / 1000, TotalMS: float64(time.Since(start).Microseconds()) / 1000, Bytes: n}
	if err != nil {
		r.Error = err.Error()
	} else if resp.StatusCode >= 400 {
		r.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else if n != int64(len(expected)) || string(hash.Sum(nil)) != string(expectedDigest(expected)) {
		r.Error = "response body mismatch"
	}
	return r
}

func expectedDigest(body []byte) []byte { sum := sha256.Sum256(body); return sum[:] }

func main() {
	var count int
	var bulkMiB int
	var chunkKiB int
	var putMS, getMS, listMS, pollMS, settleMS, idleBeforeMS int
	var profile string
	var burst bool
	var uploads, downloads int
	flag.IntVar(&count, "small", 24, "number of concurrent small HTTP requests")
	flag.IntVar(&bulkMiB, "bulk-mib", 32, "bulk response size in MiB")
	flag.IntVar(&chunkKiB, "chunk-kib", 16384, "configured tunnel chunk size in KiB")
	flag.IntVar(&putMS, "put-ms", 0, "synthetic object upload delay")
	flag.IntVar(&getMS, "get-ms", 0, "synthetic object download delay")
	flag.IntVar(&listMS, "list-ms", 0, "synthetic object list delay")
	flag.IntVar(&pollMS, "poll-ms", 1000, "configured poll interval")
	flag.StringVar(&profile, "profile", "auto", "tunnel profile")
	flag.IntVar(&uploads, "uploads", 16, "upload concurrency cap")
	flag.IntVar(&downloads, "downloads", 16, "download concurrency cap")
	flag.BoolVar(&burst, "burst", false, "enable bounded burst polling")
	flag.IntVar(&settleMS, "settle-ms", 0, "wait after workload to count deferred cleanup")
	flag.IntVar(&idleBeforeMS, "idle-before-ms", 0, "wait after warm-up before starting the measured requests")
	flag.Parse()
	if count < 1 || count > 100 || bulkMiB < 1 || bulkMiB > 128 || pollMS < 1 || uploads < 1 || uploads > 64 || downloads < 1 || downloads > 64 || putMS < 0 || getMS < 0 || listMS < 0 || settleMS < 0 || idleBeforeMS < 0 {
		fmt.Fprintln(os.Stderr, "invalid benchmark bounds")
		os.Exit(2)
	}
	payload := make([]byte, bulkMiB*1024*1024)
	if _, err := rand.Read(payload); err != nil {
		panic(err)
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/small" {
			w.WriteHeader(204)
			return
		}
		if r.URL.Path != "/bulk" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer origin.Close()
	data := &countingStore{inner: skirk.NewMemoryStore(), putDelay: time.Duration(putMS) * time.Millisecond, getDelay: time.Duration(getMS) * time.Millisecond, listDelay: time.Duration(listMS) * time.Millisecond}
	secret, err := skirk.RandomSecret()
	if err != nil {
		panic(err)
	}
	addr := freeAddr()
	cfg := &skirk.Config{Secret: secret, SessionID: "00112233445566778899aabbccddeeff", Client: skirk.ClientConfig{ID: "bench-client", RunID: "bench-run"}, Tunnel: skirk.TunnelConfig{Listen: addr, Profile: profile, ChunkSize: chunkKiB * 1024, PollIntervalMS: pollMS, BurstPoll: burst, UploadConcurrency: uploads, DownloadConcurrency: downloads, CleanupProcessed: true}}
	cfg.ApplyDefaults()
	clientTunnel, err := skirk.NewTunnel(data, cfg)
	if err != nil {
		panic(err)
	}
	exitTunnel, err := skirk.NewTunnel(data, cfg)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnel.ServeClient(ctx, addr) }()
	proxyURL := &url.URL{Scheme: "socks5h", Host: addr}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 90 * time.Second}
	var warm result
	for i := 0; i < 12; i++ {
		warm = req(client, origin.URL+"/small", nil)
		if warm.Error == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if warm.Error != "" {
		panic(warm.Error)
	}
	if idleBeforeMS > 0 {
		time.Sleep(time.Duration(idleBeforeMS) * time.Millisecond)
	}
	baseline := map[string]int64{"upload": data.put.Load(), "download": data.get.Load(), "list": data.list.Load(), "delete": data.del.Load(), "generate_ids": data.reserve.Load()}
	start := time.Now()
	bulkCh := make(chan result, 1)
	go func() { bulkCh <- req(client, origin.URL+"/bulk", payload) }()
	time.Sleep(25 * time.Millisecond)
	results := make([]result, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(i int) { defer wg.Done(); results[i] = req(client, origin.URL+"/small", nil) }(i)
	}
	wg.Wait()
	bulk := <-bulkCh
	wall := time.Since(start)
	failures := 0
	for _, r := range results {
		if r.Error != "" {
			failures++
		}
	}
	countCalls := func() map[string]any {
		calls := map[string]int64{"upload": data.put.Load(), "download": data.get.Load(), "list": data.list.Load(), "delete": data.del.Load(), "generate_ids": data.reserve.Load()}
		for op := range calls {
			calls[op] -= baseline[op]
		}
		units := calls["upload"]*50 + calls["download"]*200 + calls["list"]*100 + calls["delete"]*50 + calls["generate_ids"]*5
		return map[string]any{"calls": calls, "estimated_drive_quota_units": units}
	}
	foreground := countCalls()
	var settleSnapshots []map[string]int64
	if settleMS > 0 {
		for elapsed := 0; elapsed < settleMS; {
			step := min(500, settleMS-elapsed)
			time.Sleep(time.Duration(step) * time.Millisecond)
			elapsed += step
			settleSnapshots = append(settleSnapshots, map[string]int64{
				"at_ms":    int64(elapsed),
				"list":     data.list.Load(),
				"upload":   data.put.Load(),
				"download": data.get.Load(),
				"delete":   data.del.Load(),
			})
		}
	}
	out := map[string]any{"synthetic": true, "store_model": "memory prefix list; fixed operation delays; no network bandwidth, visibility lag, rate limiting, or production class pagination", "body_validation": "random payload, exact length and SHA-256", "quota_window": "excludes warm-up; foreground until workload completes; after_settle includes deferred work", "warmup_store_calls": baseline, "upload_concurrency": uploads, "download_concurrency": downloads, "profile": profile, "poll_ms": pollMS, "effective_poll_ms": cfg.PollInterval().Milliseconds(), "chunk_kib": chunkKiB, "burst": burst, "idle_before_ms": idleBeforeMS, "small_requests": count, "small_failures": failures, "small_ttfb_p50_ms": pct(results, 50, "ttfb"), "small_ttfb_p95_ms": pct(results, 95, "ttfb"), "small_total_p95_ms": pct(results, 95, "total"), "bulk": bulk, "bulk_mbps": float64(bulk.Bytes*8) / bulk.TotalMS / 1000, "wall_ms": float64(wall.Microseconds()) / 1000, "foreground_store_calls": foreground, "after_settle_store_calls": countCalls(), "settle_snapshots": settleSnapshots, "delays_ms": map[string]int{"put": putMS, "get": getMS, "list": listMS}}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if failures > 0 || bulk.Error != "" {
		os.Exit(1)
	}
}
