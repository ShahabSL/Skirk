package skirk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fsHybridLiveFakeRequest(host string) *http.Request {
	return &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "www.google.com", Path: "/v1/public"}, Host: host, Header: make(http.Header)}
}

func TestFSHybridLiveWorkloadRunsOfflineThroughActualTLSMux(t *testing.T) {
	f := fsHybridFullMuxFixture(t, false)
	file, e := os.OpenFile(filepath.Join(t.TempDir(), "progress.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	j := &fsHybridLiveJournal{file: file, cancel: f.cancel}
	r := &fsHybridLiveRun{ctx: f.ctx, cancel: f.cancel, cm: f.cm, em: f.em, stores: []*fsHybridPriorityStore{f.client, f.exit}, journal: j, logger: f.cm.t.Logger, settings: fsHybridLiveSettings{Mode: "hybrid", PayloadSeedHex: strings.Repeat("31", 32)}}
	t.Cleanup(func() {
		if e := r.stop(); e != nil {
			t.Error(e)
		}
	})
	if e = r.workload(); e != nil {
		t.Fatal("exact live workload failed in owned offline fixture", e, f.client.Error(), f.exit.Error())
	}
	if e = r.stop(); e != nil {
		t.Fatal(e)
	}
	if len(j.rows) != 43 || r.gets.Load() != 1 || r.posts.Load() != 1 || r.texts.Load() != 41 || !r.joined {
		t.Fatal("offline live-workload counts/join differ", len(j.rows))
	}
	for _, row := range j.rows {
		if !row.OK || row.RTTMS <= 0 || row.ScheduledMS < row.RTTMS {
			t.Fatal("invalid successful measurement row", row)
		}
	}
}
func TestFSHybridLiveAggregateGuardsPreserveCleanupReserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := fsHybridLiveNewBudget("drive", cancel)
	b.MaxCalls = 34
	r := fsHybridLiveFakeRequest("www.googleapis.com")
	for i := 0; i < 2; i++ {
		if err := b.reserve(r, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.reserve(r, false); err == nil || ctx.Err() == nil {
		t.Fatal("activecalls consumedcleanupreserve")
	}
	for i := 0; i < 32; i++ {
		if err := b.reserve(r, true); err != nil {
			t.Fatal("cleanupreserve unavailable", i)
		}
	}
	if err := b.reserve(r, true); err == nil {
		t.Fatal("absolutecallcap ignored")
	}
	if got := b.snapshot()["attempts_upper"]; got != int64(34*8) {
		t.Fatal("opaqueattemptupper notreserved", got)
	}
}

func TestFSHybridLiveDriveQuotaUsesOneMinuteWindowAndRetainsWholeArmBound(t *testing.T) {
	request := fsHybridLiveFakeRequest("www.googleapis.com")
	active := fsHybridLiveNewBudget("drive", nil)
	active.MaxMinuteUnits = 40080 // 80 active units, with 40,000 reserved.
	for i := 0; i < 2; i++ {
		if err := active.reserve(request, false); err != nil {
			t.Fatal("initial per-minute admission", err)
		}
	}
	active.mu.Lock()
	for i := range active.MinuteReservations {
		active.MinuteReservations[i].At = time.Now().Add(-61 * time.Second)
	}
	active.mu.Unlock()
	if err := active.reserve(request, false); err != nil {
		t.Fatal("older-than-one-minute units were not released", err)
	}
	if active.Units != 120 || active.MinuteUnits != 40 {
		t.Fatal("whole-arm and minute upper reservations diverged", active.Units, active.MinuteUnits)
	}
	limitedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limited := fsHybridLiveNewBudget("drive", cancel)
	limited.MaxMinuteUnits = 40080
	for i := 0; i < 2; i++ {
		if err := limited.reserve(request, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := limited.reserve(request, false); err == nil || limitedCtx.Err() == nil {
		t.Fatal("same-minute upper bound admitted third active request")
	}
	if err := limited.reserve(request, true); err != nil {
		t.Fatal("same-minute cleanup reserve was unavailable", err)
	}
}
func TestFSHybridLiveAggregateResponseBoundAndDirectOriginRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := fsHybridLiveNewBudget("firestore", cancel)
	b.MaxResponse = (4 << 20) + 3
	var calls atomic.Int64
	g := &fsHybridLiveGate{budget: b, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return fsHybridHTTP(200, []byte("four")), nil
	})}
	r := fsHybridLiveFakeRequest("firestore.googleapis.com")
	r.URL.Host = "firestore.googleapis.com"
	if _, e := g.RoundTrip(r); e == nil || calls.Load() != 0 {
		t.Fatal("directorigin exposure")
	}
	r.URL.Host = "www.google.com"
	resp, e := g.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(data) != 3 || e == nil || ctx.Err() == nil {
		t.Fatal("response aggregate overread", len(data), e)
	}
	if b.Response != 3 || b.PendingResponse != 0 {
		t.Fatal("response accounting invalid")
	}
	g.stop()
}
func TestFSHybridLiveCohortPersistsTimeoutsAndRejectsReuse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private-file permission fixture")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "campaign.json")
	campaign := map[string]any{"campaign_id": "fixture", "timeout_limit": 64, "arms": []map[string]string{{"arm_id": "arm-1", "mode": "hybrid"}, {"arm_id": "arm-2", "mode": "drive"}}}
	raw, _ := json.Marshal(campaign)
	if e := os.WriteFile(ledger, raw, 0600); e != nil {
		t.Fatal(e)
	}
	s := fsHybridLiveSettings{CampaignLedger: ledger, ArmID: "arm-1", Mode: "hybrid"}
	c, e := fsHybridLiveBeginCohort(s)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 15; i++ {
		if e = c.timeout(); e != nil {
			t.Fatal("rollingthreshold prematurelyspent", i)
		}
	}
	if e = c.timeout(); e == nil {
		t.Fatal("16/minthresholddidnotstop")
	}
	if e = c.close(false); e != nil {
		t.Fatal(e)
	}
	if _, e = fsHybridLiveBeginCohort(s); e == nil {
		t.Fatal("failedarm oractivecohort reused")
	}
	if e = os.Remove(ledger + ".active"); e != nil {
		t.Fatal(e)
	}
	s.ArmID = "arm-2"
	s.Mode = "drive"
	c, e = fsHybridLiveBeginCohort(s)
	if e != nil {
		t.Fatal(e)
	}
	if c.count != 16 {
		t.Fatal("cohort lost previoustimeouts")
	}
	if e = c.close(true); e != nil {
		t.Fatal(e)
	}
}

func TestFSHybridLiveStoppedActivePoolCannotBlockSeparateCleanup(t *testing.T) {
	b := fsHybridLiveNewBudget("drive", func() {})
	var calls atomic.Int64
	base := roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return fsHybridHTTP(204, nil), nil })
	active := &fsHybridLiveGate{base: base, budget: b}
	r := fsHybridLiveFakeRequest("www.googleapis.com")
	resp, e := active.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	if active.mu.TryLock() {
		active.mu.Unlock()
		t.Fatal("gate released an unjoined response body")
	}
	if e = resp.Body.Close(); e != nil {
		t.Fatal(e)
	}
	active.stop()
	if _, e = active.RoundTrip(r); e == nil {
		t.Fatal("stopped active pool admitted another request")
	}
	cleanup := &fsHybridLiveGate{base: base, budget: b, cleanup: true}
	resp, e = cleanup.RoundTrip(r)
	if e != nil {
		t.Fatal("separatecleanup pool blocked", e)
	}
	_ = resp.Body.Close()
	cleanup.stop()
	if calls.Load() != 2 || b.CleanupCalls != 1 {
		t.Fatal("cleanup gate accounting changed")
	}
}

