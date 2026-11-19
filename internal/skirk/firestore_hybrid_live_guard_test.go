package skirk

// All code in this file is research-only and is excluded from product builds.
import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fsHybridLiveDuration          = 5 * time.Minute
	fsHybridLiveDriveCalls        = int64(1000)
	fsHybridLiveDriveUnits        = int64(800000)
	fsHybridLiveDriveMinuteUnits  = int64(280000)
	fsHybridLiveDriveRequestUpper = int64(1 << 30)
	fsHybridLiveDriveResponse     = int64(256 << 20)
	fsHybridLiveFSCalls           = int64(512)
	fsHybridLiveFSRequestUpper    = int64(128 << 20)
	fsHybridLiveFSResponse        = int64(128 << 20)
	fsHybridLiveTransientLimit    = int64(8)
	fsHybridLiveTransientIDs      = 8
	fsHybridLiveProofReads        = int64(3)
)

type fsHybridLiveSettings struct {
	Mode                      string `json:"mode"`
	Workload                  string `json:"workload,omitempty"`
	PriorityStreamWindow      int    `json:"priority_stream_window,omitempty"`
	PrioritySelectiveReceipts bool   `json:"priority_selective_receipts,omitempty"`
	SessionID                 string `json:"session_id"`
	Epoch                     string `json:"epoch"`
	Secret                    string `json:"secret"`
	ClientID                  string `json:"client_id"`
	RunID                     string `json:"run_id"`
	PayloadSeedHex            string `json:"payload_seed_hex"`
	Database                  string `json:"database"`
	UpDocument                string `json:"up_document"`
	DownDocument              string `json:"down_document"`
	ClientConfig              string `json:"client_config"`
	ExitConfig                string `json:"exit_config"`
	OwnerTokenFile            string `json:"owner_token_file"`
	ClientTokenFile           string `json:"client_token_file"`
	ExitTokenFile             string `json:"exit_token_file"`
	TokensValidUntil          string `json:"tokens_valid_until"`
	CampaignLedger            string `json:"campaign_ledger"`
	ArmID                     string `json:"arm_id"`
}

