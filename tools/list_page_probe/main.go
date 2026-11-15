// Bounded live comparison of Drive files.list page sizes. This is not a transport.
package main

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	taggedP0       = 8
	untaggedP0     = 8
	taggedP1       = 504
	untaggedP1     = 8
	objectCount    = taggedP0 + untaggedP0 + taggedP1 + untaggedP1
	activeUnitCap  = 38000
	allUnitCap     = 80000
	maxListPages   = 16
	runtimePageCap = 4
	visibilityWait = 90 * time.Second
)

type options struct {
	config string
	out    string
	live   bool
}

type object struct {
	ID     string
	Name   string
	Class  string
	Tagged bool
}

type pageResult struct {
	DurationMS         float64 `json:"duration_ms"`
	DecodedBytes       int     `json:"decoded_bytes"`
	ContentLengthBytes int64   `json:"content_length_bytes,omitempty"`
	Returned           int     `json:"returned"`
	LocallyMatched     int     `json:"locally_matched"`
	Incomplete         bool    `json:"incomplete_search"`
}

type sweepResult struct {
	Round              int          `json:"round"`
	PageSize           int          `json:"page_size"`
	Variant            string       `json:"variant"`
	DurationMS         float64      `json:"duration_ms"`
	Pages              []pageResult `json:"pages"`
	Returned           int          `json:"returned"`
	LocallyMatched     int          `json:"locally_matched"`
	Expected           int          `json:"expected"`
	Missing            int          `json:"missing"`
	MatchedWithin4     int          `json:"matched_within_runtime_page_cap"`
	MissingWithin4     int          `json:"missing_within_runtime_page_cap"`
	Unexpected         int          `json:"unexpected"`
	Duplicate          int          `json:"duplicate"`
	DecodedBytes       int          `json:"decoded_bytes"`
	ContentLengthBytes int64        `json:"content_length_bytes"`
	ListUnits          int          `json:"list_units"`
	IncompleteSearch   bool         `json:"incomplete_search"`
	Failure            string       `json:"failure,omitempty"`
}

type report struct {
	SchemaVersion       int           `json:"schema_version"`
	Route               string        `json:"route"`
	DescriptionIncluded bool          `json:"description_included"`
	RuntimePageCap      int           `json:"runtime_page_cap"`
	ObjectsScheduled    int           `json:"objects_scheduled"`
	ObjectsCreated      int           `json:"objects_created"`
	VisibilityChecks    int           `json:"visibility_checks"`
	VisibilityMS        float64       `json:"visibility_ms,omitempty"`
	Sweeps              []sweepResult `json:"sweeps"`
	UntaggedP0          int           `json:"untagged_p0"`
	UntaggedP1          int           `json:"untagged_p1"`
	CleanupDeletes      int           `json:"cleanup_deletes"`
	CleanupExact404     int           `json:"cleanup_exact_404"`
	CleanupPrefixChecks int           `json:"cleanup_prefix_checks"`
	CleanupVerified     bool          `json:"cleanup_verified"`
	DriveUnitsEstimated int           `json:"drive_units_estimated"`
	Failure             string        `json:"failure,omitempty"`
	OK                  bool          `json:"ok"`
}

type driveFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type filesPayload struct {
	NextPageToken    string      `json:"nextPageToken"`
	IncompleteSearch bool        `json:"incompleteSearch"`
	Files            []driveFile `json:"files"`
}

