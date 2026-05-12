package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DriveStore struct {
	http        *GoogleHTTPClient
	tokenSource *AccessTokenSource
	folderID    string
	space       string
	quota       *driveQuotaStats
	Logger      *log.Logger
}

const driveListPageSize = "100"
const driveListMaxPages = 16
const driveSlowRequestThreshold = 4 * time.Second
const defaultDriveQuotaLogInterval = time.Minute

func NewDriveStore(httpClient *GoogleHTTPClient, token string, cfg DriveConfig) *DriveStore {
	return NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: token}, RouteConfig{Mode: "direct"}), cfg)
}

func NewDriveStoreWithTokenSource(httpClient *GoogleHTTPClient, tokenSource *AccessTokenSource, cfg DriveConfig) *DriveStore {
	space := strings.TrimSpace(cfg.Space)
	folderID := strings.TrimSpace(cfg.FolderID)
	if folderID == "appDataFolder" && space == "" {
		space = "appDataFolder"
		folderID = ""
	}
	return &DriveStore{http: httpClient, tokenSource: tokenSource, folderID: folderID, space: space, quota: newDriveQuotaStats(driveQuotaLogInterval())}
}

func (d *DriveStore) Put(ctx context.Context, name string, data []byte) error {
	_, err := d.PutObject(ctx, name, data)
	return err
}

