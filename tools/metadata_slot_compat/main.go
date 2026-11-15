// Bounded compatibility probe for opt-in Drive metadata slots. No mux traffic.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	maxUnits       = 4000
	activeMaxUnits = 2500
	slotBytes      = 8192
	sweepDelay     = 30 * time.Second
)

type options struct {
	config string
	out    string
	live   bool
}

type report struct {
	SchemaVersion          int    `json:"schema_version"`
	Route                  string `json:"route"`
	GeneratedIDCreate      bool   `json:"generated_id_create"`
	CreatedVersion         uint64 `json:"created_version,omitempty"`
	ReadbackVersion        uint64 `json:"readback_version,omitempty"`
	PatchedVersion         uint64 `json:"patched_version,omitempty"`
	DescriptionBytes       int    `json:"description_bytes"`
	DescriptionSHA256      string `json:"description_sha256,omitempty"`
	ReadbackExact          bool   `json:"readback_exact"`
	ExactID404Checks       int    `json:"exact_id_404_checks"`
	ImmediatePrefixObjects int    `json:"immediate_prefix_objects"`
	DelayedPrefixObjects   int    `json:"delayed_prefix_objects"`
	CleanupDeletes         int    `json:"cleanup_deletes"`
	CleanupVerified        bool   `json:"cleanup_verified"`
	DriveUnitsEstimated    int64  `json:"drive_units_estimated"`
	DriveAPIErrors         int64  `json:"drive_api_errors"`
	ExpectedVisibility404s int64  `json:"expected_visibility_404s"`
	UnexpectedAPIErrors    int64  `json:"unexpected_api_errors"`
	ManualUnits            int64  `json:"manual_units"`
	Failure                string `json:"failure,omitempty"`
	OK                     bool   `json:"ok"`
}

type probe struct {
	cfg         *skirk.Config
	tokens      *skirk.AccessTokenSource
	client      *skirk.GoogleHTTPClient
	writer      *skirk.DriveStore
	reader      *skirk.DriveStore
	manualUnits int64
	expected404 int64
	id          string
	name        string
	prefix      string
	recovery    string
	result      report
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("metadata-slot-compat", flag.ContinueOnError)
	fs.StringVar(&o.config, "config", "", "isolated personal-test kit exit.json")
	fs.StringVar(&o.out, "out", "", "fresh ignored .skirk-runs JSON report")
	fs.BoolVar(&o.live, "live", false, "permit bounded live Drive calls")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if !o.live || o.config == "" || o.out == "" || len(fs.Args()) != 0 {
		return o, errors.New("requires --live, --config, and --out")
	}
	configPath, err := filepath.EvalSymlinks(o.config)
	if err != nil {
		return o, errors.New("isolated test config does not exist")
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil || !strings.HasSuffix(configPath, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		return o, errors.New("config must be the isolated personal-test kit exit.json")
	}
	o.config = configPath
	out, err := filepath.Abs(o.out)
	if err != nil || filepath.Ext(out) != ".json" {
		return o, errors.New("output must be a JSON file")
	}
	privateRoot, err := filepath.Abs(".skirk-runs")
	if err != nil {
		return o, err
	}
	if err := os.MkdirAll(privateRoot, 0700); err != nil {
		return o, err
	}
	relative, err := filepath.Rel(privateRoot, out)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.Contains(relative, string(filepath.Separator)) {
		return o, errors.New("output must be directly under .skirk-runs")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		return o, errors.New("output already exists")
	}
	if _, err := os.Lstat(out + ".recovery-id"); !os.IsNotExist(err) {
		return o, errors.New("recovery record already exists")
	}
	o.out = out
	return o, nil
}

func newProbe(cfg *skirk.Config, out string) *probe {
	cfg.Route.Mode = "google_front_pinned"
	cfg.Drive.MetadataSlotV1 = true
	tokens := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	return &probe{
		cfg: cfg, tokens: tokens, client: skirk.NewGoogleHTTPClient(cfg.Route),
		writer:   skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(cfg.Route), tokens, cfg.Drive),
		reader:   skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(cfg.Route), tokens, cfg.Drive),
		recovery: out + ".recovery-id",
		result:   report{SchemaVersion: 1, Route: "google_front_pinned", DescriptionBytes: slotBytes},
	}
}

