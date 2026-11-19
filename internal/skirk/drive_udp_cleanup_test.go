package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const udpCleanupFixtureFolder = "udp-cleanup-owned-folder"

type udpCleanupFixtureFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Parents      []string `json:"parents"`
	MimeType     string   `json:"mimeType"`
	CreatedTime  string   `json:"createdTime"`
	ModifiedTime string   `json:"modifiedTime"`
}

func udpCleanupFile(id string, lease, sequence uint64, at time.Time) udpCleanupFixtureFile {
	return udpCleanupFixtureFile{
		ID: id, Name: fmt.Sprintf("%s%032x/1/%020d", UDPPreviewCleanupPrefix, lease, sequence), Parents: []string{udpCleanupFixtureFolder},
		MimeType: "application/octet-stream", CreatedTime: at.Format(time.RFC3339Nano), ModifiedTime: at.Format(time.RFC3339Nano),
	}
}

func udpCleanupFilePage(t *testing.T, files []udpCleanupFixtureFile, next string, clock time.Time) *http.Response {
	t.Helper()
	data, err := json.Marshal(map[string]any{"files": files, "nextPageToken": next})
	if err != nil {
		t.Fatal(err)
	}
	response := stringResponse(http.StatusOK, string(data))
	response.Header = http.Header{"Date": []string{clock.UTC().Format(http.TimeFormat)}}
	return response
}

func udpCleanupStore(transport roundTripFunc) *DriveStore {
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: transport}}
	return NewDriveStore(httpClient, "udp-cleanup-fixture-only", DriveConfig{FolderID: udpCleanupFixtureFolder})
}

func TestUDPPreviewCleanupSweepsExpiredScopedOrphansAcrossRestart(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		old := now.Add(-5 * time.Minute)
		first, second := udpCleanupFile("orphan-1", 1, 8, old), udpCleanupFile("orphan-2", 2, 1, old)
		otherFolder := udpCleanupFile("other-folder-owned", 2, 2, old)
		otherFolder.Parents = []string{"different-folder"}
		active := udpCleanupFile("active", 1, 9, now.Add(-5*time.Second))
		recentlyModified := udpCleanupFile("recently-modified", 2, 3, old)
		recentlyModified.ModifiedTime = now.Add(-5 * time.Second).Format(time.RFC3339Nano)
		recentlyCreated := udpCleanupFile("recently-created", 2, 4, old)
		recentlyCreated.CreatedTime = now.Add(-5 * time.Second).Format(time.RFC3339Nano)
		otherMIME := udpCleanupFile("other-mime", 3, 1, old)
		otherMIME.MimeType = "application/vnd.google-apps.folder"
		badSequence := udpCleanupFile("zero-sequence", 3, 0, old)
		foreignName := udpCleanupFile("foreign-name", 3, 2, old)
		foreignName.Name = "backup-of-" + foreignName.Name
		badID := udpCleanupFile("bad/id", 3, 3, old)
		rows := [][]udpCleanupFixtureFile{{active, first, otherFolder, otherMIME}, {first, second, recentlyModified, recentlyCreated, badSequence, foreignName, badID}}
		var deleted []string
		var deleteTimes []time.Time
		var lists int
		mock := func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "Bearer udp-cleanup-fixture-only" {
				return nil, errors.New("sweep did not use its scoped Drive store")
			}
			if req.Method == http.MethodDelete {
				id, _ := strings.CutPrefix(req.URL.Path, "/drive/v3/files/")
				deleted, deleteTimes = append(deleted, id), append(deleteTimes, time.Now())
				return stringResponse(http.StatusNoContent, ""), nil
			}
			if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files" {
				return nil, errors.New("sweep made an unexpected Drive request")
			}
			lists++
			query := req.URL.Query()
			for _, clause := range []string{"'" + udpCleanupFixtureFolder + "' in parents", "name contains 'udp-pilot/'", "createdTime <", "modifiedTime <"} {
				if !strings.Contains(query.Get("q"), clause) {
					return nil, errors.New("unscoped UDP preview cleanup query")
				}
			}
			index, next := 0, "next"
			if query.Get("pageToken") == "next" {
				index, next = 1, ""
			}
			page := slices.DeleteFunc(slices.Clone(rows[index]), func(file udpCleanupFixtureFile) bool { return slices.Contains(deleted, file.ID) })
			return udpCleanupFilePage(t, page, next, time.Now()), nil
		}
		opts := DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix, Now: now, OlderThan: time.Second}
		firstRun, err := udpCleanupStore(mock).Cleanup(t.Context(), opts)
		if err != nil || firstRun.Scanned != 11 || firstRun.Pages != 2 || firstRun.Deleted != 2 || !firstRun.Cutoff.Equal(now.Add(-udpPreviewCleanupMinAge)) {
			t.Fatalf("unexpected orphan sweep: %+v, %v", firstRun, err)
		}
		if !slices.Equal(deleted, []string{"orphan-1", "orphan-2"}) || deleteTimes[1].Sub(deleteTimes[0]) < udpPreviewCleanupMinPace {
			t.Fatalf("sweep deleted unrelated/live IDs or exceeded pacing: %v", deleted)
		}
		// A new DriveStore represents a restarted exit with no lease state.
		secondRun, err := udpCleanupStore(mock).Cleanup(t.Context(), opts)
		if err != nil || secondRun.Matched != 0 || secondRun.Deleted != 0 || lists != 4 || len(deleted) != 2 {
			t.Fatalf("restart did not preserve live/unrelated files: %+v, %v", secondRun, err)
		}
	})
}

