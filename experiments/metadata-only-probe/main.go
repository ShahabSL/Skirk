// Bounded Google Drive API experiment. It never runs unless --live is supplied.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	maxFiles = 27
	maxUnits = 25000
	marker   = "skirk-sealed-v1:"
)

type requester func(context.Context, string, string, string, map[string]string, []byte) (*skirk.HTTPResult, error)

type probe struct {
	do           requester
	tokens       *skirk.AccessTokenSource
	quotaProject string
	folder       string
	prefix       string
	units        int
	requests     int
}

type row struct {
	Mode             string  `json:"mode"`
	Size             int     `json:"bytes"`
	Repetition       int     `json:"repetition"`
	ID               string  `json:"file_id"`
	CreateMS         float64 `json:"create_ms"`
	VisibleMS        float64 `json:"list_visibility_ms"`
	CoreMS           float64 `json:"core_ms"`
	CleanupMS        float64 `json:"cleanup_ms"`
	CoreUnits        int     `json:"estimated_core_units"`
	TotalUnits       int     `json:"estimated_total_units"`
	ListCalls        int     `json:"list_calls"`
	CreateStatus     int     `json:"create_status"`
	MetadataStatus   int     `json:"metadata_status"`
	MediaStatus      int     `json:"media_status"`
	MediaBytes       int     `json:"media_bytes"`
	DescriptionExact bool    `json:"description_exact"`
	MediaExact       bool    `json:"media_exact"`
	NameExact        bool    `json:"name_exact"`
	CleanupOK        bool    `json:"cleanup_ok"`
	Failure          string  `json:"failure,omitempty"`
}

type report struct {
	Prefix            string `json:"prefix"`
	Route             string `json:"route"`
	Rows              []row  `json:"rows"`
	TotalUnits        int    `json:"estimated_total_units"`
	Requests          int    `json:"http_attempts"`
	FinalSweepMatches int    `json:"final_sweep_matches"`
	FinalSweepDeleted int    `json:"final_sweep_deleted"`
	FinalSweepErrors  int    `json:"final_sweep_errors"`
	Stopped           string `json:"stopped,omitempty"`
}

func main() {
	configPath := flag.String("config", "", "isolated quota-kit exit profile")
	outPath := flag.String("out", "", "new private .skirk-runs JSON result path")
	live := flag.Bool("live", false, "authorize this bounded 27-file disposable experiment")
	flag.Parse()
	if !*live || *configPath == "" || *outPath == "" {
		panic("requires --live, --config, and --out")
	}
	configAbs, err := filepath.Abs(*configPath)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(configAbs), "/.skirk-runs/personal-test/quota-kit/exit.json") {
		panic("requires the isolated personal-test/quota-kit/exit.json")
	}
	outAbs, err := filepath.Abs(*outPath)
	if err != nil || !strings.Contains(filepath.ToSlash(outAbs), "/.skirk-runs/") {
		panic("result must be in private .skirk-runs")
	}
	if err := os.MkdirAll(filepath.Dir(outAbs), 0700); err != nil {
		panic("cannot create result directory")
	}
	file, err := os.OpenFile(outAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		panic("result already exists or cannot be created")
	}
	file.Close()
	cfg, err := skirk.LoadConfig(configAbs)
	if err != nil || cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" || !strings.HasPrefix(cfg.Route.Mode, "google_front") {
		panic("requires an isolated visible folder and Google-front route")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := report{Prefix: "bench-drive/metadata-only/" + hex.EncodeToString(nonce[:]) + "/", Route: cfg.Route.Mode, Rows: []row{}}
	client := skirk.NewGoogleHTTPClient(cfg.Route)
	p := &probe{do: client.RequestNoRateLimitRetry, tokens: skirk.NewAccessTokenSource(cfg.Auth, cfg.Route), quotaProject: cfg.Drive.QuotaProject, folder: cfg.Drive.FolderID, prefix: r.Prefix}
	defer func() {
		r.TotalUnits, r.Requests = p.units, p.requests
		encoded, _ := json.MarshalIndent(r, "", "  ")
		_ = os.WriteFile(outAbs, append(encoded, '\n'), 0600)
	}()
	ids, status, err := p.generateIDs(ctx, maxFiles)
	if err != nil || status != 200 || len(ids) != maxFiles {
		r.Stopped = "id_reservation_failed"
		return
	}
	reserved := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || reserved[id] {
			r.Stopped = "invalid_or_duplicate_reserved_id"
			return
		}
		reserved[id] = true
	}
	sizes := []int{256, 2048, 8192}
	shuffleSizes(sizes)
	var offset [1]byte
	if _, err := rand.Read(offset[:]); err != nil {
		r.Stopped = "random_order_failed"
		return
	}
	modes := []string{"metadata_only", "dual_inline", "media_multipart"}
	position := 0
	for _, size := range sizes {
		for repetition := 0; repetition < 3; repetition++ {
			for turn := 0; turn < len(modes); turn++ {
				if ctx.Err() != nil || p.units >= maxUnits {
					r.Stopped = "time_or_quota_guard"
					goto sweep
				}
				mode := modes[(int(offset[0])+repetition+turn)%len(modes)]
				data := make([]byte, size)
				if _, err := rand.Read(data); err != nil {
					r.Stopped = "random_payload_failed"
					goto sweep
				}
				name := fmt.Sprintf("%s%02d-%s-%d", r.Prefix, position, mode, size)
				item := p.runOne(ctx, mode, size, repetition, ids[position], name, data)
				r.Rows = append(r.Rows, item)
				position++
				fmt.Printf("item=%d mode=%s bytes=%d create_ms=%.1f visible_ms=%.1f core_ms=%.1f ok=%t units=%d\n", position, mode, size, item.CreateMS, item.VisibleMS, item.CoreMS, item.Failure == "" && item.CleanupOK, item.TotalUnits)
				// A 400/413 for metadata-only may reveal a field-size limit.
				// Preserve that result and continue to the other methods/sizes.
				fieldRejected := mode == "metadata_only" && item.Failure == "create_failed" && (item.CreateStatus == 400 || item.CreateStatus == 413)
				if !item.CleanupOK || (item.Failure != "" && !fieldRejected) {
					r.Stopped = "integrity_or_cleanup_gate"
					goto sweep
				}
			}
		}
	}