type probe struct {
	cfg       *skirk.Config
	tokens    *skirk.AccessTokenSource
	http      *skirk.GoogleHTTPClient
	out       string
	recovery  string
	prefix    string
	objects   []object
	attempted []object
	units     int
	result    report
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("list-page-probe", flag.ContinueOnError)
	fs.StringVar(&o.config, "config", "", "isolated personal-test kit exit.json")
	fs.StringVar(&o.out, "out", "", "fresh private report under this worktree's .skirk-runs")
	fs.BoolVar(&o.live, "live", false, "allow bounded Drive calls")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if !o.live || o.config == "" || o.out == "" || len(fs.Args()) != 0 {
		return o, errors.New("requires --live, --config, and --out")
	}
	configPath, err := filepath.EvalSymlinks(o.config)
	if err != nil {
		return o, err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil || !(strings.HasSuffix(configPath, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) ||
		strings.HasSuffix(configPath, filepath.Join(".skirk-runs", "personal-test", "quota-kit", "exit.json"))) {
		return o, errors.New("config must be an isolated personal-test kit exit.json")
	}
	privateRoot, err := filepath.Abs(".skirk-runs")
	if err != nil {
		return o, err
	}
	if err := os.MkdirAll(privateRoot, 0700); err != nil {
		return o, err
	}
	rootInfo, err := os.Lstat(privateRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return o, errors.New("private root is not a real directory")
	}
	out, err := filepath.Abs(o.out)
	if err != nil || filepath.Ext(out) != ".json" || filepath.Dir(out) != privateRoot {
		return o, errors.New("output must be a direct child JSON file of .skirk-runs")
	}
	for _, candidate := range []string{out, out + ".recovery-ids"} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			return o, errors.New("report or recovery ledger already exists")
		}
	}
	o.config, o.out = configPath, out
	return o, nil
}

func newProbe(cfg *skirk.Config, out string) (*probe, error) {
	if cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" || cfg.Drive.FolderID == "appDataFolder" {
		return nil, errors.New("isolated test folder required")
	}
	if cfg.Route.Mode != "direct" && cfg.Route.Mode != "google_front_pinned" {
		return nil, errors.New("only the user's direct or pinned www.google.com routes are supported")
	}
	if cfg.Route.Mode == "google_front_pinned" && cfg.Route.GoogleIP == "" {
		return nil, errors.New("pinned www.google.com route requires Google IP")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &probe{
		cfg: cfg, tokens: skirk.NewAccessTokenSource(cfg.Auth, cfg.Route), http: skirk.NewGoogleHTTPClient(cfg.Route),
		out: out, recovery: out + ".recovery-ids", prefix: "muxv4/list-page-probe-" + hex.EncodeToString(nonce) + "/down/peer/run/",
		result: report{SchemaVersion: 1, Route: cfg.Route.Mode, DescriptionIncluded: true, RuntimePageCap: runtimePageCap, ObjectsScheduled: objectCount,
			UntaggedP0: untaggedP0, UntaggedP1: untaggedP1, Sweeps: []sweepResult{}},
	}, nil
}

func (p *probe) request(ctx context.Context, method, path string, body []byte, cost int, active bool) (*skirk.HTTPResult, error) {
	limit := allUnitCap
	if active {
		limit = activeUnitCap
	}
	if p.units+cost > limit {
		return nil, errors.New("quota_guard")
	}
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, errors.New("token_unavailable")
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache"}
	if body != nil {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	if project := p.cfg.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	// One physical request per call: an uncertain create is never replayed.
	p.units += cost
	res, err := p.http.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	if err != nil {
		return res, errors.New("drive_transport")
	}
	return res, nil
}

func (p *probe) reserveIDs(ctx context.Context) ([]string, error) {
	v := url.Values{"count": {strconv.Itoa(objectCount)}, "fields": {"ids"}}
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files/generateIds?"+v.Encode(), nil, 5, true)
	if err != nil || res == nil || res.Status != http.StatusOK {
		return nil, errors.New("id_reservation_failed")
	}
	var payload struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(res.Body, &payload) != nil || len(payload.IDs) != objectCount {
		return nil, errors.New("id_reservation_incomplete")
	}
	seen := map[string]bool{}
	for _, id := range payload.IDs {
		if id == "" || seen[id] {
			return nil, errors.New("id_reservation_invalid")
		}
		seen[id] = true
	}
	return payload.IDs, nil
}

func makeObjects(prefix string, ids []string) []object {
	result := make([]object, 0, len(ids))
	add := func(class string, tagged bool, count int) {
		for i := 0; i < count; i++ {
			index := len(result)
			name := fmt.Sprintf("%sepoch/%s/s%016d/l00/%016d.f1.b64", prefix, class, index, index)
			if class == "p0" {
				name += ".mo1"
			}
			result = append(result, object{ID: ids[index], Name: name, Class: class, Tagged: tagged})
		}
	}
	// Priority records are deliberately older, so a broad prefix scan must
	// traverse newer p1 pollution before it reaches p0.
	add("p0", true, taggedP0)
	add("p0", false, untaggedP0)
	add("p1", true, taggedP1)
	add("p1", false, untaggedP1)
	return result
}

func (p *probe) startLedger() error {
	f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "# %s\n", p.prefix); err != nil {
		return err
	}
	return f.Sync()
}

