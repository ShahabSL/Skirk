// fixed_rendezvous_probe is a bounded, test-only Drive primitive. It does not
// change Skirk's product carrier or profiles and has no live path without -live.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	snapshotMaxBytes = 16 << 10
	pollDelay        = 100 * time.Millisecond
	armTimeout       = 25 * time.Second
	maxCASAttempts   = 8
)

type claim struct {
	ID   string
	Body string
	MAC  string
}

type snapshot struct {
	Session string
	Probe   int
	Claims  []claim
}

type anchor struct {
	Session    string
	Generation string
	MAC        string
}

type armReport struct {
	Carrier       string
	Writers       int
	Delivered     int
	P50MS         float64
	P95MS         float64
	MaxMS         float64
	ReadyGateMS   float64
	PostGateP50MS float64
	PostGateP95MS float64
	PostGateMaxMS float64
	WallMS        float64
	Conflicts412  int
	CASAttempts   int
	SnapshotBytes int
	Calls         map[string]int
	Statuses      map[string]int
	NominalUnits  int64
	UpperUnits    int64
	ResponseBytes int64
	RequestBytes  int64
	Failure       string
}

type report struct {
	Schema           int
	Route            string
	IdentityScope    string
	Arms             []armReport
	CapabilityChecks int
	LostResponse     bool
	Calls            map[string]int
	Statuses         map[string]int
	NominalUnits     int64
	UpperUnits       int64
	ResponseBytes    int64
	RequestBytes     int64
	CleanupExact404  bool
	CleanupImmediate bool
	CleanupDelayed   bool
	Failure          string
	Completed        bool
}

func (p *probe) mac(parts ...string) string {
	h := hmac.New(sha256.New, p.key[:])
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *probe) sessionID() string { return hex.EncodeToString(p.session[:]) }

func (p *probe) newClaim() (claim, error) {
	var id [12]byte
	var body [64]byte
	if _, err := rand.Read(id[:]); err != nil {
		return claim{}, err
	}
	if _, err := rand.Read(body[:]); err != nil {
		return claim{}, err
	}
	c := claim{ID: hex.EncodeToString(id[:]), Body: base64.RawURLEncoding.EncodeToString(body[:])}
	c.MAC = p.mac(p.sessionID(), c.ID, c.Body)
	return c, nil
}