sweep:
	// A canceled upload can commit after an immediate delete. Reconcile only
	// exact IDs reserved for this run; never delete another mailbox file.
	time.Sleep(3 * time.Second)
	sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer sweepCancel()
	remaining, sweepErr := p.listAll(sweepCtx)
	if sweepErr != nil {
		r.FinalSweepErrors++
	}
	for _, item := range remaining {
		if !strings.HasPrefix(item.Name, p.prefix) || !reserved[item.ID] {
			continue
		}
		r.FinalSweepMatches++
		status, err := p.deleteID(sweepCtx, item.ID)
		if err == nil && (status == 204 || status == 404) {
			r.FinalSweepDeleted++
		} else {
			r.FinalSweepErrors++
		}
	}
	if r.FinalSweepErrors > 0 && r.Stopped == "" {
		r.Stopped = "late_cleanup_failed"
	}
}

func (p *probe) request(ctx context.Context, method, path string, body []byte, contentType string, cost int) (*skirk.HTTPResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := p.tokens.Token(callCtx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	if p.quotaProject != "" {
		headers["X-Goog-User-Project"] = p.quotaProject
	}
	result, err := p.do(callCtx, method, "www.googleapis.com", path, headers, body)
	attempts := 1
	if result != nil && result.Attempts > 0 {
		attempts = result.Attempts
	} else if err != nil {
		attempts = 5
	} // Conservative failed-request bound.
	p.requests += attempts
	p.units += cost * attempts
	return result, err
}

func (p *probe) generateIDs(ctx context.Context, n int) ([]string, int, error) {
	path := "/drive/v3/files/generateIds?count=" + strconv.Itoa(n) + "&fields=ids"
	result, err := p.request(ctx, "GET", path, nil, "", 5)
	if err != nil || result == nil {
		return nil, 0, err
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(result.Body, &body) != nil {
		return nil, result.Status, errors.New("bad id response")
	}
	return body.IDs, result.Status, nil
}

func fileMeta(id, name, folder, description string) map[string]any {
	meta := map[string]any{"id": id, "name": name, "mimeType": "application/octet-stream", "parents": []string{folder}}
	if description != "" {
		meta["description"] = description
	}
	return meta
}

func (p *probe) create(ctx context.Context, mode, id, name string, data []byte) (int, error) {
	description := ""
	if mode != "media_multipart" {
		description = marker + base64.StdEncoding.EncodeToString(data)
	}
	meta, _ := json.Marshal(fileMeta(id, name, p.folder, description))
	if mode == "metadata_only" {
		result, err := p.request(ctx, "POST", "/drive/v3/files?fields=id,name,size", meta, "application/json; charset=UTF-8", 50)
		if err != nil || result == nil {
			return 0, errors.New("create_request_failed")
		}
		if result.Status != 200 && result.Status != 201 {
			return result.Status, errors.New("create_status")
		}
		if !createIdentityExact(result, id, name, 0, false) {
			return result.Status, errors.New("create_identity_mismatch")
		}
		return result.Status, nil
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{"Content-Type": []string{"application/json; charset=UTF-8"}}
	part, err := w.CreatePart(h)
	if err != nil {
		return 0, err
	}
	_, _ = part.Write(meta)
	h = textproto.MIMEHeader{"Content-Type": []string{"application/octet-stream"}}
	part, err = w.CreatePart(h)
	if err != nil {
		return 0, err
	}
	_, _ = part.Write(data)
	_ = w.Close()
	result, err := p.request(ctx, "POST", "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", buf.Bytes(), "multipart/related; boundary="+w.Boundary(), 50)
	if err != nil || result == nil {
		return 0, errors.New("multipart_request_failed")
	}
	if result.Status != 200 && result.Status != 201 {
		return result.Status, errors.New("multipart_status")
	}
	if !createIdentityExact(result, id, name, len(data), true) {
		return result.Status, errors.New("multipart_identity_mismatch")
	}
	return result.Status, nil
}

type fileInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        string `json:"size"`
	Description string `json:"description"`
}

func createIdentityExact(result *skirk.HTTPResult, id, name string, size int, requireSize bool) bool {
	if result == nil {
		return false
	}
	var f fileInfo
	if json.Unmarshal(result.Body, &f) != nil || f.ID != id || f.Name != name {
		return false
	}
	return !requireSize || f.Size == strconv.Itoa(size)
}
func (p *probe) listPage(ctx context.Context, pageToken string) ([]fileInfo, string, int, error) {
	query := "'" + strings.ReplaceAll(p.folder, "'", "\\'") + "' in parents and name contains '" + p.prefix + "' and trashed = false"
	v := url.Values{"q": {query}, "fields": {"nextPageToken,incompleteSearch,files(id,name,size,description)"}, "pageSize": {"100"}}
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	result, err := p.request(ctx, "GET", "/drive/v3/files?"+v.Encode(), nil, "", 100)
	if err != nil || result == nil {
		return nil, "", 0, errors.New("list_request_failed")
	}
	if result.Status != 200 {
		return nil, "", result.Status, errors.New("list_status")
	}
	var payload struct {
		Files      []fileInfo `json:"files"`
		Next       string     `json:"nextPageToken"`
		Incomplete bool       `json:"incompleteSearch"`
	}
	if json.Unmarshal(result.Body, &payload) != nil || payload.Incomplete {
		return nil, "", result.Status, errors.New("list_bad_or_incomplete")
	}
	return payload.Files, payload.Next, result.Status, nil
}

func (p *probe) find(ctx context.Context, id string) (fileInfo, int, error) {
	calls := 0
	for i := 0; i < 30; i++ {
		page := ""
		for j := 0; j < 4; j++ {
			files, next, _, err := p.listPage(ctx, page)
			calls++
			if err != nil {
				return fileInfo{}, calls, err
			}
			for _, f := range files {
				if f.ID == id {
					return f, calls, nil
				}
			}
			if next == "" {
				break
			}
			page = next
		}
		if p.units >= maxUnits || ctx.Err() != nil {
			return fileInfo{}, calls, errors.New("quota_or_time_guard")
		}
		select {
		case <-ctx.Done():
			return fileInfo{}, calls, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fileInfo{}, calls, errors.New("visibility_timeout")
}

func (p *probe) getMetadata(ctx context.Context, id string) (fileInfo, int, error) {
	result, err := p.request(ctx, "GET", "/drive/v3/files/"+url.PathEscape(id)+"?fields=id,name,size,description", nil, "", 5)
	if err != nil || result == nil {
		return fileInfo{}, 0, errors.New("metadata_request_failed")
	}
	if result.Status != 200 {
		return fileInfo{}, result.Status, errors.New("metadata_status")
	}
	var info fileInfo
	if json.Unmarshal(result.Body, &info) != nil {
		return fileInfo{}, result.Status, errors.New("metadata_bad_json")
	}
	return info, result.Status, nil
}
func (p *probe) getMedia(ctx context.Context, id string) ([]byte, int, error) {
	result, err := p.request(ctx, "GET", "/drive/v3/files/"+url.PathEscape(id)+"?alt=media", nil, "", 200)
	if err != nil || result == nil {
		return nil, 0, errors.New("media_request_failed")
	}
	return result.Body, result.Status, nil
}
func (p *probe) deleteID(ctx context.Context, id string) (int, error) {
	result, err := p.request(ctx, "DELETE", "/drive/v3/files/"+url.PathEscape(id), nil, "", 50)
	if err != nil || result == nil {
		return 0, errors.New("delete_request_failed")
	}
	return result.Status, nil
}

func exactDescription(description string, data []byte) bool {
	if !strings.HasPrefix(description, marker) {
		return false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(description, marker))
	if err != nil || len(decoded) != len(data) {
		return false
	}
	a, b := sha256.Sum256(decoded), sha256.Sum256(data)
	return a == b && bytes.Equal(decoded, data)
}

func (p *probe) runOne(ctx context.Context, mode string, size, repetition int, id, name string, data []byte) (r row) {
	r.Mode, r.Size, r.Repetition, r.ID = mode, size, repetition, id
	startUnits := p.units
	started := time.Now()
	defer func() {
		cleanupStarted := time.Now()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		status, err := p.deleteID(cleanupCtx, id)
		cancel()
		r.CleanupMS = float64(time.Since(cleanupStarted).Microseconds()) / 1000
		r.CleanupOK = err == nil && (status == 204 || status == 404)
		r.TotalUnits = p.units - startUnits
	}()
	status, err := p.create(ctx, mode, id, name, data)
	r.CreateStatus = status
	r.CreateMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		r.Failure = "create_failed"
		return
	}
	createdAt := time.Now()
	info, calls, err := p.find(ctx, id)
	r.ListCalls = calls
	r.VisibleMS = float64(time.Since(createdAt).Microseconds()) / 1000
	if err != nil {
		r.Failure = "not_visible"
		return
	}
	r.NameExact = info.Name == name && info.ID == id
	if !r.NameExact {
		r.Failure = "identity_mismatch"
		return
	}
	if mode != "media_multipart" {
		r.DescriptionExact = exactDescription(info.Description, data)
		if !r.DescriptionExact {
			r.Failure = "list_description_mismatch"
			return
		}
		r.CoreMS = float64(time.Since(started).Microseconds()) / 1000
	}
	if mode == "media_multipart" {
		media, mediaStatus, mediaErr := p.getMedia(ctx, id)
		r.MediaStatus, r.MediaBytes = mediaStatus, len(media)
		if mediaErr != nil || mediaStatus != 200 || !bytes.Equal(media, data) {
			r.Failure = "media_mismatch"
			return
		}
		r.MediaExact = true
		r.CoreMS = float64(time.Since(started).Microseconds()) / 1000
	}
	r.CoreUnits = p.units - startUnits
	meta, metaStatus, err := p.getMetadata(ctx, id)
	r.MetadataStatus = metaStatus
	if err != nil || meta.ID != id || meta.Name != name {
		r.Failure = "metadata_mismatch"
		return
	}
	if mode != "media_multipart" && !exactDescription(meta.Description, data) {
		r.Failure = "metadata_description_mismatch"
		return
	}
	if mode == "media_multipart" {
		return
	}
	media, mediaStatus, err := p.getMedia(ctx, id)
	r.MediaStatus, r.MediaBytes = mediaStatus, len(media)
	if err != nil {
		r.Failure = "media_request_failed"
		return
	}
	if mode == "metadata_only" {
		// Record the actual Drive behavior; it may be 200/empty or an error.
		if mediaStatus == 200 && len(media) != 0 {
			r.Failure = "metadata_only_unexpected_media"
			return
		}
		if meta.Size != "" && meta.Size != "0" {
			r.Failure = "metadata_only_unexpected_size"
			return
		}
		return
	}
	r.MediaExact = mediaStatus == 200 && bytes.Equal(media, data)
	if !r.MediaExact {
		r.Failure = "media_mismatch"
		return
	}
	return
}

func (p *probe) listAll(ctx context.Context) ([]fileInfo, error) {
	var all []fileInfo
	page := ""
	for i := 0; i < 4; i++ {
		files, next, _, err := p.listPage(ctx, page)
		if err != nil {
			return all, err
		}
		all = append(all, files...)
		if next == "" {
			break
		}
		page = next
	}
	return all, nil
}

func shuffleSizes(sizes []int) {
	for i := len(sizes) - 1; i > 0; i-- {
		var b [1]byte
		_, _ = rand.Read(b[:])
		j := int(b[0]) % (i + 1)
		sizes[i], sizes[j] = sizes[j], sizes[i]
	}
}
