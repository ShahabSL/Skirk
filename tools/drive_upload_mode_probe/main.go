// Sequential, bounded multipart versus resumable upload probe on the pinned front.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	bytesPerArm = 1 << 20
	maxUnits    = 4000
)

type row struct {
	Index                    int     `json:"index"`
	Mode                     string  `json:"mode"`
	InitiationMS             float64 `json:"initiation_ms,omitempty"`
	UploadBodyMS             float64 `json:"upload_body_ms,omitempty"`
	UploadTotalMS            float64 `json:"upload_total_ms"`
	SessionHost              string  `json:"session_host,omitempty"`
	SessionCompatible        bool    `json:"session_compatible,omitempty"`
	InitiationStatus         int     `json:"initiation_status,omitempty"`
	UploadStatus             int     `json:"upload_status,omitempty"`
	MetadataSHA256Verified   bool    `json:"metadata_sha256_verified"`
	DownloadedSHA256Verified bool    `json:"downloaded_sha256_verified"`
	ExactID404Checks         int     `json:"exact_id_404_checks"`
	CleanupVerified          bool    `json:"cleanup_verified"`
	Failure                  string  `json:"failure,omitempty"`
}

type report struct {
	SchemaVersion          int     `json:"schema_version"`
	Route                  string  `json:"route"`
	BytesPerArm            int     `json:"bytes_per_arm"`
	Rows                   []row   `json:"rows"`
	MultipartMedianMS      float64 `json:"multipart_median_ms,omitempty"`
	MultipartMaxMS         float64 `json:"multipart_max_ms,omitempty"`
	ResumableMedianMS      float64 `json:"resumable_median_ms,omitempty"`
	ResumableMaxMS         float64 `json:"resumable_max_ms,omitempty"`
	DriveUnitsEstimated    int64   `json:"drive_units_estimated"`
	StoreErrors            int64   `json:"store_errors"`
	ImmediatePrefixObjects int     `json:"immediate_prefix_objects"`
	DelayedPrefixObjects   int     `json:"delayed_prefix_objects"`
	CleanupVerified        bool    `json:"cleanup_verified"`
	Failure                string  `json:"failure,omitempty"`
	OK                     bool    `json:"ok"`
}

type probe struct {
	cfg           *skirk.Config
	tokens        *skirk.AccessTokenSource
	http          *skirk.GoogleHTTPClient
	store         *skirk.DriveStore
	prefix        string
	out, recovery string
	manualUnits   int64
	result        report
}

func options() (string, string, error) {
	config := flag.String("config", "", "isolated personal-test kit exit.json")
	out := flag.String("out", "", "new private JSON report under .skirk-runs")
	live := flag.Bool("live", false, "permit bounded Drive calls")
	flag.Parse()
	if !*live || *config == "" || *out == "" || len(flag.Args()) != 0 {
		return "", "", errors.New("requires --live, --config, and --out")
	}
	c, err := filepath.EvalSymlinks(*config)
	if err != nil {
		return "", "", errors.New("isolated test config missing")
	}
	c, err = filepath.Abs(c)
	if err != nil || !strings.HasSuffix(c, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		return "", "", errors.New("requires isolated personal-test kit exit.json")
	}
	o, err := filepath.Abs(*out)
	if err != nil || filepath.Ext(o) != ".json" {
		return "", "", errors.New("invalid report path")
	}
	private, err := filepath.Abs(".skirk-runs")
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(private, 0700); err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(private, o)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, string(filepath.Separator)) {
		return "", "", errors.New("report must be directly under .skirk-runs")
	}
	for _, path := range []string{o, o + ".recovery-ids"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return "", "", errors.New("report or recovery file already exists")
		}
	}
	return c, o, nil
}

func newProbe(cfg *skirk.Config, out string) *probe {
	cfg.Route.Mode = "google_front_pinned"
	tokens := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	httpClient := skirk.NewGoogleHTTPClient(cfg.Route)
	return &probe{cfg: cfg, tokens: tokens, http: httpClient, store: skirk.NewDriveStoreWithTokenSource(httpClient, tokens, cfg.Drive), out: out, recovery: out + ".recovery-ids", result: report{SchemaVersion: 1, Route: "google_front_pinned", BytesPerArm: bytesPerArm}}
}

func (p *probe) units() int64 { return p.store.QuotaSnapshot().Units + p.manualUnits }

func (p *probe) request(ctx context.Context, method, path string, cost int64, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
	if p.units()+cost > maxUnits {
		return nil, errors.New("Drive quota guard")
	}
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Authorization"] = "Bearer " + token
	headers["Cache-Control"] = "no-cache"
	if project := p.cfg.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	res, err := p.http.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	p.manualUnits += cost
	return res, err
}

