package skirk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const denseLiveCount = 402

type denseLiveCall struct {
	Token    bool `json:"token"`
	Budget   int  `json:"budget"`
	Pages    int  `json:"pages"`
	Returned int  `json:"returned"`
}

type denseLiveStore struct {
	*DriveStore
	calls []denseLiveCall
}

func (s *denseLiveStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	result, err := s.DriveStore.ListFreshContainsPageStatus(ctx, contains, since, pageToken, maxPages)
	s.calls = append(s.calls, denseLiveCall{Token: pageToken != "", Budget: maxPages, Pages: result.Pages, Returned: len(result.Objects)})
	return result, err
}

type denseLiveReport struct {
	Prefix          string             `json:"prefix"`
	ObjectsCreated  int                `json:"objects_created"`
	Visible         int                `json:"visible"`
	TargetFirstPoll bool               `json:"target_first_poll"`
	TargetPoll      int                `json:"target_poll"`
	Calls           []denseLiveCall    `json:"calls"`
	Deleted         int                `json:"deleted"`
	Exact404        int                `json:"exact_404"`
	ImmediateLeft   int                `json:"immediate_left"`
	DelayedLeft     int                `json:"delayed_left"`
	CleanupOK       bool               `json:"cleanup_ok"`
	Quota           DriveQuotaSnapshot `json:"quota"`
	Failure         string             `json:"failure,omitempty"`
}