func fsHybridPrivateRead(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > max {
		return nil, errFSHybridBounds
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Clean(path) {
		return nil, errFSHybridBounds
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) > max {
		return nil, errFSHybridBounds
	}
	return data, nil
}
func fsHybridLiveFile(dir, name string) (string, error) {
	if filepath.IsAbs(name) || name == "" || filepath.Clean(name) != name || strings.Contains(name, string(filepath.Separator)) || name == "." || name == ".." {
		return "", errFSHybridBounds
	}
	return filepath.Join(dir, name), nil
}
func fsHybridLoadLiveSettings(dir string) (fsHybridLiveSettings, error) {
	var s fsHybridLiveSettings
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !filepath.IsAbs(dir) || !strings.Contains(filepath.Clean(dir)+string(filepath.Separator), string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) {
		return s, errFSHybridBounds
	}
	raw, err := fsHybridPrivateRead(filepath.Join(dir, "settings.json"), 32768)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&s) != nil || dec.Decode(new(any)) != io.EOF {
		return s, errFSHybridBounds
	}
	if s.Mode != "hybrid" && s.Mode != "drive" && s.Mode != "drive_v2" {
		return s, errFSHybridBounds
	}
	if s.Workload == "" {
		s.Workload = "http_serial"
	}
	if s.Workload != "http_serial" && s.Workload != "tls_pipeline" {
		return s, errFSHybridBounds
	}
	if s.Mode == "drive_v2" && (s.Database != "" || s.UpDocument != "" || s.DownDocument != "" || s.OwnerTokenFile != "") {
		return s, errFSHybridBounds
	}
	if s.Mode == "drive_v2" {
		if s.PriorityStreamWindow != 0 && s.PriorityStreamWindow != 4 && s.PriorityStreamWindow != 8 {
			return s, errFSHybridBounds
		}
	} else if s.PriorityStreamWindow != 0 || s.PrioritySelectiveReceipts {
		return s, errFSHybridBounds
	}
	if len(s.ArmID) < 3 || len(s.ArmID) > 64 || !isSafeObjectSegment(s.ArmID) || !filepath.IsAbs(s.CampaignLedger) || !strings.Contains(filepath.Clean(s.CampaignLedger), string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) {
		return s, errFSHybridBounds
	}
	sid, e1 := hex.DecodeString(s.SessionID)
	epoch, e2 := hex.DecodeString(s.Epoch)
	seed, e3 := hex.DecodeString(s.PayloadSeedHex)
	validUntil, e4 := time.Parse(time.RFC3339Nano, s.TokensValidUntil)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || len(sid) != 16 || len(epoch) != 16 || len(seed) != 32 || bytes.Equal(sid, make([]byte, 16)) || bytes.Equal(epoch, make([]byte, 16)) || len(s.Secret) < 32 || len(s.Secret) > 256 || s.ClientID == "" || s.RunID == "" || len(s.ClientID) > 64 || len(s.RunID) > 96 || !isSafeObjectSegment(s.ClientID) || !isSafeObjectSegment(s.RunID) || !validUntil.After(time.Now().Add(10*time.Minute)) {
		return s, errFSHybridBounds
	}
	for _, name := range []string{s.ClientConfig, s.ExitConfig, s.ClientTokenFile, s.ExitTokenFile} {
		if _, err = fsHybridLiveFile(dir, name); err != nil {
			return s, err
		}
	}
	if s.Mode == "hybrid" {
		if !fsHybridDatabasePattern.MatchString(s.Database) || s.UpDocument == s.DownDocument {
			return s, errFSHybridBounds
		}
		for _, doc := range []string{s.UpDocument, s.DownDocument} {
			if !strings.HasPrefix(doc, s.Database) || !fsHybridDocSuffixPattern.MatchString(strings.TrimPrefix(doc, s.Database)) {
				return s, errFSHybridBounds
			}
		}
		if _, err = fsHybridLiveFile(dir, s.OwnerTokenFile); err != nil {
			return s, err
		}
	}
	return s, nil
}
func fsHybridLiveToken(dir, name string) (string, error) {
	path, err := fsHybridLiveFile(dir, name)
	if err != nil {
		return "", err
	}
	data, err := fsHybridPrivateRead(path, 16384)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) == 0 {
		return "", errFSHybridBounds
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return "", errFSHybridBounds
		}
	}
	return token, nil
}

// The live guard is shared across the client's and exit's distinct same-route
// pools. Every visible dispatch reserves eight opaque HTTP/2 attempts before
// exposure. A separate response-byte admission covers concurrent body readers.
type fsHybridLiveBudget struct {
	mu                                                                                                                   sync.Mutex
	Service                                                                                                              string
	Calls, Attempts, Units, RequestUpper, Response, PendingResponse                                                      int64
	CleanupCalls, Canceled, Timeouts, APIError, ExpectedPreconditions, ExpectedNotFound, TransportErrors, HTTP2, Denials int64
	MaxCalls, MaxUnits, MaxMinuteUnits, MaxRequest, MaxResponse                                                          int64
	MinuteUnits                                                                                                          int64
	MinuteReservations                                                                                                   []fsHybridLiveMinuteReservation
	TimeoutTimes                                                                                                         []time.Time
	Statuses                                                                                                             map[int]int64
	Events                                                                                                               []map[string]any
	RecoveryEvents                                                                                                       []map[string]any
	LastDenial                                                                                                           map[string]any
	AllowGeneratedUploadRecovery                                                                                         bool
	UploadTransientRaw, UploadConflictRaw, UploadProofReadbacks, UploadProvenIDs                                         int64
	uploadProofs                                                                                                         map[string]*fsHybridLiveUploadProof
	cancel                                                                                                               context.CancelFunc
	onTimeout                                                                                                            func() error
}

type fsHybridLiveUploadProof struct {
	name   string
	size   int64
	digest string
	proven bool
	reads  int64
}

type fsHybridLiveMinuteReservation struct {
	At    time.Time
	Units int64
}