func (p *probe) verifyClaim(c claim) bool {
	if len(c.ID) != 24 || len(c.Body) > 128 || len(c.MAC) != 64 {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(c.Body)
	if err != nil || len(raw) != 64 {
		return false
	}
	want := p.mac(p.sessionID(), c.ID, c.Body)
	return hmac.Equal([]byte(want), []byte(c.MAC))
}

func (p *probe) encodeSnapshot(s snapshot) (string, error) {
	if s.Session != p.sessionID() {
		return "", errors.New("snapshot session mismatch")
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > snapshotMaxBytes {
		return "", errors.New("snapshot exceeds encoded cap")
	}
	return string(b), nil
}

func (p *probe) decodeSnapshot(desc string, known map[string]claim) (snapshot, error) {
	var s snapshot
	if len(desc) > snapshotMaxBytes || json.Unmarshal([]byte(desc), &s) != nil || s.Session != p.sessionID() {
		return s, errors.New("invalid snapshot")
	}
	seen := map[string]bool{}
	for _, c := range s.Claims {
		k, ok := known[c.ID]
		if !ok || seen[c.ID] || !p.verifyClaim(c) || c != k {
			return s, errors.New("invalid, unknown, or duplicate claim")
		}
		seen[c.ID] = true
	}
	return s, nil
}

func (p *probe) anchorDescription(generation ownedTarget) string {
	a := anchor{Session: p.sessionID(), Generation: generation.ID}
	a.MAC = p.mac(a.Session, a.Generation)
	b, _ := json.Marshal(a)
	return string(b)
}

func (p *probe) verifyAnchor(desc, generationID string) error {
	var a anchor
	if json.Unmarshal([]byte(desc), &a) != nil || a.Session != p.sessionID() || a.Generation != generationID || !hmac.Equal([]byte(a.MAC), []byte(p.mac(a.Session, a.Generation))) {
		return errors.New("anchor authentication failed")
	}
	return nil
}

func (p *probe) capability(ctx context.Context, generation ownedTarget) error {
	f, originalTag, err := p.get(ctx, generation)
	if err != nil {
		return err
	}
	empty := snapshot{Session: p.sessionID()}
	if f.Description != mustSnapshot(p, empty) {
		return errors.New("generation initial snapshot mismatch")
	}
	bogus := "\"skirk-probe-not-current-" + p.sessionID() + "\""
	_, status, err := p.patch(ctx, generation, bogus, mustSnapshot(p, snapshot{Session: p.sessionID(), Probe: 77}))
	if err != nil || status != 412 {
		return errors.New("v2 generation ignored bogus If-Match")
	}
	one := mustSnapshot(p, snapshot{Session: p.sessionID(), Probe: 1})
	if _, status, err = p.patch(ctx, generation, originalTag, one); err != nil || status != 200 {
		return errors.New("v2 generation rejected valid If-Match")
	}
	f, freshTag, err := p.get(ctx, generation)
	if err != nil || f.Description != one || freshTag == originalTag {
		return errors.New("v2 generation ETag did not change")
	}
	_, status, err = p.patch(ctx, generation, originalTag, mustSnapshot(p, snapshot{Session: p.sessionID(), Probe: 88}))
	if err != nil || status != 412 {
		return errors.New("v2 generation accepted stale If-Match")
	}
	zero := mustSnapshot(p, empty)
	if _, status, err = p.patch(ctx, generation, freshTag, zero); err != nil || status != 200 {
		return errors.New("v2 generation reset failed")
	}
	f, tag, err := p.get(ctx, generation)
	if err != nil || f.Description != zero || tag == freshTag {
		return errors.New("v2 generation reset tag did not change")
	}
	return nil
}

func mustSnapshot(p *probe, s snapshot) string {
	v, err := p.encodeSnapshot(s)
	if err != nil {
		panic(err)
	}
	return v
}

func pctl(v []float64, percentile int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := (len(s)*percentile + 99) / 100
	return s[i-1]
}

func summaries(start time.Time, seen map[string]time.Time) (p50, p95, max, wall float64) {
	values := make([]float64, 0, len(seen))
	for _, at := range seen {
		values = append(values, float64(at.Sub(start).Microseconds())/1000)
	}
	p50, p95 = pctl(values, 50), pctl(values, 95)
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	wall = float64(time.Since(start).Microseconds()) / 1000
	return
}

func (p *probe) fixedReader(ctx context.Context, generation ownedTarget, expected map[string]claim, seen map[string]time.Time, seenMu *sync.Mutex) error {
	for {
		f, _, err := p.get(ctx, generation)
		if err != nil {
			return err
		}
		s, err := p.decodeSnapshot(f.Description, expected)
		if err != nil {
			return err
		}
		now := time.Now()
		seenMu.Lock()
		for _, c := range s.Claims {
			if seen[c.ID].IsZero() {
				seen[c.ID] = now
			}
		}
		done := len(seen) == len(expected)
		seenMu.Unlock()
		if done {
			return nil
		}
		if err := wait(ctx, pollDelay); err != nil {
			return err
		}
	}
}

type publishResult struct {
	Attempts  int
	Conflicts int
	Err       error
}

func (p *probe) publishFixed(ctx context.Context, anchorTarget, generation ownedTarget, c claim, known map[string]claim, gate <-chan struct{}, ready chan<- error) publishResult {
	f, _, err := p.get(ctx, anchorTarget)
	if err == nil {
		err = p.verifyAnchor(f.Description, generation.ID)
	}
	if err != nil {
		ready <- err
		return publishResult{Err: err}
	}
	f, tag, err := p.get(ctx, generation)
	if err != nil {
		ready <- err
		return publishResult{Err: err}
	}
	s, err := p.decodeSnapshot(f.Description, known)
	ready <- err
	if err != nil {
		return publishResult{Err: err}
	}
	select {
	case <-gate:
	case <-ctx.Done():
		return publishResult{Err: ctx.Err()}
	}
	out := publishResult{}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		for _, old := range s.Claims {
			if old.ID == c.ID {
				if old != c {
					out.Err = errors.New("same-ID different-body claim")
				}
				return out
			}
		}
		next := s
		next.Claims = append(append([]claim(nil), s.Claims...), c)
		desc, encErr := p.encodeSnapshot(next)
		if encErr != nil {
			out.Err = encErr
			return out
		}
		out.Attempts++
		_, status, patchErr := p.patch(ctx, generation, tag, desc)
		if patchErr != nil {
			out.Err = patchErr
			return out
		}
		if status == 200 {
			return out
		}
		out.Conflicts++
		f, tag, err = p.get(ctx, generation)
		if err != nil {
			out.Err = err
			return out
		}
		s, err = p.decodeSnapshot(f.Description, known)
		if err != nil {
			out.Err = err
			return out
		}
	}
	out.Err = errors.New("bounded CAS retry exhausted")
	return out
}

func (p *probe) runFixed(parent context.Context, anchorTarget, generation ownedTarget, expected map[string]claim) (armReport, error) {
	row := armReport{Carrier: "fixed-known-id", Writers: len(expected)}
	ctx, cancel := context.WithTimeout(parent, armTimeout)
	defer cancel()
	before := p.metricCopy()
	start := time.Now()
	seen := map[string]time.Time{}
	var seenMu sync.Mutex
	readerDone := make(chan error, 1)
	go func() { readerDone <- p.fixedReader(ctx, generation, expected, seen, &seenMu) }()
	gate := make(chan struct{})
	ready := make(chan error, len(expected))
	results := make(chan publishResult, len(expected))
	var wg sync.WaitGroup
	for _, c := range expected {
		wg.Add(1)
		go func(c claim) {
			defer wg.Done()
			results <- p.publishFixed(ctx, anchorTarget, generation, c, expected, gate, ready)
		}(c)
	}
	var readinessErr error
readiness:
	for i := 0; i < len(expected); i++ {
		select {
		case err := <-ready:
			if err != nil {
				readinessErr = err
				cancel()
				break readiness
			}
		case <-ctx.Done():
			readinessErr = ctx.Err()
			break readiness
		}
	}
	release := time.Now()
	close(gate)
	wg.Wait()
	close(results)
	for v := range results {
		row.CASAttempts += v.Attempts
		row.Conflicts412 += v.Conflicts
		if v.Err != nil && row.Failure == "" {
			row.Failure = v.Err.Error()
			cancel()
		}
	}
	if readinessErr != nil && row.Failure == "" {
		row.Failure = readinessErr.Error()
	}
	if err := <-readerDone; err != nil && row.Failure == "" {
		row.Failure = err.Error()
	}
	seenMu.Lock()
	row.Delivered = len(seen)
	row.P50MS, row.P95MS, row.MaxMS, row.WallMS = summaries(start, seen)
	row.ReadyGateMS = float64(release.Sub(start).Microseconds()) / 1000
	row.PostGateP50MS, row.PostGateP95MS, row.PostGateMaxMS, _ = summaries(release, seen)
	seenMu.Unlock()
	f, _, err := p.get(parent, generation)
	if err != nil && row.Failure == "" {
		row.Failure = err.Error()
	} else if err == nil {
		row.SnapshotBytes = len(f.Description)
		s, parseErr := p.decodeSnapshot(f.Description, expected)
		if parseErr != nil || len(s.Claims) != len(expected) {
			row.Failure = "final fixed snapshot missing exact claims"
		}
	}
	diff := metricDiff(before, p.metricCopy())
	row.Calls, row.Statuses = diff.Calls, diff.Statuses
	row.NominalUnits, row.UpperUnits, row.ResponseBytes, row.RequestBytes = diff.NominalUnits, diff.UpperUnits, diff.ResponseBytes, diff.RequestBytes
	if row.Failure == "" && row.Delivered != row.Writers {
		row.Failure = "fixed reader did not deliver all claims"
	}
	if row.Failure != "" {
		return row, errors.New(row.Failure)
	}
	return row, nil
}

func (p *probe) lostResponse(ctx context.Context, generation ownedTarget, known map[string]claim) error {
	c, err := p.newClaim()
	if err != nil {
		return err
	}
	knownWithLost := make(map[string]claim, len(known)+1)
	for id, existing := range known {
		knownWithLost[id] = existing
	}
	knownWithLost[c.ID] = c
	f, tag, err := p.get(ctx, generation)
	if err != nil {
		return err
	}
	s, err := p.decodeSnapshot(f.Description, knownWithLost)
	if err != nil {
		return err
	}
	s.Claims = append(s.Claims, c)
	desc, err := p.encodeSnapshot(s)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"description": desc})
	// Inject uncertainty at the caller boundary. Send exactly once, then discard
	// both response and error; readback alone decides whether it committed.
	_, _ = p.request(ctx, false, writer, "PATCH", filePath(generation)+"?fields="+patchFields, map[string]string{"If-Match": tag}, body)
	for i := 0; i < 3; i++ {
		f, _, err = p.get(ctx, generation)
		if err != nil {
			return err
		}
		s, err = p.decodeSnapshot(f.Description, knownWithLost)
		if err != nil {
			return err
		}
		count := 0
		for _, got := range s.Claims {
			if got.ID == c.ID && got == c {
				count++
			}
		}
		if count == 1 {
			return nil
		}
		if i < 2 && wait(ctx, time.Duration(i+1)*100*time.Millisecond) != nil {
			return ctx.Err()
		}
	}
	return errors.New("lost-response exact readback missing")
}