func TestUDPPreviewCleanupProtectsAgainstUnscopedStoreAndClockSkew(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, cfg := range []DriveConfig{{}, {FolderID: "root"}, {FolderID: "appDataFolder"}, {FolderID: udpCleanupFixtureFolder, Space: "appDataFolder"}} {
		drive := NewDriveStore(nil, "unused-fixture-token", cfg)
		if _, err := drive.Cleanup(t.Context(), DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix}); err == nil || !strings.Contains(err.Error(), "explicit Drive mailbox folder") {
			t.Fatalf("unscoped cleanup was not refused before HTTP: %v", err)
		}
	}
	now := time.Now()
	drive := udpCleanupStore(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("clock-skewed sweep reached DELETE")
		}
		return udpCleanupFilePage(t, []udpCleanupFixtureFile{udpCleanupFile("old", 1, 1, now.Add(-time.Hour))}, "", now.Add(-time.Hour)), nil
	})
	if _, err := drive.Cleanup(t.Context(), DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix}); !errors.Is(err, ErrDriveClockSkew) {
		t.Fatalf("skewed Drive clock was not refused: %v", err)
	}
}

func TestUDPPreviewCleanupUsesEarlierDriveCutoff(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		driveTime := now.Add(-2 * time.Second)
		candidate := udpCleanupFile("too-recent", 1, 1, driveTime.Add(-59*time.Second))
		drive := udpCleanupStore(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatal("Drive object younger than the retention floor reached DELETE")
			}
			return udpCleanupFilePage(t, []udpCleanupFixtureFile{candidate}, "", driveTime), nil
		})
		result, err := drive.Cleanup(t.Context(), DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix})
		if err != nil || result.Matched != 0 || !result.Cutoff.Equal(driveTime.Add(-udpPreviewCleanupMinAge)) {
			t.Fatalf("cleanup failed to use the earlier Drive cutoff: %+v, %v", result, err)
		}
	})
}

func TestUDPPreviewCleanupEnforcesPageLimitAndDryRun(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	lists := 0
	drive := udpCleanupStore(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("dry-run reached DELETE")
		}
		lists++
		now := time.Now()
		file := udpCleanupFile(fmt.Sprintf("old-%d", lists), 1, uint64(lists), now.Add(-3*time.Minute))
		return udpCleanupFilePage(t, []udpCleanupFixtureFile{file}, strconv.Itoa(lists), now), nil
	})
	result, err := drive.Cleanup(t.Context(), DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix, DryRun: true, MaxPages: 10_000})
	if err != nil || lists != udpPreviewCleanupMaxPages || result.Pages != lists || result.Matched != lists || !result.Truncated {
		t.Fatalf("dry-run escaped its page bound: %+v, %v", result, err)
	}
}

func TestUDPPreviewCleanupEnforcesCandidateLimit(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	lists := 0
	const filesPerPage = 100
	drive := udpCleanupStore(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("dry-run reached DELETE")
		}
		now := time.Now()
		files := make([]udpCleanupFixtureFile, filesPerPage)
		for i := range files {
			seq := lists*filesPerPage + i + 1
			files[i] = udpCleanupFile(fmt.Sprintf("old-%d", seq), 1, uint64(seq), now.Add(-3*time.Minute))
		}
		lists++
		return udpCleanupFilePage(t, files, strconv.Itoa(lists), now), nil
	})
	result, err := drive.Cleanup(t.Context(), DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix, DryRun: true, MaxDeletes: 10_000})
	if err != nil || lists != 5 || result.Matched != udpPreviewCleanupMaxFiles || !result.Truncated {
		t.Fatalf("dry-run escaped its candidate bound: %+v, %v", result, err)
	}
}

func TestUDPPreviewCleanupStopsOnSharedBackoffOrCancellation(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, backoff := range []bool{true, false} {
		t.Run(fmt.Sprint("backoff=", backoff), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lists, deletes := 0, 0
			drive := udpCleanupStore(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					lists++
					now := time.Now()
					old := now.Add(-3 * time.Minute)
					return udpCleanupFilePage(t, []udpCleanupFixtureFile{udpCleanupFile("old-a", 1, 1, old), udpCleanupFile("old-b", 1, 2, old)}, "", now), nil
				}
				deletes++
				if backoff {
					response := stringResponse(http.StatusTooManyRequests, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`)
					response.Header = http.Header{"Retry-After": []string{"30"}}
					return response, nil
				}
				cancel()
				return nil, ctx.Err()
			})
			opts := DriveCleanupOptions{Prefix: UDPPreviewCleanupPrefix}
			result, err := drive.Cleanup(ctx, opts)
			if backoff {
				if err != nil || result.Failed != 1 || !result.Truncated || !drive.QuotaBackoffSnapshot().Active {
					t.Fatalf("rate limit did not stop sweep: %+v, %v", result, err)
				}
				if skipped, err := drive.Cleanup(ctx, opts); err != nil || skipped.Pages != 0 || !skipped.Truncated {
					t.Fatalf("backoff still made an API call: %+v, %v", skipped, err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled cleanup: %v", err)
			}
			if lists != 1 || deletes != 1 {
				t.Fatalf("cleanup continued after stopping: lists=%d deletes=%d", lists, deletes)
			}
		})
	}
}