func fsHybridLiveNewBudget(service string, cancel context.CancelFunc) *fsHybridLiveBudget {
	b := &fsHybridLiveBudget{Service: service, cancel: cancel, Statuses: make(map[int]int64)}
	if service == "drive" {
		b.MaxCalls = fsHybridLiveDriveCalls
		b.MaxUnits = fsHybridLiveDriveUnits
		b.MaxMinuteUnits = fsHybridLiveDriveMinuteUnits
		b.MaxRequest = fsHybridLiveDriveRequestUpper
		b.MaxResponse = fsHybridLiveDriveResponse
	} else {
		b.MaxCalls = fsHybridLiveFSCalls
		b.MaxRequest = fsHybridLiveFSRequestUpper
		b.MaxResponse = fsHybridLiveFSResponse
	}
	return b
}
func (b *fsHybridLiveBudget) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	statuses := make(map[int]int64, len(b.Statuses))
	for k, v := range b.Statuses {
		statuses[k] = v
	}
	events := append([]map[string]any(nil), b.Events...)
	recoveryEvents := append([]map[string]any(nil), b.RecoveryEvents...)
	last := make(map[string]any, len(b.LastDenial))
	for k, v := range b.LastDenial {
		last[k] = v
	}
	unproven := 0
	for _, proof := range b.uploadProofs {
		if !proof.proven {
			unproven++
		}
	}
	return map[string]any{"service": b.Service, "visible_calls": b.Calls, "attempts_upper": b.Attempts, "units_upper": b.Units, "minute_units_upper": b.MinuteUnits, "minute_limit_upper": b.MaxMinuteUnits, "request_bytes_upper": b.RequestUpper, "response_bytes": b.Response, "pending_response_reservation": b.PendingResponse, "cleanup_calls": b.CleanupCalls, "canceled_calls": b.Canceled, "timeouts": b.Timeouts, "api_errors": b.APIError, "expected_preconditions": b.ExpectedPreconditions, "expected_cleanup_404": b.ExpectedNotFound, "transport_errors": b.TransportErrors, "http2_responses": b.HTTP2, "guard_denials": b.Denials, "statuses": statuses, "failures": events, "last_guard_denial": last, "generated_upload_transient_statuses": b.UploadTransientRaw, "generated_upload_conflict_statuses": b.UploadConflictRaw, "generated_upload_proof_reads": b.UploadProofReadbacks, "generated_upload_proven_ids": b.UploadProvenIDs, "generated_upload_unproven_ids": unproven, "generated_upload_recovery_events": recoveryEvents}
}
func (b *fsHybridLiveBudget) addEventLocked(kind string, status int) {
	if len(b.Events) < 128 {
		b.Events = append(b.Events, map[string]any{"kind": kind, "status": status, "at_utc": time.Now().UTC().Format(time.RFC3339Nano)})
	}
}
func (b *fsHybridLiveBudget) addRecoveryEventLocked(kind string, status int) {
	if len(b.RecoveryEvents) < 32 {
		b.RecoveryEvents = append(b.RecoveryEvents, map[string]any{"kind": kind, "status": status, "at_utc": time.Now().UTC().Format(time.RFC3339Nano)})
	}
}
func (b *fsHybridLiveBudget) denyLocked(kind string) {
	b.Denials++
	b.addEventLocked(kind, 0)
	if b.cancel != nil {
		b.cancel()
	}
}
func (b *fsHybridLiveBudget) reserve(r *http.Request, cleanup bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	host := "www.googleapis.com"
	if b.Service == "firestore" {
		host = "firestore.googleapis.com"
	}
	if r.URL.Scheme != "https" || r.URL.Host != "www.google.com" || r.Host != host || r.ContentLength < 0 || r.ContentLength > 8<<20 {
		b.denyLocked("route_or_body_shape")
		return errFSHybridBounds
	}
	calls, units, request, response := b.MaxCalls, b.MaxUnits, b.MaxRequest, b.MaxResponse
	if !cleanup {
		calls -= 32
		request -= 1 << 20
		response -= 4 << 20
		if units > 0 {
			units -= 40000
		}
	}
	cost := int64(0)
	if b.Service == "drive" {
		cost = 5
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files") {
			if r.URL.Query().Get("alt") == "media" {
				cost = 200
			} else if strings.HasSuffix(r.URL.Path, "/files") {
				cost = 100
			}
		} else if r.Method != http.MethodGet {
			cost = 50
		}
	}
	minuteLimit := b.MaxMinuteUnits
	if minuteLimit > 0 {
		// The provider's per-user quota is a one-minute window. Preserve a
		// separate cleanup reserve in that same window, while keeping the
		// finite whole-arm budget and its durable attempt upper intact.
		cutoff := time.Now().Add(-time.Minute)
		retained := b.MinuteReservations[:0]
		b.MinuteUnits = 0
		for _, reserved := range b.MinuteReservations {
			if reserved.At.After(cutoff) {
				retained = append(retained, reserved)
				b.MinuteUnits += reserved.Units
			}
		}
		b.MinuteReservations = retained
		if !cleanup {
			minuteLimit -= 40000
		}
	}
	if b.Calls+1 > calls || b.Units+cost*8 > units && units > 0 || b.MaxMinuteUnits > 0 && b.MinuteUnits+cost*8 > minuteLimit || r.ContentLength > (request-b.RequestUpper)/8 || b.Response+b.PendingResponse >= response {
		bound := "response_bytes"
		switch {
		case b.Calls+1 > calls:
			bound = "calls"
		case units > 0 && b.Units+cost*8 > units:
			bound = "arm_units"
		case b.MaxMinuteUnits > 0 && b.MinuteUnits+cost*8 > minuteLimit:
			bound = "minute_units"
		case r.ContentLength > (request-b.RequestUpper)/8:
			bound = "request_bytes"
		}
		beforeEvents := len(b.Events)
		b.denyLocked("aggregate_call_unit_or_byte_limit")
		b.LastDenial = make(map[string]any)
		if n := len(b.Events); n > beforeEvents {
			b.LastDenial = b.Events[n-1]
		}
		{
			event := b.LastDenial
			event["bound"] = bound
			event["cleanup"] = cleanup
			event["next_units_upper"] = cost * 8
			event["current_calls"], event["effective_calls_limit"] = b.Calls, calls
			event["current_arm_units"], event["effective_arm_units_limit"] = b.Units, units
			event["current_minute_units"], event["effective_minute_units_limit"] = b.MinuteUnits, minuteLimit
			event["request_bytes_headroom"] = request - b.RequestUpper
			event["response_bytes_headroom"] = response - b.Response - b.PendingResponse
		}
		return errFSHybridBounds
	}
	b.Calls++
	b.Attempts += 8
	b.Units += cost * 8
	if b.MaxMinuteUnits > 0 {
		reserved := fsHybridLiveMinuteReservation{At: time.Now(), Units: cost * 8}
		b.MinuteReservations = append(b.MinuteReservations, reserved)
		b.MinuteUnits += reserved.Units
	}
	b.RequestUpper += r.ContentLength * 8
	if cleanup {
		b.CleanupCalls++
	}
	return nil
}
func fsHybridLiveTimeout(err error) bool {
	var n net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &n) && n.Timeout()
}
func (b *fsHybridLiveBudget) transportError(err error) {
	if err == nil || err == io.EOF {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if errors.Is(err, context.Canceled) {
		b.Canceled++
		return
	}
	if fsHybridLiveTimeout(err) {
		b.Timeouts++
		now := time.Now()
		cut := now.Add(-time.Minute)
		var retained []time.Time
		for _, v := range b.TimeoutTimes {
			if v.After(cut) {
				retained = append(retained, v)
			}
		}
		b.TimeoutTimes = append(retained, now)
		b.addEventLocked("transport_timeout", 0)
		if b.onTimeout != nil {
			if failure := b.onTimeout(); failure != nil {
				b.denyLocked("cohort_timeout_observation_limit")
			}
		}
		if b.Timeouts >= 32 || len(b.TimeoutTimes) >= 16 {
			b.denyLocked("timeout_observation_limit")
		}
		return
	}
	b.TransportErrors++
	b.addEventLocked("unclassified_transport_error", 0)
	if b.cancel != nil {
		b.cancel()
	}
}

func fsHybridLiveGeneratedUpload(r *http.Request) (string, fsHybridLiveUploadProof, error) {
	if r.Method != http.MethodPost || r.URL.Path != "/upload/drive/v3/files" || r.URL.Query().Get("uploadType") != "multipart" || r.GetBody == nil || r.ContentLength <= 0 || r.ContentLength > 8<<20 {
		return "", fsHybridLiveUploadProof{}, errFSHybridBounds
	}
	body, err := r.GetBody()
	if err != nil {
		return "", fsHybridLiveUploadProof{}, err
	}
	defer body.Close()
	copyRequest := r.Clone(r.Context())
	copyRequest.Body = io.NopCloser(io.LimitReader(body, r.ContentLength+1))
	upload, err := captureDriveUpload(copyRequest)
	if err != nil || !isSafeObjectSegment(upload.id) || !isNormalGeneratedMuxUpload(upload.id, upload.name, len(upload.data)) {
		return "", fsHybridLiveUploadProof{}, errFSHybridBounds
	}
	digest := sha256.Sum256(upload.data)
	return upload.id, fsHybridLiveUploadProof{name: upload.name, size: int64(len(upload.data)), digest: hex.EncodeToString(digest[:])}, nil
}

func fsHybridLiveProofJSON(contentEncoding string, body []byte) (id, name string, size int64, digest string, err error) {
	if strings.EqualFold(contentEncoding, "gzip") {
		reader, e := gzip.NewReader(bytes.NewReader(body))
		if e != nil {
			return "", "", 0, "", e
		}
		defer reader.Close()
		body, err = io.ReadAll(io.LimitReader(reader, 8193))
		if err != nil || len(body) > 8192 {
			return "", "", 0, "", errFSHybridBounds
		}
	} else if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return "", "", 0, "", errFSHybridBounds
	}
	var item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Size   string `json:"size"`
		SHA256 string `json:"sha256Checksum"`
	}
	if err := json.Unmarshal(body, &item); err != nil {
		return "", "", 0, "", err
	}
	size, err = strconv.ParseInt(item.Size, 10, 64)
	return item.ID, item.Name, size, item.SHA256, err
}