func (p *probe) units() int64 {
	return p.writer.QuotaSnapshot().Units + p.reader.QuotaSnapshot().Units + p.manualUnits
}

func (p *probe) guard(active bool) error {
	limit := int64(maxUnits)
	if active {
		limit = activeMaxUnits
	}
	if p.units() > limit {
		return errors.New("Drive quota unit guard")
	}
	if p.writer.QuotaSnapshot().Errors+p.reader.QuotaSnapshot().Errors > p.expected404 && active {
		return errors.New("unexpected Drive API error or backoff")
	}
	return nil
}

func (p *probe) manualRequest(ctx context.Context, method, path string, units int64) (*skirk.HTTPResult, error) {
	if p.units()+units > maxUnits {
		return nil, errors.New("Drive quota unit guard")
	}
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache"}
	if project := p.cfg.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	result, err := p.client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, nil)
	p.manualUnits += units
	return result, err
}

func (p *probe) exactIDMissing(ctx context.Context) bool {
	result, err := p.manualRequest(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(p.id)+"?fields=id", 5)
	return err == nil && result != nil && result.Status == http.StatusNotFound
}

func escapeQuery(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}

func (p *probe) prefixObjects(ctx context.Context) (int, error) {
	query := "trashed = false and name contains '" + escapeQuery(p.prefix) + "'"
	if folder := p.cfg.Drive.FolderID; folder != "" && p.cfg.Drive.Space != "appDataFolder" {
		query += " and '" + escapeQuery(folder) + "' in parents"
	}
	values := url.Values{}
	values.Set("q", query)
	values.Set("fields", "nextPageToken,files(id,name)")
	values.Set("pageSize", "100")
	if p.cfg.Drive.Space == "appDataFolder" {
		values.Set("spaces", "appDataFolder")
	}
	result, err := p.manualRequest(ctx, http.MethodGet, "/drive/v3/files?"+values.Encode(), 100)
	if err != nil || result == nil || result.Status != http.StatusOK {
		return 0, errors.New("prefix sweep request failed")
	}
	var payload struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil || payload.NextPageToken != "" {
		return 0, errors.New("prefix sweep incomplete")
	}
	count := 0
	for _, file := range payload.Files {
		if strings.HasPrefix(file.Name, p.prefix) {
			count++
			if file.ID != p.id {
				return count, errors.New("unexpected file under unique prefix")
			}
		}
	}
	return count, nil
}

