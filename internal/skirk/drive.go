package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DriveStore struct {
	http                   *GoogleHTTPClient
	normalUploadHTTP       *GoogleHTTPClient
	normalDownloadHTTP     *GoogleHTTPClient
	slotReadHTTP           *GoogleHTTPClient
	slotWriteHTTP          *GoogleHTTPClient
	tokenSource            *AccessTokenSource
	folderID               string
	space                  string
	quotaProject           string
	inlineSmallObjects     bool
	metadataOnlyPriority   bool
	metadataOnlyMaxBytes   int
	metadataSlotV1         bool
	indexedClassProperties bool
	normalListPageSize     int
	uploadHedgeDelay       time.Duration
	uploadHedgeSlots       chan struct{}
	uploadStallHedgeDelay  time.Duration
	uploadStallHedgeBudget int64
	uploadStallHedgeSlots  chan struct{}
	uploadStallBudgetMu    sync.Mutex
	uploadStallCharges     []driveUploadDuplicateCharge
	newHedgeHTTPClient     func() *GoogleHTTPClient
	quota                  *driveQuotaStats
	backoff                *driveQuotaBackoff
	metadataSlotMu         sync.Mutex
	metadataSlotNames      map[string]string
	Logger                 *log.Logger
}

const driveListPageSize = "100"
const driveNormalListDefaultPageSize = 1000
const driveListMaxPages = 16
const driveMuxFreshListMaxPages = 4
const defaultDriveCleanupMaxPages = 256
const driveQuotaMaxSamplesPerOp = 4096
const driveSlowRequestThreshold = 2 * time.Second
const defaultDriveQuotaLogInterval = time.Minute
const driveFolderMimeType = "application/vnd.google-apps.folder"
const driveNonFolderFilter = "mimeType != '" + driveFolderMimeType + "'"
const driveQuotaBackoffBase = 5 * time.Second
const driveQuotaBackoffMax = 64 * time.Second
const driveQuotaBackoffJitter = time.Second
const driveQuotaBackoffLogInterval = 5 * time.Second
const driveInlineMaxBytes = 8192
const driveMetadataOnlyLargeMaxBytes = 16384
const driveLargeMetadataListMaxBytes = 8 * 1024 * 1024

func metadataOnlyByteLimit(configured int) int {
	if configured == driveMetadataOnlyLargeMaxBytes {
		return configured
	}
	return driveInlineMaxBytes
}

const driveInlineMarker = "skirk-sealed-v1:"
const driveMetadataOnlyMarker = "skirk-sealed-v2:"
const driveMetadataOnlyNameSuffix = ".mo1"
const driveIndexedClassProperty = "skirkClass"
const driveClockTolerance = 3 * time.Second
const driveClockCheckMaxDuration = time.Minute

var ErrDriveClockSkew = errors.New("device clock differs from Google Drive clock; enable automatic date and time, then reconnect")
var ErrDriveClockUnverified = errors.New("could not verify Google Drive clock; reconnect when the connection is stable")

const driveUploadHedgeMinBytes = 128 * 1024
const driveUploadHedgeMaxConcurrent = 4

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
	var normalUploadHTTP, normalDownloadHTTP *GoogleHTTPClient
	if httpClient != nil && (cfg.SeparateUploadPool || cfg.SeparateBulkMediaPool) {
		// Keep bulk media on one independent same-route pool. Metadata, list,
		// priority traffic and cleanup retain the original connection pool.
		normalUploadHTTP = NewGoogleHTTPClient(httpClient.route)
		if cfg.SeparateBulkMediaPool {
			normalDownloadHTTP = normalUploadHTTP
		}
	}
	var slotReadHTTP, slotWriteHTTP *GoogleHTTPClient
	if httpClient != nil && cfg.MetadataSlotV1 && cfg.SeparateMetadataSlotPools {
		// Read and write each get a same-route pool. Authentication, quota
		// accounting, and backoff still belong to this DriveStore.
		slotReadHTTP = NewGoogleHTTPClient(httpClient.route)
		slotWriteHTTP = NewGoogleHTTPClient(httpClient.route)
	}
	normalListPageSize := cfg.NormalListPageSize
	if normalListPageSize == 0 {
		normalListPageSize = driveNormalListDefaultPageSize
	}
	if normalListPageSize != 100 && normalListPageSize != 500 && normalListPageSize != 1000 {
		normalListPageSize = 100 // Direct constructor calls still fail safe without Config.Validate.
	}
	return &DriveStore{http: httpClient, normalUploadHTTP: normalUploadHTTP, normalDownloadHTTP: normalDownloadHTTP, slotReadHTTP: slotReadHTTP, slotWriteHTTP: slotWriteHTTP, tokenSource: tokenSource, folderID: folderID, space: space, quotaProject: cfg.QuotaProject, inlineSmallObjects: cfg.InlineSmallObjects, metadataOnlyPriority: cfg.MetadataOnlyPriority, metadataOnlyMaxBytes: metadataOnlyByteLimit(cfg.MetadataOnlyMaxBytes), metadataSlotV1: cfg.MetadataSlotV1, indexedClassProperties: cfg.IndexedClassProperties, normalListPageSize: normalListPageSize, uploadHedgeDelay: time.Duration(cfg.UploadHedgeDelayMS) * time.Millisecond, uploadHedgeSlots: make(chan struct{}, driveUploadHedgeMaxConcurrent), uploadStallHedgeDelay: time.Duration(cfg.UploadStallHedgeDelayMS) * time.Millisecond, uploadStallHedgeBudget: cfg.UploadStallHedgeBudgetBytes, uploadStallHedgeSlots: make(chan struct{}, 1), quota: newDriveQuotaStats(driveQuotaLogInterval()), backoff: newDriveQuotaBackoff()}
}

