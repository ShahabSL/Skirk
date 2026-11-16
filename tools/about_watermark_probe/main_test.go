package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type fakeDrive struct {
	mu                sync.Mutex
	nextID            int
	change            uint64
	files             map[string]fileMeta
	lagAfterMutation  int
	lagRemaining      int
	tokenLagRemaining int
	oldChange         uint64
	aboutForbidden    bool
	tokenForbidden    bool
}

func fakeResult(status int, data any) (*skirk.HTTPResult, error) {
	b, err := json.Marshal(data)
	return &skirk.HTTPResult{Status: status, Body: b, Proto: "HTTP/2.0"}, err
}

func (f *fakeDrive) mutate() {
	f.oldChange = f.change
	f.change++
	f.lagRemaining = f.lagAfterMutation
	f.tokenLagRemaining = f.lagAfterMutation
}

func (f *fakeDrive) call(_ context.Context, _ actor, method, path string, _ map[string]string, body []byte) (*skirk.HTTPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == "/drive/v2/about?fields=largestChangeId" {
		if f.aboutForbidden {
			return fakeResult(403, map[string]string{"error": "forbidden"})
		}
		value := f.change
		if f.lagRemaining > 0 {
			value = f.oldChange
			f.lagRemaining--
		}
		return fakeResult(200, map[string]string{"largestChangeId": fmt.Sprint(value)})
	}
	if path == "/drive/v3/changes/startPageToken?fields=startPageToken" {
		if f.tokenForbidden {
			return fakeResult(403, map[string]string{"error": "forbidden"})
		}
		value := f.change
		if f.tokenLagRemaining > 0 {
			value = f.oldChange
			f.tokenLagRemaining--
		}
		return fakeResult(200, map[string]string{"startPageToken": fmt.Sprint(value)})
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/generateIds?") {
		f.nextID++
		return fakeResult(200, map[string]any{"ids": []string{fmt.Sprintf("generated_file_%04d", f.nextID)}})
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files?") {
		u, err := url.Parse(path)
		if err != nil {
			return nil, err
		}
		q := u.Query().Get("q")
		start := strings.Index(q, "name contains '")
		if start < 0 {
			return nil, errors.New("missing prefix query")
		}
		prefix := strings.Split(q[start+len("name contains '"):], "'")[0]
		files := []fileMeta{}
		for _, meta := range f.files {
			if strings.HasPrefix(meta.Name, prefix) {
				files = append(files, meta)
			}
		}
		return fakeResult(200, map[string]any{"files": files})
	}
	if method == http.MethodPost && strings.HasPrefix(path, "/drive/v3/files?fields=") {
		var meta fileMeta
		if err := json.Unmarshal(body, &meta); err != nil {
			return nil, err
		}
		if _, exists := f.files[meta.ID]; exists {
			return fakeResult(409, map[string]string{})
		}
		meta.OwnedByMe = true
		f.files[meta.ID] = meta
		f.mutate()
		return fakeResult(200, meta)
	}
	parts := strings.Split(strings.Split(path, "?")[0], "/")
	if len(parts) < 5 || parts[1] != "drive" || parts[2] != "v3" || parts[3] != "files" {
		return fakeResult(404, map[string]string{})
	}
	id := parts[4]
	meta, exists := f.files[id]
	if !exists {
		return fakeResult(404, map[string]string{})
	}
	switch method {
	case http.MethodGet:
		return fakeResult(200, meta)
	case http.MethodPatch:
		var v struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		meta.Description = v.Description
		f.files[id] = meta
		f.mutate()
		return fakeResult(200, meta)
	case http.MethodDelete:
		delete(f.files, id)
		f.mutate()
		return &skirk.HTTPResult{Status: 204, Proto: "HTTP/2.0"}, nil
	default:
		return fakeResult(400, map[string]string{})
	}
}

func testProbe(t *testing.T, f *fakeDrive) *probe {
	t.Helper()
	if f.files == nil {
		f.files = map[string]fileMeta{}
	}
	p := &probe{folder: "shared-folder", out: filepath.Join(t.TempDir(), "result.json"), call: f.call, pollDelay: time.Millisecond, cleanupDelay: time.Millisecond, result: report{Schema: 1, Calls: map[string]int{}}}
	p.targets[0] = target{Name: "wm-test-primary", Actor: client}
	p.targets[1] = target{Name: "wm-test-unrelated", Actor: exit}
	return p
}

