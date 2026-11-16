package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

// driveFake models conditional metadata writes and exact-ID visibility. It is
// deliberately synchronous, so its timing values are not Google measurements.
type driveFake struct {
	mu                sync.Mutex
	files             map[string]driveFile
	versions          map[string]int
	nextID            int
	ignoreFence       bool
	dropPatchResponse bool
}

func newDriveFake() *driveFake {
	return &driveFake{files: map[string]driveFile{}, versions: map[string]int{}}
}

func (d *driveFake) response(status int, body any, tag string) (*skirk.HTTPResult, error) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	h := http.Header{}
	if tag != "" {
		h.Set("ETag", tag)
	}
	return &skirk.HTTPResult{Status: status, Body: b, Header: h}, nil
}

func (d *driveFake) tag(id string) string { return fmt.Sprintf("\"%d\"", d.versions[id]) }

func (d *driveFake) call(_ context.Context, _ actor, method, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && u.Path == "/drive/v2/files/generateIds" {
		d.nextID++
		return d.response(200, map[string]any{"ids": []string{fmt.Sprintf("generated-%016d", d.nextID)}}, "")
	}
	if method == http.MethodGet && u.Path == "/drive/v2/files" {
		q := u.Query().Get("q")
		i := strings.Index(q, "title contains '")
		if i < 0 {
			return nil, fmt.Errorf("missing title filter")
		}
		prefix := strings.TrimSuffix(q[i+len("title contains '"):], "'")
		items := make([]driveFile, 0)
		for _, f := range d.files {
			if strings.Contains(f.Title, prefix) {
				items = append(items, f)
			}
		}
		return d.response(200, map[string]any{"items": items}, "")
	}
	if method == http.MethodPost && u.Path == "/drive/v2/files" {
		var f driveFile
		if err := json.Unmarshal(body, &f); err != nil {
			return nil, err
		}
		if _, exists := d.files[f.ID]; exists {
			return d.response(409, nil, "")
		}
		f.OwnedByMe, f.IsAppAuthorized = true, true
		d.versions[f.ID] = 1
		f.ETag = d.tag(f.ID)
		d.files[f.ID] = f
		return d.response(200, f, f.ETag)
	}
	id := strings.TrimPrefix(u.Path, "/drive/v2/files/")
	if id == u.Path {
		return nil, fmt.Errorf("unexpected path %q", path)
	}
	f, exists := d.files[id]
	if !exists {
		return d.response(404, nil, "")
	}
	switch method {
	case http.MethodGet:
		return d.response(200, f, f.ETag)
	case http.MethodPatch:
		if !d.ignoreFence && headers["If-Match"] != f.ETag {
			return d.response(412, nil, "")
		}
		var patch struct{ Description string }
		if err := json.Unmarshal(body, &patch); err != nil {
			return nil, err
		}
		f.Description = patch.Description
		d.versions[id]++
		f.ETag = d.tag(id)
		d.files[id] = f
		if d.dropPatchResponse {
			d.dropPatchResponse = false
			return nil, fmt.Errorf("injected response loss after commit")
		}
		return d.response(200, f, f.ETag)
	case http.MethodDelete:
		delete(d.files, id)
		return d.response(204, nil, "")
	default:
		return nil, fmt.Errorf("unexpected method %s", method)
	}
}

func fakeProbe(t *testing.T, d *driveFake) *probe {
	t.Helper()
	p, err := newProbe("test-folder-1234567890", t.TempDir()+"/result.json")
	if err != nil {
		t.Fatal(err)
	}
	p.call = d.call
	return p
}

