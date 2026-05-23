package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
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
	backoff     *driveQuotaBackoff
	Logger      *log.Logger
}

const driveListPageSize = "100"
const driveListMaxPages = 16
const driveMuxFreshListMaxPages = 4
const defaultDriveCleanupMaxPages = 256
const driveQuotaMaxSamplesPerOp = 4096
const driveSlowRequestThreshold = 2 * time.Second
const defaultDriveQuotaLogInterval = time.Minute
const driveFolderMimeType = "application/vnd.google-apps.folder"
const driveQuotaBackoffBase = 5 * time.Second
const driveQuotaBackoffMax = 64 * time.Second
const driveQuotaBackoffJitter = time.Second
const driveQuotaBackoffLogInterval = 5 * time.Second

type DriveCleanupOptions struct {
	Prefix            string
	All               bool
	OlderThan         time.Duration
	Now               time.Time
	DryRun            bool
	DeleteConcurrency int
	MaxDeletes        int
	DeleteDelay       time.Duration
	MaxPages          int
}

type DriveCleanupResult struct {
	Prefix      string    `json:"prefix"`
	Cutoff      time.Time `json:"cutoff"`
	DryRun      bool      `json:"dry_run"`
	Scanned     int       `json:"scanned"`
	Matched     int       `json:"matched"`
	Deleted     int       `json:"deleted"`
	Failed      int       `json:"failed"`
	MatchedSize int64     `json:"matched_size"`
	Pages       int       `json:"pages,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
	Errors      []string  `json:"errors,omitempty"`
}

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
	return &DriveStore{http: httpClient, tokenSource: tokenSource, folderID: folderID, space: space, quota: newDriveQuotaStats(driveQuotaLogInterval()), backoff: newDriveQuotaBackoff()}
}

func (d *DriveStore) Put(ctx context.Context, name string, data []byte) error {
	_, err := d.PutObject(ctx, name, data)
	return err
}

func (d *DriveStore) GenerateObjectIDs(ctx context.Context, count int) ([]string, error) {
	if count < 1 || count > 1000 {
		return nil, fmt.Errorf("drive generate ids count must be between 1 and 1000")
	}
	values := url.Values{}
	values.Set("count", strconv.Itoa(count))
	values.Set("fields", "ids")
	if d.isAppData() {
		values.Set("space", "appDataFolder")
	}
	result, err := d.request(ctx, http.MethodGet, "/drive/v3/files/generateIds?"+values.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "drive generate ids"); err != nil {
		return nil, err
	}
	var payload struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.IDs))
	for _, id := range payload.IDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("drive generate ids response missing ids")
	}
	return ids, nil
}

func (d *DriveStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	return d.putObject(ctx, "", name, data)
}

func (d *DriveStore) PutObjectWithID(ctx context.Context, fileID, name string, data []byte) (ObjectInfo, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return ObjectInfo{}, errors.New("drive upload with id requires file id")
	}
	return d.putObject(ctx, fileID, name, data)
}

func (d *DriveStore) putObject(ctx context.Context, fileID, name string, data []byte) (ObjectInfo, error) {
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
	if fileID != "" {
		metadata["id"] = fileID
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
	if result.Status == http.StatusConflict && fileID != "" {
		info, err := d.getObjectInfoByID(ctx, fileID)
		if err != nil {
			return ObjectInfo{}, err
		}
		if info.Name != "" && info.Name != name {
			return ObjectInfo{}, fmt.Errorf("drive upload id conflict name=%q want=%q", info.Name, name)
		}
		if info.Size != int64(len(data)) {
			return ObjectInfo{}, fmt.Errorf("drive upload id conflict size=%d want=%d", info.Size, len(data))
		}
		return info, nil
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

func (d *DriveStore) EnsureFolder(ctx context.Context, name string) (ObjectInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ObjectInfo{}, errors.New("drive folder name is required")
	}
	if d.isAppData() {
		return ObjectInfo{}, errors.New("drive visible folder creation is not available in appDataFolder mode")
	}
	values := url.Values{}
	values.Set("pageSize", "10")
	values.Set("fields", "files(id,name)")
	values.Set("q", "trashed = false and mimeType = '"+driveFolderMimeType+"' and name = '"+escapeDriveQuery(name)+"'")
	result, err := d.request(ctx, http.MethodGet, "/drive/v3/files?"+values.Encode(), nil, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := require2xx(result, "drive folder lookup"); err != nil {
		return ObjectInfo{}, err
	}
	var listPayload struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Body, &listPayload); err != nil {
		return ObjectInfo{}, err
	}
	for _, file := range listPayload.Files {
		if strings.TrimSpace(file.ID) != "" {
			return ObjectInfo{ID: file.ID, Name: file.Name}, nil
		}
	}

	body, err := json.Marshal(map[string]string{
		"name":     name,
		"mimeType": driveFolderMimeType,
	})
	if err != nil {
		return ObjectInfo{}, err
	}
	result, err = d.request(ctx, http.MethodPost, "/drive/v3/files?fields=id,name", map[string]string{"Content-Type": "application/json; charset=UTF-8"}, body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := require2xx(result, "drive folder create"); err != nil {
		return ObjectInfo{}, err
	}
	var createPayload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result.Body, &createPayload); err != nil {
		return ObjectInfo{}, err
	}
	if strings.TrimSpace(createPayload.ID) == "" {
		return ObjectInfo{}, errors.New("drive folder create response missing id")
	}
	return ObjectInfo{ID: createPayload.ID, Name: createPayload.Name}, nil
}

func (d *DriveStore) getObjectInfoByID(ctx context.Context, fileID string) (ObjectInfo, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return ObjectInfo{}, errors.New("drive metadata by id requires file id")
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?fields=id,name,size,modifiedTime"
	result, err := d.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := require2xx(result, "drive metadata by id"); err != nil {
		return ObjectInfo{}, err
	}
	var payload struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Size         string `json:"size"`
		ModifiedTime string `json:"modifiedTime"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return ObjectInfo{}, err
	}
	size, _ := strconv.ParseInt(payload.Size, 10, 64)
	return ObjectInfo{Name: payload.Name, ID: payload.ID, Size: size, Updated: payload.ModifiedTime}, nil
}

