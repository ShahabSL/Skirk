package skirk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	UDPPreviewCleanupPrefix     = "udp-pilot/"
	UDPPreviewCleanupDefaultAge = 2 * time.Minute
)

const (
	// The datagram lifetime is six seconds, and create, list and fetch each
	// take at most two seconds. Keep a margin for within-lease clock skew.
	udpPreviewCleanupMinAge   = time.Minute
	udpPreviewCleanupMaxPages = 8
	udpPreviewCleanupMaxFiles = 500
	udpPreviewCleanupMinPace  = 100 * time.Millisecond
	udpPreviewCleanupTimeout  = 2 * time.Minute
)

// Preview objects are immutable. A pass can collect orphans from prior
// leases after restart without knowing any lease IDs or reading payloads.
func (d *DriveStore) cleanupUDPPreview(ctx context.Context, opts DriveCleanupOptions) (DriveCleanupResult, error) {
	result := DriveCleanupResult{Prefix: UDPPreviewCleanupPrefix, DryRun: opts.DryRun}
	if d == nil || !validPilotMediaID(d.folderID) || d.folderID == "root" || d.folderID == "appDataFolder" || d.isAppData() {
		return result, errors.New("UDP preview cleanup requires an explicit Drive mailbox folder")
	}
	if opts.OlderThan < 0 {
		return result, errors.New("cleanup older-than must be non-negative")
	}
	now := time.Now()
	if !opts.Now.IsZero() && opts.Now.Before(now) {
		now = opts.Now
	}
	age := max(opts.OlderThan, udpPreviewCleanupMinAge)
	result.Cutoff = now.Add(-age).UTC()
	maxPages, maxFiles := udpPreviewCleanupMaxPages, udpPreviewCleanupMaxFiles
	if opts.MaxPages > 0 {
		maxPages = min(maxPages, opts.MaxPages)
	}
	if opts.MaxDeletes > 0 {
		maxFiles = min(maxFiles, opts.MaxDeletes)
	}
	ctx, cancel := context.WithTimeout(ctx, udpPreviewCleanupTimeout)
	defer cancel()
	cutoff := "'" + result.Cutoff.Format(time.RFC3339Nano) + "'"
	values := url.Values{
		"q":        {d.containsQuery([]string{UDPPreviewCleanupPrefix}) + " and createdTime < " + cutoff + " and modifiedTime < " + cutoff},
		"spaces":   {"drive"},
		"fields":   {"nextPageToken,incompleteSearch,files(id,name,parents,mimeType,createdTime,modifiedTime,size)"},
		"pageSize": {driveListPageSize},
		"orderBy":  {"modifiedTime asc"},
	}
	var candidates []string
	seenIDs := make(map[string]bool)
	seenPages := make(map[string]bool)
	for range maxPages {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if d.QuotaBackoffSnapshot().Active {
			result.Truncated = true
			return result, nil
		}
		req := withGoogleResponseLimitAtMost(ctx, 128<<10)
		reply, err := d.requestWithHTTPMode(req, d.http, http.MethodGet, "/drive/v3/files?"+values.Encode(), nil, nil, true)
		if err != nil {
			return result, err
		}
		if isDriveRateLimitResult(reply) {
			result.Truncated = true
			return result, nil
		}
		if err := require2xx(reply, "list expired UDP preview objects"); err != nil {
			return result, err
		}
		serverTime, err := verifiedDriveServerTime(reply.Header, reply.RequestStarted, reply.ResponseReceived)
		if err != nil {
			return result, err
		}
		// Use the earlier boundary if the local clock leads Drive's clock.
		// Keep one cutoff across the entire paginated query.
		if result.Pages == 0 {
			if serverCutoff := serverTime.Add(-age); serverCutoff.Before(result.Cutoff) {
				result.Cutoff = serverCutoff.UTC()
			}
		}
		var page struct {
			NextPageToken    string
			IncompleteSearch bool
			Files            []struct {
				ID, Name, MimeType, CreatedTime, ModifiedTime, Size string
				Parents                                             []string
			}
		}
		if err := json.Unmarshal(reply.Body, &page); err != nil {
			return result, err
		}
		result.Pages++
		result.Truncated = result.Truncated || page.IncompleteSearch
		for _, file := range page.Files {
			result.Scanned++
			if !validPilotMediaID(file.ID) || seenIDs[file.ID] || !validUDPPreviewCleanupName(file.Name) ||
				file.MimeType != "application/octet-stream" || len(file.Parents) != 1 || file.Parents[0] != d.folderID {
				continue
			}
			created, createdErr := time.Parse(time.RFC3339Nano, file.CreatedTime)
			updated, updatedErr := time.Parse(time.RFC3339Nano, file.ModifiedTime)
			if createdErr != nil || updatedErr != nil || !created.Before(result.Cutoff) || !updated.Before(result.Cutoff) {
				continue
			}
			seenIDs[file.ID] = true
			candidates = append(candidates, file.ID)
			result.Matched++
			if size, err := strconv.ParseInt(file.Size, 10, 64); err == nil && size >= 0 && size <= pilotMediaWidePlain+pilotMediaOverhead {
				result.MatchedSize += size
			}
			if len(candidates) == maxFiles {
				break
			}
		}
		if len(candidates) == maxFiles {
			result.Truncated = true
			break
		}
		if page.NextPageToken == "" {
			break
		}
		if seenPages[page.NextPageToken] || result.Pages == maxPages {
			result.Truncated = true
			break
		}
		seenPages[page.NextPageToken] = true
		values.Set("pageToken", page.NextPageToken)
	}
	if opts.DryRun {
		return result, nil
	}
	for i, id := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if d.QuotaBackoffSnapshot().Active {
			result.Truncated = true
			return result, nil
		}
		req := withGoogleResponseLimitAtMost(ctx, 8<<10)
		reply, err := d.requestWithHTTPMode(req, d.http, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(id), nil, nil, true)
		if err == nil && (reply.Status >= 200 && reply.Status < 300 || reply.Status == http.StatusNotFound) {
			result.Deleted++
		} else {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Failed++
			if isDriveRateLimitResult(reply) {
				result.Truncated = true
				return result, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if i+1 < len(candidates) {
			timer := time.NewTimer(max(opts.DeleteDelay, udpPreviewCleanupMinPace))
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return result, nil
}

func validUDPPreviewCleanupName(name string) bool {
	suffix, ok := strings.CutPrefix(name, UDPPreviewCleanupPrefix)
	if !ok {
		return false
	}
	parts := strings.SplitN(suffix, "/", 4)
	if len(parts) != 3 || len(parts[0]) != 32 || (parts[1] != "1" && parts[1] != "2") || len(parts[2]) != 20 {
		return false
	}
	var lease [16]byte
	if _, err := hex.Decode(lease[:], []byte(parts[0])); err != nil || hex.EncodeToString(lease[:]) != parts[0] {
		return false
	}
	seq, err := strconv.ParseUint(parts[2], 10, 64)
	return err == nil && seq > 0 && seq <= maxSequence
}