// A raw retryable status is provisional only for a generated-ID bulk upload.
// The budget separately proves the same ID/name/size/content via the product's
// checksum readback and fails the arm if a provisional status remains unproved.
func (b *fsHybridLiveBudget) generatedUploadStatusLocked(r *http.Request, response *http.Response, contentEncoding string, body []byte) bool {
	if !b.AllowGeneratedUploadRecovery || b.Service != "drive" {
		return false
	}
	status := response.StatusCode
	if r.Method == http.MethodPost && r.URL.Path == "/upload/drive/v3/files" && (status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || status == http.StatusConflict) {
		id, expected, err := fsHybridLiveGeneratedUpload(r)
		if err != nil || b.UploadTransientRaw+b.UploadConflictRaw >= fsHybridLiveTransientLimit {
			b.denyLocked("unbounded_or_unidentifiable_generated_upload_error")
			return true
		}
		if b.uploadProofs == nil {
			b.uploadProofs = make(map[string]*fsHybridLiveUploadProof)
		}
		proof := b.uploadProofs[id]
		if proof == nil {
			if len(b.uploadProofs) >= fsHybridLiveTransientIDs {
				b.denyLocked("generated_upload_error_id_limit")
				return true
			}
			proof = &expected
			b.uploadProofs[id] = proof
		} else if proof.name != expected.name || proof.size != expected.size || proof.digest != expected.digest {
			b.denyLocked("generated_upload_retry_identity_or_content_changed")
			return true
		}
		if status == http.StatusConflict {
			b.UploadConflictRaw++
		} else {
			b.UploadTransientRaw++
		}
		b.addRecoveryEventLocked("generated_upload_status_pending_exact_proof", status)
		return true
	}
	if r.Method != http.MethodGet || r.Context().Value(driveUploadCommitProbeKey{}) != true || !strings.HasPrefix(r.URL.Path, "/drive/v3/files/") || r.URL.Query().Get("fields") != "id,name,size,sha256Checksum" {
		return false
	}
	id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
	proof := b.uploadProofs[id]
	if proof == nil {
		return false
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return false
	}
	proof.reads++
	b.UploadProofReadbacks++
	if proof.reads > fsHybridLiveProofReads {
		b.denyLocked("generated_upload_proof_read_limit")
		return true
	}
	if status == http.StatusNotFound {
		b.addRecoveryEventLocked("generated_upload_proof_not_yet_visible", status)
		return true
	}
	gotID, name, size, digest, err := fsHybridLiveProofJSON(contentEncoding, body)
	if err != nil || gotID != id || name != proof.name || size != proof.size || digest != "" && !strings.EqualFold(digest, proof.digest) {
		b.denyLocked("generated_upload_exact_proof_mismatch")
		return true
	}
	if digest == "" {
		b.addRecoveryEventLocked("generated_upload_checksum_not_yet_visible", status)
		return true
	}
	if !proof.proven {
		proof.proven = true
		b.UploadProvenIDs++
		b.addRecoveryEventLocked("generated_upload_exact_content_proven", status)
	}
	return true
}