func (d *DriveStore) Get(ctx context.Context, name string) ([]byte, error) {
	info, err := d.latest(ctx, name)
	if err != nil {
		return nil, err
	}
	path := "/drive/v3/files/" + url.PathEscape(info.ID) + "?alt=media"
	result, err := d.mediaRequest(ctx, http.MethodGet, path, driveMediaDownloadHeaders(), nil)
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
	headers := driveMediaDownloadHeaders()
	var last *HTTPResult
	for attempt := 0; attempt < 5; attempt++ {
		result, err := d.mediaRequest(ctx, http.MethodGet, path, headers, nil)
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

func driveMediaDownloadHeaders() map[string]string {
	return map[string]string{"Accept-Encoding": "identity"}
}

func (d *DriveStore) GetRangeByID(ctx context.Context, fileID string, start, end int64) ([]byte, int, error) {
	body, _, status, err := d.getRangeByID(ctx, fileID, start, end)
	return body, status, err
}

func (d *DriveStore) GetObjectRangeByID(ctx context.Context, fileID string, start, end int64) ([]byte, ObjectRangeInfo, error) {
	body, info, _, err := d.getRangeByID(ctx, fileID, start, end)
	return body, info, err
}

func (d *DriveStore) getRangeByID(ctx context.Context, fileID string, start, end int64) ([]byte, ObjectRangeInfo, int, error) {
	if start < 0 || end < start {
		return nil, ObjectRangeInfo{}, 0, fmt.Errorf("invalid byte range %d-%d", start, end)
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?alt=media"
	headers := map[string]string{
		"Range":           fmt.Sprintf("bytes=%d-%d", start, end),
		"Accept-Encoding": "identity",
	}
	var last *HTTPResult
	for attempt := 0; attempt < 5; attempt++ {
		result, err := d.mediaRequest(ctx, http.MethodGet, path, headers, nil)
		if err != nil {
			return nil, ObjectRangeInfo{}, 0, err
		}
		last = result
		if result.Status != http.StatusNotFound {
			if result.Status != http.StatusPartialContent {
				if err := require2xx(result, "drive range download by id"); err != nil {
					return nil, ObjectRangeInfo{}, result.Status, err
				}
				return nil, ObjectRangeInfo{}, result.Status, fmt.Errorf("drive range download by id returned status=%d, want 206", result.Status)
			}
			info, err := validateContentRange(result.Header.Get("Content-Range"), start, end, len(result.Body))
			if err != nil {
				return nil, ObjectRangeInfo{}, result.Status, err
			}
			return result.Body, info, result.Status, nil
		}
		if attempt == 4 {
			break
		}
		delay := time.Duration(150*(attempt+1)*(attempt+1)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ObjectRangeInfo{}, 0, ctx.Err()
		case <-timer.C:
		}
	}
	if err := require2xx(last, "drive range download by id"); err != nil {
		return nil, ObjectRangeInfo{}, last.Status, err
	}
	return last.Body, ObjectRangeInfo{}, last.Status, nil
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
	info, err := d.ListFreshStatus(ctx, prefix, since)
	return info.Objects, err
}

func (d *DriveStore) ListFreshStatus(ctx context.Context, prefix string, since time.Time) (ObjectListInfo, error) {
	return d.ListFreshPageStatus(ctx, prefix, since, "")
}

func (d *DriveStore) ListFreshPageStatus(ctx context.Context, prefix string, since time.Time, pageToken string) (ObjectListInfo, error) {
	info, err := d.ListFreshContainsPageStatus(ctx, []string{prefix}, since, pageToken, driveListMaxPages)
	if err != nil {
		return ObjectListInfo{}, err
	}
	filtered := info.Objects[:0]
	for _, object := range info.Objects {
		if strings.HasPrefix(object.Name, prefix) {
			filtered = append(filtered, object)
		}
	}
	info.Objects = filtered
	return info, nil
}

func (d *DriveStore) ChangesStartPageToken(ctx context.Context) (string, error) {
	result, err := d.request(ctx, http.MethodGet, "/drive/v3/changes/startPageToken?fields=startPageToken", nil, nil)
	if err != nil {
		return "", err
	}
	if err := require2xx(result, "drive changes start page token"); err != nil {
		return "", err
	}
	var payload struct {
		StartPageToken string `json:"startPageToken"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.StartPageToken) == "" {
		return "", errors.New("drive changes start page token response missing token")
	}
	return payload.StartPageToken, nil
}

func (d *DriveStore) ListChanges(ctx context.Context, pageToken string, includeRemoved bool) (ChangeListInfo, error) {
	pageToken = strings.TrimSpace(pageToken)
	if pageToken == "" {
		return ChangeListInfo{}, errors.New("drive changes page token is required")
	}
	values := url.Values{}
	values.Set("pageToken", pageToken)
	values.Set("pageSize", driveListPageSize)
	values.Set("includeRemoved", strconv.FormatBool(includeRemoved))
	values.Set("fields", "nextPageToken,newStartPageToken,changes(fileId,removed,time,file(id,name,size,modifiedTime))")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	result, err := d.request(ctx, http.MethodGet, "/drive/v3/changes?"+values.Encode(), nil, nil)
	if err != nil {
		return ChangeListInfo{}, err
	}
	if err := require2xx(result, "drive changes list"); err != nil {
		return ChangeListInfo{}, err
	}
	var payload driveChangesPayload
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return ChangeListInfo{}, err
	}
	info := ChangeListInfo{
		NextPageToken:     payload.NextPageToken,
		NewStartPageToken: payload.NewStartPageToken,
	}
	for _, change := range payload.Changes {
		item := ChangeInfo{
			ID:      change.ID,
			FileID:  change.FileID,
			Updated: change.Time,
			Removed: change.Removed,
		}
		if item.FileID == "" {
			item.FileID = change.File.ID
		}
		item.Name = change.File.Name
		if change.File.ModifiedTime != "" {
			item.Updated = change.File.ModifiedTime
		}
		size, _ := strconv.ParseInt(change.File.Size, 10, 64)
		item.Size = size
		info.Changes = append(info.Changes, item)
	}
	return info, nil
}

func (d *DriveStore) listContains(ctx context.Context, contains []string) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.containsQuery(contains))
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime)")
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

func (d *DriveStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if maxPages <= 0 {
		maxPages = driveMuxFreshListMaxPages
	}
	values := url.Values{}
	query := d.containsQuery(contains)
	if !since.IsZero() {
		query += fmt.Sprintf(" and modifiedTime >= '%s'", escapeDriveQuery(since.UTC().Format(time.RFC3339Nano)))
	}
	values.Set("q", query)
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime)")
	values.Set("pageSize", driveListPageSize)
	values.Set("orderBy", "modifiedTime desc")
	if strings.TrimSpace(pageToken) != "" {
		values.Set("pageToken", strings.TrimSpace(pageToken))
	}
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	status, err := d.eachFilesPageUntilLimitStatus(ctx, values, "drive list", maxPages, func(payload driveListPayload) (bool, error) {
		for _, item := range payload.Files {
			matched := true
			for _, value := range contains {
				if strings.TrimSpace(value) == "" {
					continue
				}
				if !strings.Contains(item.Name, value) {
					matched = false
					break
				}
			}
			if !matched {
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
		return ObjectListInfo{}, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return ObjectListInfo{Objects: infos, Truncated: status.Truncated, NextPageToken: status.NextPageToken, Pages: status.Pages, Incomplete: status.Incomplete}, nil
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

func (d *DriveStore) WaitForDriveQuota(ctx context.Context, op string) error {
	if d == nil || d.backoff == nil {
		return nil
	}
	return d.backoff.Wait(ctx, op, d.Logger)
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

func (d *DriveStore) Cleanup(ctx context.Context, opts DriveCleanupOptions) (DriveCleanupResult, error) {
	prefix := strings.TrimSpace(opts.Prefix)
	if prefix == "" && !opts.All {
		return DriveCleanupResult{}, fmt.Errorf("cleanup prefix is required")
	}
	if opts.OlderThan < 0 {
		return DriveCleanupResult{}, fmt.Errorf("cleanup older-than must be non-negative")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.OlderThan)
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = defaultDriveCleanupMaxPages
	}
	resultPrefix := prefix
	if opts.All {
		resultPrefix = "<all>"
	}
	result := DriveCleanupResult{
		Prefix: resultPrefix,
		Cutoff: cutoff.UTC(),
		DryRun: opts.DryRun,
	}
	type candidate struct {
		id string
	}
	var candidates []candidate
	values := url.Values{}
	var contains []string
	if !opts.All {
		contains = []string{prefix}
	}
	values.Set("q", d.containsQuery(contains))
	values.Set("fields", "nextPageToken,files(id,name,size,modifiedTime)")
	values.Set("pageSize", driveListPageSize)
	values.Set("orderBy", "modifiedTime asc")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	status, err := d.eachFilesPageUntilLimitStatus(ctx, values, "drive list", maxPages, func(payload driveListPayload) (bool, error) {
		for _, item := range payload.Files {
			result.Scanned++
			if !opts.All && !strings.HasPrefix(item.Name, prefix) {
				continue
			}
			updated, err := time.Parse(time.RFC3339Nano, item.ModifiedTime)
			if err != nil {
				continue
			}
			if !updated.Before(cutoff) {
				return false, nil
			}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			result.Matched++
			result.MatchedSize += size
			if opts.MaxDeletes > 0 && len(candidates) >= opts.MaxDeletes {
				return false, nil
			}
			candidates = append(candidates, candidate{id: item.ID})
		}
		return true, nil
	})
	result.Pages = status.Pages
	result.Truncated = status.Truncated || status.Incomplete
	if opts.MaxDeletes > 0 && len(candidates) >= opts.MaxDeletes {
		result.Truncated = true
	}
	if err != nil {
		return result, err
	}
	if opts.DryRun || len(candidates) == 0 {
		return result, nil
	}
	concurrency := opts.DeleteConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 32 {
		concurrency = 32
	}
	jobs := make(chan candidate)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if err := d.DeleteID(ctx, item.id); err != nil {
					mu.Lock()
					result.Failed++
					if len(result.Errors) < 8 {
						result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", item.id, err))
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				result.Deleted++
				mu.Unlock()
				if opts.DeleteDelay > 0 {
					timer := time.NewTimer(opts.DeleteDelay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	for _, item := range candidates {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return result, ctx.Err()
		case jobs <- item:
		}
	}
	close(jobs)
	wg.Wait()
	return result, nil
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
	NextPageToken    string `json:"nextPageToken"`
	IncompleteSearch bool   `json:"incompleteSearch"`
	Files            []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Size         string `json:"size"`
		ModifiedTime string `json:"modifiedTime"`
	} `json:"files"`
}

type driveChangesPayload struct {
	NextPageToken     string `json:"nextPageToken"`
	NewStartPageToken string `json:"newStartPageToken"`
	Changes           []struct {
		ID      string `json:"id"`
		FileID  string `json:"fileId"`
		Removed bool   `json:"removed"`
		Time    string `json:"time"`
		File    struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Size         string `json:"size"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"file"`
	} `json:"changes"`
}

type driveListPageStatus struct {
	Truncated     bool
	NextPageToken string
	Pages         int
	Incomplete    bool
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
	return d.eachFilesPageUntilLimit(ctx, values, op, driveListMaxPages, fn)
}

func (d *DriveStore) eachFilesPageUntilLimit(ctx context.Context, values url.Values, op string, maxPages int, fn func(driveListPayload) (bool, error)) error {
	_, err := d.eachFilesPageUntilLimitStatus(ctx, values, op, maxPages, fn)
	return err
}

func (d *DriveStore) eachFilesPageUntilLimitStatus(ctx context.Context, values url.Values, op string, maxPages int, fn func(driveListPayload) (bool, error)) (driveListPageStatus, error) {
	if maxPages <= 0 {
		maxPages = driveListMaxPages
	}
	pageValues := cloneValues(values)
	incomplete := false
	for page := 0; page < maxPages; page++ {
		result, err := d.request(ctx, http.MethodGet, "/drive/v3/files?"+pageValues.Encode(), nil, nil)
		if err != nil {
			return driveListPageStatus{}, err
		}
		if err := require2xx(result, op); err != nil {
			return driveListPageStatus{}, err
		}
		var payload driveListPayload
		if err := json.Unmarshal(result.Body, &payload); err != nil {
			return driveListPageStatus{}, err
		}
		incomplete = incomplete || payload.IncompleteSearch
		pages := page + 1
		keepGoing, err := fn(payload)
		if err != nil {
			return driveListPageStatus{}, err
		}
		if !keepGoing {
			return driveListPageStatus{Truncated: incomplete, Pages: pages, Incomplete: incomplete}, nil
		}
		if payload.NextPageToken == "" {
			return driveListPageStatus{Truncated: incomplete, Pages: pages, Incomplete: incomplete}, nil
		}
		if page == maxPages-1 {
			return driveListPageStatus{Truncated: true, NextPageToken: payload.NextPageToken, Pages: pages, Incomplete: incomplete}, nil
		}
		pageValues.Set("pageToken", payload.NextPageToken)
	}
	return driveListPageStatus{}, nil
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, list := range values {
		out[key] = append([]string(nil), list...)
	}
	return out
}

func validateContentRange(header string, wantStart, wantEnd int64, bodyLen int) (ObjectRangeInfo, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bytes ") {
		return ObjectRangeInfo{}, fmt.Errorf("drive range missing Content-Range: %q", header)
	}
	value := strings.TrimSpace(header[len("bytes "):])
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 {
		return ObjectRangeInfo{}, fmt.Errorf("drive range malformed Content-Range: %q", header)
	}
	rangePart := strings.SplitN(parts[0], "-", 2)
	if len(rangePart) != 2 {
		return ObjectRangeInfo{}, fmt.Errorf("drive range malformed byte interval: %q", header)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(rangePart[0]), 10, 64)
	if err != nil {
		return ObjectRangeInfo{}, fmt.Errorf("drive range malformed start %q: %w", header, err)
	}
	end, err := strconv.ParseInt(strings.TrimSpace(rangePart[1]), 10, 64)
	if err != nil {
		return ObjectRangeInfo{}, fmt.Errorf("drive range malformed end %q: %w", header, err)
	}
	total := int64(-1)
	if strings.TrimSpace(parts[1]) != "*" {
		total, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return ObjectRangeInfo{}, fmt.Errorf("drive range malformed total %q: %w", header, err)
		}
	}
	if start != wantStart || end != wantEnd {
		return ObjectRangeInfo{}, fmt.Errorf("drive range mismatch got=%d-%d want=%d-%d", start, end, wantStart, wantEnd)
	}
	if gotLen := end - start + 1; gotLen != int64(bodyLen) {
		return ObjectRangeInfo{}, fmt.Errorf("drive range body length=%d want=%d", bodyLen, gotLen)
	}
	if total >= 0 && end >= total {
		return ObjectRangeInfo{}, fmt.Errorf("drive range end=%d exceeds total=%d", end, total)
	}
	return ObjectRangeInfo{Start: start, End: end, Total: total}, nil
}

func (d *DriveStore) request(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	if d.http == nil {
		return nil, errors.New("drive http client is nil")
	}
	var last *HTTPResult
	label := driveRequestLabel(method, path)
	started := time.Now()
	for attempt := 0; attempt < 2; attempt++ {
		if d.backoff != nil {
			if err := d.backoff.Wait(ctx, label, d.Logger); err != nil {
				return nil, err
			}
		}
		merged, err := d.authHeaders(ctx)
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			merged[key] = value
		}
		result, err := d.http.RequestNoRateLimitRetry(ctx, method, "www.googleapis.com", path, merged, body)
		if err != nil {
			d.logDriveRequest(label, 1, 0, nil, time.Since(started), err)
			return nil, err
		}
		last = result
		httpAttempts := result.Attempts
		if httpAttempts < 1 {
			httpAttempts = 1
		}
		if result.Status != http.StatusUnauthorized {
			d.logDriveRequest(label, httpAttempts, result.Status, result.Body, time.Since(started), nil)
			if d.backoff != nil {
				d.backoff.Observe(label, result, d.Logger)
			}
			return result, nil
		}
		if d.tokenSource == nil {
			d.logDriveRequest(label, httpAttempts, result.Status, result.Body, time.Since(started), nil)
			return result, nil
		}
		if d.Logger != nil {
			d.Logger.Printf("drive auth rejected op=%s status=401 action=refresh-token", label)
		}
		d.tokenSource.Invalidate()
	}
	if last != nil {
		httpAttempts := last.Attempts
		if httpAttempts < 1 {
			httpAttempts = 1
		}
		d.logDriveRequest(label, httpAttempts, last.Status, last.Body, time.Since(started), nil)
	}
	return last, nil
}

func (d *DriveStore) logDriveRequest(label string, attempts int, status int, body []byte, duration time.Duration, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if attempts < 1 {
		attempts = 1
	}
	if d.quota != nil {
		if report, ok := d.quota.Record(label, status, len(body), duration, err, driveQuotaErrorReason(status, body, err), attempts); ok && d.Logger != nil {
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

func driveQuotaErrorReason(status int, body []byte, err error) string {
	if err != nil {
		return sanitizeTransportErrorText(err.Error())
	}
	if status >= 400 {
		reason := strings.TrimSpace(driveErrorReason(body))
		if reason != "" && reason != "unknown" {
			return reason
		}
		return "status_" + strconv.Itoa(status)
	}
	return ""
}

func (d *DriveStore) mediaRequest(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	return d.request(ctx, method, path, headers, body)
}

type driveQuotaBackoff struct {
	mu      sync.Mutex
	base    time.Duration
	max     time.Duration
	jitter  time.Duration
	until   time.Time
	fails   int
	reason  string
	op      string
	lastLog time.Time
}

func newDriveQuotaBackoff() *driveQuotaBackoff {
	return &driveQuotaBackoff{base: driveQuotaBackoffBase, max: driveQuotaBackoffMax, jitter: driveQuotaBackoffJitter}
}

func (b *driveQuotaBackoff) Wait(ctx context.Context, op string, logger *log.Logger) error {
	if b == nil {
		return nil
	}
	for {
		b.mu.Lock()
		until := b.until
		reason := b.reason
		wait := time.Until(until)
		if wait <= 0 {
			b.mu.Unlock()
			return nil
		}
		if logger != nil && time.Since(b.lastLog) >= driveQuotaBackoffLogInterval {
			logger.Printf("drive quota backoff active op=%s wait=%s reason=%s", op, wait.Round(time.Millisecond), reason)
			b.lastLog = time.Now()
		}
		b.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *driveQuotaBackoff) Observe(op string, result *HTTPResult, logger *log.Logger) {
	if b == nil || result == nil {
		return
	}
	if !isDriveRateLimitResult(result) {
		b.noteNonRateLimitedResult(result)
		return
	}
	reason := driveErrorReason(result.Body)
	delay := retryAfter(result)
	if delay <= 0 {
		delay = b.nextDelay()
	}
	now := time.Now()
	until := now.Add(delay)
	b.mu.Lock()
	if until.After(b.until) {
		b.until = until
	}
	b.fails++
	b.reason = reason
	b.op = op
	if logger != nil && time.Since(b.lastLog) >= driveQuotaBackoffLogInterval {
		logger.Printf("drive quota backoff tripped op=%s status=%d reason=%s delay=%s failures=%d", op, result.Status, reason, time.Until(b.until).Round(time.Millisecond), b.fails)
		b.lastLog = now
	}
	b.mu.Unlock()
}

func (b *driveQuotaBackoff) noteNonRateLimitedResult(result *HTTPResult) {
	if result == nil || result.Status < 200 || result.Status >= 300 {
		return
	}
	now := time.Now()
	b.mu.Lock()
	if b.until.IsZero() || now.After(b.until) {
		b.fails = 0
		b.reason = ""
		b.op = ""
	}
	b.mu.Unlock()
}

func (b *driveQuotaBackoff) nextDelay() time.Duration {
	b.mu.Lock()
	failures := b.fails
	base := b.base
	maximum := b.max
	jitter := b.jitter
	b.mu.Unlock()
	if base <= 0 {
		base = driveQuotaBackoffBase
	}
	if maximum <= 0 {
		maximum = driveQuotaBackoffMax
	}
	if failures < 0 {
		failures = 0
	}
	if failures > 6 {
		failures = 6
	}
	delay := base * time.Duration(1<<failures)
	if delay > maximum {
		delay = maximum
	}
	if jitter > 0 {
		delay += time.Duration(rand.Int63n(int64(jitter)))
	}
	if delay > maximum {
		delay = maximum
	}
	return delay
}

func isDriveRateLimitResult(result *HTTPResult) bool {
	if result == nil {
		return false
	}
	if result.Status == http.StatusTooManyRequests {
		return true
	}
	if result.Status != http.StatusForbidden {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(driveErrorReason(result.Body)))
	return reason == "ratelimitexceeded" || reason == "userratelimitexceeded" || reason == "dailylimitexceeded"
}

type driveQuotaStats struct {
	mu                   sync.Mutex
	interval             time.Duration
	since                time.Time
	calls                int64
	units                int64
	errors               int64
	lastErrorReason      string
	bytes                int64
	ops                  map[string]driveQuotaOpStats
	totalCalls           int64
	totalUnits           int64
	totalError           int64
	totalLastErrorReason string
	totalBytes           int64
	totalOps             map[string]driveQuotaOpStats
}

type driveQuotaOpStats struct {
	Calls           int64
	Units           int64
	Errors          int64
	LastErrorReason string
	TotalDurationMS int64
	MaxDurationMS   int64
	SamplesMS       []int64
}

type driveQuotaReport struct {
	Duration        time.Duration
	Calls           int64
	Units           int64
	Errors          int64
	LastErrorReason string
	ResponseBytes   int64
	Ops             map[string]driveQuotaOpStats
}

type DriveQuotaOpSnapshot struct {
	Calls           int64  `json:"calls"`
	Units           int64  `json:"units"`
	Errors          int64  `json:"errors"`
	LastErrorReason string `json:"last_error_reason,omitempty"`
	TotalDurationMS int64  `json:"total_duration_ms"`
	AvgDurationMS   int64  `json:"avg_duration_ms"`
	P50DurationMS   int64  `json:"p50_duration_ms"`
	P95DurationMS   int64  `json:"p95_duration_ms"`
	MaxDurationMS   int64  `json:"max_duration_ms"`
}

type DriveQuotaSnapshot struct {
	Calls           int64                           `json:"calls"`
	Units           int64                           `json:"units"`
	Errors          int64                           `json:"errors"`
	LastErrorReason string                          `json:"last_error_reason,omitempty"`
	ResponseBytes   int64                           `json:"response_bytes"`
	Ops             map[string]DriveQuotaOpSnapshot `json:"ops"`
}

type DriveQuotaBackoffSnapshot struct {
	Active      bool    `json:"active"`
	Until       string  `json:"until,omitempty"`
	WaitSeconds float64 `json:"wait_seconds,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Op          string  `json:"op,omitempty"`
	Failures    int     `json:"failures,omitempty"`
}

func newDriveQuotaStats(interval time.Duration) *driveQuotaStats {
	if interval < 0 {
		interval = 0
	}
	return &driveQuotaStats{
		interval: interval,
		since:    time.Now(),
		ops:      map[string]driveQuotaOpStats{},
		totalOps: map[string]driveQuotaOpStats{},
	}
}

func (s *driveQuotaStats) Record(op string, status int, responseBytes int, duration time.Duration, err error, errorReason string, httpAttempts ...int) (driveQuotaReport, bool) {
	if s == nil {
		return driveQuotaReport{}, false
	}
	attempts := int64(1)
	if len(httpAttempts) > 0 && httpAttempts[0] > 1 {
		attempts = int64(httpAttempts[0])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.since.IsZero() {
		s.since = time.Now()
	}
	units := int64(driveQuotaUnits(op)) * attempts
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	failed := err != nil || status >= 400
	s.calls += attempts
	s.units += units
	s.bytes += int64(responseBytes)
	s.totalCalls += attempts
	s.totalUnits += units
	s.totalBytes += int64(responseBytes)
	if failed {
		s.errors += attempts
		s.totalError += attempts
		s.lastErrorReason = errorReason
		s.totalLastErrorReason = errorReason
	}
	current := s.ops[op]
	current.Calls += attempts
	current.Units += units
	addDriveQuotaDuration(&current, durationMS)
	if failed {
		current.Errors += attempts
		current.LastErrorReason = errorReason
	}
	s.ops[op] = current
	totalCurrent := s.totalOps[op]
	totalCurrent.Calls += attempts
	totalCurrent.Units += units
	addDriveQuotaDuration(&totalCurrent, durationMS)
	if failed {
		totalCurrent.Errors += attempts
		totalCurrent.LastErrorReason = errorReason
	}
	s.totalOps[op] = totalCurrent
	if s.interval == 0 || time.Since(s.since) < s.interval {
		return driveQuotaReport{}, false
	}
	report := driveQuotaReport{
		Duration:        time.Since(s.since),
		Calls:           s.calls,
		Units:           s.units,
		Errors:          s.errors,
		LastErrorReason: s.lastErrorReason,
		ResponseBytes:   s.bytes,
		Ops:             cloneDriveQuotaOps(s.ops),
	}
	s.since = time.Now()
	s.calls = 0
	s.units = 0
	s.errors = 0
	s.lastErrorReason = ""
	s.bytes = 0
	clear(s.ops)
	return report, true
}

func (s *driveQuotaStats) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.since = time.Now()
	s.calls = 0
	s.units = 0
	s.errors = 0
	s.lastErrorReason = ""
	s.bytes = 0
	clear(s.ops)
	s.totalCalls = 0
	s.totalUnits = 0
	s.totalError = 0
	s.totalLastErrorReason = ""
	s.totalBytes = 0
	clear(s.totalOps)
}

func (d *DriveStore) QuotaSnapshot() DriveQuotaSnapshot {
	if d == nil || d.quota == nil {
		return DriveQuotaSnapshot{Ops: map[string]DriveQuotaOpSnapshot{}}
	}
	return d.quota.Snapshot()
}

func (d *DriveStore) QuotaBackoffSnapshot() DriveQuotaBackoffSnapshot {
	if d == nil || d.backoff == nil {
		return DriveQuotaBackoffSnapshot{}
	}
	return d.backoff.Snapshot(time.Now())
}

func (d *DriveStore) ResetTelemetry() {
	if d != nil && d.quota != nil {
		d.quota.Reset()
	}
}

func (s *driveQuotaStats) Snapshot() DriveQuotaSnapshot {
	if s == nil {
		return DriveQuotaSnapshot{Ops: map[string]DriveQuotaOpSnapshot{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return DriveQuotaSnapshot{
		Calls:           s.totalCalls,
		Units:           s.totalUnits,
		Errors:          s.totalError,
		LastErrorReason: s.totalLastErrorReason,
		ResponseBytes:   s.totalBytes,
		Ops:             exportedDriveQuotaOps(s.totalOps),
	}
}

func (b *driveQuotaBackoff) Snapshot(now time.Time) DriveQuotaBackoffSnapshot {
	if b == nil {
		return DriveQuotaBackoffSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	wait := time.Until(b.until)
	if !now.IsZero() {
		wait = b.until.Sub(now)
	}
	if wait <= 0 {
		return DriveQuotaBackoffSnapshot{
			Reason:   b.reason,
			Op:       b.op,
			Failures: b.fails,
		}
	}
	return DriveQuotaBackoffSnapshot{
		Active:      true,
		Until:       b.until.UTC().Format(time.RFC3339Nano),
		WaitSeconds: wait.Seconds(),
		Reason:      b.reason,
		Op:          b.op,
		Failures:    b.fails,
	}
}

func (s DriveQuotaSnapshot) Delta(before DriveQuotaSnapshot) DriveQuotaSnapshot {
	out := DriveQuotaSnapshot{
		Calls:         s.Calls - before.Calls,
		Units:         s.Units - before.Units,
		Errors:        s.Errors - before.Errors,
		ResponseBytes: s.ResponseBytes - before.ResponseBytes,
		Ops:           map[string]DriveQuotaOpSnapshot{},
	}
	if out.Errors > 0 {
		out.LastErrorReason = s.LastErrorReason
	}
	for key, after := range s.Ops {
		prev := before.Ops[key]
		delta := DriveQuotaOpSnapshot{
			Calls:           after.Calls - prev.Calls,
			Units:           after.Units - prev.Units,
			Errors:          after.Errors - prev.Errors,
			TotalDurationMS: after.TotalDurationMS - prev.TotalDurationMS,
			MaxDurationMS:   after.MaxDurationMS,
			AvgDurationMS:   after.AvgDurationMS,
			P50DurationMS:   after.P50DurationMS,
			P95DurationMS:   after.P95DurationMS,
		}
		if delta.Calls == 0 && delta.Units == 0 && delta.Errors == 0 && delta.TotalDurationMS == 0 {
			continue
		}
		if delta.Errors > 0 {
			delta.LastErrorReason = after.LastErrorReason
			if out.LastErrorReason == "" {
				out.LastErrorReason = delta.LastErrorReason
			}
		}
		out.Ops[key] = delta
	}
	return out
}

func cloneDriveQuotaOps(in map[string]driveQuotaOpStats) map[string]driveQuotaOpStats {
	out := make(map[string]driveQuotaOpStats, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func exportedDriveQuotaOps(in map[string]driveQuotaOpStats) map[string]DriveQuotaOpSnapshot {
	out := make(map[string]DriveQuotaOpSnapshot, len(in))
	for key, value := range in {
		out[key] = driveQuotaOpSnapshot(value)
	}
	return out
}

func addDriveQuotaDuration(stats *driveQuotaOpStats, durationMS int64) {
	stats.TotalDurationMS += durationMS
	if durationMS > stats.MaxDurationMS {
		stats.MaxDurationMS = durationMS
	}
	stats.SamplesMS = append(stats.SamplesMS, durationMS)
	if len(stats.SamplesMS) > driveQuotaMaxSamplesPerOp {
		copy(stats.SamplesMS, stats.SamplesMS[len(stats.SamplesMS)-driveQuotaMaxSamplesPerOp:])
		stats.SamplesMS = stats.SamplesMS[:driveQuotaMaxSamplesPerOp]
	}
}

func driveQuotaOpSnapshot(stats driveQuotaOpStats) DriveQuotaOpSnapshot {
	out := DriveQuotaOpSnapshot{
		Calls:           stats.Calls,
		Units:           stats.Units,
		Errors:          stats.Errors,
		LastErrorReason: stats.LastErrorReason,
		TotalDurationMS: stats.TotalDurationMS,
		MaxDurationMS:   stats.MaxDurationMS,
	}
	if stats.Calls > 0 {
		out.AvgDurationMS = stats.TotalDurationMS / stats.Calls
	}
	samples := append([]int64(nil), stats.SamplesMS...)
	out.P50DurationMS = durationPercentileMS(samples, 0.50)
	out.P95DurationMS = durationPercentileMS(samples, 0.95)
	return out
}

func durationPercentileMS(samples []int64, p float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if p <= 0 {
		return samples[0]
	}
	if p >= 1 {
		return samples[len(samples)-1]
	}
	index := int(p * float64(len(samples)))
	if index >= len(samples) {
		index = len(samples) - 1
	}
	return samples[index]
}

func (s DriveQuotaSnapshot) OpSummary() string {
	if len(s.Ops) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(s.Ops))
	for key := range s.Ops {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		stats := s.Ops[key]
		if stats.Errors > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d/%du/%de", key, stats.Calls, stats.Units, stats.Errors))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d/%du", key, stats.Calls, stats.Units))
	}
	return strings.Join(parts, ",")
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
	case "list", "changes":
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
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/changes"):
		return "changes"
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/generateIds"):
		return "generate_ids"
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/"):
		return "metadata"
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
