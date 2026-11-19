package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	pilotMediaMaxPlain        = 64 << 10
	pilotMediaWidePlain       = 128 << 10
	pilotMediaWideBatch       = 128
	pilotMediaReadSlots       = 2
	pilotMediaReadTimeout     = 1600 * time.Millisecond
	pilotMediaRecoveryTimeout = 2 * pilotMediaReadTimeout
	pilotMediaOverhead        = headerLen + 16 // The envelope header and AES-GCM tag.
	pilotMediaMinWire         = pilotMediaOverhead + len(pilotDatagramMagic) + pilotPacketHeader
	pilotMediaMaxWire         = pilotMediaMaxPlain + pilotMediaOverhead
)

var errPilotMediaBackoff = errors.New("UDP media skipped during Drive quota backoff")

func (p *pilotMailbox) mediaPlainLimit() int {
	if p.mediaBytes == pilotMediaWidePlain {
		return pilotMediaWidePlain
	}
	return pilotMediaMaxPlain
}

func pilotMediaPacketLimit(plainLimit int) int {
	if plainLimit == pilotMediaWidePlain {
		return pilotMediaWideBatch
	}
	return pilotMaxBatch
}

func (p *pilotMailbox) validMediaSize(size int) bool {
	return size >= pilotMediaMinWire && size <= p.mediaPlainLimit()+pilotMediaOverhead
}

func validPilotMediaID(id string) bool {
	if len(id) == 0 || len(id) > 512 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

// uploadMediaBatch publishes one already sealed batch. A failed or ambiguous
// create must not be retried as another object with the same batch sequence.
func (p *pilotMailbox) uploadMediaBatch(ctx context.Context, name string, wire []byte) error {
	prefix := p.dirPrefix(p.sendDir)
	suffix, inScope := strings.CutPrefix(name, prefix)
	seq, err := strconv.ParseUint(suffix, 10, 64)
	if !inScope || len(suffix) != 20 || err != nil || seq == 0 || seq > maxSequence {
		return errors.New("invalid UDP media batch name")
	}
	if !p.validMediaSize(len(wire)) {
		return errors.New("invalid UDP media batch size")
	}
	if p.drive == nil || !validPilotMediaID(p.drive.folderID) || p.drive.folderID == "root" ||
		p.drive.folderID == "appDataFolder" || p.drive.isAppData() {
		return errors.New("UDP media requires an explicit Drive mailbox folder")
	}
	meta, err := json.Marshal(struct {
		Name     string   `json:"name"`
		Parents  []string `json:"parents"`
		MimeType string   `json:"mimeType"`
	}{name, []string{p.drive.folderID}, "application/octet-stream"})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	parts := []struct {
		contentType string
		data        []byte
	}{
		{"application/json; charset=UTF-8", meta},
		{"application/octet-stream", wire},
	}
	for _, part := range parts {
		out, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {part.contentType}})
		if err != nil {
			return err
		}
		if _, err := out.Write(part.data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}

	op, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := p.http
	if p.adaptiveUploads != nil {
		client = p.adaptiveUploads.mediaHTTP.Load()
	}
	result, err := p.requestPilotBatchOnce(withGoogleResponseLimitAtMost(op, 16<<10), client, http.MethodPost,
		"/upload/drive/v3/files?uploadType=multipart&fields=id,name,size",
		map[string]string{"Content-Type": "multipart/related; boundary=" + writer.Boundary()}, body.Bytes())
	if err != nil {
		return err
	}
	if err := require2xx(result, "create UDP media batch"); err != nil {
		return err
	}
	var file struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size string `json:"size"`
	}
	if err := json.Unmarshal(result.Body, &file); err != nil {
		return err
	}
	if !validPilotMediaID(file.ID) || file.Name != name || file.Size != strconv.Itoa(len(wire)) {
		return errors.New("Drive returned unexpected UDP media batch metadata")
	}
	return nil
}

// fetchMediaBatch fetches a previously discovered ID with a validated size.
// The caller still authenticates the envelope against the lease and filename
// before treating it as delivered or queuing the ID for cleanup.
func (p *pilotMailbox) fetchMediaBatch(ctx context.Context, id string, exactSize int) ([]byte, error) {
	if !validPilotMediaID(id) || !p.validMediaSize(exactSize) {
		return nil, errors.New("invalid UDP media batch ID or size")
	}
	result, err := p.requestPilotBatchOnce(withGoogleResponseLimitAtMost(ctx, int64(exactSize)), p.http, http.MethodGet,
		"/drive/v3/files/"+url.PathEscape(id)+"?alt=media", driveMediaDownloadHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if err := require2xx(result, "get UDP media batch"); err != nil {
		return nil, err
	}
	if result.Status != http.StatusOK || result.Header.Get("Content-Range") != "" || len(result.Body) != exactSize {
		return nil, errors.New("Drive returned an incomplete or unexpected UDP media batch")
	}
	return result.Body, nil
}

// These workers avoid waiting through backoff or retrying a 401 in place.
// Record each attempt in the shared Drive counters and let later work observe
// any quota backoff or refresh the invalidated token.
func (p *pilotMailbox) requestPilotBatchOnce(ctx context.Context, client *GoogleHTTPClient, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	if p.drive == nil || client == nil {
		return nil, errors.New("UDP batch Drive connection is not configured")
	}
	drive := p.drive
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if drive.QuotaBackoffSnapshot().Active {
		return nil, errPilotMediaBackoff
	}
	if drive.quotaProject != "" {
		if err := ValidateGoogleProjectID(drive.quotaProject); err != nil {
			return nil, err
		}
	}
	merged, err := drive.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		merged[key] = value
	}
	if drive.quotaProject != "" {
		merged["X-Goog-User-Project"] = drive.quotaProject
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if drive.QuotaBackoffSnapshot().Active {
		return nil, errPilotMediaBackoff
	}
	label := driveRequestLabel(method, path)
	started := time.Now()
	result, err := client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, merged, body)
	if err != nil {
		drive.logDriveRequest(label, httpAttemptCount(result, err), 0, nil, time.Since(started), err)
		if method == http.MethodPost && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", errPilotUploadDeadline, err)
		}
		return nil, err
	}
	result.RequestStarted = started
	result.ResponseReceived = time.Now()
	drive.logDriveRequest(label, result.Attempts, result.Status, result.Body, time.Since(started), nil)
	drive.backoff.Observe(label, result, drive.Logger)
	if result.Status == http.StatusUnauthorized && drive.tokenSource != nil {
		drive.tokenSource.invalidateRejectedToken(strings.TrimPrefix(merged["Authorization"], "Bearer "))
	}
	return result, nil
}