func (b *fsHybridLiveBudget) finalizeGeneratedUploadRecovery() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, proof := range b.uploadProofs {
		if !proof.proven {
			b.addEventLocked("generated_upload_unproven_at_workload_end", 0)
			return fmt.Errorf("generated upload transient status lacks exact commit proof: %w", errFSHybridIntegrity)
		}
	}
	return nil
}

func (b *fsHybridLiveBudget) responseDone(r *http.Request, response *http.Response, contentEncoding string, body []byte, cleanup bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Statuses[response.StatusCode]++
	if response.Proto == "HTTP/2.0" {
		b.HTTP2++
	} else {
		b.addEventLocked("unexpected_http_protocol", response.StatusCode)
		if b.cancel != nil {
			b.cancel()
		}
	}
	if response.StatusCode >= 400 {
		b.APIError++
	}
	if cleanup && r.Method == http.MethodGet && response.StatusCode == 404 {
		b.ExpectedNotFound++
		return
	}
	if b.generatedUploadStatusLocked(r, response, contentEncoding, body) {
		return
	}
	if response.StatusCode < 400 {
		return
	}
	reason, _ := googleAPIErrorDetails(body)
	if b.Service == "firestore" && r.Method == http.MethodPatch && r.URL.Query().Get("currentDocument.updateTime") != "" && (response.StatusCode == 412 || (response.StatusCode == 400 || response.StatusCode == 409) && reason == "FAILED_PRECONDITION") {
		b.ExpectedPreconditions++
		b.addEventLocked("conditional_write_precondition", response.StatusCode)
		return
	}
	if marker, ok := r.Context().Value(driveV2CASAttemptKey{}).(string); ok && b.Service == "drive" && response.StatusCode == 412 && r.Method == http.MethodPatch && marker != "" && r.URL.Path == "/drive/v2/files/"+marker && r.Header.Get("If-Match") != "" {
		b.ExpectedPreconditions++
		b.addEventLocked("drive_v2_conditional_write_precondition", response.StatusCode)
		return
	}
	beforeEvents := len(b.Events)
	b.addEventLocked("unexpected_api_status", response.StatusCode)
	if len(b.Events) > beforeEvents {
		event := b.Events[len(b.Events)-1]
		event["method"] = r.Method
		event["owned_cleanup"] = cleanup
		event["google_reason"] = reason
		event["request_canceled"] = r.Context().Err() != nil
		switch {
		case strings.Contains(r.URL.Path, "/google.firestore.v1.Firestore/Listen/channel"):
			event["route_kind"] = "listen_channel"
		case strings.Contains(r.URL.Path, "/documents/"):
			event["route_kind"] = "document"
		default:
			event["route_kind"] = "other"
		}
		event["webchannel_terminate"] = r.URL.Query().Get("TYPE") == "terminate"
	}
	if b.cancel != nil {
		b.cancel()
	}
}