func (p *probe) appendLedger(item object) error {
	f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(f, "%s %s\n", item.ID, item.Name)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (p *probe) create(ctx context.Context, item object) error {
	metadata := map[string]any{"id": item.ID, "name": item.Name, "mimeType": "application/octet-stream", "parents": []string{p.cfg.Drive.FolderID}}
	if item.Class == "p0" {
		// Same shape as a metadata-only priority object; list responses include
		// this description, whereas bulk p1 objects have no description.
		sealed := make([]byte, 384)
		if _, err := rand.Read(sealed); err != nil {
			return errors.New("priority_payload_random_failed")
		}
		metadata["description"] = "skirk-sealed-v2:" + base64.StdEncoding.EncodeToString(sealed)
	}
	if item.Tagged {
		metadata["properties"] = map[string]string{"skirkClass": item.Class}
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return errors.New("create_metadata_invalid")
	}
	res, err := p.request(ctx, http.MethodPost, "/drive/v3/files?fields=id,name,size", body, 50, true)
	if err != nil || res == nil || (res.Status != http.StatusOK && res.Status != http.StatusCreated) {
		return errors.New("create_failed")
	}
	var got driveFile
	if json.Unmarshal(res.Body, &got) != nil || got.ID != item.ID || got.Name != item.Name {
		return errors.New("create_response_mismatch")
	}
	return nil
}

func escapeQuery(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}

func (p *probe) listQuery(variant string) (string, error) {
	q := "trashed = false and '" + escapeQuery(p.cfg.Drive.FolderID) + "' in parents and name contains '" + escapeQuery(p.prefix) + "'"
	switch variant {
	case "broad":
	case "legacy_p0":
		q += " and name contains '/p0/'"
	case "legacy_p1":
		q += " and name contains '/p1/'"
	case "indexed_p0":
		q += " and properties has { key='skirkClass' and value='p0' }"
	case "indexed_p1":
		q += " and properties has { key='skirkClass' and value='p1' }"
	default:
		return "", errors.New("invalid_variant")
	}
	return q, nil
}

func (p *probe) list(ctx context.Context, variant string, pageSize int, active bool) ([]driveFile, sweepResult, error) {
	q, err := p.listQuery(variant)
	if err != nil {
		return nil, sweepResult{}, err
	}
	result := sweepResult{Variant: variant, PageSize: pageSize, Pages: []pageResult{}}
	all := []driveFile{}
	expected := expectedFor(p.objects, variant)
	pageToken := ""
	start := time.Now()
	seenTokens := map[string]bool{}
	for page := 0; page < maxListPages; page++ {
		v := url.Values{}
		v.Set("q", q)
		v.Set("pageSize", strconv.Itoa(pageSize))
		v.Set("orderBy", "modifiedTime desc")
		v.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime,description)")
		if pageToken != "" {
			v.Set("pageToken", pageToken)
		}
		pageStart := time.Now()
		result.ListUnits += 100
		res, err := p.request(ctx, http.MethodGet, "/drive/v3/files?"+v.Encode(), nil, 100, active)
		if err != nil || res == nil || res.Status != http.StatusOK {
			result.Failure = "list_request_failed"
			return all, result, errors.New(result.Failure)
		}
		var payload filesPayload
		if json.Unmarshal(res.Body, &payload) != nil {
			result.Failure = "list_response_invalid"
			return all, result, errors.New(result.Failure)
		}
		contentLength, _ := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
		pageMatched := 0
		for _, file := range payload.Files {
			if expected[file.ID] && strings.HasPrefix(file.Name, p.prefix) {
				pageMatched++
			}
		}
		result.Pages = append(result.Pages, pageResult{DurationMS: millis(time.Since(pageStart)), DecodedBytes: len(res.Body),
			ContentLengthBytes: contentLength, Returned: len(payload.Files), LocallyMatched: pageMatched, Incomplete: payload.IncompleteSearch})
		if page < runtimePageCap {
			result.MatchedWithin4 += pageMatched
		}
		result.DecodedBytes += len(res.Body)
		result.ContentLengthBytes += contentLength
		result.Returned += len(payload.Files)
		result.IncompleteSearch = result.IncompleteSearch || payload.IncompleteSearch
		all = append(all, payload.Files...)
		if payload.NextPageToken == "" {
			result.DurationMS = millis(time.Since(start))
			return all, result, nil
		}
		if seenTokens[payload.NextPageToken] {
			result.Failure = "page_token_loop"
			return all, result, errors.New(result.Failure)
		}
		seenTokens[payload.NextPageToken] = true
		pageToken = payload.NextPageToken
	}
	result.Failure = "max_pages_exceeded"
	return all, result, errors.New(result.Failure)
}

