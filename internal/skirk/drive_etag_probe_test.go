package skirk

// Research primitive only. Never linked into the CLI or a product profile.
import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type driveETagCall struct {
	Phase, Method, IfMatch, IfNoneMatch, ETag, Protocol, Error string
	MetadataVersion                                            string
	Status                                                     int
	Finished                                                   bool
}
type driveETagReport struct {
	Schema                                                                         int `json:"schema"`
	Route, Front, AccountBinding, FileID, FileName, Projection, Failure            string
	Calls                                                                          []driveETagCall
	VisibleCalls, ReservedCalls, AttemptsUpper, QuotaUnitsUpper, RequestBytesUpper int
	InitialHash, BogusReadbackHash, ValidReadbackHash, StaleReadbackHash           string
	BogusRejected, Cache304, ValidAccepted, StaleRejected, StalePreserved          bool
	Deleted                                                                        bool
	Immediate404, Delayed404                                                       int
	CleanupVerified                                                                bool
	WriteCASObserved                                                               bool
	TokenSourceCalls                                                               int
	ReportWriteErrors                                                              int
	BorrowedConfigRemoved                                                          bool
	CleanupOwnershipVerified, AbsentBeforeCleanup                                  bool
	LateOwnedDeleteAttempted, CleanupError                                         bool
}
type driveETagRequest func(context.Context, string, string, map[string]string, []byte) (*HTTPResult, error)
type driveETagProbe struct {
	do            driveETagRequest
	report        driveETagReport
	parent, space string
	full          bool
	values        map[string]string
	save          func(driveETagReport) error
}