func (d *DriveStore) MetadataOnlyPriorityEnabled() bool {
	return d != nil && d.metadataOnlyPriority
}

func (d *DriveStore) MetadataOnlyPriorityLimit() int {
	return metadataOnlyByteLimit(d.metadataOnlyMaxBytes)
}

func indexedMuxClass(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 9 || (parts[0] != "muxv4" && parts[0] != "muxv5") || !strings.HasPrefix(parts[len(parts)-2], "l") {
		return ""
	}
	class := parts[len(parts)-4]
	if class == "p0" || class == "p1" {
		return class
	}
	return ""
}

func isMuxNamespace(name string) bool {
	return strings.HasPrefix(name, "muxv4/") || strings.HasPrefix(name, "muxv5/")
}

func (d *DriveStore) addIndexedClassProperty(metadata map[string]any, name string) {
	if d == nil || !d.indexedClassProperties {
		return
	}
	if class := indexedMuxClass(name); class != "" {
		metadata["properties"] = map[string]string{driveIndexedClassProperty: class}
	}
}

// PutMetadataOnlyObject uses files.create metadata rather than an upload URL.
// The sealed bytes are still authenticated by the mux AEAD on the reader.
func (d *DriveStore) PutMetadataOnlyObject(ctx context.Context, name string, sealed []byte) (ObjectInfo, error) {
	if !d.MetadataOnlyPriorityEnabled() || !isMetadataOnlyPriorityName(name) {
		return ObjectInfo{}, errors.New("metadata-only priority upload is disabled or name is invalid")
	}
	if len(sealed) == 0 || len(sealed) > metadataOnlyByteLimit(d.metadataOnlyMaxBytes) {
		return ObjectInfo{}, fmt.Errorf("metadata-only sealed size must be 1-%d bytes", metadataOnlyByteLimit(d.metadataOnlyMaxBytes))
	}
	metadata := map[string]any{
		"name":        name,
		"mimeType":    "application/octet-stream",
		"description": driveMetadataOnlyMarker + base64.StdEncoding.EncodeToString(sealed),
	}
	d.addIndexedClassProperty(metadata, name)
	if d.isAppData() {
		metadata["parents"] = []string{"appDataFolder"}
	} else if d.folderID != "" {
		metadata["parents"] = []string{d.folderID}
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return ObjectInfo{}, err
	}
	result, err := d.request(ctx, http.MethodPost, "/drive/v3/files?fields=id,name,size", map[string]string{"Content-Type": "application/json; charset=UTF-8"}, body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := require2xx(result, "drive metadata-only create"); err != nil {
		return ObjectInfo{}, err
	}
	if d.Logger != nil && len(sealed) > driveInlineMaxBytes {
		d.Logger.Printf("drive large metadata publication sealed_bytes=%d configured_limit=%d", len(sealed), metadataOnlyByteLimit(d.metadataOnlyMaxBytes))
	}
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size string `json:"size"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return ObjectInfo{}, err
	}
	if payload.ID == "" || payload.Name != name || (payload.Size != "" && payload.Size != "0") {
		return ObjectInfo{}, errors.New("drive metadata-only create returned unexpected file metadata")
	}
	return ObjectInfo{Name: name, ID: payload.ID, Size: 0}, nil
}

func isMetadataOnlyPriorityName(name string) bool {
	return isMuxNamespace(name) && strings.Contains(name, "/p0/") && strings.HasSuffix(name, driveMetadataOnlyNameSuffix)
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
	d.addIndexedClassProperty(metadata, name)
	if d.inlineSmallObjects && len(data) > 0 && len(data) <= driveInlineMaxBytes && isMuxNamespace(name) && strings.Contains(name, "/p0/") {
		// Retain the media body so existing readers can read the same file.
		metadata["description"] = driveInlineMarker + base64.StdEncoding.EncodeToString(data)
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
	// Reserve the media and closing delimiter together so Close cannot
	// reallocate and copy a payload that fills the buffer's capacity.
	closingBoundaryLen := len("\r\n--") + len(boundary) + len("--\r\n")
	body.Grow(len(data) + closingBoundaryLen)
	if _, err := dataPart.Write(data); err != nil {
		return ObjectInfo{}, err
	}
	if err := writer.Close(); err != nil {
		return ObjectInfo{}, err
	}
	headers := map[string]string{
		"Content-Type": "multipart/related; boundary=" + boundary,
	}
	primaryHTTP := d.http
	if d.normalUploadHTTP != nil && isNormalGeneratedMuxUpload(fileID, name, len(data)) {
		primaryHTTP = d.normalUploadHTTP
	}
	var proofDigest func() string
	if fileID != "" {
		// Most uploads succeed; hash only when a duplicate ID needs proof.
		proofDigest = func() string {
			digest := sha256.Sum256(data)
			return hex.EncodeToString(digest[:])
		}
	}
	upload := func(uploadCtx context.Context, client *GoogleHTTPClient) (ObjectInfo, error) {
		return d.uploadObjectWithHTTPMode(uploadCtx, client, fileID, name, len(data), headers, body.Bytes(), proofDigest, false)
	}
	if d.uploadStallHedgeDelay > 0 && isNormalGeneratedMuxUpload(fileID, name, len(data)) {
		return d.uploadObjectWithStallHedge(ctx, primaryHTTP, fileID, name, data, headers, body.Bytes())
	}
	if d.shouldHedgeUpload(fileID, name, len(data)) {
		return d.uploadObjectWithHedge(ctx, primaryHTTP, upload)
	}
	return upload(ctx, primaryHTTP)
}

func (d *DriveStore) shouldHedgeUpload(fileID, name string, size int) bool {
	return d.uploadHedgeDelay > 0 && d.uploadHedgeSlots != nil && isNormalGeneratedMuxUpload(fileID, name, size)
}

func isNormalGeneratedMuxUpload(fileID, name string, size int) bool {
	return fileID != "" && size >= driveUploadHedgeMinBytes &&
		isMuxNamespace(name) && strings.Contains(name, "/p1/")
}

type driveUploadOutcome struct {
	info ObjectInfo
	err  error
}

// A generated ID makes the second POST safe to issue after an indeterminate
// first attempt. The extra request gets a fresh connection pool, while both
// legs retain this store's token, quota accounting, backoff, and logger.
func (d *DriveStore) uploadObjectWithHedge(ctx context.Context, primaryHTTP *GoogleHTTPClient, upload func(context.Context, *GoogleHTTPClient) (ObjectInfo, error)) (ObjectInfo, error) {
	legCtx, cancelLegs := context.WithCancel(ctx)
	defer cancelLegs()
	primary := make(chan driveUploadOutcome, 1)
	go func() {
		info, err := upload(legCtx, primaryHTTP)
		primary <- driveUploadOutcome{info: info, err: err}
	}()
	timer := time.NewTimer(d.uploadHedgeDelay)
	defer timer.Stop()
	select {
	case result := <-primary:
		return result.info, result.err
	case <-timer.C:
	}
	// Prefer an already-completed primary when the timer and result race.
	select {
	case result := <-primary:
		return result.info, result.err
	default:
	}
	if d.QuotaBackoffSnapshot().Active {
		result := <-primary
		return result.info, result.err
	}
	if ctx.Err() != nil {
		result := <-primary
		return result.info, result.err
	}
	select {
	case d.uploadHedgeSlots <- struct{}{}:
	default:
		result := <-primary
		return result.info, result.err
	}
	select {
	case result := <-primary:
		<-d.uploadHedgeSlots
		return result.info, result.err
	default:
	}
	if ctx.Err() != nil || d.QuotaBackoffSnapshot().Active {
		<-d.uploadHedgeSlots
		result := <-primary
		return result.info, result.err
	}
	hedgeClient := d.freshHedgeHTTPClient()
	if hedgeClient == nil {
		<-d.uploadHedgeSlots
		result := <-primary
		return result.info, result.err
	}
	hedgeStarted := time.Now()
	if d.Logger != nil {
		d.Logger.Printf("drive upload hedge action=start delay=%s active_extra=%d", d.uploadHedgeDelay, len(d.uploadHedgeSlots))
	}
	secondary := make(chan driveUploadOutcome, 1)
	go func() {
		info, err := upload(legCtx, hedgeClient)
		hedgeClient.client.CloseIdleConnections()
		<-d.uploadHedgeSlots
		secondary <- driveUploadOutcome{info: info, err: err}
	}()
	primaryPending, secondaryPending := (<-chan driveUploadOutcome)(primary), (<-chan driveUploadOutcome)(secondary)
	var primaryResult driveUploadOutcome
	for primaryPending != nil || secondaryPending != nil {
		select {
		case result := <-primaryPending:
			primaryResult = result
			primaryPending = nil
			if primaryResult.err == nil {
				cancelLegs()
				if secondaryPending != nil {
					<-secondaryPending // Join the canceled leg before returning success.
				}
				d.logUploadHedgeComplete("primary", hedgeStarted)
				return primaryResult.info, nil
			}
		case result := <-secondaryPending:
			secondaryPending = nil
			if result.err == nil {
				cancelLegs()
				if primaryPending != nil {
					<-primaryPending
				}
				d.logUploadHedgeComplete("secondary", hedgeStarted)
				return result.info, nil
			}
		}
	}
	d.logUploadHedgeComplete("none", hedgeStarted)
	return primaryResult.info, primaryResult.err
}

func (d *DriveStore) logUploadHedgeComplete(winner string, started time.Time) {
	if d.Logger != nil {
		d.Logger.Printf("drive upload hedge action=complete winner=%s duration=%s", winner, time.Since(started).Round(time.Millisecond))
	}
}

func (d *DriveStore) freshHedgeHTTPClient() *GoogleHTTPClient {
	if d.newHedgeHTTPClient != nil {
		return d.newHedgeHTTPClient()
	}
	if d.http == nil {
		return nil
	}
	return NewGoogleHTTPClient(d.http.route)
}

func (d *DriveStore) uploadObjectWithHTTPMode(ctx context.Context, httpClient *GoogleHTTPClient, fileID, name string, size int, headers map[string]string, body []byte, proofDigest func() string, singleAttempt bool) (ObjectInfo, error) {
	result, err := d.requestWithHTTPMode(ctx, httpClient, http.MethodPost, "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", headers, body, singleAttempt)
	if err != nil {
		return ObjectInfo{}, err
	}
	if result.Status == http.StatusConflict && fileID != "" {
		if proofDigest == nil {
			return ObjectInfo{}, errors.New("upload conflict has no prepared content digest")
		}
		return d.proveGeneratedUploadConflict(ctx, httpClient, fileID, name, size, proofDigest())
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
	parsedSize, _ := strconv.ParseInt(payload.Size, 10, 64)
	return ObjectInfo{Name: payload.Name, ID: payload.ID, Size: parsedSize}, nil
}

func (d *DriveStore) proveGeneratedUploadConflict(ctx context.Context, client *GoogleHTTPClient, id, name string, size int, digest string) (ObjectInfo, error) {
	for attempt := 0; attempt < 3; attempt++ {
		info, committed, err := d.readUploadCommitProof(ctx, client, id, name, size, digest)
		if err != nil {
			return ObjectInfo{}, err
		}
		if committed {
			return info, nil
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ObjectInfo{}, ctx.Err()
		case <-timer.C:
		}
	}
	return ObjectInfo{}, errors.New("upload conflict has no exact content commit proof")
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
	return d.getObjectInfoByIDWithHTTP(ctx, d.http, fileID)
}

func (d *DriveStore) GetMuxObjectMetadataByID(ctx context.Context, fileID string) (ObjectInfo, bool, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return ObjectInfo{}, false, errors.New("mux object metadata requires a file id")
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?fields=id,name,size,modifiedTime,description,trashed,parents,spaces"
	result, err := d.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	if err := require2xx(result, "drive mux object metadata by id"); err != nil {
		return ObjectInfo{}, false, err
	}
	var payload struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Size         string   `json:"size"`
		ModifiedTime string   `json:"modifiedTime"`
		Description  string   `json:"description"`
		Trashed      bool     `json:"trashed"`
		Parents      []string `json:"parents"`
		Spaces       []string `json:"spaces"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return ObjectInfo{}, false, err
	}
	if payload.ID == "" || payload.ID != fileID {
		return ObjectInfo{}, false, errors.New("drive mux object metadata returned a mismatched file id")
	}
	size, _ := strconv.ParseInt(payload.Size, 10, 64)
	info := ObjectInfo{Name: payload.Name, ID: payload.ID, Size: size, Updated: payload.ModifiedTime, InlineData: d.decodeInlineData(payload.Name, payload.Description, size)}
	if payload.Trashed {
		return info, false, nil
	}
	if d.folderID != "" {
		if payload.Parents == nil {
			return info, false, errors.New("drive mux object mailbox parents were omitted")
		}
		if !slices.Contains(payload.Parents, d.folderID) {
			return info, false, nil
		}
	}
	if d.isAppData() {
		if payload.Spaces == nil {
			return info, false, errors.New("drive mux object mailbox spaces were omitted")
		}
		if !slices.Contains(payload.Spaces, "appDataFolder") {
			return info, false, nil
		}
	}
	return info, true, nil
}