func TestAllArmsFencedReadbackAndCleanup(t *testing.T) {
	d := newDriveFake()
	p := fakeProbe(t, d)
	d.files["unrelated-sentinel-1234567890"] = driveFile{ID: "unrelated-sentinel-1234567890", Title: "user-sentinel", Parents: []parentRef{{ID: p.folder}}, OwnedByMe: true, IsAppAuthorized: true}
	r := report{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.run(ctx, &r); err != nil {
		t.Fatalf("full fake run: %v; arms=%+v", err, r.Arms)
	}
	if len(r.Arms) != 8 || r.CapabilityChecks != 4 || !r.LostResponse || len(p.targets) != 23 {
		t.Fatalf("incomplete experiment: arms=%d capability=%d lost=%t targets=%d", len(r.Arms), r.CapabilityChecks, r.LostResponse, len(p.targets))
	}
	conflicts := 0
	for _, arm := range r.Arms {
		if arm.Delivered != arm.Writers || arm.P95MS < 0 || arm.UpperUnits < arm.NominalUnits {
			t.Fatalf("invalid arm: %+v", arm)
		}
		conflicts += arm.Conflicts412
	}
	if conflicts < 11 { // 1+3+7 forced same-tag losers; scheduler may add more.
		t.Fatalf("missing expected conditional contention: %d", conflicts)
	}
	m := p.metricCopy()
	t.Logf("fake: conflicts=%d calls=%v nominal=%d upper=%d", conflicts, m.Calls, m.NominalUnits, m.UpperUnits)
	if exact, immediate, delayed := p.cleanupOwned(time.Millisecond); !exact || !immediate || !delayed {
		t.Fatalf("incomplete cleanup: exact=%t immediate=%t delayed=%t", exact, immediate, delayed)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.files) != 1 || d.files["unrelated-sentinel-1234567890"].Title != "user-sentinel" {
		t.Fatalf("owned cleanup touched sentinel or leaked files: %v", d.files)
	}
}

func TestIgnoredIfMatchAbortsBeforeExperiment(t *testing.T) {
	d := newDriveFake()
	d.ignoreFence = true
	p := fakeProbe(t, d)
	ctx := context.Background()
	gen, err := p.reserve(ctx, p.stem+"-generation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.create(ctx, gen, mustSnapshot(p, snapshot{Session: p.sessionID()})); err != nil {
		t.Fatal(err)
	}
	if err := p.capability(ctx, gen); err == nil || !strings.Contains(err.Error(), "ignored bogus If-Match") {
		t.Fatalf("unsafe conditional update was not rejected: %v", err)
	}
}

func TestLostResponseAfterCommitUsesExactReadback(t *testing.T) {
	d := newDriveFake()
	p := fakeProbe(t, d)
	ctx := context.Background()
	gen, err := p.reserve(ctx, p.stem+"-generation")
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.newClaim()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.create(ctx, gen, mustSnapshot(p, snapshot{Session: p.sessionID(), Claims: []claim{c}})); err != nil {
		t.Fatal(err)
	}
	d.dropPatchResponse = true
	known := map[string]claim{c.ID: c}
	if err := p.lostResponse(ctx, gen, known); err != nil {
		t.Fatal(err)
	}
	if len(known) != 1 {
		t.Fatal("lost-response check mutated the next arm's expected claims")
	}
	f, _, err := p.get(ctx, gen)
	if err != nil {
		t.Fatal(err)
	}
	var s snapshot
	if json.Unmarshal([]byte(f.Description), &s) != nil || len(s.Claims) != 2 || d.dropPatchResponse {
		t.Fatalf("uncertain write did not resolve to two exact claims: %+v", s)
	}
}

func TestActiveQuotaGuardStopsBeforeCall(t *testing.T) {
	p := fakeProbe(t, newDriveFake())
	p.active = activeUpperLimit
	called := false
	p.call = func(context.Context, actor, string, string, map[string]string, []byte) (*skirk.HTTPResult, error) {
		called = true
		return nil, nil
	}
	_, err := p.request(context.Background(), false, reader, http.MethodGet, "/drive/v2/files/some-id-1234567890?fields=id", nil, nil)
	if err == nil || called {
		t.Fatalf("quota guard did not fail closed before network: err=%v called=%t", err, called)
	}
}