func (p *driveETagProbe) persist() error {
	if p.save != nil {
		return p.save(p.report)
	}
	return nil
}
func (p *driveETagProbe) filePath(full bool) string {
	path := "/drive/v3/files/" + url.PathEscape(p.report.FileID)
	if !full {
		path += "?fields=id,name,description,version"
	}
	return path
}
func (p *driveETagProbe) call(ctx context.Context, phase, method, path string, headers map[string]string, body []byte, cleanup bool) (*HTTPResult, error) {
	limit := 12
	if cleanup {
		limit = 20
	}
	cost := 5
	if method == http.MethodPatch || method == http.MethodPost || method == http.MethodDelete {
		cost = 50
	}
	if p.report.ReservedCalls >= limit || p.report.QuotaUnitsUpper+cost*8 > 8000 || p.report.RequestBytesUpper+8*len(body) > 1<<20 {
		return nil, errors.New("etag_probe_fixed_budget")
	}
	// The fixture can address only its reserved ID, its one create, and ID
	// generation. Never accept an arbitrary existing Drive path.
	valid := false
	if strings.HasPrefix(path, "/drive/v3/files/generateIds?") {
		query := url.Values{"count": []string{"1"}, "fields": []string{"ids"}}
		if p.space == "appDataFolder" {
			query.Set("space", "appDataFolder")
		}
		valid = phase == "generate" && method == http.MethodGet && p.report.FileID == "" && path == "/drive/v3/files/generateIds?"+query.Encode()
	} else if strings.HasPrefix(path, "/drive/v3/files?") {
		valid = phase == "create" && method == http.MethodPost && p.report.FileID != "" && path == "/drive/v3/files?fields=id,name,description,version"
	} else {
		valid = p.report.FileID != "" && (path == p.filePath(false) || path == p.filePath(true)) && (method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete)
	}
	if !valid {
		return nil, errors.New("etag_probe_owned_path_only")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.report.ReservedCalls++
	p.report.AttemptsUpper += 8
	p.report.QuotaUnitsUpper += 8 * cost
	p.report.RequestBytesUpper += 8 * len(body)
	p.report.Calls = append(p.report.Calls, driveETagCall{Phase: phase, Method: method, IfMatch: headers["If-Match"], IfNoneMatch: headers["If-None-Match"]})
	at := len(p.report.Calls) - 1
	if err := p.persist(); err != nil {
		p.report.ReportWriteErrors++
		if !cleanup {
			return nil, err
		}
	}
	requestCtx, cancel := context.WithTimeout(withGoogleResponseLimit(ctx, 1<<20), 10*time.Second)
	defer cancel()
	p.report.VisibleCalls++
	r, err := p.do(requestCtx, method, path, headers, body)
	entry := &p.report.Calls[at]
	entry.Finished = true
	if err != nil {
		entry.Error = errorSummary(err)
	}
	if r != nil {
		entry.Status, entry.Protocol = r.Status, r.Proto
		if tag := r.Header.Get("ETag"); len(tag) <= 512 && !strings.ContainsAny(tag, "\r\n") {
			entry.ETag = tag
		}
		if r.Status == 200 {
			var metadata struct{ Version string }
			if json.Unmarshal(r.Body, &metadata) == nil {
				entry.MetadataVersion = metadata.Version
			}
		}
	}
	if saveErr := p.persist(); saveErr != nil {
		p.report.ReportWriteErrors++
		if !cleanup {
			return r, saveErr
		}
	}
	if err != nil {
		return r, errors.New("etag_probe_transport_failed")
	}
	if r == nil || r.Proto != "HTTP/2.0" {
		return r, errors.New("etag_probe_pinned_http2_required")
	}
	return r, nil
}
func driveETagHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func (p *driveETagProbe) metadata(r *HTTPResult) (string, error) {
	var value struct{ ID, Name, Description string }
	if r == nil || r.Status != 200 || json.Unmarshal(r.Body, &value) != nil || value.ID != p.report.FileID || value.Name != p.report.FileName {
		return "", errors.New("etag_probe_metadata_identity")
	}
	return value.Description, nil
}
func (p *driveETagProbe) get(ctx context.Context, phase string, full bool, headers map[string]string) (*HTTPResult, string, error) {
	if headers == nil {
		headers = make(map[string]string)
	}
	headers["Cache-Control"] = "no-cache"
	r, err := p.call(ctx, phase, http.MethodGet, p.filePath(full), headers, nil, false)
	if err != nil {
		return r, "", err
	}
	value, err := p.metadata(r)
	return r, value, err
}
func (p *driveETagProbe) patch(ctx context.Context, phase, value, tag string) (*HTTPResult, error) {
	body, _ := json.Marshal(map[string]string{"description": value})
	r, err := p.call(ctx, phase, http.MethodPatch, p.filePath(p.full), map[string]string{"Content-Type": "application/json", "If-Match": tag}, body, false)
	if err != nil {
		return r, err
	}
	if r.Status != 200 && r.Status != 412 {
		return r, errors.New("etag_probe_unexpected_patch_status")
	}
	return r, nil
}
func (p *driveETagProbe) run(ctx context.Context) error {
	query := url.Values{"count": []string{"1"}, "fields": []string{"ids"}}
	if p.space == "appDataFolder" {
		query.Set("space", "appDataFolder")
	}
	r, err := p.call(ctx, "generate", http.MethodGet, "/drive/v3/files/generateIds?"+query.Encode(), nil, nil, false)
	if err != nil {
		return err
	}
	var generated struct {
		IDs []string `json:"ids"`
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &generated) != nil || len(generated.IDs) != 1 || len(generated.IDs[0]) < 16 || len(generated.IDs[0]) > 128 || !isSafeObjectSegment(generated.IDs[0]) {
		return errors.New("etag_probe_id_reservation")
	}
	p.report.FileID = generated.IDs[0]
	if err = p.persist(); err != nil {
		return err
	} // Recovery identity exists BEFORE create.
	metadata := map[string]any{"id": p.report.FileID, "name": p.report.FileName, "mimeType": "application/octet-stream", "description": p.values["A"]}
	if p.parent != "" {
		metadata["parents"] = []string{p.parent}
	}
	body, _ := json.Marshal(metadata)
	r, err = p.call(ctx, "create", http.MethodPost, "/drive/v3/files?fields=id,name,description,version", map[string]string{"Content-Type": "application/json"}, body, false)
	if err != nil {
		return err
	}
	if _, err = p.metadata(r); err != nil {
		return err
	}
	r, initial, err := p.get(ctx, "get_projected_A", false, nil)
	if err != nil || initial != p.values["A"] {
		return errors.New("etag_probe_initial_readback")
	}
	tag := r.Header.Get("ETag")
	full := false
	p.report.Projection = "id,name,description,version"
	p.report.InitialHash = driveETagHash(initial)
	if tag == "" {
		r, initial, err = p.get(ctx, "get_full_A", true, nil)
		if err != nil || initial != p.values["A"] {
			return errors.New("etag_probe_full_readback")
		}
		tag = r.Header.Get("ETag")
		full = true
		p.full = true
		p.report.Projection = "full_metadata_header_only"
	}
	if tag != "" {
		r, err = p.call(ctx, "if_none_match_A", http.MethodGet, p.filePath(full), map[string]string{"If-None-Match": tag, "Cache-Control": "no-cache"}, nil, false)
		if err != nil {
			return err
		}
		if r.Status != 200 && r.Status != 304 {
			return errors.New("etag_probe_unexpected_cache_status")
		}
		p.report.Cache304 = r.Status == 304
	}
	bogus := "\"skirk-impossible-" + strings.TrimPrefix(p.report.FileName, "skirk-etag-probe-") + "\""
	r, err = p.patch(ctx, "bogus_if_match", p.values["D"], bogus)
	if err != nil {
		return err
	}
	p.report.BogusRejected = r.Status == 412
	r, current, err := p.get(ctx, "read_after_bogus", full, nil)
	if err != nil {
		return err
	}
	p.report.BogusReadbackHash = driveETagHash(current)
	if p.report.BogusRejected && current != p.values["A"] || !p.report.BogusRejected && current != p.values["D"] {
		return errors.New("etag_probe_bogus_mutation_mismatch")
	}
	// A GET cache ETag is not yet a write fence. It must accept a valid
	// description update, then reject that SAME now-stale value without C.
	tag = r.Header.Get("ETag")
	if tag == "" {
		p.report.Failure = "no_metadata_etag_for_valid_stale_test"
		return p.persist()
	}
	r, err = p.patch(ctx, "valid_if_match_B", p.values["B"], tag)
	if err != nil {
		return err
	}
	p.report.ValidAccepted = r.Status == 200
	_, afterB, err := p.get(ctx, "read_after_valid", full, nil)
	if err != nil {
		return err
	}
	p.report.ValidReadbackHash = driveETagHash(afterB)
	if !p.report.ValidAccepted {
		p.report.Failure = "provider_etag_not_accepted_for_write"
		return p.persist()
	}
	if afterB != p.values["B"] {
		return errors.New("etag_probe_valid_write_mismatch")
	}
	r, err = p.patch(ctx, "stale_if_match_C", p.values["C"], tag)
	if err != nil {
		return err
	}
	p.report.StaleRejected = r.Status == 412
	_, afterC, err := p.get(ctx, "read_after_stale", full, nil)
	if err != nil {
		return err
	}
	p.report.StaleReadbackHash = driveETagHash(afterC)
	p.report.StalePreserved = afterC == p.values["B"]
	if p.report.StaleRejected && !p.report.StalePreserved || !p.report.StaleRejected && afterC != p.values["C"] {
		return errors.New("etag_probe_stale_mutation_mismatch")
	}
	p.report.WriteCASObserved = p.report.BogusRejected && p.report.ValidAccepted && p.report.StaleRejected && p.report.StalePreserved
	return p.persist()
}

func (p *driveETagProbe) cleanup(ctx context.Context, delay time.Duration) error {
	if p.report.FileID == "" {
		return nil
	}
	preflight, preflightErr := p.call(ctx, "cleanup_verify_identity", http.MethodGet, p.filePath(false), map[string]string{"Cache-Control": "no-cache"}, nil, true)
	if preflightErr != nil {
		return preflightErr
	}
	if preflight.Status == 404 {
		p.report.AbsentBeforeCleanup = true
	} else {
		if _, identityErr := p.metadata(preflight); identityErr != nil {
			return identityErr
		}
		p.report.CleanupOwnershipVerified = true
	}
	var r *HTTPResult
	var err error
	if !p.report.AbsentBeforeCleanup {
		r, err = p.call(ctx, "cleanup_delete", http.MethodDelete, p.filePath(true), nil, nil, true)
	}
	var joined error
	if p.report.AbsentBeforeCleanup {
		// No delete is authorized or needed for an already absent ID.
	} else if err != nil {
		joined = errors.Join(joined, err)
	} else if r.Status != 204 && r.Status != 200 && r.Status != 404 {
		joined = errors.Join(joined, errors.New("etag_probe_cleanup_delete"))
	} else {
		p.report.Deleted = true
	}
	retried := false
	for phase := 0; phase < 2; phase++ {
		if phase == 1 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		for i := 0; i < 2; i++ {
			label := "cleanup_immediate"
			if phase == 1 {
				label = "cleanup_delayed"
			}
			r, err = p.call(ctx, label, http.MethodGet, p.filePath(false), map[string]string{"Cache-Control": "no-cache"}, nil, true)
			if err != nil {
				joined = errors.Join(joined, err)
				continue
			}
			if r.Status != 404 {
				if r.Status == 200 && phase == 0 && !retried {
					if _, identityErr := p.metadata(r); identityErr == nil {
						retried = true
						deleted, deleteErr := p.call(ctx, "cleanup_redelete", http.MethodDelete, p.filePath(true), nil, nil, true)
						if deleteErr == nil && deleted.Status != 200 && deleted.Status != 204 && deleted.Status != 404 {
							deleteErr = errors.New("etag_probe_cleanup_redelete_status")
						}
						joined = errors.Join(joined, deleteErr)
						i--
						continue
					}
				}
				if r.Status == 200 && phase == 1 && !p.report.LateOwnedDeleteAttempted {
					if _, identityErr := p.metadata(r); identityErr == nil {
						p.report.LateOwnedDeleteAttempted = true
						deleted, deleteErr := p.call(ctx, "cleanup_late_owned_delete", http.MethodDelete, p.filePath(true), nil, nil, true)
						if deleteErr == nil && deleted.Status != 200 && deleted.Status != 204 && deleted.Status != 404 {
							deleteErr = errors.New("etag_probe_cleanup_late_delete_status")
						}
						joined = errors.Join(joined, deleteErr)
					}
				}
				joined = errors.Join(joined, errors.New("etag_probe_cleanup_not_404"))
				continue
			}
			if phase == 0 {
				p.report.Immediate404++
			} else {
				p.report.Delayed404++
			}
		}
	}
	p.report.CleanupVerified = p.report.Immediate404 == 2 && p.report.Delayed404 == 2
	return errors.Join(joined, p.persist())
}

func driveETagSave(dir string, report driveETagReport) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "etag-probe-result.json")
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = parent.Sync()
	return errors.Join(err, parent.Close())
}

