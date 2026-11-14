// Bounded live diagnosis of large-body stalls. No protocol/default changes.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func main() {
	config := flag.String("config", "", "isolated test exit config")
	out := flag.String("out", "", "private result JSON")
	live := flag.Bool("live", false, "allow at most 32 generated test files with exact-ID cleanup")
	hedgeAfter := flag.Duration("hedge-after", 0, "optional second-connection same-ID retry delay (0 disables)")
	flag.Parse()
	if !*live || *config == "" || *out == "" {
		panic("requires --live, --config, --out")
	}
	if *hedgeAfter != 0 && (*hedgeAfter < time.Second || *hedgeAfter > 10*time.Second) {
		panic("hedge delay must be 0 or between 1 and 10 seconds")
	}
	cfg, err := skirk.LoadConfig(*config)
	if err != nil {
		panic("cannot load test config")
	}
	if cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" {
		panic("requires isolated test folder")
	}
	if _, err := os.Stat(*out); !os.IsNotExist(err) {
		panic("output already exists or inaccessible")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	prefix := "bench-drive/astra-upload/" + hex.EncodeToString(nonce) + "/"
	tokenSource := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	var rows []map[string]any
	var mu sync.Mutex
	var totalUnits int64
	defer func() {
		encoded, _ := json.MarshalIndent(map[string]any{"prefix": prefix, "route": cfg.Route.Mode, "rows": rows, "estimated_units": totalUnits, "hedge_after_ms": hedgeAfter.Milliseconds()}, "", "  ")
		if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
			panic(err)
		}
		if err := os.WriteFile(*out, append(encoded, '\n'), 0o600); err != nil {
			panic(err)
		}
	}()
	for round, size := range []int{524288, 1048631, 1048631, 524288, 1048631, 524288, 524288, 1048631} {
		// Fresh HTTP/2 pool per round, warmed by one batched ID reservation.
		httpClient := skirk.NewGoogleHTTPClient(cfg.Route)
		store := skirk.NewDriveStoreWithTokenSource(httpClient, tokenSource, cfg.Drive)
		// Independent pool: an extra stream on the original HTTP/2 connection
		// would not isolate a stalled connection. No extra account or project.
		hedgeStore := skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(cfg.Route), tokenSource, cfg.Drive)
		var probeUnits atomic.Int64
		var integrityOrCleanupFailed atomic.Bool
		ids, err := store.GenerateObjectIDs(ctx, 4)
		if err != nil || len(ids) != 4 {
			panic("ID reservation failed")
		}
		var wg sync.WaitGroup
		for index, id := range ids {
			wg.Add(1)
			go func(index int, id string) {
				defer wg.Done()
				data := make([]byte, size)
				if _, err := rand.Read(data); err != nil {
					panic(err)
				}
				started := time.Now()
				var gotConn, wrote, firstByte atomic.Int64
				var reused, writeFailed atomic.Bool
				var attempts atomic.Int64
				trace := &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) {
						attempts.Add(1)
						gotConn.Store(time.Since(started).Microseconds())
						reused.Store(info.Reused)
					},
					WroteRequest: func(info httptrace.WroteRequestInfo) {
						wrote.Store(time.Since(started).Microseconds())
						writeFailed.Store(info.Err != nil)
					},
					GotFirstResponseByte: func() { firstByte.Store(time.Since(started).Microseconds()) },
				}
				uploadCtx, stop := context.WithTimeout(ctx, 20*time.Second)
				type outcome struct {
					role string
					err  error
				}
				completed := make(chan outcome, 2)
				name := fmt.Sprintf("%s%d-%d", prefix, round, index)
				go func() {
					_, err := store.PutObjectWithID(httptrace.WithClientTrace(uploadCtx, trace), id, name, data)
					completed <- outcome{"primary", err}
				}()
				var uploadErr error
				winner := ""
				winnerAt := time.Duration(0)
				hedged := false
				outcomes := map[string]string{}
				record := func(result outcome) {
					status := "failed"
					if result.err == nil {
						status = "ok"
						if winnerAt == 0 {
							winnerAt = time.Since(started)
						}
					} else if errors.Is(result.err, context.Canceled) {
						status = "canceled"
					} else if errors.Is(result.err, context.DeadlineExceeded) {
						status = "deadline"
					}
					outcomes[result.role] = status
				}
				metadataStatus := 0
				metadataVerified := false
				delay := 3 * time.Second
				if *hedgeAfter != 0 {
					delay = *hedgeAfter
				}
				timer := time.NewTimer(delay)
				select {
				case result := <-completed:
					timer.Stop()
					record(result)
					uploadErr = result.err
					if result.err == nil {
						winner = result.role
					}
				case <-timer.C:
					if *hedgeAfter != 0 {
						hedged = true
						go func() {
							_, err := hedgeStore.PutObjectWithID(uploadCtx, id, name, data)
							completed <- outcome{"secondary", err}
						}()
						for remaining := 2; remaining > 0; remaining-- {
							result := <-completed
							record(result)
							if winner == "" {
								uploadErr = result.err
								if result.err == nil {
									winner = result.role
									stop() // Join the loser before verification and cleanup.
								}
							}
						}
						break
					}
					probeCtx, endProbe := context.WithTimeout(ctx, 5*time.Second)
					token, tokenErr := tokenSource.Token(probeCtx)
					if tokenErr == nil {
						headers := map[string]string{"Authorization": "Bearer " + token}
						if cfg.Drive.QuotaProject != "" {
							headers["X-Goog-User-Project"] = cfg.Drive.QuotaProject
						}
						result, err := httpClient.RequestNoRateLimitRetry(probeCtx, "GET", "www.googleapis.com", "/drive/v3/files/"+url.PathEscape(id)+"?fields=id,size,sha256Checksum", headers, nil)
						if err == nil {
							probeUnits.Add(int64(5 * result.Attempts))
							metadataStatus = result.Status
							var meta struct {
								Size   string `json:"size"`
								SHA256 string `json:"sha256Checksum"`
							}
							_ = json.Unmarshal(result.Body, &meta)
							digest := sha256.Sum256(data)
							metadataVerified = result.Status == 200 && meta.Size == fmt.Sprint(size) && meta.SHA256 == hex.EncodeToString(digest[:])
						} else {
							probeUnits.Add(25)
						}
					}
					endProbe()
					result := <-completed
					record(result)
					uploadErr = result.err
					if result.err == nil {
						winner = result.role
					}
				}
				stop()
				row := map[string]any{"round": round, "index": index, "bytes": size, "upload_ok": uploadErr == nil,
					"upload_ms": float64(time.Since(started).Microseconds()) / 1000, "got_connection_ms": float64(gotConn.Load()) / 1000,
					"wrote_request_ms": float64(wrote.Load()) / 1000, "first_response_byte_ms": float64(firstByte.Load()) / 1000,
					"connection_reused": reused.Load(), "write_failed": writeFailed.Load(), "connection_acquisitions": attempts.Load(),
					"timed_out": errors.Is(uploadErr, context.DeadlineExceeded)}
				row["slow_metadata_status"] = metadataStatus
				row["slow_metadata_exact_sha256"] = metadataVerified
				row["hedged"] = hedged
				row["winner"] = winner
				row["winner_ms"] = float64(winnerAt.Microseconds()) / 1000
				row["outcomes"] = outcomes
				if uploadErr == nil {
					verifyCtx, stop := context.WithTimeout(ctx, 20*time.Second)
					actual, err := store.GetByID(verifyCtx, id)
					stop()
					row["exact_bytes_verified"] = err == nil && bytes.Equal(actual, data)
					if row["exact_bytes_verified"] != true {
						integrityOrCleanupFailed.Store(true)
					}
				}
				deleteCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
				row["cleanup_ok"] = store.DeleteID(deleteCtx, id) == nil
				if row["cleanup_ok"] != true {
					integrityOrCleanupFailed.Store(true)
				}
				stop()
				mu.Lock()
				rows = append(rows, row)
				mu.Unlock()
			}(index, id)
		}
		wg.Wait()
		snapshot := store.QuotaSnapshot()
		hedgeSnapshot := hedgeStore.QuotaSnapshot()
		totalUnits += snapshot.Units + hedgeSnapshot.Units + probeUnits.Load()
		fmt.Printf("round=%d bytes=%d calls=%d errors=%d estimated_units=%d\n", round, size, snapshot.Calls+hedgeSnapshot.Calls, snapshot.Errors+hedgeSnapshot.Errors, snapshot.Units+hedgeSnapshot.Units+probeUnits.Load())
		if integrityOrCleanupFailed.Load() || store.QuotaBackoffSnapshot().Active || hedgeStore.QuotaBackoffSnapshot().Active || ctx.Err() != nil || totalUnits > 20000 {
			break
		}
	}
}