func TestFSHybridLiveCohort64ThresholdSurvivesPriorArms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private-file permission fixture")
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ledger := filepath.Join(dir, "campaign.json")
	campaign := map[string]any{"campaign_id": "fixture", "timeout_limit": 64, "arms": []map[string]string{{"arm_id": "arm-1", "mode": "drive"}, {"arm_id": "arm-2", "mode": "hybrid"}, {"arm_id": "arm-3", "mode": "hybrid"}}}
	raw, _ := json.Marshal(campaign)
	if e = os.WriteFile(ledger, raw, 0600); e != nil {
		t.Fatal(e)
	}
	prior, e := os.OpenFile(ledger+".timeouts.jsonl", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 63; i++ {
		arm := "arm-1"
		if i >= 32 {
			arm = "arm-2"
		}
		if e = json.NewEncoder(prior).Encode(map[string]any{"arm_id": arm, "at_utc": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}); e != nil {
			t.Fatal(e)
		}
	}
	_ = prior.Close()
	c, e := fsHybridLiveBeginCohort(fsHybridLiveSettings{CampaignLedger: ledger, ArmID: "arm-3", Mode: "hybrid"})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.timeout(); e == nil || c.count != 64 {
		t.Fatal("cohort timeout cap reset between arms")
	}
	if e = c.close(false); e != nil {
		t.Fatal(e)
	}
}