func (d *DriveStore) getObjectInfoByIDWithHTTP(ctx context.Context, httpClient *GoogleHTTPClient, fileID string) (ObjectInfo, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return ObjectInfo{}, errors.New("drive metadata by id requires file id")
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?fields=id,name,size,modifiedTime"
	result, err := d.requestWithHTTP(ctx, httpClient, http.MethodGet, path, nil, nil)
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
	return d.getByIDWithHTTP(ctx, d.http, fileID)
}

// GetNormalByID uses the optional bulk pool. The mux calls this only for
// normal data objects; priority reads and control calls stay on d.http.
func (d *DriveStore) GetNormalByID(ctx context.Context, fileID string) ([]byte, error) {
	client := d.normalDownloadHTTP
	if client == nil {
		client = d.http
	}
	return d.getByIDWithHTTP(withGoogleResponseLimitAtMost(ctx, int64(muxMaxSealedBatch)), client, fileID)
}

func (d *DriveStore) getByIDWithHTTP(ctx context.Context, client *GoogleHTTPClient, fileID string) ([]byte, error) {
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?alt=media"
	headers := driveMediaDownloadHeaders()
	var last *HTTPResult
	for attempt := 0; attempt < 5; attempt++ {
		result, err := d.requestWithHTTP(ctx, client, http.MethodGet, path, headers, nil)
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
	return d.listFreshPrefixPageStatus(ctx, prefix, since, pageToken, driveListMaxPages, driveListPageSize)
}

func (d *DriveStore) ListFreshAdaptiveMuxPageStatus(ctx context.Context, prefix string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if !isMuxNamespace(prefix) {
		return ObjectListInfo{}, errors.New("adaptive mux list requires a mux prefix")
	}
	if maxPages < 1 || maxPages > driveMuxFreshListMaxPages {
		return ObjectListInfo{}, errors.New("adaptive mux list page budget must be 1..4")
	}
	return d.listFreshPrefixPageStatus(ctx, prefix, since, pageToken, maxPages, strconv.Itoa(d.normalListPageSize))
}

func (d *DriveStore) listFreshPrefixPageStatus(ctx context.Context, prefix string, since time.Time, pageToken string, maxPages int, pageSize string) (ObjectListInfo, error) {
	info, err := d.listFreshQueryPageStatus(ctx, d.containsQuery([]string{prefix}), since, pageToken, maxPages, pageSize, func(name string) bool {
		return strings.HasPrefix(name, prefix)
	})
	if err != nil {
		return ObjectListInfo{}, err
	}
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
	if d.metadataOnlyPriority && metadataOnlyByteLimit(d.metadataOnlyMaxBytes) > driveInlineMaxBytes {
		ctx = withGoogleResponseLimit(ctx, driveLargeMetadataListMaxBytes)
	}
	values := url.Values{}
	values.Set("q", d.containsQuery(contains))
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime)")
	if d.inlineSmallObjects || d.metadataOnlyPriority {
		values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime,description)")
	}
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
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime, InlineData: d.decodeInlineData(item.Name, item.Description, size)})
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
	pageSize := driveListPageSize
	if len(contains) == 2 && isMuxNamespace(contains[0]) && contains[1] == "/p1/" {
		pageSize = strconv.Itoa(d.normalListPageSize)
	}
	return d.listFreshQueryPageStatus(ctx, d.containsQuery(contains), since, pageToken, maxPages, pageSize, func(name string) bool {
		for _, value := range contains {
			if strings.TrimSpace(value) != "" && !strings.Contains(name, value) {
				return false
			}
		}
		return true
	})
}