func expectedFor(objects []object, variant string) map[string]bool {
	expected := map[string]bool{}
	for _, item := range objects {
		switch variant {
		case "broad":
			expected[item.ID] = true
		case "legacy_p0":
			if item.Class == "p0" {
				expected[item.ID] = true
			}
		case "legacy_p1":
			if item.Class == "p1" {
				expected[item.ID] = true
			}
		case "indexed_p0":
			if item.Class == "p0" && item.Tagged {
				expected[item.ID] = true
			}
		case "indexed_p1":
			if item.Class == "p1" && item.Tagged {
				expected[item.ID] = true
			}
		}
	}
	return expected
}

func assess(result *sweepResult, files []driveFile, expected map[string]bool, prefix string) {
	result.Expected = len(expected)
	seen := map[string]bool{}
	for _, file := range files {
		if seen[file.ID] {
			result.Duplicate++
			continue
		}
		seen[file.ID] = true
		if expected[file.ID] && strings.HasPrefix(file.Name, prefix) {
			result.LocallyMatched++
		} else if strings.HasPrefix(file.Name, prefix) {
			// A legacy interior-name predicate can return both classes. The
			// actual mux filters such false matches locally.
			if strings.HasPrefix(result.Variant, "indexed_") || result.Variant == "broad" {
				result.Unexpected++
			}
		}
	}
	result.Missing = result.Expected - result.LocallyMatched
	result.MissingWithin4 = result.Expected - result.MatchedWithin4
	if result.Duplicate > 0 || result.Missing > 0 || result.Unexpected > 0 || result.IncompleteSearch {
		result.Failure = "query_result_incomplete_or_unexpected"
	}
}

func millis(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }

func (p *probe) measure(ctx context.Context) error {
	variants := []string{"legacy_p0", "indexed_p0", "legacy_p1", "indexed_p1"}
	pageSizes := []int{100, 500, 1000, 1000, 500, 100}
	for round, pageSize := range pageSizes {
		for offset := range variants {
			variant := variants[(offset+round)%len(variants)]
			files, result, err := p.list(ctx, variant, pageSize, true)
			result.Round = round + 1
			if err == nil {
				assess(&result, files, expectedFor(p.objects, variant), p.prefix)
			}
			p.result.Sweeps = append(p.result.Sweeps, result)
			if err != nil || result.Failure != "" {
				return errors.New("measurement_failed")
			}
		}
	}
	return nil
}

