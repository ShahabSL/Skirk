package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type stressArgs struct {
	clients, messages, intervalMS, bulkMiB, bulkRequests int
	putMS, getMS, listMS, pollMS                         int
	bulkPutMSPerMiB, bulkPutSlots, chunkMiB              int
	bulkDirection                                        string
	sparse, interactionEpoch                             bool
}

type stressLogCounter struct {
	gapRepair, gapTimeout, terminal, transportError atomic.Int64
}

func (c *stressLogCounter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("mux receive gap repair")) {
		c.gapRepair.Add(1)
	}
	if bytes.Contains(p, []byte("mux receive gap timeout")) {
		c.gapTimeout.Add(1)
	}
	if bytes.Contains(p, []byte("mux process terminal failure")) {
		c.terminal.Add(1)
	}
	if bytes.Contains(p, []byte("transport_error")) {
		c.transportError.Add(1)
	}
	return len(p), nil
}

func stressWriteAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

func stressConfig(secret string, sessionID string, clientID string, runID string, proxyAddr string, a stressArgs) *skirk.Config {
	cfg := &skirk.Config{
		Secret: secret, SessionID: sessionID,
		Client: skirk.ClientConfig{ID: clientID, RunID: runID},
		Tunnel: skirk.TunnelConfig{
			Listen: proxyAddr, Profile: "fixed", ChunkSize: a.chunkMiB * 1024 * 1024,
			PollIntervalMS: a.pollMS, SparsePriority: a.sparse,
			InteractionEpoch: a.interactionEpoch, CleanupProcessed: true,
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func stressConnectAndWarmup(proxyAddr string, echoAddr string, warmup []byte) (net.Conn, error) {
	deadline := time.Now().Add(25 * time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = socksConnect(proxyAddr, echoAddr)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := stressWriteAll(conn, warmup); err != nil {
		conn.Close()
		return nil, err
	}
	echoed := make([]byte, len(warmup))
	if _, err := io.ReadFull(conn, echoed); err != nil || !bytes.Equal(echoed, warmup) {
		conn.Close()
		return nil, fmt.Errorf("warmup echo mismatch: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

type stressTextResult struct {
	client    int
	latencies []float64
	lastAt    time.Time
	err       error
}

func stressTextMessages(conn net.Conn, client int, count int, interval time.Duration, start time.Time) stressTextResult {
	result := stressTextResult{client: client, latencies: make([]float64, 0, count)}
	sent := make(chan error, 1)
	go func() {
		for i := 0; i < count; i++ {
			scheduled := start.Add(time.Duration(i) * interval)
			if wait := time.Until(scheduled); wait > 0 {
				time.Sleep(wait)
			}
			if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
				sent <- err
				_ = conn.Close()
				return
			}
			payload := bytes.Repeat([]byte{byte(client), byte(i)}, 64)
			if err := stressWriteAll(conn, payload); err != nil {
				sent <- err
				_ = conn.Close()
				return
			}
		}
		sent <- nil
	}()
	for i := 0; i < count; i++ {
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			result.err = err
			_ = conn.Close()
			<-sent
			return result
		}
		got := make([]byte, 128)
		_, err := io.ReadFull(conn, got)
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(client), byte(i)}, 64)) {
			result.err = fmt.Errorf("client %d message %d echo mismatch: %w", client, i, err)
			_ = conn.Close()
			<-sent
			return result
		}
		result.lastAt = time.Now()
		scheduled := start.Add(time.Duration(i) * interval)
		result.latencies = append(result.latencies, float64(result.lastAt.Sub(scheduled).Microseconds())/1000)
	}
	result.err = <-sent
	return result
}

type stressBulkResult struct {
	client     int
	bytes      int64
	finishedAt time.Time
	err        error
}

func stressBulkDownload(ctx context.Context, client int, proxyAddr, originURL string, requests int, expectedBytes int, hash [32]byte) stressBulkResult {
	result := stressBulkResult{client: client}
	proxy := &url.URL{Scheme: "socks5h", Host: proxyAddr}
	httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}, Timeout: 90 * time.Second}
	for i := 0; i < requests; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL, nil)
		if err != nil {
			result.err = err
			return result
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			result.err = err
			return result
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(body) != expectedBytes || sha256.Sum256(body) != hash {
			result.err = fmt.Errorf("client %d bulk response %d failed integrity: %w", client, i, err)
			return result
		}
		result.bytes += int64(len(body))
	}
	result.finishedAt = time.Now()
	return result
}