type fsHybridLiveGate struct {
	mu      sync.RWMutex
	stopped bool
	base    http.RoundTripper
	budget  *fsHybridLiveBudget
	cleanup bool
}

// Research-only in-process marker for carrier-owned REMOVE/terminate calls.
// It is never an HTTP header or a value supplied by the SDK or application.
type fsHybridLiveCleanupKey struct{}

func (g *fsHybridLiveGate) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.RLock()
	if g.stopped {
		g.mu.RUnlock()
		return nil, context.Canceled
	}
	cleanup := g.cleanup || r.Context().Value(fsHybridLiveCleanupKey{}) == true
	if err := g.budget.reserve(r, cleanup); err != nil {
		g.mu.RUnlock()
		return nil, err
	}
	resp, err := g.base.RoundTrip(r)
	if err == nil && resp == nil {
		err = errFSHybridIntegrity
	}
	if err != nil || resp == nil || resp.Body == nil {
		g.budget.transportError(err)
		g.mu.RUnlock()
		return resp, err
	}
	if g.budget.Service == "firestore" && resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		_ = resp.Body.Close()
		g.budget.mu.Lock()
		g.budget.denyLocked("unexpected_firestore_response_encoding")
		g.budget.mu.Unlock()
		g.mu.RUnlock()
		return nil, errFSHybridBounds
	}
	resp.Body = &fsHybridLiveBody{ReadCloser: resp.Body, gate: g, request: r, response: resp, contentEncoding: resp.Header.Get("Content-Encoding"), cleanup: cleanup}
	return resp, nil
}
func (g *fsHybridLiveGate) stop() { g.mu.Lock(); g.stopped = true; g.mu.Unlock() }
func (g *fsHybridLiveGate) CloseIdleConnections() {
	if base, ok := g.base.(interface{ CloseIdleConnections() }); ok {
		base.CloseIdleConnections()
	}
}