// ListFreshIndexedClassPageStatus is a probe path. The mux keeps legacy
// discovery until mixed-version peers have a negotiated rollout path.
func (d *DriveStore) ListFreshIndexedClassPageStatus(ctx context.Context, prefix, class string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if d == nil || !d.indexedClassProperties {
		return ObjectListInfo{}, errors.New("indexed class properties are disabled")
	}
	if !isMuxNamespace(prefix) || (class != "p0" && class != "p1") {
		return ObjectListInfo{}, errors.New("indexed class query requires a mux prefix and p0 or p1 class")
	}
	query := d.containsQuery([]string{prefix}) + fmt.Sprintf(" and properties has { key='%s' and value='%s' }", driveIndexedClassProperty, class)
	return d.listFreshQueryPageStatus(ctx, query, since, pageToken, maxPages, driveListPageSize, func(name string) bool {
		return strings.HasPrefix(name, prefix) && indexedMuxClass(name) == class
	})
}

func (d *DriveStore) listFreshQueryPageStatus(ctx context.Context, query string, since time.Time, pageToken string, maxPages int, pageSize string, matches func(string) bool) (ObjectListInfo, error) {
	if d.metadataOnlyPriority && metadataOnlyByteLimit(d.metadataOnlyMaxBytes) > driveInlineMaxBytes {
		pageSize = driveListPageSize
		ctx = withGoogleResponseLimit(ctx, driveLargeMetadataListMaxBytes)
	}
	if maxPages <= 0 {
		maxPages = driveMuxFreshListMaxPages
	}
	values := url.Values{}
	if !since.IsZero() {
		query += fmt.Sprintf(" and modifiedTime >= '%s'", escapeDriveQuery(since.UTC().Format(time.RFC3339Nano)))
	}
	values.Set("q", query)
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime)")
	if d.inlineSmallObjects || d.metadataOnlyPriority {
		values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,size,modifiedTime,description)")
	}
	values.Set("pageSize", pageSize)
	values.Set("orderBy", "modifiedTime desc")
	if strings.TrimSpace(pageToken) != "" {
		values.Set("pageToken", strings.TrimSpace(pageToken))
	}
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	status, err := d.eachFilesPageUntilLimitStatusChecked(ctx, values, "drive list", maxPages, true, func(payload driveListPayload) (bool, error) {
		for _, item := range payload.Files {
			if !matches(item.Name) {
				continue
			}
			if !since.IsZero() && item.ModifiedTime != "" {
				updated, err := time.Parse(time.RFC3339Nano, item.ModifiedTime)
				if err == nil && updated.Before(since) {
					return false, nil
				}
			}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime, InlineData: d.decodeInlineData(item.Name, item.Description, size)})
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
	infos, err := d.listExact(ctx, name, false)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if err := d.DeleteID(ctx, info.ID); err != nil {
			return err
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
		d.forgetMetadataSlot(fileID)
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

// CleanupPrefixesDue skips empty namespaces only when Drive returned a complete
// page. Each selected namespace still gets its own cleanup and deletion budget.
func (d *DriveStore) CleanupPrefixesDue(ctx context.Context, prefixes []string, cutoff time.Time) ([]string, error) {
	if len(prefixes) < 2 {
		return prefixes, nil
	}
	values := url.Values{
		"q":        {d.anyPrefixQuery(prefixes) + " and " + driveNonFolderFilter + " and modifiedTime < '" + cutoff.UTC().Format(time.RFC3339Nano) + "'"},
		"fields":   {"incompleteSearch,nextPageToken,files(name)"},
		"pageSize": {driveListPageSize},
	}
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var due []string
	status, err := d.eachFilesPageUntilLimitStatus(ctx, values, "list cleanup prefixes", 1, func(page driveListPayload) (bool, error) {
		for _, prefix := range prefixes {
			for _, file := range page.Files {
				if strings.HasPrefix(file.Name, prefix) {
					due = append(due, prefix)
					break
				}
			}
		}
		return true, nil
	})
	if err != nil || status.Truncated {
		return prefixes, err
	}
	return due, nil
}

func (d *DriveStore) Cleanup(ctx context.Context, opts DriveCleanupOptions) (DriveCleanupResult, error) {
	prefix := strings.TrimSpace(opts.Prefix)
	if !opts.All && prefix == UDPPreviewCleanupPrefix {
		return d.cleanupUDPPreview(ctx, opts)
	}
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
	seenIDs := make(map[string]struct{})
	values := url.Values{}
	var contains []string
	if !opts.All {
		contains = []string{prefix}
	}
	values.Set("q", d.containsQuery(contains)+" and "+driveNonFolderFilter+" and modifiedTime < '"+cutoff.UTC().Format(time.RFC3339Nano)+"'")
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,mimeType,size,modifiedTime)")
	values.Set("pageSize", driveListPageSize)
	values.Set("orderBy", "modifiedTime asc")
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	status, err := d.eachFilesPageUntilLimitStatus(ctx, values, "drive list", maxPages, func(payload driveListPayload) (bool, error) {
		for _, item := range payload.Files {
			result.Scanned++
			// Deleting a Drive folder also deletes its owned descendants, even
			// when they fall outside this scan's name and age limits.
			if item.MimeType == "" || item.MimeType == driveFolderMimeType || (!opts.All && !strings.HasPrefix(item.Name, prefix)) {
				continue
			}
			updated, err := time.Parse(time.RFC3339Nano, item.ModifiedTime)
			if err != nil {
				continue
			}
			if !updated.Before(cutoff) {
				return false, nil
			}
			if _, seen := seenIDs[item.ID]; item.ID == "" || seen {
				continue
			}
			seenIDs[item.ID] = struct{}{}
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
	infos, err := d.listExact(ctx, name, true)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(infos) == 0 {
		return ObjectInfo{}, fmt.Errorf("drive object not found: %s", name)
	}
	return infos[0], nil
}

func (d *DriveStore) listExact(ctx context.Context, name string, latestOnly bool) ([]ObjectInfo, error) {
	values := url.Values{}
	values.Set("q", d.query(name, true)+" and "+driveNonFolderFilter)
	values.Set("fields", "nextPageToken,incompleteSearch,files(id,name,mimeType,size,modifiedTime)")
	values.Set("orderBy", "modifiedTime desc")
	values.Set("pageSize", driveListPageSize)
	if latestOnly {
		values.Set("pageSize", "1")
	}
	if d.isAppData() {
		values.Set("spaces", "appDataFolder")
	}
	var infos []ObjectInfo
	seen := make(map[string]struct{})
	status, err := d.eachFilesPageUntilLimitStatus(ctx, values, "drive lookup", driveListMaxPages, func(payload driveListPayload) (bool, error) {
		if payload.IncompleteSearch {
			return false, errors.New("Drive exact-name search was incomplete")
		}
		for _, item := range payload.Files {
			if item.Name != name || item.ID == "" || item.MimeType == "" || item.MimeType == driveFolderMimeType {
				continue
			}
			if _, duplicate := seen[item.ID]; duplicate {
				continue
			}
			seen[item.ID] = struct{}{}
			size, _ := strconv.ParseInt(item.Size, 10, 64)
			infos = append(infos, ObjectInfo{Name: item.Name, ID: item.ID, Size: size, Updated: item.ModifiedTime})
			if latestOnly {
				return false, nil // Drive orders the whole result by modifiedTime.
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if status.Truncated {
		return nil, errors.New("Drive exact-name search exceeded its page limit")
	}
	return infos, nil
}

type driveListPayload struct {
	NextPageToken    string `json:"nextPageToken"`
	IncompleteSearch bool   `json:"incompleteSearch"`
	Files            []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		MimeType     string `json:"mimeType"`
		Size         string `json:"size"`
		ModifiedTime string `json:"modifiedTime"`
		Description  string `json:"description"`
	} `json:"files"`
}

func (d *DriveStore) decodeInlineData(name, description string, size int64) []byte {
	if isMetadataOnlyPriorityName(name) {
		if !d.metadataOnlyPriority || size != 0 || !strings.HasPrefix(description, driveMetadataOnlyMarker) {
			return nil
		}
		encoded := strings.TrimPrefix(description, driveMetadataOnlyMarker)
		limit := metadataOnlyByteLimit(d.metadataOnlyMaxBytes)
		if len(encoded) > base64.StdEncoding.EncodedLen(limit) {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || len(data) > limit {
			return nil
		}
		return data
	}
	if !d.inlineSmallObjects || size <= 0 || size > driveInlineMaxBytes || !isMuxNamespace(name) || !strings.Contains(name, "/p0/") || !strings.HasPrefix(description, driveInlineMarker) {
		return nil
	}
	encoded := strings.TrimPrefix(description, driveInlineMarker)
	if len(encoded) > base64.StdEncoding.EncodedLen(driveInlineMaxBytes) {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || int64(len(data)) != size {
		return nil
	}
	return data
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
	return d.eachFilesPageUntilLimitStatusChecked(ctx, values, op, maxPages, false, fn)
}

func (d *DriveStore) eachFilesPageUntilLimitStatusChecked(ctx context.Context, values url.Values, op string, maxPages int, checkClock bool, fn func(driveListPayload) (bool, error)) (driveListPageStatus, error) {
	if maxPages <= 0 {
		maxPages = driveListMaxPages
	}
	pageValues := cloneValues(values)
	incomplete := false
	for page := 0; page < maxPages; page++ {
		result, err := d.requestFilesPage(ctx, "/drive/v3/files?"+pageValues.Encode(), op, checkClock)
		if err != nil {
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

func (d *DriveStore) requestFilesPage(ctx context.Context, path, op string, checkClock bool) (*HTTPResult, error) {
	var headers map[string]string
	for {
		result, err := d.request(ctx, http.MethodGet, path, headers, nil)
		if err != nil {
			return nil, err
		}
		if err := require2xx(result, op); err != nil {
			return nil, err
		}
		if !checkClock {
			return result, nil
		}
		err = checkDriveServerClock(result.Header, result.RequestStarted, result.ResponseReceived)
		if err == nil {
			return result, nil
		}
		if headers != nil {
			return nil, err
		}
		// Discard the entire suspect page. Revalidate the same query once
		// before letting a single stale Date tear down every active stream.
		headers = map[string]string{"Cache-Control": "no-cache"}
	}
}

// A fresh mailbox query uses Drive's modifiedTime as its lower bound. A device
// clock far ahead misses new traffic; one far behind can replay an old OPEN.
// The Date header must fall within the request's local time interval, allowing
// for HTTP second precision and small server clock differences. Never widen
// the 30-second startup catch-up to compensate for clock skew.
func checkDriveServerClock(header http.Header, requestStarted, requestEnded time.Time) error {
	_, err := verifiedDriveServerTime(header, requestStarted, requestEnded)
	return err
}

func verifiedDriveServerTime(header http.Header, requestStarted, requestEnded time.Time) (time.Time, error) {
	if requestEnded.Before(requestStarted) || requestEnded.Sub(requestStarted) > driveClockCheckMaxDuration {
		return time.Time{}, fmt.Errorf("%w: Drive list took too long for a reliable time comparison", ErrDriveClockUnverified)
	}
	date := strings.TrimSpace(header.Get("Date"))
	if date == "" {
		return time.Time{}, fmt.Errorf("%w: Drive list response did not include a Date header", ErrDriveClockUnverified)
	}
	serverTime, err := http.ParseTime(date)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: Drive list response had an invalid Date header", ErrDriveClockUnverified)
	}
	// HTTP dates have no fractional seconds. Compare the earliest
	// acceptable second in the same precision as the observation.
	if serverTime.Before(requestStarted.Add(-driveClockTolerance).Truncate(time.Second)) {
		return time.Time{}, fmt.Errorf("%w: this device is ahead of Google Drive", ErrDriveClockSkew)
	}
	if serverTime.After(requestEnded.Add(driveClockTolerance)) {
		return time.Time{}, fmt.Errorf("%w: this device is behind Google Drive", ErrDriveClockSkew)
	}
	return serverTime, nil
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
	return d.requestWithHTTP(ctx, d.http, method, path, headers, body)
}

func (d *DriveStore) requestWithHTTP(ctx context.Context, httpClient *GoogleHTTPClient, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	return d.requestWithHTTPMode(ctx, httpClient, method, path, headers, body, false)
}

func (d *DriveStore) requestSlotPatch(ctx context.Context, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	httpClient := d.http
	if d.slotWriteHTTP != nil {
		httpClient = d.slotWriteHTTP
	}
	return d.requestWithHTTPMode(ctx, httpClient, http.MethodPatch, path, headers, body, true)
}

func (d *DriveStore) requestWithHTTPMode(ctx context.Context, httpClient *GoogleHTTPClient, method, path string, headers map[string]string, body []byte, singleAttempt bool) (*HTTPResult, error) {
	return d.requestWithHTTPTimeout(ctx, httpClient, method, path, headers, body, singleAttempt, 0)
}

// A positive timeout bounds each admitted authentication and HTTP attempt.
// Quota waits, including one before a 401 retry, still obey the caller's context.
func (d *DriveStore) requestWithHTTPTimeout(ctx context.Context, httpClient *GoogleHTTPClient, method, path string, headers map[string]string, body []byte, singleAttempt bool, timeout time.Duration) (*HTTPResult, error) {
	if httpClient == nil {
		return nil, errors.New("drive http client is nil")
	}
	if d.quotaProject != "" {
		if err := ValidateGoogleProjectID(d.quotaProject); err != nil {
			return nil, fmt.Errorf("drive quota project: %w", err)
		}
	}
	var last *HTTPResult
	label := driveRequestLabel(method, path)
	if ctx.Value(driveUploadCommitProbeKey{}) == true && method == http.MethodGet {
		label = "upload_commit_probe"
	}
	if ctx.Value(driveSlotCleanupRequestKey{}) == true && method == http.MethodGet {
		label = "slot_cleanup_probe"
	}
	started := time.Now()
	for attempt := 0; attempt < 2; attempt++ {
		if d.backoff != nil {
			if err := d.backoff.Wait(ctx, label, d.Logger); err != nil {
				return nil, err
			}
		}
		requestCtx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			requestCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel() // At most two attempts, including token refresh.
		}
		merged, err := d.authHeaders(requestCtx)
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			if strings.EqualFold(key, "X-Goog-User-Project") {
				continue
			}
			merged[key] = value
		}
		if d.quotaProject != "" {
			merged["X-Goog-User-Project"] = d.quotaProject
		}
		var result *HTTPResult
		if singleAttempt {
			result, err = httpClient.RequestSingleAttempt(requestCtx, method, "www.googleapis.com", path, merged, body)
		} else {
			// A different request can trip the quota gate while this request is
			// sleeping after a transient HTTP or network failure.
			result, err = httpClient.request(requestCtx, method, "www.googleapis.com", path, merged, body, false, func() error {
				return d.WaitForDriveQuota(requestCtx, label)
			})
		}
		if err != nil {
			if errors.Is(err, errDriveUploadDuplicateBudget) {
				return nil, err
			}
			d.logDriveRequest(label, httpAttemptCount(result, err), 0, nil, time.Since(started), err)
			return nil, err
		}
		last = result
		httpAttempts := result.Attempts
		if httpAttempts < 1 {
			httpAttempts = 1
		}
		d.logDriveRequest(label, httpAttempts, result.Status, result.Body, time.Since(started), nil)
		if result.Status != http.StatusUnauthorized {
			if d.backoff != nil {
				d.backoff.Observe(label, result, d.Logger)
			}
			return result, nil
		}
		if d.tokenSource == nil {
			return result, nil
		}
		if d.Logger != nil {
			d.Logger.Printf("drive auth rejected op=%s status=401 action=refresh-token", label)
		}
		d.tokenSource.invalidateRejectedToken(strings.TrimPrefix(merged["Authorization"], "Bearer "))
		started = time.Now()
	}
	return last, nil
}

func (d *DriveStore) logDriveRequest(label string, attempts int, status int, body []byte, duration time.Duration, err error) {
	canceled := errors.Is(err, context.Canceled)
	if attempts < 1 {
		attempts = 1
	}
	if d.quota != nil {
		quotaErr := err
		if canceled {
			quotaErr = nil
		} // Intentional hedge cancellation is not an API failure.
		if report, ok := d.quota.Record(label, status, len(body), duration, quotaErr, driveQuotaErrorReason(status, body, quotaErr), attempts); ok && d.Logger != nil {
			d.Logger.Printf("drive quota window=%s calls=%d est_units=%d errors=%d response_bytes=%d ops=%s",
				report.Duration.Round(time.Second), report.Calls, report.Units, report.Errors, report.ResponseBytes, report.OpSummary())
		}
	}
	if canceled || d.Logger == nil {
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
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	delay := retryAfter(result)
	// Requests already in flight when another response gated the store are
	// part of the same rate-limit wave. A delayed response cannot amplify
	// the local backoff, but an explicit Retry-After still sets a floor.
	newWave := !now.Before(b.until) && (result.RequestStarted.IsZero() || !result.RequestStarted.Before(b.until))
	if newWave {
		if delay <= 0 {
			delay = b.nextDelayLocked()
		}
		b.fails++
	}
	if delay <= 0 || !now.Add(delay).After(b.until) {
		return
	}
	b.until = now.Add(delay)
	b.reason = reason
	b.op = op
	if logger != nil && time.Since(b.lastLog) >= driveQuotaBackoffLogInterval {
		logger.Printf("drive quota backoff tripped op=%s status=%d reason=%s delay=%s failures=%d", op, result.Status, reason, time.Until(b.until).Round(time.Millisecond), b.fails)
		b.lastLog = now
	}
}

func (b *driveQuotaBackoff) noteNonRateLimitedResult(result *HTTPResult) {
	if result == nil || result.Status < 200 || result.Status >= 300 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// A success from an already in-flight request is not evidence that the
	// provider accepted traffic after the latest cooldown.
	if !time.Now().Before(b.until) && (result.RequestStarted.IsZero() || !result.RequestStarted.Before(b.until)) {
		b.fails = 0
		b.reason = ""
		b.op = ""
	}
}

func (b *driveQuotaBackoff) nextDelayLocked() time.Duration {
	failures := b.fails
	base := b.base
	maximum := b.max
	jitter := b.jitter
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
	reason := normalizeGoogleErrorReason(driveErrorReason(result.Body))
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
	nextSample      int
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
	expectedMissing := status == http.StatusNotFound && (op == "upload_commit_probe" || op == "slot_cleanup_probe")
	failed := err != nil || status >= 400 && !expectedMissing
	failedAttempts := attempts - 1 // Every retry follows a failed HTTP attempt.
	if failed {
		failedAttempts = attempts
	}
	if failedAttempts > 0 && errorReason == "" {
		errorReason = "retry_recovered"
	}
	s.calls += attempts
	s.units += units
	s.bytes += int64(responseBytes)
	s.totalCalls += attempts
	s.totalUnits += units
	s.totalBytes += int64(responseBytes)
	if failedAttempts > 0 {
		s.errors += failedAttempts
		s.totalError += failedAttempts
		s.lastErrorReason = errorReason
		s.totalLastErrorReason = errorReason
	}
	current := s.ops[op]
	current.Calls += attempts
	current.Units += units
	if failedAttempts > 0 {
		current.Errors += failedAttempts
		current.LastErrorReason = errorReason
	}
	s.ops[op] = current
	totalCurrent := s.totalOps[op]
	totalCurrent.Calls += attempts
	totalCurrent.Units += units
	addDriveQuotaDuration(&totalCurrent, durationMS)
	if failedAttempts > 0 {
		totalCurrent.Errors += failedAttempts
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

// QuotaCounters returns cumulative local counters without building operation statistics.
func (d *DriveStore) QuotaCounters() (units, failures int64) {
	if d == nil || d.quota == nil {
		return 0, 0
	}
	d.quota.mu.Lock()
	defer d.quota.mu.Unlock()
	return d.quota.totalUnits, d.quota.totalError
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
	if len(stats.SamplesMS) < driveQuotaMaxSamplesPerOp {
		stats.SamplesMS = append(stats.SamplesMS, durationMS)
		return
	}
	stats.SamplesMS[stats.nextSample] = durationMS
	stats.nextSample++
	if stats.nextSample == len(stats.SamplesMS) {
		stats.nextSample = 0
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
	if len(stats.SamplesMS) > 0 {
		samples := slices.Clone(stats.SamplesMS)
		slices.Sort(samples)
		out.P50DurationMS = samples[len(samples)/2]
		out.P95DurationMS = samples[len(samples)*95/100]
	}
	return out
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
	case "upload", "delete", "create", "patch", "update":
		return 50
	default:
		return 5
	}
}

func driveErrorReason(body []byte) string {
	reason, message := googleAPIErrorDetails(body)
	if reason != "" {
		return reason
	}
	if message != "" {
		if len(message) > 80 {
			message = message[:80]
		}
		return message
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

func (d *DriveStore) anyPrefixQuery(prefixes []string) string {
	if len(prefixes) < 2 {
		return d.containsQuery(prefixes)
	}
	alternatives := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		alternatives = append(alternatives, "name contains '"+escapeDriveQuery(prefix)+"'")
	}
	return d.containsQuery(nil) + " and (" + strings.Join(alternatives, " or ") + ")"
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
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/changes/startPageToken?"):
		return "changes_start_token"
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