func (p *probe) appendRecovery(id, name string) error {
	f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(f, "%s %s\n", id, name)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (p *probe) exactMissing(ctx context.Context, id string) bool {
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id", 5, nil, nil)
	return err == nil && res != nil && res.Status == http.StatusNotFound
}

func (p *probe) cleanupID(id string, r *row) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		if p.units()+100 > maxUnits {
			break
		}
		if err := p.store.DeleteID(ctx, id); err != nil {
			continue
		}
		if !p.exactMissing(ctx, id) {
			continue
		}
		r.ExactID404Checks++
		time.Sleep(time.Second)
		if !p.exactMissing(ctx, id) {
			continue
		}
		r.ExactID404Checks++
		r.CleanupVerified = true
		return
	}
}

func (p *probe) prefixObjects(ctx context.Context) (int, error) {
	q := "trashed = false and name contains '" + p.prefix + "'"
	if folder := p.cfg.Drive.FolderID; folder != "" {
		q += " and '" + folder + "' in parents"
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("fields", "nextPageToken,files(id,name)")
	v.Set("pageSize", "100")
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files?"+v.Encode(), 100, nil, nil)
	if err != nil || res == nil || res.Status != http.StatusOK {
		return 0, errors.New("prefix sweep failed")
	}
	var payload struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil || payload.NextPageToken != "" {
		return 0, errors.New("prefix sweep incomplete")
	}
	count := 0
	for _, file := range payload.Files {
		if strings.HasPrefix(file.Name, p.prefix) {
			count++
		}
	}
	return count, nil
}

func (p *probe) verify(ctx context.Context, id, name string, data []byte, r *row) {
	digest := sha256.Sum256(data)
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id,name,size,sha256Checksum", 5, nil, nil)
	if err == nil && res != nil && res.Status == http.StatusOK {
		var meta struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Size string `json:"size"`
			SHA  string `json:"sha256Checksum"`
		}
		if json.Unmarshal(res.Body, &meta) == nil {
			r.MetadataSHA256Verified = meta.ID == id && meta.Name == name && meta.Size == fmt.Sprint(bytesPerArm) && meta.SHA == hex.EncodeToString(digest[:])
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	actual, err := p.store.GetByID(readCtx, id)
	if err == nil {
		readSHA := sha256.Sum256(actual)
		r.DownloadedSHA256Verified = len(actual) == len(data) && readSHA == digest && bytes.Equal(actual, data)
	}
}

func (p *probe) multipart(ctx context.Context, id, name string, data []byte, r *row) {
	started := time.Now()
	uploadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := p.store.PutObjectWithID(uploadCtx, id, name, data)
	r.UploadTotalMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		r.Failure = "multipart_upload_failed"
		return
	}
	if info.ID != id || info.Name != name || info.Size != bytesPerArm {
		r.Failure = "multipart_response_mismatch"
		return
	}
	r.UploadStatus = http.StatusOK
}

func (p *probe) resumable(ctx context.Context, id, name string, data []byte, r *row) {
	started := time.Now()
	metadata, _ := json.Marshal(map[string]any{"id": id, "name": name, "mimeType": "application/octet-stream", "parents": []string{p.cfg.Drive.FolderID}})
	headers := map[string]string{"Content-Type": "application/json; charset=UTF-8", "X-Upload-Content-Type": "application/octet-stream", "X-Upload-Content-Length": fmt.Sprint(len(data))}
	initCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	res, err := p.request(initCtx, http.MethodPost, "/upload/drive/v3/files?uploadType=resumable&fields=id,name,size,sha256Checksum", 50, headers, metadata)
	cancel()
	r.InitiationMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil || res == nil {
		r.Failure = "resumable_initiation_failed"
		return
	}
	r.InitiationStatus = res.Status
	if res.Status != http.StatusOK {
		r.Failure = "resumable_initiation_status"
		return
	}
	location := res.Header.Get("Location")
	session, err := url.Parse(location)
	if err != nil {
		r.Failure = "resumable_session_uri_invalid"
		return
	}
	r.SessionHost = session.Hostname()
	r.SessionCompatible = session.Scheme == "https" && session.Host == "www.googleapis.com" && session.Path == "/upload/drive/v3/files" && session.Query().Get("uploadType") == "resumable" && session.Query().Get("upload_id") != "" && session.User == nil && session.Fragment == ""
	if !r.SessionCompatible {
		r.Failure = "resumable_session_uri_not_pinned_route_compatible"
		return
	}
	bodyStarted := time.Now()
	putCtx, stop := context.WithTimeout(ctx, 20*time.Second)
	putRes, err := p.request(putCtx, http.MethodPut, session.RequestURI(), 50, map[string]string{"Content-Type": "application/octet-stream"}, data)
	stop()
	r.UploadBodyMS = float64(time.Since(bodyStarted).Microseconds()) / 1000
	r.UploadTotalMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil || putRes == nil {
		r.Failure = "resumable_body_failed"
		return
	}
	r.UploadStatus = putRes.Status
	if putRes.Status != http.StatusOK && putRes.Status != http.StatusCreated {
		r.Failure = "resumable_body_status"
		return
	}
	var file struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size string `json:"size"`
	}
	if json.Unmarshal(putRes.Body, &file) != nil || file.ID != id || file.Name != name || file.Size != fmt.Sprint(bytesPerArm) {
		r.Failure = "resumable_response_mismatch"
	}
}

