package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DriveStore struct {
	http     *GoogleHTTPClient
	token    string
	folderID string
	space    string
}

func NewDriveStore(httpClient *GoogleHTTPClient, token string, cfg DriveConfig) *DriveStore {
	space := strings.TrimSpace(cfg.Space)
	folderID := strings.TrimSpace(cfg.FolderID)
	if folderID == "appDataFolder" && space == "" {
		space = "appDataFolder"
		folderID = ""
	}
	return &DriveStore{http: httpClient, token: token, folderID: folderID, space: space}
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
		"name":          name,
		"mimeType":      "application/octet-stream",
		"appProperties": map[string]string{"skirkName": name},
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
		"Authorization": "Bearer " + d.token,
		"Content-Type":  "multipart/related; boundary=" + boundary,
	}
	result, err := d.http.Request(ctx, http.MethodPost, "www.googleapis.com", "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", headers, body.Bytes())
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
	result, err := d.http.Request(ctx, http.MethodGet, "www.googleapis.com", path, d.authHeaders(), nil)
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
	result, err := d.http.Request(ctx, http.MethodGet, "www.googleapis.com", path, d.authHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "drive download by id"); err != nil {
		return nil, err
	}
	return result.Body, nil
}

func (d *DriveStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.query(prefix, false))
	values.Set("fields", "files(id,name,size,modifiedTime)")
	values.Set("pageSize", "1000")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	result, err := d.http.Request(ctx, http.MethodGet, "www.googleapis.com", "/drive/v3/files?"+values.Encode(), d.authHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "drive list"); err != nil {
		return nil, err
	}
	var payload struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Size         string `json:"size"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return nil, err
	}
	var infos []ObjectInfo
	for _, item := range payload.Files {
		if !strings.HasPrefix(item.Name, prefix) {
			continue
		}
		size, _ := strconv.ParseInt(item.Size, 10, 64)
		infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
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
		result, err := d.http.Request(ctx, http.MethodDelete, "www.googleapis.com", "/drive/v3/files/"+url.PathEscape(info.ID), d.authHeaders(), nil)
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
	result, err := d.http.Request(ctx, http.MethodDelete, "www.googleapis.com", "/drive/v3/files/"+url.PathEscape(fileID), d.authHeaders(), nil)
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
	values.Set("fields", "files(id,name,size,modifiedTime)")
	values.Set("orderBy", "modifiedTime desc")
	values.Set("pageSize", "1000")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	result, err := d.http.Request(ctx, http.MethodGet, "www.googleapis.com", "/drive/v3/files?"+values.Encode(), d.authHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "drive lookup"); err != nil {
		return nil, err
	}
	var payload struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Size         string `json:"size"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return nil, err
	}
	var infos []ObjectInfo
	for _, item := range payload.Files {
		if item.Name != name {
			continue
		}
		size, _ := strconv.ParseInt(item.Size, 10, 64)
		infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
	}
	return infos, nil
}

func (d *DriveStore) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + d.token}
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

func (d *DriveStore) isAppData() bool {
	return d.space == "appDataFolder"
}

func escapeDriveQuery(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "'", "\\'")
	return value
}
