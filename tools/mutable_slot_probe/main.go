// Bounded live probe of a fixed Drive metadata mailbox. It is not a transport.
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
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	updates       = 16
	maxGets       = 100
	pollInterval  = 200 * time.Millisecond
	visibilityMax = 10 * time.Second
)

type options struct {
	config string
	out    string
	live   bool
}

type slotMetadata struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type row struct {
	Sequence            int     `json:"sequence"`
	PatchMS             float64 `json:"patch_ms"`
	PublishToObservedMS float64 `json:"publish_to_observed_ms"`
	PatchToObservedMS   float64 `json:"patch_to_observed_ms"`
	Gets                int     `json:"gets"`
	StaleGets           int     `json:"stale_gets"`
	PatchVersion        int64   `json:"patch_version"`
	ObservedVersion     int64   `json:"observed_version"`
	StaleVersions       []int64 `json:"stale_versions,omitempty"`
}

type report struct {
	SchemaVersion        int     `json:"schema_version"`
	Route                string  `json:"route"`
	UpdatesScheduled     int     `json:"updates_scheduled"`
	UpdatesObserved      int     `json:"updates_observed"`
	Gets                 int     `json:"gets"`
	Patches              int     `json:"patches"`
	StaleGets            int     `json:"stale_gets"`
	InitialVisibilityMS  float64 `json:"initial_visibility_ms,omitempty"`
	Rows                 []row   `json:"rows"`
	CleanupDeletes       int     `json:"cleanup_deletes"`
	CleanupGets          int     `json:"cleanup_gets"`
	CleanupVerified      bool    `json:"cleanup_verified"`
	Failure              string  `json:"failure,omitempty"`
	QuotaUnitsUpperBound int     `json:"quota_units_upper_bound"`
	OK                   bool    `json:"ok"`
}

func parseOptions(args []string) (options, error) {
	var value options
	flags := flag.NewFlagSet("mutable-slot-probe", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&value.config, "config", "", "isolated test exit config")
	flags.StringVar(&value.out, "out", "", "fresh private JSON under .skirk-runs")
	flags.BoolVar(&value.live, "live", false, "allow bounded Drive calls")
	if err := flags.Parse(args); err != nil {
		return value, err
	}
	if !value.live || value.config == "" || value.out == "" || len(flags.Args()) != 0 {
		return value, errors.New("requires --live, --config, and --out")
	}
	if err := validateOutputPath(value.out); err != nil {
		return value, err
	}
	return value, nil
}

func validateOutputPath(path string) error {
	if filepath.Ext(path) != ".json" {
		return errors.New("--out must be a fresh JSON file under .skirk-runs")
	}
	root, err := filepath.Abs(".skirk-runs")
	if err != nil {
		return errors.New("cannot resolve private output root")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return errors.New("private output directory must already exist")
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return errors.New("cannot resolve private output directory")
	}
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("--out must be inside .skirk-runs")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return errors.New("--out already exists or is inaccessible")
	}
	if _, err := os.Lstat(path + ".recovery-id"); !os.IsNotExist(err) {
		return errors.New("a private exact-ID recovery record already exists")
	}
	return nil
}

type probe struct {
	config        *skirk.Config
	tokens        *skirk.AccessTokenSource
	writer        *skirk.GoogleHTTPClient
	reader        *skirk.GoogleHTTPClient
	store         *skirk.DriveStore
	recovery      string
	recoveryOwned bool
	gets          int
	patches       int
	quotaUnits    int
}

func newProbe(config *skirk.Config, outputPath string) *probe {
	tokens := skirk.NewAccessTokenSource(config.Auth, config.Route)
	return &probe{
		config:   config,
		tokens:   tokens,
		writer:   skirk.NewGoogleHTTPClient(config.Route),
		reader:   skirk.NewGoogleHTTPClient(config.Route),
		store:    skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(config.Route), tokens, config.Drive),
		recovery: outputPath + ".recovery-id",
	}
}

