// Bounded live primitive comparison. Run: go run ./tools/inline_probe --help.
// Does not change Skirk's transport or kits.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func main() {
	config := flag.String("config", "", "isolated Skirk test exit config")
	live := flag.Bool("live", false, "authorize at most 24 test files and cleanup in that mailbox")
	output := flag.String("out", "", "private result path")
	flag.Parse()
	if !*live || *config == "" || *output == "" {
		panic("requires --live --config and --out")
	}
	cfg, err := skirk.LoadConfig(*config)
	if err != nil {
		panic(err)
	}
	if cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" {
		panic("requires an isolated visible Skirk mailbox folder")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	source := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	httpClient := skirk.NewGoogleHTTPClient(cfg.Route)
	store := skirk.NewDriveStoreWithTokenSource(httpClient, source, cfg.Drive)
	call := func(method, path string, body []byte) (*skirk.HTTPResult, error) {
		token, err := source.Token(ctx)
		if err != nil {
			return nil, err
		}
		headers := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
		if cfg.Drive.QuotaProject != "" {
			headers["X-Goog-User-Project"] = cfg.Drive.QuotaProject
		}
		result, err := httpClient.RequestNoRateLimitRetry(ctx, method, "www.googleapis.com", path, headers, body)
		if err != nil {
			return nil, err
		}
		if result.Status < 200 || result.Status >= 300 {
			return nil, fmt.Errorf("HTTP %d", result.Status)
		}
		return result, nil
	}
	// Both candidates share a warm route connection and token source.
	if _, err := call("GET", "/drive/v3/files/"+url.PathEscape(cfg.Drive.FolderID)+"?fields=id", nil); err != nil {
		panic(err)
	}
	var rows []map[string]any
	for _, size := range []int{256, 2048, 8192} {
		for repeat := 0; repeat < 4; repeat++ {
			order := []bool{false, true}
			if repeat%2 == 1 {
				order = []bool{true, false}
			}
			for _, inline := range order {
				payload := make([]byte, size)
				if _, err := rand.Read(payload); err != nil {
					panic(err)
				}
				nonce := make([]byte, 12)
				if _, err := rand.Read(nonce); err != nil {
					panic(err)
				}
				name := "bench-drive/inline-probe/" + hex.EncodeToString(nonce)
				started := time.Now()
				id := ""
				listCalls := 0
				receivedMS := 0.0
				err := func() (trialErr error) {
					if inline {
						meta := map[string]any{"name": name, "mimeType": "application/octet-stream", "parents": []string{cfg.Drive.FolderID}, "description": "skirk-probe-v1:" + base64.StdEncoding.EncodeToString(payload)}
						body, _ := json.Marshal(meta)
						result, err := call("POST", "/drive/v3/files?fields=id", body)
						if err != nil {
							return err
						}
						var value struct {
							ID string `json:"id"`
						}
						if err = json.Unmarshal(result.Body, &value); err != nil {
							return err
						}
						id = value.ID
					} else {
						obj, err := store.PutObject(ctx, name, payload)
						if err != nil {
							return err
						}
						id = obj.ID
					}
					if id == "" {
						return fmt.Errorf("create response missing ID")
					}
					// Delete only the exact file created by this trial, even on a read failure.
					defer func() {
						if err := store.DeleteID(ctx, id); err != nil {
							trialErr = errors.Join(trialErr, fmt.Errorf("test-file cleanup failed: %w", err))
						}
					}()
					q := url.Values{"q": {"trashed = false and '" + cfg.Drive.FolderID + "' in parents and name = '" + name + "'"}, "fields": {"files(id,description)"}, "pageSize": {"10"}}
					for attempt := 0; attempt < 20; attempt++ {
						result, err := call("GET", "/drive/v3/files?"+q.Encode(), nil)
						listCalls++
						if err != nil {
							return err
						}
						var values struct {
							Files []struct {
								ID          string `json:"id"`
								Description string `json:"description"`
							} `json:"files"`
						}
						if err = json.Unmarshal(result.Body, &values); err != nil {
							return err
						}
						for _, file := range values.Files {
							if file.ID == id {
								var got []byte
								if inline {
									if !strings.HasPrefix(file.Description, "skirk-probe-v1:") {
										return fmt.Errorf("inline record missing marker")
									}
									got, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(file.Description, "skirk-probe-v1:"))
								} else {
									got, err = store.GetByID(ctx, id)
								}
								if err != nil {
									return err
								}
								if !bytes.Equal(got, payload) {
									return fmt.Errorf("payload mismatch")
								}
								receivedMS = float64(time.Since(started).Microseconds()) / 1000
								return nil
							}
						}
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(150 * time.Millisecond):
						}
					}
					return fmt.Errorf("not visible after bounded discovery")
				}()
				// Timing includes cleanup equally for both candidates.
				units := 100 + 100*listCalls
				if !inline {
					units += 200
				}
				row := map[string]any{"inline": inline, "bytes": size, "repeat": repeat, "receive_ms": receivedMS, "total_with_cleanup_ms": float64(time.Since(started).Microseconds()) / 1000, "list_calls": listCalls, "estimated_units_minimum": units, "ok": err == nil}
				if err != nil {
					row["error"] = err.Error()
				}
				rows = append(rows, row)
				if err != nil {
					break
				}
			}
		}
	}
	report := map[string]any{"experimental": true, "route": cfg.Route.Mode, "quota_includes": "create, discovery, optional media download, delete; retries may raise estimate", "rows": rows}
	body, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(*output, append(body, '\n'), 0600); err != nil {
		panic(err)
	}
	fmt.Println(string(body))
	for _, row := range rows {
		if row["ok"] != true {
			os.Exit(1)
		}
	}
}