func stressBulkUpload(ctx context.Context, client int, proxyAddr, originURL string, requests int, payload []byte) stressBulkResult {
	result := stressBulkResult{client: client}
	proxy := &url.URL{Scheme: "socks5h", Host: proxyAddr}
	httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}, Timeout: 120 * time.Second}
	for i := 0; i < requests; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, originURL, bytes.NewReader(payload))
		if err != nil {
			result.err = err
			return result
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			result.err = err
			return result
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte("verified")) {
			result.err = fmt.Errorf("client %d bulk upload response %d failed status=%d body=%q: %v", client, i, resp.StatusCode, body, err)
			return result
		}
		result.bytes += int64(len(payload))
	}
	result.finishedAt = time.Now()
	return result
}

func runMultiClientStress(a stressArgs) error {
	runStarted := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &localStore{
		MemoryStore:   skirk.NewMemoryStore(),
		putDelay:      time.Duration(a.putMS) * time.Millisecond,
		getDelay:      time.Duration(a.getMS) * time.Millisecond,
		listDelay:     time.Duration(a.listMS) * time.Millisecond,
		bulkPutPerMiB: time.Duration(a.bulkPutMSPerMiB) * time.Millisecond,
	}
	if a.bulkPutSlots > 0 {
		store.bulkPutSlots = make(chan struct{}, a.bulkPutSlots)
	}
	logger := &stressLogCounter{}
	secret, err := skirk.RandomSecret()
	if err != nil {
		return err
	}
	const sessionID = "00112233445566778899aabbccddeeff"
	echoAddr, stopEcho, err := echoServer()
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		stopEcho()
	}()
	exit, err := skirk.NewTunnel(store, stressConfig(secret, sessionID, "exit", "exit-run", "", a))
	if err != nil {
		return err
	}
	exit.Logger = log.New(logger, "", 0)
	go func() { _ = exit.ServeExit(ctx) }()
	proxyAddrs := make([]string, a.clients)
	for i := range proxyAddrs {
		proxyAddrs[i] = localAddr()
		cfg := stressConfig(secret, sessionID, fmt.Sprintf("bench-client-%02d", i), fmt.Sprintf("bench-run-%02d", i), proxyAddrs[i], a)
		client, err := skirk.NewTunnel(store, cfg)
		if err != nil {
			return err
		}
		client.Logger = log.New(logger, "", 0)
		addr := proxyAddrs[i]
		go func() { _ = client.ServeClient(ctx, addr) }()
	}
	warmup := bytes.Repeat([]byte{0x5a}, 384*1024)
	connections := make([]net.Conn, a.clients)
	defer func() {
		for _, conn := range connections {
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	warmupErrors := make(chan error, a.clients)
	var warmupWG sync.WaitGroup
	for i := range connections {
		warmupWG.Add(1)
		go func(i int) {
			defer warmupWG.Done()
			conn, err := stressConnectAndWarmup(proxyAddrs[i], echoAddr, warmup)
			connections[i] = conn
			if err != nil {
				warmupErrors <- fmt.Errorf("client %d: %w", i, err)
			}
		}(i)
	}
	warmupWG.Wait()
	close(warmupErrors)
	for err := range warmupErrors {
		if err != nil {
			return fmt.Errorf("multi-client warmup: %w", err)
		}
	}
	time.Sleep(time.Second)
	priorityBefore := store.priorityPuts.Load()
	normalBefore := store.normalPuts.Load()
	payload := make([]byte, a.bulkMiB*1024*1024)
	_, _ = rand.New(rand.NewSource(42)).Read(payload)
	hash := sha256.Sum256(payload)
	var originHashFailures atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if a.bulkDirection == "upload" {
			body, err := io.ReadAll(req.Body)
			if err != nil || req.Method != http.MethodPost || len(body) != len(payload) || sha256.Sum256(body) != hash {
				originHashFailures.Add(1)
				http.Error(w, "bulk integrity failure", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("verified"))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer func() {
		cancel()
		origin.Close()
	}()
	bulkStarted := time.Now()
	bulkDone := make(chan stressBulkResult, a.clients)
	for i := range proxyAddrs {
		go func(i int) {
			if a.bulkDirection == "upload" {
				bulkDone <- stressBulkUpload(ctx, i, proxyAddrs[i], origin.URL, a.bulkRequests, payload)
				return
			}
			bulkDone <- stressBulkDownload(ctx, i, proxyAddrs[i], origin.URL, a.bulkRequests, len(payload), hash)
		}(i)
	}
	messageStart := bulkStarted.Add(250 * time.Millisecond)
	textDone := make(chan stressTextResult, a.clients)
	for i := range connections {
		go func(i int) {
			textDone <- stressTextMessages(connections[i], i, a.messages, time.Duration(a.intervalMS)*time.Millisecond, messageStart)
		}(i)
	}
	textResults := make([]stressTextResult, a.clients)
	latencies := make([]float64, 0, a.clients*a.messages)
	for i := 0; i < a.clients; i++ {
		result := <-textDone
		if result.err != nil || len(result.latencies) != a.messages {
			return fmt.Errorf("client %d text failed: %w", result.client, result.err)
		}
		textResults[result.client] = result
		latencies = append(latencies, result.latencies...)
	}
	bulkResults := make([]stressBulkResult, a.clients)
	totalBulk := int64(0)
	bulkFinished := bulkStarted
	for i := 0; i < a.clients; i++ {
		result := <-bulkDone
		if result.err != nil || result.bytes != int64(a.bulkMiB*a.bulkRequests*1024*1024) {
			return fmt.Errorf("client %d bulk failed: %w", result.client, result.err)
		}
		bulkResults[result.client] = result
		totalBulk += result.bytes
		if result.finishedAt.After(bulkFinished) {
			bulkFinished = result.finishedAt
		}
	}
	activeThroughText := 0
	perClientP95 := make([]float64, a.clients)
	for i := range textResults {
		if bulkResults[i].finishedAt.After(textResults[i].lastAt) {
			activeThroughText++
		}
		perClientP95[i] = percentile(textResults[i].latencies, .95)
		_ = connections[i].Close()
	}
	cleanupStarted := time.Now()
	cleanupRemaining := -1
	var remainingNames []string
	for time.Since(cleanupStarted) < 70*time.Second {
		infos, err := store.MemoryStore.List(ctx, "muxv")
		if err != nil {
			return err
		}
		cleanupRemaining = len(infos)
		remainingNames = remainingNames[:0]
		for _, info := range infos {
			if len(remainingNames) >= 5 {
				break
			}
			remainingNames = append(remainingNames, info.Name)
		}
		if cleanupRemaining == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	listCalls := store.listCalls.Load()
	getCalls := store.getCalls.Load()
	putCalls := store.priorityPuts.Load() + store.normalPuts.Load()
	deleteCalls := store.deleteCalls.Load()
	units := 100*listCalls + 200*getCalls + 50*putCalls + 50*deleteCalls
	output := map[string]any{
		"synthetic": true, "store_model": "shared MemoryStore with exact class filtering, configurable large-PUT delay and slots; no Drive visibility, page pollution, or quota enforcement",
		"clients": a.clients, "messages_per_client": a.messages, "message_interval_ms": a.intervalMS,
		"bulk_mib_per_request": a.bulkMiB, "bulk_requests_per_client": a.bulkRequests,
		"bulk_direction": a.bulkDirection, "chunk_mib": a.chunkMiB,
		"bulk_put_ms_per_mib": a.bulkPutMSPerMiB, "bulk_put_slots": a.bulkPutSlots,
		"sparse_priority": a.sparse, "interaction_epoch": a.interactionEpoch,
		"text_verified_messages": len(latencies), "text_scheduled_p50_ms": percentile(latencies, .50),
		"text_scheduled_p95_ms": percentile(latencies, .95), "text_scheduled_max_ms": percentile(latencies, 1),
		"text_client_p95_ms":  perClientP95,
		"bulk_verified_bytes": totalBulk, "bulk_mbps": float64(totalBulk*8) / bulkFinished.Sub(bulkStarted).Seconds() / 1e6,
		"bulk_duration_ms":                 bulkFinished.Sub(bulkStarted).Milliseconds(),
		"bulk_clients_active_through_text": activeThroughText,
		"origin_hash_failures":             originHashFailures.Load(),
		"priority_objects_after_warmup":    store.priorityPuts.Load() - priorityBefore,
		"normal_objects_after_warmup":      store.normalPuts.Load() - normalBefore,
		"list_calls_total":                 listCalls, "get_calls_total": getCalls,
		"put_calls_total": putCalls, "delete_calls_total": deleteCalls, "estimated_units_total": units,
		"estimated_units_formula": "100*list + 200*get + 50*put + 50*delete; one synthetic page per list",
		"receive_gap_repairs":     logger.gapRepair.Load(), "receive_gap_timeouts": logger.gapTimeout.Load(),
		"terminal_process_failures": logger.terminal.Load(), "transport_errors": logger.transportError.Load(),
		"cleanup_remaining_objects": cleanupRemaining, "cleanup_wait_ms": time.Since(cleanupStarted).Milliseconds(),
		"cleanup_remaining_names": remainingNames, "run_duration_ms": time.Since(runStarted).Milliseconds(),
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Println(string(encoded)); err != nil {
		return err
	}
	if cleanupRemaining != 0 {
		return errors.New("processed mux objects remain after cleanup window")
	}
	return nil
}