func (p *probe) controlReader(ctx context.Context, prefix string, expected map[string]claim, byID map[string]string, seen map[string]time.Time, mu *sync.Mutex) error {
	for {
		files, err := p.list(ctx, false, prefix)
		if err != nil {
			return err
		}
		now := time.Now()
		mu.Lock()
		for _, f := range files {
			claimID, ok := byID[f.ID]
			if !ok || f.Title != prefix+claimID {
				mu.Unlock()
				return errors.New("unknown control file")
			}
			var got claim
			if json.Unmarshal([]byte(f.Description), &got) != nil || got != expected[claimID] || !p.verifyClaim(got) {
				mu.Unlock()
				return errors.New("control claim authentication failure")
			}
			if seen[claimID].IsZero() {
				seen[claimID] = now
			}
		}
		done := len(seen) == len(expected)
		mu.Unlock()
		if done {
			return nil
		}
		if err := wait(ctx, pollDelay); err != nil {
			return err
		}
	}
}

func (p *probe) runControl(parent context.Context, prefix string, targets []ownedTarget, expected map[string]claim, byID map[string]string) (armReport, error) {
	row := armReport{Carrier: "immutable-create-list", Writers: len(expected)}
	ctx, cancel := context.WithTimeout(parent, armTimeout)
	defer cancel()
	before := p.metricCopy()
	start := time.Now()
	seen := map[string]time.Time{}
	var mu sync.Mutex
	readerDone := make(chan error, 1)
	go func() { readerDone <- p.controlReader(ctx, prefix, expected, byID, seen, &mu) }()
	gate := make(chan struct{})
	results := make(chan error, len(targets))
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t ownedTarget) {
			defer wg.Done()
			select {
			case <-gate:
			case <-ctx.Done():
				results <- ctx.Err()
				return
			}
			claimID := byID[t.ID]
			body, _ := json.Marshal(expected[claimID])
			_, err := p.create(ctx, t, string(body))
			results <- err
		}(t)
	}
	release := time.Now()
	close(gate)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && row.Failure == "" {
			row.Failure = err.Error()
			cancel()
		}
	}
	if err := <-readerDone; err != nil && row.Failure == "" {
		row.Failure = err.Error()
	}
	mu.Lock()
	row.Delivered = len(seen)
	row.P50MS, row.P95MS, row.MaxMS, row.WallMS = summaries(start, seen)
	row.ReadyGateMS = float64(release.Sub(start).Microseconds()) / 1000
	row.PostGateP50MS, row.PostGateP95MS, row.PostGateMaxMS, _ = summaries(release, seen)
	mu.Unlock()
	files, err := p.list(parent, false, prefix)
	if err != nil && row.Failure == "" {
		row.Failure = err.Error()
	} else if err == nil {
		if len(files) != len(expected) {
			row.Failure = "control final list missing exact files"
		}
		for _, f := range files {
			claimID, ok := byID[f.ID]
			var got claim
			if !ok || f.Title != prefix+claimID || json.Unmarshal([]byte(f.Description), &got) != nil || got != expected[claimID] || !p.verifyClaim(got) {
				row.Failure = "control final list identity mismatch"
				break
			}
		}
	}
	diff := metricDiff(before, p.metricCopy())
	row.Calls, row.Statuses = diff.Calls, diff.Statuses
	row.NominalUnits, row.UpperUnits, row.ResponseBytes, row.RequestBytes = diff.NominalUnits, diff.UpperUnits, diff.ResponseBytes, diff.RequestBytes
	if row.Failure == "" && row.Delivered != row.Writers {
		row.Failure = "control reader did not deliver all claims"
	}
	if row.Failure != "" {
		return row, errors.New(row.Failure)
	}
	return row, nil
}