func (p *probe) waitDescription(ctx context.Context, expected string) (skirk.MetadataSlotInfo, error) {
	for attempt := 0; attempt < 12; attempt++ {
		read, err := p.reader.GetMetadataSlot(ctx, p.id, p.name)
		if err == nil && read.Description == expected {
			return read, nil
		}
		if err != nil {
			var googleErr *skirk.GoogleAPIError
			if !errors.As(err, &googleErr) || googleErr.Status != http.StatusNotFound {
				return skirk.MetadataSlotInfo{}, err
			}
			p.expected404++
		}
		if err := p.guard(true); err != nil {
			return skirk.MetadataSlotInfo{}, err
		}
		select {
		case <-ctx.Done():
			return skirk.MetadataSlotInfo{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return skirk.MetadataSlotInfo{}, errors.New("metadata description did not become visible")
}

func (p *probe) cleanup() {
	if p.id == "" {
		p.result.CleanupVerified = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		if p.units()+800 > maxUnits {
			break
		}
		p.result.CleanupDeletes++
		if err := p.writer.DeleteID(ctx, p.id); err != nil {
			continue
		}
		if !p.exactIDMissing(ctx) {
			continue
		}
		p.result.ExactID404Checks++
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if !p.exactIDMissing(ctx) {
			continue
		}
		p.result.ExactID404Checks++
		count, err := p.prefixObjects(ctx)
		p.result.ImmediatePrefixObjects = count
		if err != nil || count != 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepDelay):
		}
		count, err = p.prefixObjects(ctx)
		p.result.DelayedPrefixObjects = count
		if err == nil && count == 0 {
			if err := os.Remove(p.recovery); err != nil && !os.IsNotExist(err) {
				p.result.Failure = "recovery_record_removal_failed"
				return
			}
			p.result.CleanupVerified = true
			return
		}
	}
}

func (p *probe) run() (result report) {
	defer func() {
		p.cleanup()
		p.result.DriveUnitsEstimated = p.units()
		p.result.ManualUnits = p.manualUnits
		p.result.DriveAPIErrors = p.writer.QuotaSnapshot().Errors + p.reader.QuotaSnapshot().Errors
		p.result.ExpectedVisibility404s = p.expected404
		p.result.UnexpectedAPIErrors = p.result.DriveAPIErrors - p.expected404
		p.result.OK = p.result.Failure == "" && p.result.GeneratedIDCreate && p.result.ReadbackExact && p.result.CleanupVerified && p.result.UnexpectedAPIErrors == 0 && p.result.DriveUnitsEstimated <= maxUnits
		result = p.result
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		p.result.Failure = "random_failure"
		return
	}
	p.prefix = "slotv1/compat/" + hex.EncodeToString(nonce) + "/"
	p.name = p.prefix + "up"
	ids, err := p.writer.GenerateObjectIDs(ctx, 1)
	if err != nil || len(ids) != 1 || p.guard(true) != nil {
		p.result.Failure = "generated_id_failed_or_quota_guard"
		return
	}
	p.id = ids[0]
	f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		p.result.Failure = "recovery_record_failed"
		return
	}
	_, writeErr := fmt.Fprintf(f, "%s\n%s\n", p.id, p.name)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		p.result.Failure = "recovery_record_failed"
		return
	}
	created, err := p.writer.CreateMetadataSlot(ctx, p.id, p.name)
	if err != nil || p.guard(true) != nil {
		p.result.Failure = "generated_id_metadata_create_failed"
		return
	}
	p.result.GeneratedIDCreate = true
	p.result.CreatedVersion = created.Version
	read, err := p.waitDescription(ctx, "")
	if err != nil || p.guard(true) != nil {
		p.result.Failure = "initial_exact_id_readback_failed"
		return
	}
	p.result.ReadbackVersion = read.Version
	bytes := make([]byte, 6144)
	if _, err := rand.Read(bytes); err != nil {
		p.result.Failure = "random_failure"
		return
	}
	description := base64.RawURLEncoding.EncodeToString(bytes)
	if len(description) != slotBytes {
		p.result.Failure = "encoded_size_mismatch"
		return
	}
	hash := sha256.Sum256([]byte(description))
	p.result.DescriptionSHA256 = hex.EncodeToString(hash[:])
	patched, err := p.writer.PatchMetadataSlot(ctx, p.id, p.name, description)
	if err != nil || p.guard(true) != nil {
		p.result.Failure = "exact_8k_patch_failed"
		return
	}
	p.result.PatchedVersion = patched.Version
	read, err = p.waitDescription(ctx, description)
	if err != nil || p.guard(true) != nil || read.Description != description || read.Version < patched.Version {
		p.result.Failure = "exact_8k_readback_failed"
		return
	}
	p.result.ReadbackExact = true
	return
}

func writeReport(path string, result report) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

func main() {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg, err := skirk.LoadConfig(o.config)
	if err != nil || cfg.Drive.FolderID == "" && cfg.Drive.Space != "appDataFolder" {
		fmt.Fprintln(os.Stderr, "isolated test config is invalid")
		os.Exit(2)
	}
	p := newProbe(cfg, o.out)
	result := p.run()
	if err := writeReport(o.out, result); err != nil {
		fmt.Fprintln(os.Stderr, "private report write failed")
		os.Exit(2)
	}
	fmt.Printf("ok=%t generated_id_create=%t exact_8k_readback=%t cleanup=%t units=%d\n", result.OK, result.GeneratedIDCreate, result.ReadbackExact, result.CleanupVerified, result.DriveUnitsEstimated)
	if !result.OK {
		os.Exit(1)
	}
}