func TestLiveDriveMetadataETagCAS(t *testing.T) {
	dir := os.Getenv("SKIRK_DRIVE_ETAG_PRIVATE_DIR")
	if dir == "" {
		t.Skip("explicit bounded owned Drive metadata ETag research")
	}
	if os.Getenv("SKIRK_DRIVE_ETAG_ACCOUNT") != "isolated-secondary" || !filepath.IsAbs(dir) || !strings.Contains(filepath.Clean(dir), string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) {
		t.Fatal("explicit secondary-account private fixture required")
	}
	configPath := filepath.Join(dir, "config.json")
	for _, path := range []string{dir, configPath} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 || path == dir && !info.IsDir() || path != dir && !info.Mode().IsRegular() {
			t.Fatal("private fixture required")
		}
	}
	defer os.Remove(configPath)
	cfg, err := LoadConfig(configPath)
	if err != nil || cfg.Route.Mode != "google_front_pinned" || cfg.Route.Proxy != "" || net.ParseIP(cfg.Route.GoogleIP) == nil {
		t.Fatal("approved pinned-front profile required")
	}
	marker, err := os.OpenFile(filepath.Join(dir, "probe-started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fresh fixture directory required")
	}
	_, err = marker.WriteString("{\"max_visible_calls\":20,\"max_http_attempts_upper\":160}\n")
	if err == nil {
		err = marker.Sync()
	}
	err = errors.Join(err, marker.Close())
	if err != nil {
		t.Fatal("cannot retain probe guard")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tokens := NewAccessTokenSource(cfg.Auth, cfg.Route)
	token, err := tokens.Token(ctx)
	if err != nil {
		t.Fatal("secondary token acquisition failed")
	}
	client := NewGoogleHTTPClient(cfg.Route)
	defer client.client.CloseIdleConnections()
	nameNonce := make([]byte, 16)
	key := make([]byte, 32)
	if _, err = rand.Read(nameNonce); err != nil {
		t.Fatal("randomness unavailable")
	}
	if _, err = rand.Read(key); err != nil {
		t.Fatal("randomness unavailable")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	p := &driveETagProbe{parent: cfg.Drive.FolderID, space: cfg.Drive.Space, values: make(map[string]string)}
	if p.space == "appDataFolder" {
		p.parent = "appDataFolder"
	}
	p.report = driveETagReport{Schema: 1, Route: cfg.Route.Mode, Front: "www.google.com", AccountBinding: "operator_asserted_isolated_secondary_profile", FileName: "skirk-etag-probe-" + hex.EncodeToString(nameNonce), TokenSourceCalls: 1}
	for _, phase := range []string{"A", "B", "C", "D"} {
		nonce := make([]byte, aead.NonceSize())
		plain := make([]byte, 128)
		if _, err = rand.Read(nonce); err != nil {
			t.Fatal("randomness unavailable")
		}
		if _, err = rand.Read(plain); err != nil {
			t.Fatal("randomness unavailable")
		}
		wire := aead.Seal(nonce, nonce, plain, []byte(p.report.FileName+phase))
		p.values[phase] = base64.RawURLEncoding.EncodeToString(wire)
	}
	p.save = func(report driveETagReport) error { return driveETagSave(dir, report) }
	p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
		selected := map[string]string{"Authorization": "Bearer " + token, "Accept-Encoding": "identity"}
		if cfg.Drive.QuotaProject != "" {
			selected["X-Goog-User-Project"] = cfg.Drive.QuotaProject
		}
		for key, value := range headers {
			selected[key] = value
		}
		return client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, selected, body)
	}
	runErr := p.run(ctx)
	if runErr != nil {
		p.report.Failure = runErr.Error()
	}
	cleanupCtx, stop := context.WithTimeout(context.Background(), 120*time.Second)
	cleanupErr := p.cleanup(cleanupCtx, 31*time.Second)
	stop()
	p.report.CleanupError = cleanupErr != nil
	removeErr := os.Remove(configPath)
	p.report.BorrowedConfigRemoved = removeErr == nil || os.IsNotExist(removeErr)
	_ = p.persist()
	if runErr != nil || cleanupErr != nil || !p.report.BorrowedConfigRemoved {
		t.Fatalf("owned ETag primitive incomplete: probe_error=%t cleanup_error=%t", runErr != nil, cleanupErr != nil)
	}
	t.Logf("ETag primitive complete: write_CAS_observed=%t cache304=%t calls=%d cleanup=%t", p.report.WriteCASObserved, p.report.Cache304, p.report.VisibleCalls, p.report.CleanupVerified)
}