func (p *probe) request(ctx context.Context, client *skirk.GoogleHTTPClient, method, path string, body []byte) (*skirk.HTTPResult, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache"}
	if body != nil {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	if project := p.config.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	return client.RequestNoRateLimitRetry(ctx, method, "www.googleapis.com", path, headers, body)
}

func attempts(result *skirk.HTTPResult) int {
	if result != nil && result.Attempts > 0 {
		return result.Attempts
	}
	return 5 // Pinned-front HTTP requests can retry up to five times.
}

func (p *probe) get(ctx context.Context, fileID string, cleanup bool) (slotMetadata, int, error) {
	if p.gets >= maxGets || (!cleanup && p.gets >= maxGets-6) {
		return slotMetadata{}, 0, errors.New("get_cap")
	}
	p.gets++
	result, err := p.request(ctx, p.reader, http.MethodGet, "/drive/v3/files/"+url.PathEscape(fileID)+"?fields=id,version,description", nil)
	p.quotaUnits += attempts(result) * 5
	if err != nil || result == nil {
		return slotMetadata{}, 0, errors.New("metadata_get_transport")
	}
	if result.Status == http.StatusNotFound {
		return slotMetadata{}, result.Status, nil
	}
	if result.Status != http.StatusOK {
		return slotMetadata{}, result.Status, errors.New("metadata_get_status")
	}
	var metadata slotMetadata
	if err := json.Unmarshal(result.Body, &metadata); err != nil || metadata.ID != fileID || metadata.Version == "" {
		return slotMetadata{}, result.Status, errors.New("metadata_get_invalid")
	}
	return metadata, result.Status, nil
}

func (p *probe) patch(ctx context.Context, fileID, description string) (int64, error) {
	if p.patches >= updates {
		return 0, errors.New("patch_cap")
	}
	p.patches++
	body, _ := json.Marshal(map[string]string{"description": description})
	result, err := p.request(ctx, p.writer, http.MethodPatch, "/drive/v3/files/"+url.PathEscape(fileID)+"?fields=id,version,description", body)
	p.quotaUnits += attempts(result) * 50
	if err != nil || result == nil {
		return 0, errors.New("metadata_patch_transport")
	}
	if result.Status != http.StatusOK {
		return 0, errors.New("metadata_patch_status")
	}
	var metadata slotMetadata
	if err := json.Unmarshal(result.Body, &metadata); err != nil || metadata.ID != fileID || metadata.Description != description {
		return 0, errors.New("metadata_patch_invalid")
	}
	version, err := strconv.ParseInt(metadata.Version, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("metadata_patch_version_invalid")
	}
	return version, nil
}

func (p *probe) cleanup(fileID string, result *report) {
	if fileID == "" {
		result.CleanupVerified = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		result.CleanupDeletes++
		p.quotaUnits += 500 // The store can refresh a rejected token, then retry five times.
		if err := p.store.DeleteID(ctx, fileID); err != nil {
			continue
		}
		before := p.gets
		_, status, err := p.get(ctx, fileID, true)
		result.CleanupGets += p.gets - before
		if err != nil || status != http.StatusNotFound {
			continue
		}
		time.Sleep(2 * time.Second)
		before = p.gets
		_, status, err = p.get(ctx, fileID, true)
		result.CleanupGets += p.gets - before
		if err == nil && status == http.StatusNotFound {
			result.CleanupVerified = true
			if p.recoveryOwned {
				if err := os.Remove(p.recovery); err != nil && !os.IsNotExist(err) {
					result.CleanupVerified = false
					result.Failure = "recovery_record_removal_failed"
				}
			}
			return
		}
	}
	if result.Failure == "" {
		result.Failure = "exact_id_cleanup_unverified"
	}
}

func (p *probe) run() (result report) {
	result = report{SchemaVersion: 1, Route: "google_front_pinned", UpdatesScheduled: updates, Rows: []row{}}
	var fileID string
	defer func() {
		p.cleanup(fileID, &result)
		result.Gets = p.gets
		result.Patches = p.patches
		result.QuotaUnitsUpperBound = p.quotaUnits
		result.OK = result.Failure == "" && result.UpdatesObserved == updates && result.CleanupVerified
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ids, err := p.store.GenerateObjectIDs(ctx, 1)
	p.quotaUnits += 50 // Store-level auth refresh can double five HTTP attempts.
	if err != nil || len(ids) != 1 {
		result.Failure = "id_reservation_failed"
		return
	}
	fileID = ids[0]
	if err := writeRecoveryID(p.recovery, fileID); err != nil {
		result.Failure = "recovery_record_write_failed"
		return
	}
	p.recoveryOwned = true
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		result.Failure = "random_failed"
		return
	}
	name := "bench-drive/mutable-slot/" + hex.EncodeToString(nonce)
	_, err = p.store.PutObjectWithID(ctx, fileID, name, []byte("probe"))
	p.quotaUnits += 500
	if err != nil {
		result.Failure = "slot_create_failed"
		return
	}

	initialStart := time.Now()
	var previousDescription string
	initialSeen := false
	for time.Since(initialStart) < visibilityMax {
		metadata, status, err := p.get(ctx, fileID, false)
		if err != nil {
			result.Failure = "initial_get_failed"
			return
		}
		if status == http.StatusOK {
			previousDescription = metadata.Description
			result.InitialVisibilityMS = millis(time.Since(initialStart))
			initialSeen = true
			break
		}
		time.Sleep(pollInterval)
	}
	if !initialSeen {
		result.Failure = "initial_visibility_timeout"
		return
	}

	for sequence := 1; sequence <= updates; sequence++ {
		payload := make([]byte, 64)
		if _, err := rand.Read(payload); err != nil {
			result.Failure = "random_failed"
			return
		}
		description := fmt.Sprintf("skirk-slot-probe-v1:%d:%s", sequence, base64.StdEncoding.EncodeToString(payload))
		publishStart := time.Now()
		version, err := p.patch(ctx, fileID, description)
		patchEnd := time.Now()
		if err != nil {
			result.Failure = err.Error()
			return
		}
		entry := row{Sequence: sequence, PatchMS: millis(patchEnd.Sub(publishStart)), PatchVersion: version}
		for time.Since(publishStart) < visibilityMax {
			before := p.gets
			metadata, status, err := p.get(ctx, fileID, false)
			entry.Gets += p.gets - before
			if err != nil || status != http.StatusOK {
				result.Failure = "message_get_failed"
				result.Rows = append(result.Rows, entry)
				return
			}
			seenVersion, err := strconv.ParseInt(metadata.Version, 10, 64)
			if err != nil || seenVersion < 1 {
				result.Failure = "message_get_version_invalid"
				result.Rows = append(result.Rows, entry)
				return
			}
			if metadata.Description == description && seenVersion >= version {
				entry.ObservedVersion = seenVersion
				entry.PublishToObservedMS = millis(time.Since(publishStart))
				entry.PatchToObservedMS = millis(time.Since(patchEnd))
				result.Rows = append(result.Rows, entry)
				result.UpdatesObserved++
				previousDescription = description
				break
			}
			if metadata.Description != previousDescription || seenVersion >= version {
				result.Failure = "unexpected_slot_state"
				result.Rows = append(result.Rows, entry)
				return
			}
			entry.StaleGets++
			entry.StaleVersions = append(entry.StaleVersions, seenVersion)
			result.StaleGets++
			time.Sleep(pollInterval)
		}
		if result.UpdatesObserved != sequence {
			result.Failure = "message_visibility_timeout"
			result.Rows = append(result.Rows, entry)
			return
		}
	}
	return
}

func millis(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

func writeReport(path string, value report) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(value)
}

func writeRecoveryID(path, fileID string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(fileID + "\n")
	return err
}

func main() {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutable-slot-probe: invalid bounded live arguments")
		os.Exit(2)
	}
	config, err := skirk.LoadConfig(options.config)
	if err != nil || config.Route.Mode != "google_front_pinned" || config.Route.GoogleIP == "" ||
		config.Drive.FolderID == "" || config.Drive.Space == "appDataFolder" {
		fmt.Fprintln(os.Stderr, "mutable-slot-probe: requires an isolated pinned-front test folder")
		os.Exit(2)
	}
	result := newProbe(config, options.out).run()
	if err := writeReport(options.out, result); err != nil {
		fmt.Fprintln(os.Stderr, "mutable-slot-probe: private result write failed")
		os.Exit(2)
	}
	fmt.Printf("{\"ok\":%t,\"updates_observed\":%d,\"gets\":%d,\"cleanup_verified\":%t,\"failure\":%q}\n",
		result.OK, result.UpdatesObserved, result.Gets, result.CleanupVerified, result.Failure)
	if !result.OK {
		os.Exit(1)
	}
}