type fsHybridLiveBody struct {
	io.ReadCloser
	gate            *fsHybridLiveGate
	request         *http.Request
	response        *http.Response
	contentEncoding string
	once            sync.Once
	errorOnce       sync.Once
	captured        []byte
	cleanup         bool
}

func (b *fsHybridLiveBody) recordError(err error) {
	if err != nil && err != io.EOF {
		b.errorOnce.Do(func() { b.gate.budget.transportError(err) })
	}
}

func (b *fsHybridLiveBody) finish() {
	b.once.Do(func() {
		b.gate.budget.responseDone(b.request, b.response, b.contentEncoding, b.captured, b.cleanup)
		b.gate.mu.RUnlock()
	})
}
func (b *fsHybridLiveBody) Read(p []byte) (int, error) {
	budget := b.gate.budget
	budget.mu.Lock()
	limit := budget.MaxResponse
	if !b.cleanup {
		limit -= 4 << 20
	}
	available := limit - budget.Response - budget.PendingResponse
	if available <= 0 {
		budget.denyLocked("aggregate_response_limit")
		budget.mu.Unlock()
		b.finish()
		return 0, errFSHybridBounds
	}
	if int64(len(p)) > available {
		p = p[:available]
	}
	budget.PendingResponse += int64(len(p))
	budget.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	budget.mu.Lock()
	budget.PendingResponse -= int64(len(p))
	budget.Response += int64(n)
	budget.mu.Unlock()
	if len(b.captured) < 8192 {
		take := min(n, 8192-len(b.captured))
		b.captured = append(b.captured, p[:take]...)
	}
	if err != nil {
		b.recordError(err)
		b.finish()
	}
	return n, err
}
func (b *fsHybridLiveBody) Close() error {
	err := b.ReadCloser.Close()
	b.recordError(err)
	b.finish()
	return err
}

func fsHybridLiveAttach(client *GoogleHTTPClient, budget *fsHybridLiveBudget, cleanup bool) *fsHybridLiveGate {
	gate := &fsHybridLiveGate{base: client.client.Transport, budget: budget, cleanup: cleanup}
	client.client.Transport = gate
	return gate
}
func fsHybridLiveGateDrive(store *DriveStore, budget *fsHybridLiveBudget, cleanup bool) []*fsHybridLiveGate {
	seen := make(map[*GoogleHTTPClient]bool)
	var gates []*fsHybridLiveGate
	for _, client := range []*GoogleHTTPClient{store.http, store.normalUploadHTTP, store.normalDownloadHTTP} {
		if client != nil && !seen[client] {
			seen[client] = true
			gates = append(gates, fsHybridLiveAttach(client, budget, cleanup))
		}
	}
	return gates
}
