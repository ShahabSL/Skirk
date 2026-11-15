// id_train_probe compares immutable pre-announced IDs with prefix discovery.
// It is a bounded API substrate probe, not a Skirk TCP transport.
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	maxFrames   = 12
	maxPayload  = 2048
	maxUnits    = 18000
	activeUnits = 12000
	marker      = "skirk-idtrain-v1:"
)

type packet struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Parents     []string `json:"parents"`
	Spaces      []string `json:"spaces"`
}
type sample struct {
	Sequence       int     `json:"sequence"`
	CreateMS       float64 `json:"create_ms"`
	ScheduledMS    float64 `json:"scheduled_ms"`
	AfterPublishMS float64 `json:"after_publish_ms"`
}
type arm struct {
	Mode           string   `json:"mode"`
	Verified       int      `json:"verified"`
	Samples        []sample `json:"samples"`
	ScheduledP50MS float64  `json:"scheduled_p50_ms"`
	ScheduledP95MS float64  `json:"scheduled_p95_ms"`
	Units          int64    `json:"estimated_units"`
	Expected404    int      `json:"expected_precreate_404"`
}
type report struct {
	Schema            int            `json:"schema_version"`
	Route             string         `json:"route"`
	Frames            int            `json:"frames_per_arm"`
	Payload           int            `json:"payload_bytes"`
	PollMS            int            `json:"poll_ms"`
	IntervalMS        int            `json:"interval_ms"`
	ManifestCost      string         `json:"manifest_assumption"`
	WriterConcurrency int            `json:"writer_concurrency"`
	IDTrainCatchup    bool           `json:"id_train_catchup"`
	Arms              []arm          `json:"arms"`
	Calls             map[string]int `json:"calls"`
	Units             int64          `json:"estimated_units"`
	UnexpectedErrors  int            `json:"unexpected_errors"`
	ResponseBytes     int64          `json:"response_bytes"`
	RequestBodyBytes  int64          `json:"request_body_bytes"`
	ListFalseMatches  int            `json:"list_false_matches"`
	Deletes           int            `json:"cleanup_deletes"`
	ImmediateZero     bool           `json:"immediate_prefix_zero"`
	DelayedZero       bool           `json:"delayed_prefix_zero"`
	Exact404          bool           `json:"exact_ids_missing_twice"`
	Failure           string         `json:"failure,omitempty"`
	OK                bool           `json:"ok"`
}
type caller func(context.Context, string, string, map[string]string, []byte) (*skirk.HTTPResult, error)
type probe struct {
	call              caller
	folder            string
	prefix            string
	out               string
	key               []byte
	sid               [16]byte
	mu                sync.Mutex
	units             int64
	calls             map[string]int
	unexpected        int
	expected404       int
	responseBytes     int64
	requestBodyBytes  int64
	listFalseMatches  int
	ids               []string
	names             map[string]string
	result            report
	writerConcurrency int
	idTrainCatchup    bool
}