func TestFullFakeWatermarkAndCleanup(t *testing.T) {
	f := &fakeDrive{files: map[string]fileMeta{}, change: 100}
	p := testProbe(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.result.Phases) != 6 || p.result.QuiescentSamples != 8 || p.result.QuiescentTokenSamples != 8 || p.result.QuiescentChanges != 0 || p.result.QuiescentTokenChanges != 0 {
		t.Fatalf("incomplete run: %+v", p.result)
	}
	if !p.result.V2PreflightOK || !p.result.V3PreflightOK || len(p.result.CallDurationsMS["about.get"]) == 0 || len(p.result.CallDurationsMS["changes.getStartPageToken"]) == 0 || p.result.Protocols["HTTP/2.0"] == 0 {
		t.Fatalf("preflight or front observations missing: %+v", p.result)
	}
	for _, row := range p.result.Phases {
		if !row.WatermarkObserved || !row.TokenObserved || !row.ListObserved || row.WatermarkDelta == 0 || row.ListVisibleWatermarkUnchanged || row.ListVisibleTokenUnchanged || row.AboutCalls == 0 || row.TokenCalls == 0 || row.ListCalls == 0 || row.NominalUnits != int64(row.AboutCalls*5+row.TokenCalls*5+row.ListCalls*100)+50 || row.UpperUnits != 8*row.NominalUnits {
			t.Fatalf("bad phase: %+v", row)
		}
		if strings.HasPrefix(row.Name, "unrelated_") && !row.PrimaryStayedEmpty {
			t.Fatal("unrelated activity polluted primary prefix")
		}
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupImmediate || !p.result.CleanupDelayed {
		t.Fatalf("incomplete exact cleanup: %+v", p.result)
	}
	if _, err := os.Stat(p.out + ".recovery.json"); !os.IsNotExist(err) {
		t.Fatal("cleanup retained recovery ledger", err)
	}
	if p.result.Calls["changes.list"] != 0 || p.result.Calls["about.get"] == 0 || p.result.Calls["changes.getStartPageToken"] == 0 {
		t.Fatal("wrong method family", p.result.Calls)
	}
}

func TestListVisibleWhileWatermarkUnchanged(t *testing.T) {
	f := &fakeDrive{files: map[string]fileMeta{}, change: 50, lagAfterMutation: 2}
	p := testProbe(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.reserve(ctx, 0); err != nil {
		t.Fatal(err)
	}
	row, err := p.phase(ctx, "primary_create", p.targets[0], "absent", "created", func(ctx context.Context) error { return p.create(ctx, p.targets[0], "created") }, false)
	if err != nil || !row.ListObserved || !row.WatermarkObserved || !row.TokenObserved || !row.ListVisibleWatermarkUnchanged || !row.ListVisibleTokenUnchanged {
		t.Fatalf("lag witness missed: row=%+v err=%v", row, err)
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupDelayed {
		t.Fatal("lag case left file")
	}
}

func TestScopeFailureDoesNotCreateFile(t *testing.T) {
	f := &fakeDrive{files: map[string]fileMeta{}, change: 20, aboutForbidden: true}
	p := testProbe(t, f)
	ctx := context.Background()
	if err := p.run(ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("scope denial not surfaced: %v", err)
	}
	if len(f.files) != 0 {
		t.Fatal("scope failure created a file")
	}
	if p.result.V2PreflightOK || !p.result.V3PreflightOK || f.nextID != 0 {
		t.Fatal("independent read preflight failed", p.result)
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupDelayed {
		t.Fatal("preflight failure cleanup state invalid")
	}
}

func TestStartPageTokenScopeFailureDoesNotCreateFile(t *testing.T) {
	f := &fakeDrive{files: map[string]fileMeta{}, change: 20, tokenForbidden: true}
	p := testProbe(t, f)
	ctx := context.Background()
	if err := p.run(ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("token scope denial not surfaced: %v", err)
	}
	if len(f.files) != 0 {
		t.Fatal("scope failure created a file")
	}
	if !p.result.V2PreflightOK || p.result.V3PreflightOK || f.nextID != 0 {
		t.Fatal("independent read preflight failed", p.result)
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupDelayed {
		t.Fatal("preflight failure cleanup state invalid")
	}
}

func TestQuotaGuardAndCostAllowlist(t *testing.T) {
	p := testProbe(t, &fakeDrive{files: map[string]fileMeta{}})
	if label, cost, err := methodCost("GET", "/drive/v2/about?fields=largestChangeId"); err != nil || label != "about.get" || cost != 5 {
		t.Fatal("about GET wrongly costed", label, cost, err)
	}
	if label, cost, err := methodCost("GET", "/drive/v3/changes/startPageToken?fields=startPageToken"); err != nil || label != "changes.getStartPageToken" || cost != 5 {
		t.Fatal("start page token GET wrongly costed", label, cost, err)
	}
	if _, _, err := methodCost("GET", "/drive/v2/changes/123"); err == nil {
		t.Fatal("unapproved path admitted")
	}
	p.activeUpper = activeUpperLimit
	if _, err := p.request(context.Background(), false, exit, "GET", "/drive/v2/about?fields=largestChangeId", nil); err == nil {
		t.Fatal("active quota guard ignored")
	}
	p.targets[0].ID = "generated_file_0001"
	if _, err := p.request(context.Background(), true, client, "GET", path(p.targets[0])+"?fields=id", nil); err != nil {
		t.Fatal("active cap consumed cleanup reserve", err)
	}
}

func TestCleanupRefusesForeignIdentity(t *testing.T) {
	f := &fakeDrive{files: map[string]fileMeta{}}
	p := testProbe(t, f)
	ctx := context.Background()
	if err := p.reserve(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.create(ctx, p.targets[0], "created"); err != nil {
		t.Fatal(err)
	}
	original := p.targets[0].Name
	p.targets[0].Name = "different-name"
	p.cleanupOwned()
	if p.result.CleanupExact404 || len(f.files) != 1 {
		t.Fatal("unproven file deleted")
	}
	p.targets[0].Name = original
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupDelayed {
		t.Fatal("proven file not cleaned")
	}
}