func (p *probe) run(ctx context.Context, out *report) error {
	for _, count := range []int{1, 2, 4, 8} {
		gen, err := p.reserve(ctx, fmt.Sprintf("%s-%d-generation", p.stem, count))
		if err != nil {
			return err
		}
		anchorTarget, err := p.reserve(ctx, fmt.Sprintf("%s-%d-anchor", p.stem, count))
		if err != nil {
			return err
		}
		known := make(map[string]claim, count+1)
		controlTargets := make([]ownedTarget, 0, count)
		byID := make(map[string]string, count)
		prefix := fmt.Sprintf("%s-%d-control-", p.stem, count)
		for i := 0; i < count; i++ {
			c, e := p.newClaim()
			if e != nil {
				return e
			}
			known[c.ID] = c
			t, e := p.reserve(ctx, prefix+c.ID)
			if e != nil {
				return e
			}
			controlTargets = append(controlTargets, t)
			byID[t.ID] = c.ID
		}
		if _, err = p.create(ctx, gen, mustSnapshot(p, snapshot{Session: p.sessionID()})); err != nil {
			return err
		}
		if err = p.capability(ctx, gen); err != nil {
			return err
		}
		out.CapabilityChecks++
		if _, err = p.create(ctx, anchorTarget, p.anchorDescription(gen)); err != nil {
			return err
		}
		// A same-binary primitive alternates order; each arm has fresh IDs.
		fixedFirst := count == 1 || count == 4
		oneFixed := func() error {
			row, e := p.runFixed(ctx, anchorTarget, gen, known)
			out.Arms = append(out.Arms, row)
			if e == nil && count == 1 {
				e = p.lostResponse(ctx, gen, known)
				out.LostResponse = e == nil
			}
			return e
		}
		oneControl := func() error {
			row, e := p.runControl(ctx, prefix, controlTargets, known, byID)
			out.Arms = append(out.Arms, row)
			return e
		}
		if fixedFirst {
			if err := oneFixed(); err != nil {
				return err
			}
			if err := oneControl(); err != nil {
				return err
			}
		} else {
			if err := oneControl(); err != nil {
				return err
			}
			if err := oneFixed(); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	clientPath := flag.String("client-config", "", "isolated personal-test client.json")
	exitPath := flag.String("exit-config", "", "isolated personal-test exit.json")
	outPath := flag.String("out", "", "fresh JSON result directly under .skirk-runs")
	route := flag.String("route", "front", "front or explicit direct")
	live := flag.Bool("live", false, "permit bounded live Google calls")
	flag.Parse()
	if !*live || (*route != "front" && *route != "direct") {
		fmt.Fprintln(os.Stderr, "bounded live mode and a valid route are required")
		os.Exit(2)
	}
	clientAbs, e1 := filepath.Abs(*clientPath)
	exitAbs, e2 := filepath.Abs(*exitPath)
	if e1 == nil {
		clientAbs, e1 = filepath.EvalSymlinks(clientAbs)
	}
	if e2 == nil {
		exitAbs, e2 = filepath.EvalSymlinks(exitAbs)
	}
	if e1 != nil || e2 != nil || !strings.HasSuffix(clientAbs, filepath.Join(".skirk-runs", "personal-test", "kit", "client.json")) || !strings.HasSuffix(exitAbs, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		fmt.Fprintln(os.Stderr, "isolated personal-test kit configs required")
		os.Exit(2)
	}
	root, _ := filepath.Abs(".skirk-runs")
	_ = os.MkdirAll(root, 0700)
	out, _ := filepath.Abs(*outPath)
	if filepath.Dir(out) != root || filepath.Ext(out) != ".json" {
		fmt.Fprintln(os.Stderr, "fresh report directly under .skirk-runs required")
		os.Exit(2)
	}
	for _, path := range []string{out, out + ".recovery.json", out + ".recovery.json.tmp"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "report or recovery ledger already exists")
			os.Exit(2)
		}
	}
	lock, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot reserve fresh report path")
		os.Exit(2)
	}
	if err := lock.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot close reserved report")
		os.Exit(1)
	}
	clientCfg, e1 := skirk.LoadConfig(clientAbs)
	exitCfg, e2 := skirk.LoadConfig(exitAbs)
	if e1 != nil || e2 != nil || clientCfg.Drive.FolderID == "" || clientCfg.Drive.FolderID == "appDataFolder" || clientCfg.Drive.FolderID != exitCfg.Drive.FolderID || clientCfg.Drive.Space != "" && clientCfg.Drive.Space != "drive" || exitCfg.Drive.Space != "" && exitCfg.Drive.Space != "drive" || clientCfg.Drive.QuotaProject != exitCfg.Drive.QuotaProject || clientCfg.Auth.ClientID == "" || clientCfg.Auth.ClientID != exitCfg.Auth.ClientID || clientCfg.Auth.RefreshToken == "" || clientCfg.Auth.RefreshToken != exitCfg.Auth.RefreshToken {
		fmt.Fprintln(os.Stderr, "same-grant visible-folder test kit required")
		os.Exit(2)
	}
	if *route == "front" {
		if clientCfg.Route.GoogleIP == "" || exitCfg.Route.GoogleIP == "" {
			fmt.Fprintln(os.Stderr, "pinned Google IP required")
			os.Exit(2)
		}
		clientCfg.Route.Mode, exitCfg.Route.Mode = "google_front_pinned", "google_front_pinned"
	} else {
		clientCfg.Route.Mode, exitCfg.Route.Mode = "direct", "direct"
		clientCfg.Route.GoogleIP, exitCfg.Route.GoogleIP = "", ""
	}
	p, err := newProbe(clientCfg.Drive.FolderID, out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "random setup failed")
		os.Exit(1)
	}
	sources := [2]*skirk.AccessTokenSource{skirk.NewAccessTokenSource(clientCfg.Auth, clientCfg.Route), skirk.NewAccessTokenSource(exitCfg.Auth, exitCfg.Route)}
	clients := [2]*skirk.GoogleHTTPClient{skirk.NewGoogleHTTPClient(clientCfg.Route), skirk.NewGoogleHTTPClient(exitCfg.Route)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	for _, source := range sources {
		if _, err := source.Token(ctx); err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "test-grant preload failed")
			os.Exit(1)
		}
	}
	p.call = func(ctx context.Context, who actor, method, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
		token, err := sources[who].Token(ctx)
		if err != nil {
			return nil, err
		}
		headers["Authorization"] = "Bearer " + token
		if project := clientCfg.Drive.QuotaProject; project != "" {
			headers["X-Goog-User-Project"] = project
		}
		return clients[who].RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	}
	r := report{Schema: 1, Route: *route, IdentityScope: "same OAuth grant; client and exit process profiles"}
	err = p.run(ctx, &r)
	cancel()
	if err != nil {
		r.Failure = err.Error()
	}
	r.CleanupExact404, r.CleanupImmediate, r.CleanupDelayed = p.cleanupOwned(31 * time.Second)
	m := p.metricCopy()
	r.Calls, r.Statuses, r.NominalUnits, r.UpperUnits, r.ResponseBytes, r.RequestBytes = m.Calls, m.Statuses, m.NominalUnits, m.UpperUnits, m.ResponseBytes, m.RequestBytes
	r.Completed = r.Failure == "" && len(r.Arms) == 8 && r.CapabilityChecks == 4 && r.LostResponse && r.CleanupExact404 && r.CleanupImmediate && r.CleanupDelayed
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(out, append(data, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "report write failed")
		os.Exit(1)
	}
	fmt.Printf("completed=%t arms=%d nominal_units=%d upper_units=%d cleanup=%t\n", r.Completed, len(r.Arms), r.NominalUnits, r.UpperUnits, r.CleanupExact404 && r.CleanupImmediate && r.CleanupDelayed)
	if !r.Completed {
		os.Exit(1)
	}
}