func TestLiveDenseP1Continuation(t *testing.T) {
	if os.Getenv("SKIRK_DENSE_LIVE") != "1" {
		t.Skip("isolated live Drive probe")
	}
	configPath := os.Getenv("SKIRK_DENSE_CONFIG")
	if !strings.HasSuffix(configPath, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		t.Fatal("isolated personal-test config required")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal("isolated config unavailable:", err)
	}
	if cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" || cfg.Route.GoogleIP == "" {
		t.Fatal("isolated folder and configured Google front IP required")
	}
	// The archived test kit predates the front-default UI and declares direct.
	// Select the user's pinned Google front in memory for this run.
	cfg.Route.Mode = "google_front_pinned"
	cfg.Drive.NormalListPageSize = 100
	store := NewDriveStoreWithTokenSource(NewGoogleHTTPClient(cfg.Route), NewAccessTokenSource(cfg.Auth, cfg.Route), cfg.Drive)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var sid [16]byte
	if _, err := rand.Read(sid[:]); err != nil {
		t.Fatal(err)
	}
	prefix := muxDirPrefix(sid, DirectionDown, "dense-probe", "isolated-run")
	if !strings.HasPrefix(prefix, "muxv4/") {
		t.Fatal("invalid unique prefix")
	}
	report := denseLiveReport{Prefix: prefix}
	runTag := fmt.Sprintf("dense-p1-live-%s-%x", time.Now().UTC().Format("20060102T150405Z"), sid[:4])
	reportPath := filepath.Join(".skirk-runs", runTag+".json")
	ledgerPath := filepath.Join(".skirk-runs", runTag+".recovery-ids")
	if err := os.MkdirAll(".skirk-runs", 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{reportPath, ledgerPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("live report or ledger already exists")
		}
	}
	ids, err := store.GenerateObjectIDs(ctx, denseLiveCount)
	if err != nil || len(ids) != denseLiveCount {
		t.Fatal("could not reserve exact Drive IDs:", err)
	}
	ledger, err := os.OpenFile(ledgerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(ledger, "#", prefix)
	for _, id := range ids {
		fmt.Fprintln(ledger, id)
	}
	if err := ledger.Sync(); err != nil {
		ledger.Close()
		t.Fatal("could not sync recovery IDs:", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated prefix=%s recovery_ledger=%s", prefix, ledgerPath)
	// Cleanup is registered before the first write. The ledger retains every
	// reserved ID if the process is interrupted before cleanup can finish.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Minute)
		defer cleanupCancel()
		paced := time.NewTicker(350 * time.Millisecond)
		defer paced.Stop()
		for _, id := range ids {
			for attempt := 0; attempt < 3; attempt++ {
				<-paced.C
				if err := store.DeleteID(cleanupCtx, id); err == nil {
					report.Deleted++
					break
				} else if attempt == 2 {
					report.Failure = "delete_failed"
					t.Error("exact Drive ID delete failed:", err)
				}
			}
		}
		for _, id := range ids {
			_, _, err := store.GetMuxObjectMetadataByID(cleanupCtx, id)
			var apiErr *GoogleAPIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				report.Exact404++
			} else {
				report.Failure = "exact_404_missing"
				t.Error("exact Drive ID still visible or unverifiable:", id, err)
			}
		}
		check := func() int {
			listed, err := store.ListFreshContainsPageStatus(cleanupCtx, []string{prefix}, time.Time{}, "", 16)
			if err != nil || listed.Truncated || listed.Incomplete {
				report.Failure = "prefix_check_failed"
				t.Error("exact prefix check failed:", err)
				return -1
			}
			return len(listed.Objects)
		}
		report.ImmediateLeft = check()
		select {
		case <-time.After(30 * time.Second):
		case <-cleanupCtx.Done():
		}
		report.DelayedLeft = check()
		report.CleanupOK = report.Deleted == denseLiveCount && report.Exact404 == denseLiveCount && report.ImmediateLeft == 0 && report.DelayedLeft == 0
		if !report.CleanupOK {
			t.Error("isolated Drive cleanup was not verified; keep recovery ledger")
		}
		report.Quota = store.QuotaSnapshot()
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.WriteFile(reportPath, data, 0600)
		}
		if err != nil {
			t.Error("could not write private report:", err)
		}
		if report.CleanupOK {
			if err := os.Remove(ledgerPath); err != nil {
				t.Error("could not remove completed recovery ledger:", err)
			}
		}
	})

	startedAt := time.Now().Add(-time.Second)
	pace := time.NewTicker(350 * time.Millisecond)
	defer pace.Stop()
	payload := make([]byte, 64)
	for i, id := range ids {
		<-pace.C
		name := muxObjectNameWithStreamIDs(sid, DirectionDown, "dense-probe", "isolated-run", "epoch", uint64(i+1), nil, 0, uint64(i+1), 1, 1, 1, 64, false)
		info, err := store.PutObjectWithID(ctx, id, name, payload)
		if err != nil || info.ID != id || info.Name != name {
			report.Failure = "create_failed"
			t.Fatalf("object %d create failed: %v", i, err)
		}
		report.ObjectsCreated++
		if report.ObjectsCreated%50 == 0 || report.ObjectsCreated == denseLiveCount {
			t.Logf("created %d/%d isolated Drive objects", report.ObjectsCreated, denseLiveCount)
		}
	}
	// Wait for all exact IDs to appear under this run's unique prefix before
	// measuring the four-page runtime scan.
	var visible ObjectListInfo
	for attempt := 0; attempt < 30; attempt++ {
		visible, err = store.ListFreshContainsPageStatus(ctx, []string{prefix, "/p1/"}, startedAt, "", 16)
		if err == nil && !visible.Truncated && !visible.Incomplete && len(visible.Objects) == denseLiveCount {
			break
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
		}
	}
	report.Visible = len(visible.Objects)
	if err != nil || report.Visible != denseLiveCount || visible.Truncated || visible.Incomplete {
		report.Failure = "visibility_failed"
		t.Fatalf("only %d/%d objects visible: %v", report.Visible, denseLiveCount, err)
	}

	counted := &denseLiveStore{DriveStore: store}
	mux := &driveMux{t: &Tunnel{Data: counted, PollInterval: 250 * time.Millisecond}, startedAt: startedAt, listSince: startedAt}
	target := ids[0] // Created first, older than all 401 other p1 objects.
	for poll := 1; poll <= 10; poll++ {
		infos, err := mux.listRecvMuxObjectsByClass(ctx, prefix, "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
		if err != nil {
			report.Failure = "runtime_list_failed"
			t.Fatal("runtime p1 list failed:", err)
		}
		for _, info := range infos {
			if info.ID == target {
				report.TargetPoll = poll
			}
		}
		if poll == 1 {
			report.TargetFirstPoll = report.TargetPoll == 1
			if !mux.hasListFreshPageToken() {
				report.Failure = "missing_continuation"
				t.Fatal("first four-page scan did not retain a continuation")
			}
		}
		if report.TargetPoll != 0 {
			break
		}
	}
	report.Calls = counted.calls
	if report.TargetFirstPoll || report.TargetPoll < 2 || report.TargetPoll > 10 {
		report.Failure = "target_not_reached_after_continuation"
		t.Fatalf("older target reached on poll %d; want 2..10", report.TargetPoll)
	}
	for _, call := range counted.calls {
		if call.Pages > call.Budget || call.Budget > 4 {
			report.Failure = "page_budget_exceeded"
			t.Fatal("runtime p1 poll exceeded page budget")
		}
	}
	t.Logf("live older p1 reached on poll %d after %d Drive list calls; cleanup pending", report.TargetPoll, len(counted.calls))
}