func (d *DriveStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	var body bytes.Buffer
	boundary := fmt.Sprintf("skirk-%d", time.Now().UnixNano())
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(boundary); err != nil {
		return ObjectInfo{}, err
	}
	metadata := map[string]any{
		"name":     name,
		"mimeType": "application/octet-stream",
	}
	if d.isAppData() {
		metadata["parents"] = []string{"appDataFolder"}
	} else if d.folderID != "" {
		metadata["parents"] = []string{d.folderID}
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		return ObjectInfo{}, err
	}
	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return ObjectInfo{}, err
	}
	if _, err := metaPart.Write(metaBytes); err != nil {
		return ObjectInfo{}, err
	}
	dataHeader := textproto.MIMEHeader{}
	dataHeader.Set("Content-Type", "application/octet-stream")
	dataPart, err := writer.CreatePart(dataHeader)
	if err != nil {
		return ObjectInfo{}, err
	}
	if _, err := dataPart.Write(data); err != nil {
		return ObjectInfo{}, err
	}
	if err := writer.Close(); err != nil {
		return ObjectInfo{}, err
	}
	headers := map[string]string{
		"Content-Type": "multipart/related; boundary=" + boundary,
	}
	result, err := d.request(ctx, http.MethodPost, "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", headers, body.Bytes())
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := require2xx(result, "drive upload"); err != nil {
		return ObjectInfo{}, err
	}
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size string `json:"size"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return ObjectInfo{}, err
	}
	size, _ := strconv.ParseInt(payload.Size, 10, 64)
	return ObjectInfo{Name: payload.Name, ID: payload.ID, Size: size}, nil
}

func (d *DriveStore) Get(ctx context.Context, name string) ([]byte, error) {
	info, err := d.latest(ctx, name)
	if err != nil {
		return nil, err
	}
	path := "/drive/v3/files/" + url.PathEscape(info.ID) + "?alt=media"
	result, err := d.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "drive download"); err != nil {
		return nil, err
	}
	return result.Body, nil
}

func (d *DriveStore) GetByID(ctx context.Context, fileID string) ([]byte, error) {
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?alt=media"
	var last *HTTPResult
	for attempt := 0; attempt < 5; attempt++ {
		result, err := d.request(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return nil, err
		}
		last = result
		if result.Status != http.StatusNotFound {
			if err := require2xx(result, "drive download by id"); err != nil {
				return nil, err
			}
			return result.Body, nil
		}
		if attempt == 4 {
			break
		}
		delay := time.Duration(150*(attempt+1)*(attempt+1)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := require2xx(last, "drive download by id"); err != nil {
		return nil, err
	}
	return last.Body, nil
}

func (d *DriveStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	infos, err := d.listContains(ctx, []string{prefix})
	if err != nil {
		return nil, err
	}
	filtered := infos[:0]
	for _, info := range infos {
		if strings.HasPrefix(info.Name, prefix) {
			filtered = append(filtered, info)
		}
	}
	return filtered, nil
}

func (d *DriveStore) ListFresh(ctx context.Context, prefix string, since time.Time) ([]ObjectInfo, error) {
	infos, err := d.listContainsFresh(ctx, prefix, since)
	if err != nil {
		return nil, err
	}
	filtered := infos[:0]
	for _, info := range infos {
		if strings.HasPrefix(info.Name, prefix) {
			filtered = append(filtered, info)
		}
	}
	return filtered, nil
}

func (d *DriveStore) ListContains(ctx context.Context, contains []string) ([]ObjectInfo, error) {
	return d.listContains(ctx, contains)
}

func (d *DriveStore) listContains(ctx context.Context, contains []string) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.containsQuery(contains))
	values.Set("fields", "nextPageToken,files(id,name,size,modifiedTime)")
	values.Set("pageSize", driveListPageSize)
	values.Set("orderBy", "modifiedTime desc")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	err := d.eachFilesPage(ctx, values, "drive list", func(payload driveListPayload) error {
		for _, item := range payload.Files {
			matched := true
			for _, value := range contains {
				if !strings.Contains(item.Name, value) {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (d *DriveStore) listContainsFresh(ctx context.Context, prefix string, since time.Time) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.containsQuery([]string{prefix}))
	values.Set("fields", "nextPageToken,files(id,name,size,modifiedTime)")
	values.Set("pageSize", driveListPageSize)
	values.Set("orderBy", "modifiedTime desc")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	err := d.eachFilesPageUntil(ctx, values, "drive list", func(payload driveListPayload) (bool, error) {
		for _, item := range payload.Files {
			if !strings.Contains(item.Name, prefix) {
				continue
			}
			if !since.IsZero() && item.ModifiedTime != "" {
				updated, err := time.Parse(time.RFC3339Nano, item.ModifiedTime)
				if err == nil && updated.Before(since) {
					return false, nil
				}
			}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (d *DriveStore) Delete(ctx context.Context, name string) error {
	infos, err := d.listExact(ctx, name)
	if err != nil {
		return err
	}
	for _, info := range infos {
		result, err := d.request(ctx, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(info.ID), nil, nil)
		if err != nil {
			return err
		}
		if result.Status != http.StatusNoContent && result.Status != http.StatusOK && result.Status != http.StatusNotFound {
			return require2xx(result, "drive delete")
		}
	}
	return nil
}

func (d *DriveStore) DeleteID(ctx context.Context, fileID string) error {
	result, err := d.request(ctx, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(fileID), nil, nil)
	if err != nil {
		return err
	}
	if result.Status == http.StatusNoContent || result.Status == http.StatusOK || result.Status == http.StatusNotFound {
		return nil
	}
	return require2xx(result, "drive delete id")
}

func (d *DriveStore) DeleteIDs(ctx context.Context, fileIDs []string, concurrency int) error {
	if concurrency < 1 {
		concurrency = 1
	}
	jobs := make(chan string)
	errs := make(chan error, len(fileIDs))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if id == "" {
					continue
				}
				if err := d.DeleteID(ctx, id); err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, id := range fileIDs {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *DriveStore) latest(ctx context.Context, name string) (ObjectInfo, error) {
	infos, err := d.listExact(ctx, name)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(infos) == 0 {
		return ObjectInfo{}, fmt.Errorf("drive object not found: %s", name)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Updated > infos[j].Updated })
	return infos[0], nil
}

func (d *DriveStore) listExact(ctx context.Context, name string) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.query(name, true))
	values.Set("fields", "nextPageToken,files(id,name,size,modifiedTime)")
	values.Set("orderBy", "modifiedTime desc")
	values.Set("pageSize", driveListPageSize)
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	err := d.eachFilesPage(ctx, values, "drive lookup", func(payload driveListPayload) error {
		for _, item := range payload.Files {
			if item.Name != name {
				continue
			}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

type driveListPayload struct {
	NextPageToken string `json:"nextPageToken"`
	Files         []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Size         string `json:"size"`
		ModifiedTime string `json:"modifiedTime"`
	} `json:"files"`
}

func (d *DriveStore) eachFilesPage(ctx context.Context, values url.Values, op string, fn func(driveListPayload) error) error {
	return d.eachFilesPageUntil(ctx, values, op, func(payload driveListPayload) (bool, error) {
		if err := fn(payload); err != nil {
			return false, err
		}
		return true, nil
	})
}

func (d *DriveStore) eachFilesPageUntil(ctx context.Context, values url.Values, op string, fn func(driveListPayload) (bool, error)) error {
	pageValues := cloneValues(values)
	for page := 0; page < driveListMaxPages; page++ {
		result, err := d.request(ctx, http.MethodGet, "/drive/v3/files?"+pageValues.Encode(), nil, nil)
		if err != nil {
			return err
		}
		if err := require2xx(result, op); err != nil {
			return err
		}
		var payload driveListPayload
		if err := json.Unmarshal(result.Body, &payload); err != nil {
			return err
		}
		keepGoing, err := fn(payload)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
		if payload.NextPageToken == "" {
			return nil
		}
		pageValues.Set("pageToken", payload.NextPageToken)
	}
	return nil
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, list := range values {
		out[key] = append([]string(nil), list...)
	}
	return out
}

func (d *DriveStore) request(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	var last *HTTPResult
	label := driveRequestLabel(method, path)
	started := time.Now()
	for attempt := 0; attempt < 2; attempt++ {
		merged, err := d.authHeaders(ctx)
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			merged[key] = value
		}
		result, err := d.http.Request(ctx, method, "www.googleapis.com", path, merged, body)
		if err != nil {
			d.logDriveRequest(label, attempt+1, 0, nil, time.Since(started), err)
			return nil, err
		}
		last = result
		if result.Status != http.StatusUnauthorized {
			d.logDriveRequest(label, attempt+1, result.Status, result.Body, time.Since(started), nil)
			return result, nil
		}
		if d.tokenSource == nil {
			d.logDriveRequest(label, attempt+1, result.Status, result.Body, time.Since(started), nil)
			return result, nil
		}
		if d.Logger != nil {
			d.Logger.Printf("drive auth rejected op=%s status=401 action=refresh-token", label)
		}
		d.tokenSource.Invalidate()
	}
	if last != nil {
		d.logDriveRequest(label, 2, last.Status, last.Body, time.Since(started), nil)
	}
	return last, nil
}

func (d *DriveStore) logDriveRequest(label string, attempts int, status int, body []byte, duration time.Duration, err error) {
	if d.quota != nil {
		if report, ok := d.quota.Record(label, status, len(body), err); ok && d.Logger != nil {
			d.Logger.Printf("drive quota window=%s calls=%d est_units=%d errors=%d response_bytes=%d ops=%s",
				report.Duration.Round(time.Second), report.Calls, report.Units, report.Errors, report.ResponseBytes, report.OpSummary())
		}
	}
	if d.Logger == nil {
		return
	}
	statusText := strconv.Itoa(status)
	if status == 0 {
		statusText = "none"
	}
	duration = duration.Round(time.Millisecond)
	switch {
	case err != nil:
		d.Logger.Printf("drive request failed op=%s attempts=%d status=%s duration=%s error=%v", label, attempts, statusText, duration, err)
	case status == http.StatusUnauthorized:
		d.Logger.Printf("drive request unauthorized op=%s attempts=%d duration=%s reason=%s", label, attempts, duration, driveErrorReason(body))
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		d.Logger.Printf("drive request limited_or_forbidden op=%s attempts=%d status=%d duration=%s reason=%s", label, attempts, status, duration, driveErrorReason(body))
	case status >= 500:
		d.Logger.Printf("drive request server_error op=%s attempts=%d status=%d duration=%s reason=%s", label, attempts, status, duration, driveErrorReason(body))
	case duration >= driveSlowRequestThreshold:
		d.Logger.Printf("drive request slow op=%s attempts=%d status=%d duration=%s", label, attempts, status, duration)
	}
}

type driveQuotaStats struct {
	mu       sync.Mutex
	interval time.Duration
	since    time.Time
	calls    int64
	units    int64
	errors   int64
	bytes    int64
	ops      map[string]driveQuotaOpStats
}

type driveQuotaOpStats struct {
	Calls  int64
	Units  int64
	Errors int64
}

type driveQuotaReport struct {
	Duration      time.Duration
	Calls         int64
	Units         int64
	Errors        int64
	ResponseBytes int64
	Ops           map[string]driveQuotaOpStats
}

func newDriveQuotaStats(interval time.Duration) *driveQuotaStats {
	if interval < 0 {
		interval = 0
	}
	return &driveQuotaStats{
		interval: interval,
		since:    time.Now(),
		ops:      map[string]driveQuotaOpStats{},
	}
}

func (s *driveQuotaStats) Record(op string, status int, responseBytes int, err error) (driveQuotaReport, bool) {
	if s == nil {
		return driveQuotaReport{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.since.IsZero() {
		s.since = time.Now()
	}
	units := int64(driveQuotaUnits(op))
	failed := err != nil || status >= 400
	s.calls++
	s.units += units
	s.bytes += int64(responseBytes)
	if failed {
		s.errors++
	}
	current := s.ops[op]
	current.Calls++
	current.Units += units
	if failed {
		current.Errors++
	}
	s.ops[op] = current
	if s.interval == 0 || time.Since(s.since) < s.interval {
		return driveQuotaReport{}, false
	}
	report := driveQuotaReport{
		Duration:      time.Since(s.since),
		Calls:         s.calls,
		Units:         s.units,
		Errors:        s.errors,
		ResponseBytes: s.bytes,
		Ops:           cloneDriveQuotaOps(s.ops),
	}
	s.since = time.Now()
	s.calls = 0
	s.units = 0
	s.errors = 0
	s.bytes = 0
	clear(s.ops)
	return report, true
}

func cloneDriveQuotaOps(in map[string]driveQuotaOpStats) map[string]driveQuotaOpStats {
	out := make(map[string]driveQuotaOpStats, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (r driveQuotaReport) OpSummary() string {
	if len(r.Ops) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(r.Ops))
	for key := range r.Ops {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		stats := r.Ops[key]
		if stats.Errors > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d/%du/%de", key, stats.Calls, stats.Units, stats.Errors))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d/%du", key, stats.Calls, stats.Units))
	}
	return strings.Join(parts, ",")
}

func driveQuotaLogInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SKIRK_QUOTA_LOG_INTERVAL"))
	if raw == "" {
		return defaultDriveQuotaLogInterval
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false") {
		return 0
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval < time.Second {
		return defaultDriveQuotaLogInterval
	}
	return interval
}

func driveQuotaUnits(op string) int {
	switch op {
	case "list":
		return 100
	case "download":
		return 200
	case "upload", "delete", "create":
		return 50
	default:
		return 5
	}
}

func driveErrorReason(body []byte) string {
	var payload struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if len(payload.Error.Errors) > 0 && payload.Error.Errors[0].Reason != "" {
			return payload.Error.Errors[0].Reason
		}
		if payload.Error.Status != "" {
			return payload.Error.Status
		}
		if payload.Error.Message != "" {
			message := payload.Error.Message
			if len(message) > 80 {
				message = message[:80]
			}
			return message
		}
	}
	return "unknown"
}

func (d *DriveStore) authHeaders(ctx context.Context) (map[string]string, error) {
	if d.tokenSource == nil {
		return nil, errors.New("drive token source is not configured")
	}
	token, err := d.tokenSource.Token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}

func (d *DriveStore) query(value string, exact bool) string {
	clauses := []string{"trashed = false"}
	if d.folderID != "" && !d.isAppData() {
		clauses = append(clauses, fmt.Sprintf("'%s' in parents", escapeDriveQuery(d.folderID)))
	}
	if exact {
		clauses = append(clauses, fmt.Sprintf("name = '%s'", escapeDriveQuery(value)))
	} else {
		clauses = append(clauses, fmt.Sprintf("name contains '%s'", escapeDriveQuery(value)))
	}
	return strings.Join(clauses, " and ")
}

func (d *DriveStore) containsQuery(values []string) string {
	clauses := []string{"trashed = false"}
	if d.folderID != "" && !d.isAppData() {
		clauses = append(clauses, fmt.Sprintf("'%s' in parents", escapeDriveQuery(d.folderID)))
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		clauses = append(clauses, fmt.Sprintf("name contains '%s'", escapeDriveQuery(value)))
	}
	return strings.Join(clauses, " and ")
}

func (d *DriveStore) isAppData() bool {
	return d.space == "appDataFolder"
}

func driveRequestLabel(method, path string) string {
	switch {
	case strings.HasPrefix(path, "/upload/drive/v3/files"):
		return "upload"
	case method == http.MethodGet && strings.Contains(path, "alt=media"):
		return "download"
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files?"):
		return "list"
	case method == http.MethodDelete && strings.HasPrefix(path, "/drive/v3/files/"):
		return "delete"
	case method == http.MethodPost && strings.HasPrefix(path, "/drive/v3/files"):
		return "create"
	default:
		return strings.ToLower(method)
	}
}

func escapeDriveQuery(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "'", "\\'")
	return value
}
