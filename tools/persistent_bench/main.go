// Synthetic persistent TCP message plus bulk benchmark. It uses only localhost
// sockets and MemoryStore; it never calls Google Drive.
// Example: go run ./tools/persistent_bench -sparse-priority -messages=12
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type localStore struct {
	*skirk.MemoryStore
	putDelay, getDelay, listDelay            time.Duration
	priorityPuts, normalPuts, tinyNormalPuts atomic.Int64
	listCalls, getCalls                      atomic.Int64
}

func pause(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
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

func (s *localStore) notePut(name string, size int) {
	if strings.Contains(name, "/p0/") {
		s.priorityPuts.Add(1)
	} else if strings.Contains(name, "/p1/") {
		s.normalPuts.Add(1)
		if size < 64*1024 {
			s.tinyNormalPuts.Add(1)
		}
	}
}

func (s *localStore) PutObject(ctx context.Context, name string, data []byte) (skirk.ObjectInfo, error) {
	if err := pause(ctx, s.putDelay); err != nil {
		return skirk.ObjectInfo{}, err
	}
	s.notePut(name, len(data))
	return s.MemoryStore.PutObject(ctx, name, data)
}

func (s *localStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (skirk.ObjectInfo, error) {
	if err := pause(ctx, s.putDelay); err != nil {
		return skirk.ObjectInfo{}, err
	}
	s.notePut(name, len(data))
	return s.MemoryStore.PutObjectWithID(ctx, id, name, data)
}

func (s *localStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	s.getCalls.Add(1)
	if err := pause(ctx, s.getDelay); err != nil {
		return nil, err
	}
	return s.MemoryStore.GetByID(ctx, id)
}

func (s *localStore) Get(ctx context.Context, name string) ([]byte, error) {
	s.getCalls.Add(1)
	if err := pause(ctx, s.getDelay); err != nil {
		return nil, err
	}
	return s.MemoryStore.Get(ctx, name)
}

func (s *localStore) List(ctx context.Context, prefix string) ([]skirk.ObjectInfo, error) {
	s.listCalls.Add(1)
	if err := pause(ctx, s.listDelay); err != nil {
		return nil, err
	}
	return s.MemoryStore.List(ctx, prefix)
}

func (s *localStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, _ time.Time, _ string, _ int) (skirk.ObjectListInfo, error) {
	s.listCalls.Add(1)
	if err := pause(ctx, s.listDelay); err != nil {
		return skirk.ObjectListInfo{}, err
	}
	if len(contains) == 0 {
		return skirk.ObjectListInfo{}, nil
	}
	infos, err := s.MemoryStore.List(ctx, contains[0])
	if err != nil {
		return skirk.ObjectListInfo{}, err
	}
	matched := make([]skirk.ObjectInfo, 0, len(infos))
	for _, info := range infos {
		keep := true
		for _, part := range contains[1:] {
			if !strings.Contains(info.Name, part) {
				keep = false
				break
			}
		}
		if keep {
			matched = append(matched, info)
		}
	}
	return skirk.ObjectListInfo{Objects: matched, Pages: 1}, nil
}

func localAddr() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func echoServer() (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	var peers sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			peers.Add(1)
			go func() {
				defer peers.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String(), func() { _ = listener.Close(); peers.Wait() }, nil
}

func socksConnect(proxyAddr, targetAddr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) { _ = conn.Close(); return nil, err }
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return fail(err)
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting != [2]byte{5, 0} {
		return fail(fmt.Errorf("SOCKS greeting=%v: %w", greeting, err))
	}
	host, portText, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fail(err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return fail(errors.New("echo target is not IPv4"))
	}
	var port uint16
	if _, err := fmt.Sscan(portText, &port); err != nil {
		return fail(err)
	}
	request := []byte{5, 1, 0, 1}
	request = append(request, ip...)
	request = binary.BigEndian.AppendUint16(request, port)
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fail(err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fail(fmt.Errorf("SOCKS connect reply=%v", reply))
	}
	addressBytes := 0
	switch reply[3] {
	case 1:
		addressBytes = 4
	case 4:
		addressBytes = 16
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return fail(err)
		}
		addressBytes = int(length[0])
	default:
		return fail(fmt.Errorf("SOCKS reply address type=%d", reply[3]))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressBytes+2)); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(float64(len(sorted))*p)) - 1
	return sorted[max(0, min(index, len(sorted)-1))]
}