func summary(values []float64) (median, max float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		return (values[middle-1] + values[middle]) / 2, values[len(values)-1]
	}
	return values[middle], values[len(values)-1]
}

func (p *probe) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		p.result.Failure = "random_failure"
		return
	}
	p.prefix = "bench-drive/upload-mode/" + hex.EncodeToString(nonce) + "/"
	// ABBA order reduces drift from network conditions while keeping calls sequential.
	modes := []string{"multipart", "resumable", "resumable", "multipart", "multipart", "resumable", "resumable", "multipart"}
	for index, mode := range modes {
		if ctx.Err() != nil || p.units()+450 > maxUnits || p.store.QuotaBackoffSnapshot().Active {
			p.result.Failure = "time_or_quota_guard"
			break
		}
		ids, err := p.store.GenerateObjectIDs(ctx, 1)
		if err != nil || len(ids) != 1 {
			p.result.Failure = "generated_id_failed"
			break
		}
		id := ids[0]
		name := fmt.Sprintf("%s%02d.bin", p.prefix, index)
		r := row{Index: index, Mode: mode}
		if err := p.appendRecovery(id, name); err != nil {
			r.Failure = "recovery_record_failed"
			p.cleanupID(id, &r)
			p.result.Rows = append(p.result.Rows, r)
			p.result.Failure = r.Failure
			break
		}
		data := make([]byte, bytesPerArm)
		if _, err := rand.Read(data); err != nil {
			r.Failure = "random_failure"
		} else if mode == "multipart" {
			p.multipart(ctx, id, name, data, &r)
		} else {
			p.resumable(ctx, id, name, data, &r)
		}
		if r.Failure == "" {
			p.verify(ctx, id, name, data, &r)
			if !r.MetadataSHA256Verified || !r.DownloadedSHA256Verified {
				r.Failure = "exact_sha256_readback_failed"
			}
		}
		p.cleanupID(id, &r)
		p.result.Rows = append(p.result.Rows, r)
		fmt.Printf("arm=%d mode=%s upload_ms=%.1f init_ms=%.1f body_ms=%.1f verified=%t cleanup=%t units=%d failure=%s\n", index, mode, r.UploadTotalMS, r.InitiationMS, r.UploadBodyMS, r.MetadataSHA256Verified && r.DownloadedSHA256Verified, r.CleanupVerified, p.units(), r.Failure)
		if r.Failure != "" || !r.CleanupVerified {
			p.result.Failure = r.Failure
			if p.result.Failure == "" {
				p.result.Failure = "exact_id_cleanup_failed"
			}
			break
		}
	}
	if len(p.result.Rows) > 0 {
		count, err := p.prefixObjects(ctx)
		p.result.ImmediatePrefixObjects = count
		if err == nil && count == 0 {
			time.Sleep(30 * time.Second)
			count, err = p.prefixObjects(ctx)
			p.result.DelayedPrefixObjects = count
			p.result.CleanupVerified = err == nil && count == 0
		}
	}
	if p.result.CleanupVerified {
		_ = os.Remove(p.recovery)
	}
	var multipart, resumable []float64
	for _, r := range p.result.Rows {
		if r.Failure != "" {
			continue
		}
		if r.Mode == "multipart" {
			multipart = append(multipart, r.UploadTotalMS)
		} else {
			resumable = append(resumable, r.UploadTotalMS)
		}
	}
	p.result.MultipartMedianMS, p.result.MultipartMaxMS = summary(multipart)
	p.result.ResumableMedianMS, p.result.ResumableMaxMS = summary(resumable)
	p.result.DriveUnitsEstimated = p.units()
	p.result.StoreErrors = p.store.QuotaSnapshot().Errors
	p.result.OK = p.result.Failure == "" && len(p.result.Rows) == len(modes) && p.result.CleanupVerified && p.result.DriveUnitsEstimated <= maxUnits
}

func main() {
	config, out, err := options()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg, err := skirk.LoadConfig(config)
	if err != nil || cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" {
		fmt.Fprintln(os.Stderr, "isolated test folder config invalid")
		os.Exit(2)
	}
	p := newProbe(cfg, out)
	p.run()
	data, err := json.MarshalIndent(p.result, "", "  ")
	if err != nil || os.WriteFile(out, append(data, '\n'), 0600) != nil {
		fmt.Fprintln(os.Stderr, "private report write failed")
		os.Exit(2)
	}
	fmt.Printf("ok=%t arms=%d multipart_median_ms=%.1f resumable_median_ms=%.1f cleanup=%t units=%d failure=%s\n", p.result.OK, len(p.result.Rows), p.result.MultipartMedianMS, p.result.ResumableMedianMS, p.result.CleanupVerified, p.result.DriveUnitsEstimated, p.result.Failure)
	if !p.result.OK {
		os.Exit(1)
	}
}