func unitsFor(method, path string) int64 {
	if method == http.MethodGet {
		if strings.Contains(path, "generateIds") {
			return 5
		}
		if strings.HasPrefix(path, "/drive/v3/files?") {
			return 100
		}
		return 5
	}
	return 50
}
func (p *probe) request(ctx context.Context, method, path string, body []byte, expected404 bool) (*skirk.HTTPResult, error) {
	cost := unitsFor(method, path)
	p.mu.Lock()
	if p.units+cost > maxUnits {
		p.mu.Unlock()
		return nil, errors.New("quota guard")
	}
	p.units += cost
	p.requestBodyBytes += int64(len(body))
	label := method + " metadata"
	if strings.Contains(path, "generateIds") {
		label = method + " generateIds"
	} else if strings.HasPrefix(path, "/drive/v3/files?") && method == http.MethodGet {
		label = method + " list"
	}
	p.calls[label]++
	p.mu.Unlock()
	r, err := p.call(ctx, method, path, map[string]string{"Content-Type": "application/json; charset=UTF-8", "Cache-Control": "no-cache"}, body)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil || r == nil {
		p.unexpected++
		return r, errors.New("Drive request failed")
	}
	p.responseBytes += int64(len(r.Body))
	if r.Status == http.StatusNotFound && expected404 {
		p.expected404++
		return r, nil
	}
	if r.Status < 200 || r.Status >= 300 {
		p.unexpected++
		return r, fmt.Errorf("Drive status %d", r.Status)
	}
	return r, nil
}
func (p *probe) snapshot() (int64, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.units, p.expected404
}
func (p *probe) guard() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.units > activeUnits || p.unexpected > 0 {
		return errors.New("active quota/error guard")
	}
	return nil
}
func (p *probe) reserve(ctx context.Context, n int) ([]string, error) {
	r, err := p.request(ctx, http.MethodGet, fmt.Sprintf("/drive/v3/files/generateIds?count=%d&fields=ids", n), nil, false)
	if err != nil {
		return nil, err
	}
	var v struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(r.Body, &v) != nil || len(v.IDs) != n {
		return nil, errors.New("bad generated IDs")
	}
	seen := map[string]bool{}
	for _, id := range v.IDs {
		if id == "" || seen[id] || strings.ContainsAny(id, " /\r\n") {
			return nil, errors.New("invalid or duplicate generated ID")
		}
		seen[id] = true
	}
	p.ids = append(p.ids, v.IDs...)
	ledger, _ := json.Marshal(p.ids)
	if err := os.WriteFile(p.out+".recovery-ids", ledger, 0600); err != nil {
		return nil, err
	}
	return v.IDs, nil
}
func (p *probe) sealed(seq int, payload []byte) (string, error) {
	b, e := skirk.Seal(p.key, p.sid, skirk.DirectionUp, uint64(seq), payload, false)
	return marker + base64.StdEncoding.EncodeToString(b), e
}
func (p *probe) verify(file packet, id, name string, seq int, want []byte) error {
	if file.ID != id || file.Name != name || len(file.Parents) != 1 || file.Parents[0] != p.folder || len(file.Spaces) != 1 || file.Spaces[0] != "drive" {
		return errors.New("identity or mailbox mismatch")
	}
	if !strings.HasPrefix(file.Description, marker) || len(file.Description) > 4096 {
		return errors.New("invalid encrypted record")
	}
	b, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(file.Description, marker))
	if e != nil {
		return errors.New("invalid record encoding")
	}
	env, got, e := skirk.OpenEnvelope(p.key, b)
	if e != nil || env.SessionID != p.sid || env.Direction != skirk.DirectionUp || env.Sequence != uint64(seq) || !bytes.Equal(got, want) {
		return errors.New("record authentication/sequence/payload mismatch")
	}
	return nil
}
func (p *probe) get(ctx context.Context, id string) (*packet, error) {
	r, e := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id,name,description,parents,spaces", nil, true)
	if e != nil {
		return nil, e
	}
	if r.Status == http.StatusNotFound {
		return nil, nil
	}
	var f packet
	if json.Unmarshal(r.Body, &f) != nil {
		return nil, errors.New("bad metadata JSON")
	}
	return &f, nil
}
func (p *probe) list(ctx context.Context, prefix string) ([]packet, error) {
	q := "trashed = false and '" + escape(p.folder) + "' in parents and name contains '" + escape(prefix) + "'"
	v := url.Values{"q": {q}, "fields": {"nextPageToken,incompleteSearch,files(id,name,description,parents,spaces)"}, "pageSize": {"100"}}
	r, e := p.request(ctx, http.MethodGet, "/drive/v3/files?"+v.Encode(), nil, false)
	if e != nil {
		return nil, e
	}
	var f struct {
		Files      []packet `json:"files"`
		Next       string   `json:"nextPageToken"`
		Incomplete bool     `json:"incompleteSearch"`
	}
	if json.Unmarshal(r.Body, &f) != nil || f.Next != "" || f.Incomplete {
		return nil, errors.New("incomplete or invalid prefix list")
	}
	matching := make([]packet, 0, len(f.Files))
	for _, x := range f.Files {
		if !strings.HasPrefix(x.Name, prefix) {
			p.mu.Lock()
			p.listFalseMatches++
			p.mu.Unlock()
			continue
		}
		matching = append(matching, x)
	}
	return matching, nil
}
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func percentile(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[(len(s)*p+99)/100-1]
}
func (p *probe) runArm(ctx context.Context, mode string, index, count, size, pollMS, intervalMS int) (arm, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := arm{Mode: mode}
	before, before404 := p.snapshot()
	ids, e := p.reserve(ctx, count)
	if e != nil {
		return a, e
	}
	prefix := fmt.Sprintf("%sa%d/", p.prefix, index)
	payloads := make([][]byte, count)
	names := make([]string, count)
	descriptions := make([]string, count)
	for i := range ids {
		names[i] = fmt.Sprintf("%s%04d", prefix, i)
		p.names[ids[i]] = names[i]
		payloads[i] = make([]byte, size)
		if _, e = rand.Read(payloads[i]); e != nil {
			return a, e
		}
		descriptions[i], e = p.sealed(index*maxFrames+i, payloads[i])
		if e != nil {
			return a, e
		}
	}
	base := time.Now().Add(200 * time.Millisecond)
	published := make([]time.Time, count)
	created := make([]time.Duration, count)
	var timingMu sync.Mutex
	writeErr := make(chan error, 1)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		var pending sync.WaitGroup
		window := p.writerConcurrency
		if window <= 0 {
			window = 1
		}
		slots := make(chan struct{}, window)
		errors := make(chan error, count+1)
	publishLoop:
		for i, id := range ids {
			if e := wait(ctx, time.Until(base.Add(time.Duration(i*intervalMS)*time.Millisecond))); e != nil {
				errors <- e
				break
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errors <- ctx.Err()
				break publishLoop
			}
			pending.Add(1)
			go func(i int, id string) {
				defer pending.Done()
				defer func() { <-slots }()
				body, _ := json.Marshal(map[string]any{"id": id, "name": names[i], "description": descriptions[i], "parents": []string{p.folder}, "mimeType": "application/octet-stream"})
				start := time.Now()
				_, e := p.request(ctx, http.MethodPost, "/drive/v3/files?fields=id", body, false)
				timingMu.Lock()
				published[i] = time.Now()
				created[i] = time.Since(start)
				timingMu.Unlock()
				if e != nil {
					errors <- e
				}
			}(i, id)
		}
		pending.Wait()
		close(errors)
		var first error
		for e := range errors {
			if first == nil {
				first = e
			}
		}
		writeErr <- first
	}()
	// No cleanup can start while a canceled publish is still in flight.
	defer func() { cancel(); <-writeDone }()
	seen := make(map[string]packet)
	delivered := 0
	arrivals := make([]time.Time, count)
	for delivered < count {
		beforeDelivery := delivered
		if e := p.guard(); e != nil {
			return a, e
		}
		if mode == "id-train" {
			f, e := p.get(ctx, ids[delivered])
			if e != nil {
				return a, e
			}
			if f != nil {
				seen[f.ID] = *f
			}
		} else {
			fs, e := p.list(ctx, prefix)
			if e != nil {
				return a, e
			}
			for _, f := range fs {
				seen[f.ID] = f
			}
		}
		for delivered < count {
			f, ok := seen[ids[delivered]]
			if !ok {
				break
			}
			if e := p.verify(f, ids[delivered], names[delivered], index*maxFrames+delivered, payloads[delivered]); e != nil {
				return a, e
			}
			arrivals[delivered] = time.Now()
			delivered++
		}
		if delivered < count && (mode != "id-train" || !p.idTrainCatchup || delivered == beforeDelivery) {
			if e := wait(ctx, time.Duration(pollMS)*time.Millisecond); e != nil {
				return a, e
			}
		}
	}
	if e := <-writeErr; e != nil {
		return a, e
	}
	var scheduled []float64
	for i := range ids {
		delay := arrivals[i].Sub(base.Add(time.Duration(i*intervalMS) * time.Millisecond))
		after := arrivals[i].Sub(published[i])
		a.Samples = append(a.Samples, sample{i, float64(created[i].Microseconds()) / 1000, float64(delay.Microseconds()) / 1000, float64(after.Microseconds()) / 1000})
		scheduled = append(scheduled, float64(delay.Microseconds())/1000)
	}
	a.Verified = delivered
	a.ScheduledP50MS = percentile(scheduled, 50)
	a.ScheduledP95MS = percentile(scheduled, 95)
	after, after404 := p.snapshot()
	a.Units = after - before
	a.Expected404 = after404 - before404
	return a, nil
}
func (p *probe) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ok := true
	for _, id := range p.ids {
		if r, e := p.request(ctx, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(id), nil, true); e != nil {
			ok = false
		} else if r.Status != http.StatusNotFound {
			p.result.Deletes++
		}
	}
	for n := 0; n < 2; n++ {
		for _, id := range p.ids {
			r, e := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id", nil, true)
			if e != nil || r.Status != 404 {
				ok = false
			}
		}
		if n == 0 {
			_ = wait(ctx, time.Second)
		}
	}
	p.result.Exact404 = ok
	f, e := p.list(ctx, p.prefix)
	p.result.ImmediateZero = e == nil && len(f) == 0
	if e = wait(ctx, 31*time.Second); e == nil {
		f, e = p.list(ctx, p.prefix)
		p.result.DelayedZero = e == nil && len(f) == 0
	}
	if ok && p.result.ImmediateZero && p.result.DelayedZero {
		_ = os.Remove(p.out + ".recovery-ids")
	}
}
func main() {
	fs := flag.NewFlagSet("id-train-probe", flag.ExitOnError)
	config := fs.String("config", "", "isolated personal-test kit exit.json")
	out := fs.String("out", "", "new JSON file directly under .skirk-runs")
	live := fs.Bool("live", false, "permit bounded Drive calls")
	count := fs.Int("count", 8, "frames per arm (1–12)")
	size := fs.Int("payload", 256, "payload bytes (1–2048)")
	poll := fs.Int("poll-ms", 200, "read interval (100–1000ms)")
	interval := fs.Int("interval-ms", 250, "frame schedule (100–2000ms)")
	window := fs.Int("writer-concurrency", 2, "maximum outstanding creates (1–4)")
	catchup := fs.Bool("id-train-catchup", true, "read next known ID immediately after progress")
	_ = fs.Parse(os.Args[1:])
	if !*live || *count < 1 || *count > maxFrames || *size < 1 || *size > maxPayload || *poll < 100 || *poll > 1000 || *interval < 100 || *interval > 2000 || *window < 1 || *window > 4 {
		fmt.Fprintln(os.Stderr, "invalid bounded live options")
		os.Exit(2)
	}
	abs, e := filepath.EvalSymlinks(*config)
	if e != nil || !strings.HasSuffix(abs, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		fmt.Fprintln(os.Stderr, "isolated personal-test exit.json required")
		os.Exit(2)
	}
	root, _ := filepath.Abs(".skirk-runs")
	target, _ := filepath.Abs(*out)
	if filepath.Dir(target) != root || filepath.Ext(target) != ".json" {
		fmt.Fprintln(os.Stderr, "fresh JSON output directly under .skirk-runs required")
		os.Exit(2)
	}
	_ = os.MkdirAll(root, 0700)
	if _, e = os.Lstat(target); !os.IsNotExist(e) {
		fmt.Fprintln(os.Stderr, "output exists")
		os.Exit(2)
	}
	if _, e = os.Lstat(target + ".recovery-ids"); !os.IsNotExist(e) {
		fmt.Fprintln(os.Stderr, "recovery ledger exists")
		os.Exit(2)
	}
	cfg, e := skirk.LoadConfig(abs)
	if e != nil || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" || cfg.Route.GoogleIP == "" {
		fmt.Fprintln(os.Stderr, "visible isolated mailbox and pinned Google IP required")
		os.Exit(2)
	}
	cfg.Route.Mode = "google_front_pinned"
	tokens := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	writer, reader := skirk.NewGoogleHTTPClient(cfg.Route), skirk.NewGoogleHTTPClient(cfg.Route)
	p := &probe{folder: cfg.Drive.FolderID, out: target, calls: map[string]int{}, names: map[string]string{}, key: make([]byte, 32)}
	p.writerConcurrency = *window
	p.idTrainCatchup = *catchup
	if _, e = rand.Read(p.key); e != nil {
		fmt.Fprintln(os.Stderr, "random key generation failed")
		os.Exit(1)
	}
	if _, e = rand.Read(p.sid[:]); e != nil {
		fmt.Fprintln(os.Stderr, "random session generation failed")
		os.Exit(1)
	}
	p.prefix = "idtrain/" + hex.EncodeToString(p.sid[:]) + "/"
	p.call = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
		token, e := tokens.Token(ctx)
		if e != nil {
			return nil, e
		}
		headers["Authorization"] = "Bearer " + token
		client := reader
		if method != http.MethodGet {
			client = writer
		}
		return client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	}
	p.result = report{Schema: 1, Route: "google_front_pinned", Frames: *count, Payload: *size, PollMS: *poll, IntervalMS: *interval, ManifestCost: "One generated-ID vector per arm is pre-shared out of band; generation call included, authenticated manifest transmission NOT included. No end-to-end TCP claim."}
	p.result.WriterConcurrency = *window
	p.result.IDTrainCatchup = *catchup
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	for i, mode := range []string{"id-train", "list", "list", "id-train"} {
		a, e := p.runArm(ctx, mode, i, *count, *size, *poll, *interval)
		p.result.Arms = append(p.result.Arms, a)
		if e != nil {
			p.result.Failure = e.Error()
			break
		}
	}
	cancel()
	p.cleanup()
	p.result.Units, _ = p.snapshot()
	p.result.Calls = p.calls
	p.result.UnexpectedErrors = p.unexpected
	p.result.ResponseBytes = p.responseBytes
	p.result.RequestBodyBytes = p.requestBodyBytes
	p.result.ListFalseMatches = p.listFalseMatches
	p.result.OK = p.result.Failure == "" && p.result.UnexpectedErrors == 0 && p.result.Exact404 && p.result.ImmediateZero && p.result.DelayedZero
	b, _ := json.MarshalIndent(p.result, "", "  ")
	if e = os.WriteFile(target, append(b, '\n'), 0600); e != nil {
		fmt.Fprintln(os.Stderr, "report write failed")
		os.Exit(1)
	}
	fmt.Printf("ok=%t arms=%d units=%d cleanup=%t\n", p.result.OK, len(p.result.Arms), p.result.Units, p.result.Exact404 && p.result.ImmediateZero && p.result.DelayedZero)
	if !p.result.OK {
		os.Exit(1)
	}
}