func (p *probe) waitVisible(ctx context.Context) error {
	start := time.Now()
	for time.Since(start) < visibilityWait {
		files, row, err := p.list(ctx, "broad", 1000, true)
		p.result.VisibilityChecks++
		if err == nil {
			assess(&row, files, expectedFor(p.objects, "broad"), p.prefix)
			if row.Failure == "" {
				p.result.VisibilityMS = millis(time.Since(start))
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("visibility_context_done")
		case <-time.After(3 * time.Second):
		}
	}
	return errors.New("visibility_timeout")
}

func (p *probe) cleanup(ctx context.Context) {
	// A prefix sweep also catches a create whose response was lost after
	// Drive committed it. Only this random, run-owned namespace is eligible.
	known := map[string]object{}
	for _, item := range p.attempted {
		known[item.ID] = item
	}
	for pass := 0; pass < 3; pass++ {
		files, _, err := p.list(ctx, "broad", 1000, false)
		if err == nil {
			p.result.CleanupPrefixChecks++
			for _, file := range files {
				if !strings.HasPrefix(file.Name, p.prefix) || file.ID == "" {
					continue
				}
				if _, exists := known[file.ID]; !exists {
					item := object{ID: file.ID, Name: file.Name}
					if p.appendLedger(item) == nil {
						known[file.ID] = item
					}
				}
			}
		}
		ids := make([]string, 0, len(known))
		for id := range known {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		remaining := 0
		for _, id := range ids {
			res, err := p.request(ctx, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(id), nil, 50, false)
			p.result.CleanupDeletes++
			if err != nil || res == nil || (res.Status != http.StatusNoContent && res.Status != http.StatusOK && res.Status != http.StatusNotFound) {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		time.Sleep(time.Duration(pass+1) * 2 * time.Second)
	}
	allMissing := true
	for id := range known {
		res, err := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id", nil, 5, false)
		if err == nil && res != nil && res.Status == http.StatusNotFound {
			p.result.CleanupExact404++
		} else {
			allMissing = false
		}
	}
	for check := 0; check < 2; check++ {
		files, _, err := p.list(ctx, "broad", 1000, false)
		if err != nil {
			allMissing = false
			break
		}
		p.result.CleanupPrefixChecks++
		for _, file := range files {
			if strings.HasPrefix(file.Name, p.prefix) {
				allMissing = false
			}
		}
		if check == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	p.result.CleanupVerified = allMissing
	if allMissing {
		if err := os.Remove(p.recovery); err != nil {
			p.result.CleanupVerified = false
		}
	}
	if !p.result.CleanupVerified && p.result.Failure == "" {
		p.result.Failure = "cleanup_unverified"
	}
}

func (p *probe) run() (final report) {
	if err := p.startLedger(); err != nil {
		p.result.Failure = "recovery_ledger_create_failed"
		return p.result
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		p.cleanup(ctx)
		p.result.DriveUnitsEstimated = p.units
		p.result.OK = p.result.Failure == "" && p.result.CleanupVerified && len(p.result.Sweeps) == 24
		final = p.result
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	ids, err := p.reserveIDs(ctx)
	if err != nil {
		p.result.Failure = err.Error()
		return p.result
	}
	p.objects = makeObjects(p.prefix, ids)
	for _, item := range p.objects {
		if err := p.appendLedger(item); err != nil {
			p.result.Failure = "recovery_ledger_append_failed"
			return p.result
		}
		p.attempted = append(p.attempted, item)
		if err := p.create(ctx, item); err != nil {
			p.result.Failure = err.Error()
			return p.result
		}
		p.result.ObjectsCreated++
	}
	if err := p.waitVisible(ctx); err != nil {
		p.result.Failure = err.Error()
		return p.result
	}
	if err := p.measure(ctx); err != nil {
		p.result.Failure = err.Error()
	}
	return p.result
}

func writeReport(path string, value report) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(value)
}

func main() {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "list-page-probe: invalid bounded live arguments")
		os.Exit(2)
	}
	cfg, err := skirk.LoadConfig(o.config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list-page-probe: test config unavailable")
		os.Exit(2)
	}
	p, err := newProbe(cfg, o.out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list-page-probe: isolated route and folder required")
		os.Exit(2)
	}
	result := p.run()
	if err := writeReport(o.out, result); err != nil {
		fmt.Fprintln(os.Stderr, "list-page-probe: report write failed")
		os.Exit(2)
	}
	fmt.Printf("{\"ok\":%t,\"objects_created\":%d,\"sweeps\":%d,\"units\":%d,\"cleanup_verified\":%t,\"failure\":%q}\n",
		result.OK, result.ObjectsCreated, len(result.Sweeps), result.DriveUnitsEstimated, result.CleanupVerified, result.Failure)
	if !result.OK {
		os.Exit(1)
	}
}