func run() error {
	var sparse bool
	var messages, intervalMS, bulkMiB, putMS, getMS, listMS, pollMS int
	flag.BoolVar(&sparse, "sparse-priority", false, "enable opt-in sparse priority replenishment")
	flag.IntVar(&messages, "messages", 8, "persistent 128-byte echo messages after a 384 KiB warmup")
	flag.IntVar(&intervalMS, "interval-ms", 3000, "scheduled interval between messages")
	flag.IntVar(&bulkMiB, "bulk-mib", 16, "bytes per repeated concurrent bulk HTTP response, MiB")
	flag.IntVar(&putMS, "put-ms", 150, "synthetic object upload delay")
	flag.IntVar(&getMS, "get-ms", 100, "synthetic object download delay")
	flag.IntVar(&listMS, "list-ms", 60, "synthetic object list delay")
	flag.IntVar(&pollMS, "poll-ms", 250, "mux poll interval")
	flag.Parse()
	if messages < 1 || messages > 100 || intervalMS < 100 || intervalMS > 10000 || bulkMiB < 1 || bulkMiB > 128 || putMS < 0 || getMS < 0 || listMS < 0 || pollMS < 1 {
		return errors.New("benchmark arguments out of range")
	}
	echoAddr, stopEcho, err := echoServer()
	if err != nil {
		return err
	}
	defer stopEcho()
	payload := make([]byte, bulkMiB*1024*1024)
	_, _ = rand.New(rand.NewSource(42)).Read(payload)
	wantHash := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	store := &localStore{MemoryStore: skirk.NewMemoryStore(), putDelay: time.Duration(putMS) * time.Millisecond, getDelay: time.Duration(getMS) * time.Millisecond, listDelay: time.Duration(listMS) * time.Millisecond}
	secret, err := skirk.RandomSecret()
	if err != nil {
		return err
	}
	proxyAddr := localAddr()
	cfg := &skirk.Config{Secret: secret, SessionID: "00112233445566778899aabbccddeeff", Client: skirk.ClientConfig{ID: "bench-client", RunID: "bench-run"}, Tunnel: skirk.TunnelConfig{Listen: proxyAddr, ChunkSize: 16 * 1024 * 1024, PollIntervalMS: pollMS, SparsePriority: sparse, CleanupProcessed: true}}
	cfg.ApplyDefaults()
	clientTunnel, err := skirk.NewTunnel(store, cfg)
	if err != nil {
		return err
	}
	exitTunnel, err := skirk.NewTunnel(store, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = exitTunnel.ServeExit(ctx) }()
	go func() { _ = clientTunnel.ServeClient(ctx, proxyAddr) }()
	var messageConn net.Conn
	for i := 0; i < 30; i++ {
		messageConn, err = socksConnect(proxyAddr, echoAddr)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("persistent SOCKS connect: %w", err)
	}
	defer messageConn.Close()
	warmup := bytes.Repeat([]byte{0x5a}, 384*1024)
	if err := messageConn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return err
	}
	if _, err := messageConn.Write(warmup); err != nil {
		return fmt.Errorf("persistent warmup write: %w", err)
	}
	gotWarmup := make([]byte, len(warmup))
	if _, err := io.ReadFull(messageConn, gotWarmup); err != nil || !bytes.Equal(gotWarmup, warmup) {
		return fmt.Errorf("persistent warmup mismatch: %w", err)
	}
	_ = messageConn.SetDeadline(time.Time{})
	time.Sleep(time.Second)
	baselinePriority := store.priorityPuts.Load()
	baselineNormal := store.normalPuts.Load()
	proxyURL := &url.URL{Scheme: "socks5h", Host: proxyAddr}
	bulkClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 90 * time.Second}
	bulkCtx, stopBulk := context.WithCancel(ctx)
	defer stopBulk()
	type bulkResult struct {
		count int
		bytes int64
		err   error
	}
	bulkDone := make(chan bulkResult, 1)
	bulkStarted := time.Now()
	go func() {
		var result bulkResult
		for bulkCtx.Err() == nil {
			req, err := http.NewRequestWithContext(bulkCtx, http.MethodGet, server.URL, nil)
			if err != nil {
				result.err = err
				break
			}
			resp, err := bulkClient.Do(req)
			if err != nil {
				if bulkCtx.Err() == nil {
					result.err = err
				}
				break
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				if bulkCtx.Err() == nil {
					result.err = err
				}
				break
			}
			if resp.StatusCode != http.StatusOK || sha256.Sum256(body) != wantHash {
				result.err = errors.New("bulk body mismatch")
				break
			}
			result.count++
			result.bytes += int64(len(body))
		}
		bulkDone <- result
	}()
	latencies := make([]float64, 0, messages)
	messageStart := time.Now()
	for i := 0; i < messages; i++ {
		scheduled := messageStart.Add(time.Duration(i) * time.Duration(intervalMS) * time.Millisecond)
		if wait := time.Until(scheduled); wait > 0 {
			time.Sleep(wait)
		}
		message := bytes.Repeat([]byte{byte(i)}, 128)
		if err := messageConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return err
		}
		if _, err := messageConn.Write(message); err != nil {
			return fmt.Errorf("message %d write: %w", i, err)
		}
		got := make([]byte, len(message))
		if _, err := io.ReadFull(messageConn, got); err != nil || !bytes.Equal(got, message) {
			return fmt.Errorf("message %d mismatch: %w", i, err)
		}
		latencies = append(latencies, float64(time.Since(scheduled).Microseconds())/1000)
	}
	stopBulk()
	bulk := <-bulkDone
	bulkWall := time.Since(bulkStarted)
	if bulk.err != nil || bulk.count == 0 {
		return fmt.Errorf("bulk workload: count=%d error=%v", bulk.count, bulk.err)
	}
	out := map[string]any{
		"synthetic":                     true,
		"store_model":                   "MemoryStore class-filtered lists with fixed operation delays; no Drive visibility, network, or quota enforcement",
		"sparse_priority":               sparse,
		"persistent_warmup_bytes":       len(warmup),
		"messages":                      messages,
		"message_interval_ms":           intervalMS,
		"message_scheduled_p50_ms":      percentile(latencies, .50),
		"message_scheduled_p95_ms":      percentile(latencies, .95),
		"message_scheduled_max_ms":      percentile(latencies, 1),
		"message_samples_ms":            latencies,
		"bulk_verified_requests":        bulk.count,
		"bulk_verified_bytes":           bulk.bytes,
		"bulk_mbps":                     float64(bulk.bytes*8) / bulkWall.Seconds() / 1e6,
		"priority_objects_after_warmup": store.priorityPuts.Load() - baselinePriority,
		"normal_objects_after_warmup":   store.normalPuts.Load() - baselineNormal,
		"tiny_normal_objects_total":     store.tinyNormalPuts.Load(),
		"list_calls_total":              store.listCalls.Load(),
		"get_calls_total":               store.getCalls.Load(),
	}
	encoded, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
