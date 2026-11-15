// Bounded two-file cumulative metadata mailbox probe; not a production transport.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	frameCount          = 20
	frameBytes          = 64
	maxGets             = 200
	maxDataPatches      = 40
	maxAckPatches       = 20
	interval            = 250 * time.Millisecond
	defaultPollInterval = 200 * time.Millisecond
	pauseAfter          = 10
	hotQuotaCeiling     = 6500 // Reserve <=3300 units for six deletes and twelve verification reads.
)

type options struct {
	config        string
	out           string
	live          bool
	readerPauseMS int
	pollMS        int
	roundtrip     bool
}

type metadata struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type frame struct {
	Sequence int    `json:"sequence"`
	Payload  string `json:"payload"`
	Tag      string `json:"tag"`
}

type dataSnapshot struct {
	Frames []frame `json:"frames"`
}

type ackSnapshot struct {
	Sequence int    `json:"sequence"`
	Tag      string `json:"tag"`
}

type report struct {
	SchemaVersion        int     `json:"schema_version"`
	Route                string  `json:"route"`
	FramesScheduled      int     `json:"frames_scheduled"`
	FramesReceived       int     `json:"frames_received"`
	FramesAcknowledged   int     `json:"frames_acknowledged"`
	DataPatches          int     `json:"data_patches"`
	AckPatches           int     `json:"ack_patches"`
	Gets                 int     `json:"gets"`
	StaleDataGets        int     `json:"stale_data_gets"`
	StaleAckGets         int     `json:"stale_ack_gets"`
	SkippedDataVersions  int64   `json:"skipped_data_versions"`
	MaxSnapshotFrames    int     `json:"max_snapshot_frames"`
	MaxUnacknowledged    int     `json:"max_unacknowledged"`
	ForcedPauseCompleted bool    `json:"forced_pause_completed"`
	ReaderPauseMS        int     `json:"reader_pause_ms"`
	TextScheduledP50MS   float64 `json:"text_scheduled_p50_ms,omitempty"`
	TextScheduledP95MS   float64 `json:"text_scheduled_p95_ms,omitempty"`
	TextScheduledMaxMS   float64 `json:"text_scheduled_max_ms,omitempty"`
	WorkloadCompletedUTC string  `json:"workload_completed_utc,omitempty"`
	CleanupVerified      bool    `json:"cleanup_verified"`
	CleanupDeletes       int     `json:"cleanup_deletes"`
	CleanupGets          int     `json:"cleanup_gets"`
	QuotaUnitsUpperBound int     `json:"quota_units_upper_bound"`
	Failure              string  `json:"failure,omitempty"`
	OK                   bool    `json:"ok"`
}

func parseOptions(args []string) (options, error) {
	var value options
	flags := flag.NewFlagSet("ack-slot-probe", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&value.config, "config", "", "isolated test exit config")
	flags.StringVar(&value.out, "out", "", "fresh private JSON under .skirk-runs")
	flags.BoolVar(&value.live, "live", false, "allow bounded Drive calls")
	flags.IntVar(&value.readerPauseMS, "reader-pause-ms", 1000, "bounded reader pause after frame ten; 0 disables delay")
	flags.IntVar(&value.pollMS, "poll-ms", int(defaultPollInterval/time.Millisecond), "known-ID poll interval, 100-500 ms")
	flags.BoolVar(&value.roundtrip, "roundtrip", false, "run bounded two-way request/reply probe")
	if err := flags.Parse(args); err != nil {
		return value, err
	}
	if !value.live || value.config == "" || value.out == "" || len(flags.Args()) != 0 {
		return value, errors.New("requires --live, --config and --out")
	}
	if value.readerPauseMS < 0 || value.readerPauseMS > 1000 {
		return value, errors.New("--reader-pause-ms must be between 0 and 1000")
	}
	if value.pollMS < 100 || value.pollMS > 500 {
		return value, errors.New("--poll-ms must be between 100 and 500")
	}
	if err := validateOutputPath(value.out); err != nil {
		return value, err
	}
	return value, nil
}

func validateOutputPath(path string) error {
	if filepath.Ext(path) != ".json" {
		return errors.New("--out must be JSON")
	}
	root, err := filepath.EvalSymlinks(".skirk-runs")
	if err != nil {
		return errors.New("private run root must exist")
	}
	root, _ = filepath.Abs(root)
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return errors.New("private output directory must exist")
	}
	parent, _ = filepath.Abs(parent)
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("--out must be inside .skirk-runs")
	}
	for _, candidate := range []string{path, path + ".recovery-ids"} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			return errors.New("private result or recovery record already exists")
		}
	}
	return nil
}

type state struct {
	mu              sync.Mutex
	produced        int
	published       int
	received        int
	ackPublished    int
	acked           int
	producerDone    bool
	forcedPauseDone bool
	maxSnapshot     int
	maxUnacked      int
	staleDataGets   int
	staleAckGets    int
	skippedVersions int64
	latencies       []float64
	scheduled       [frameCount]time.Time
	payloads        [frameCount][]byte
	tags            [frameCount]string
}

type probe struct {
	config        *skirk.Config
	tokens        *skirk.AccessTokenSource
	store         *skirk.DriveStore
	dataWriter    *skirk.GoogleHTTPClient
	dataReader    *skirk.GoogleHTTPClient
	ackWriter     *skirk.GoogleHTTPClient
	ackReader     *skirk.GoogleHTTPClient
	key           [32]byte
	state         state
	budgetMu      sync.Mutex
	gets          int
	dataWrites    int
	ackWrites     int
	units         int
	reservedUnits int
	recovery      string
	recoveryOwned bool
	readerPause   time.Duration
	pollInterval  time.Duration
	roundtrip     bool
}

func newProbe(config *skirk.Config, outputPath string, readerPause time.Duration) *probe {
	tokens := skirk.NewAccessTokenSource(config.Auth, config.Route)
	return &probe{
		config: config, tokens: tokens,
		store:        skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(config.Route), tokens, config.Drive),
		dataWriter:   skirk.NewGoogleHTTPClient(config.Route),
		dataReader:   skirk.NewGoogleHTTPClient(config.Route),
		ackWriter:    skirk.NewGoogleHTTPClient(config.Route),
		ackReader:    skirk.NewGoogleHTTPClient(config.Route),
		recovery:     outputPath + ".recovery-ids",
		readerPause:  readerPause,
		pollInterval: defaultPollInterval,
	}
}

// HMAC verifies synthetic probe frames; production mux frames use AEAD instead.
func tag(key [32]byte, domain byte, sequence int, payload []byte) string {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte{domain})
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(sequence))
	mac.Write(number[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (p *probe) dataDescription(first, last int) (string, error) {
	value := dataSnapshot{Frames: make([]frame, 0, last-first+1)}
	for sequence := first; sequence <= last; sequence++ {
		index := sequence - 1
		value.Frames = append(value.Frames, frame{Sequence: sequence,
			Payload: base64.RawURLEncoding.EncodeToString(p.state.payloads[index]),
			Tag:     p.state.tags[index]})
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	description := "skirk-ack-data-v1:" + base64.RawURLEncoding.EncodeToString(body)
	if len(description) > 8192 {
		return "", errors.New("snapshot_size_cap")
	}
	return description, nil
}

func (p *probe) request(ctx context.Context, client *skirk.GoogleHTTPClient, method, path string, body []byte) (*skirk.HTTPResult, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache"}
	if body != nil {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	if project := p.config.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	return client.RequestNoRateLimitRetry(ctx, method, "www.googleapis.com", path, headers, body)
}

func requestAttempts(result *skirk.HTTPResult) int {
	if result != nil && result.Attempts > 0 {
		return result.Attempts
	}
	return 5
}

func (p *probe) reserve(kind string, cleanup bool) error {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	cost := 25
	if kind != "get" {
		cost = 250
	}
	hotCeiling := hotQuotaCeiling
	getCap, dataCap, ackCap := maxGets, maxDataPatches, maxAckPatches
	if p.roundtrip {
		hotCeiling, getCap, dataCap, ackCap = 12000, 260, 70, 70
	}
	if !cleanup && p.units+p.reservedUnits+cost > hotCeiling {
		return errors.New("quota_cap")
	}
	switch kind {
	case "get":
		limit := getCap - 12
		if cleanup {
			limit = getCap
		}
		if p.gets >= limit {
			return errors.New("get_cap")
		}
		p.gets++
	case "data":
		if p.dataWrites >= dataCap {
			return errors.New("data_patch_cap")
		}
		p.dataWrites++
	case "ack":
		if p.ackWrites >= ackCap {
			return errors.New("ack_patch_cap")
		}
		p.ackWrites++
	default:
		return errors.New("invalid_operation")
	}
	p.reservedUnits += cost
	return nil
}

func (p *probe) recordUnits(result *skirk.HTTPResult, cost int) {
	p.budgetMu.Lock()
	p.reservedUnits -= 5 * cost
	p.units += requestAttempts(result) * cost
	p.budgetMu.Unlock()
}

func (p *probe) get(ctx context.Context, client *skirk.GoogleHTTPClient, fileID string, cleanup bool) (metadata, int, error) {
	if err := p.reserve("get", cleanup); err != nil {
		return metadata{}, 0, err
	}
	result, err := p.request(ctx, client, http.MethodGet, "/drive/v3/files/"+url.PathEscape(fileID)+"?fields=id,version,description", nil)
	p.recordUnits(result, 5)
	if err != nil || result == nil {
		return metadata{}, 0, errors.New("metadata_get_transport")
	}
	if result.Status == http.StatusNotFound {
		return metadata{}, result.Status, nil
	}
	if result.Status != http.StatusOK {
		return metadata{}, result.Status, errors.New("metadata_get_status")
	}
	var value metadata
	if err := json.Unmarshal(result.Body, &value); err != nil || value.ID != fileID || value.Version == "" {
		return metadata{}, result.Status, errors.New("metadata_get_invalid")
	}
	return value, result.Status, nil
}

func (p *probe) patch(ctx context.Context, client *skirk.GoogleHTTPClient, kind, fileID, description string) error {
	if err := p.reserve(kind, false); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"description": description})
	result, err := p.request(ctx, client, http.MethodPatch, "/drive/v3/files/"+url.PathEscape(fileID)+"?fields=id,version,description", body)
	p.recordUnits(result, 50)
	if err != nil || result == nil {
		return errors.New("metadata_patch_transport")
	}
	if result.Status != http.StatusOK {
		return errors.New("metadata_patch_status")
	}
	var value metadata
	if err := json.Unmarshal(result.Body, &value); err != nil || value.ID != fileID || value.Description != description {
		return errors.New("metadata_patch_invalid")
	}
	return nil
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *probe) produce(ctx context.Context) error {
	start := time.Now()
	for sequence := 1; sequence <= frameCount; sequence++ {
		if !sleep(ctx, time.Until(start.Add(time.Duration(sequence-1)*interval))) {
			return errors.New("workload_canceled")
		}
		p.state.mu.Lock()
		p.state.scheduled[sequence-1] = start.Add(time.Duration(sequence-1) * interval)
		p.state.produced = sequence
		p.state.mu.Unlock()
	}
	p.state.mu.Lock()
	p.state.producerDone = true
	p.state.mu.Unlock()
	return nil
}

func (p *probe) writeData(ctx context.Context, fileID string) error {
	for {
		p.state.mu.Lock()
		first, last := p.state.acked+1, p.state.produced
		published, acked, done := p.state.published, p.state.acked, p.state.producerDone
		p.state.mu.Unlock()
		if done && published == frameCount && acked == frameCount {
			return nil
		}
		if last <= published {
			if !sleep(ctx, 25*time.Millisecond) {
				return errors.New("writer_canceled")
			}
			continue
		}
		description, err := p.dataDescription(first, last)
		if err != nil {
			return errors.New("snapshot_size_cap")
		}
		if err := p.patch(ctx, p.dataWriter, "data", fileID, description); err != nil {
			return err
		}
		p.state.mu.Lock()
		p.state.published = last
		if count := last - first + 1; count > p.state.maxSnapshot {
			p.state.maxSnapshot = count
		}
		if unacked := last - p.state.acked; unacked > p.state.maxUnacked {
			p.state.maxUnacked = unacked
		}
		p.state.mu.Unlock()
	}
}

func (p *probe) verifyData(description string) ([]frame, error) {
	if !strings.HasPrefix(description, "skirk-ack-data-v1:") {
		return nil, errors.New("data_marker_invalid")
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(description, "skirk-ack-data-v1:"))
	if err != nil {
		return nil, errors.New("data_snapshot_invalid")
	}
	var value dataSnapshot
	if err := json.Unmarshal(body, &value); err != nil || len(value.Frames) == 0 || len(value.Frames) > frameCount {
		return nil, errors.New("data_snapshot_invalid")
	}
	previous := 0
	for _, item := range value.Frames {
		if item.Sequence <= previous || item.Sequence > frameCount || (previous != 0 && item.Sequence != previous+1) {
			return nil, errors.New("data_sequence_invalid")
		}
		payload, err := base64.RawURLEncoding.DecodeString(item.Payload)
		if err != nil || len(payload) != frameBytes || !hmac.Equal(payload, p.state.payloads[item.Sequence-1]) ||
			!hmac.Equal([]byte(item.Tag), []byte(tag(p.key, 'D', item.Sequence, payload))) {
			return nil, errors.New("data_integrity_invalid")
		}
		previous = item.Sequence
	}
	return value.Frames, nil
}

func (p *probe) acceptFrames(frames []frame, observed time.Time) (int, error) {
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if frames[0].Sequence > p.state.received+1 {
		return 0, errors.New("unrecoverable_data_gap")
	}
	for _, item := range frames {
		if item.Sequence <= p.state.received {
			continue
		}
		if item.Sequence != p.state.received+1 {
			return 0, errors.New("unrecoverable_data_gap")
		}
		p.state.received++
		p.state.latencies = append(p.state.latencies, millis(observed.Sub(p.state.scheduled[item.Sequence-1])))
	}
	return p.state.received, nil
}

func (p *probe) observeDataVersion(previous, current int64) error {
	if current < 1 || (previous > 0 && current < previous) {
		return errors.New("data_version_regressed")
	}
	p.state.mu.Lock()
	if previous > 0 && current == previous {
		p.state.staleDataGets++
	}
	if previous > 0 && current > previous+1 {
		p.state.skippedVersions += current - previous - 1
	}
	p.state.mu.Unlock()
	return nil
}

func (p *probe) readData(ctx context.Context, dataID, ackID string) error {
	var lastVersion int64
	for {
		p.state.mu.Lock()
		produced, received, paused := p.state.produced, p.state.received, p.state.forcedPauseDone
		p.state.mu.Unlock()
		if !paused && produced >= pauseAfter && received >= 1 {
			p.state.mu.Lock()
			p.state.forcedPauseDone = true
			p.state.mu.Unlock()
			if !sleep(ctx, p.readerPause) {
				return errors.New("reader_pause_canceled")
			}
		}
		if received == frameCount {
			return nil
		}
		value, status, err := p.get(ctx, p.dataReader, dataID, false)
		if err != nil {
			return err
		}
		if status == http.StatusOK && strings.HasPrefix(value.Description, "skirk-ack-data-v1:") {
			version, err := strconv.ParseInt(value.Version, 10, 64)
			if err != nil || version < 1 {
				return errors.New("data_version_invalid")
			}
			if err := p.observeDataVersion(lastVersion, version); err != nil {
				return err
			}
			lastVersion = version
			frames, err := p.verifyData(value.Description)
			if err != nil {
				return err
			}
			ack, err := p.acceptFrames(frames, time.Now())
			if err != nil {
				return err
			}
			p.state.mu.Lock()
			published := p.state.ackPublished
			p.state.mu.Unlock()
			if ack > published {
				snapshot := ackSnapshot{Sequence: ack, Tag: tag(p.key, 'A', ack, nil)}
				body, _ := json.Marshal(snapshot)
				description := "skirk-ack-ack-v1:" + base64.RawURLEncoding.EncodeToString(body)
				if err := p.patch(ctx, p.ackWriter, "ack", ackID, description); err != nil {
					return err
				}
				p.state.mu.Lock()
				p.state.ackPublished = ack
				p.state.mu.Unlock()
			}
		} else if status != http.StatusNotFound && status != http.StatusOK {
			return errors.New("data_get_status")
		}
		if !sleep(ctx, p.pollInterval) {
			return errors.New("reader_canceled")
		}
	}
}

func (p *probe) readAck(ctx context.Context, ackID string) error {
	var lastVersion int64
	for {
		p.state.mu.Lock()
		acked := p.state.acked
		p.state.mu.Unlock()
		if acked == frameCount {
			return nil
		}
		value, status, err := p.get(ctx, p.ackReader, ackID, false)
		if err != nil {
			return err
		}
		if status == http.StatusOK && strings.HasPrefix(value.Description, "skirk-ack-ack-v1:") {
			version, err := strconv.ParseInt(value.Version, 10, 64)
			if err != nil || version < 1 {
				return errors.New("ack_version_invalid")
			}
			if lastVersion > 0 && version == lastVersion {
				p.state.mu.Lock()
				p.state.staleAckGets++
				p.state.mu.Unlock()
			}
			if version < lastVersion {
				return errors.New("ack_version_regressed")
			}
			lastVersion = version
			body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value.Description, "skirk-ack-ack-v1:"))
			var snapshot ackSnapshot
			if err != nil || json.Unmarshal(body, &snapshot) != nil || snapshot.Sequence < 0 || snapshot.Sequence > frameCount ||
				!hmac.Equal([]byte(snapshot.Tag), []byte(tag(p.key, 'A', snapshot.Sequence, nil))) {
				return errors.New("ack_integrity_invalid")
			}
			p.state.mu.Lock()
			if snapshot.Sequence > p.state.produced {
				p.state.mu.Unlock()
				return errors.New("ack_ahead_of_writer")
			}
			if snapshot.Sequence > p.state.acked {
				p.state.acked = snapshot.Sequence
			}
			p.state.mu.Unlock()
		} else if status != http.StatusNotFound && status != http.StatusOK {
			return errors.New("ack_get_status")
		}
		if !sleep(ctx, p.pollInterval) {
			return errors.New("ack_reader_canceled")
		}
	}
}

func (p *probe) cleanup(ids []string, result *report) {
	if len(ids) == 0 {
		result.CleanupVerified = true
		return
	}
	allClean := true
	for _, id := range ids {
		clean := false
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		for attempt := 0; attempt < 3; attempt++ {
			result.CleanupDeletes++
			p.units += 500
			if err := p.store.DeleteID(ctx, id); err != nil {
				continue
			}
			before := p.gets
			_, status, err := p.get(ctx, p.dataReader, id, true)
			result.CleanupGets += p.gets - before
			if err != nil || status != http.StatusNotFound {
				continue
			}
			time.Sleep(2 * time.Second)
			before = p.gets
			_, status, err = p.get(ctx, p.dataReader, id, true)
			result.CleanupGets += p.gets - before
			if err == nil && status == http.StatusNotFound {
				clean = true
				break
			}
		}
		cancel()
		allClean = allClean && clean
	}
	result.CleanupVerified = allClean
	if allClean && p.recoveryOwned {
		if err := os.Remove(p.recovery); err != nil && !os.IsNotExist(err) {
			result.CleanupVerified = false
			result.Failure = "recovery_record_removal_failed"
		}
	}
	if !result.CleanupVerified && result.Failure == "" {
		result.Failure = "exact_id_cleanup_unverified"
	}
}

func (p *probe) run() (result report) {
	result = report{SchemaVersion: 1, Route: "google_front_pinned", FramesScheduled: frameCount,
		ReaderPauseMS: int(p.readerPause / time.Millisecond)}
	var ids []string
	defer func() {
		p.cleanup(ids, &result)
		p.budgetMu.Lock()
		result.Gets, result.DataPatches, result.AckPatches, result.QuotaUnitsUpperBound = p.gets, p.dataWrites, p.ackWrites, p.units
		p.budgetMu.Unlock()
		p.state.mu.Lock()
		result.FramesReceived, result.FramesAcknowledged = p.state.received, p.state.acked
		result.StaleDataGets, result.StaleAckGets = p.state.staleDataGets, p.state.staleAckGets
		result.SkippedDataVersions, result.MaxSnapshotFrames = p.state.skippedVersions, p.state.maxSnapshot
		result.MaxUnacknowledged, result.ForcedPauseCompleted = p.state.maxUnacked, p.state.forcedPauseDone
		if len(p.state.latencies) > 0 {
			result.TextScheduledP50MS = percentile(p.state.latencies, 50)
			result.TextScheduledP95MS = percentile(p.state.latencies, 95)
			result.TextScheduledMaxMS = percentile(p.state.latencies, 100)
		}
		p.state.mu.Unlock()
		result.OK = result.Failure == "" && result.FramesReceived == frameCount && result.FramesAcknowledged == frameCount && result.ForcedPauseCompleted && result.CleanupVerified && result.QuotaUnitsUpperBound < 10000
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := rand.Read(p.key[:]); err != nil {
		result.Failure = "random_failed"
		return
	}
	for i := 0; i < frameCount; i++ {
		payload := make([]byte, frameBytes)
		if _, err := rand.Read(payload); err != nil {
			result.Failure = "random_failed"
			return
		}
		p.state.payloads[i] = payload
		p.state.tags[i] = tag(p.key, 'D', i+1, payload)
	}
	var err error
	ids, err = p.store.GenerateObjectIDs(ctx, 2)
	p.units += 50
	if err != nil || len(ids) != 2 || ids[0] == ids[1] {
		ids = nil
		result.Failure = "id_reservation_failed"
		return
	}
	if err := writeRecoveryIDs(p.recovery, ids); err != nil {
		result.Failure = "recovery_record_write_failed"
		return
	}
	p.recoveryOwned = true
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		result.Failure = "random_failed"
		return
	}
	for index, id := range ids {
		name := fmt.Sprintf("bench-drive/ack-slot/%s/%d", hex.EncodeToString(nonce), index)
		_, err := p.store.PutObjectWithID(ctx, id, name, []byte("probe"))
		p.units += 500
		if err != nil {
			result.Failure = "slot_create_failed"
			return
		}
	}
	functions := []func(context.Context) error{
		p.produce,
		func(ctx context.Context) error { return p.writeData(ctx, ids[0]) },
		func(ctx context.Context) error { return p.readData(ctx, ids[0], ids[1]) },
		func(ctx context.Context) error { return p.readAck(ctx, ids[1]) },
	}
	outcomes := make(chan error, len(functions))
	for _, fn := range functions {
		go func(fn func(context.Context) error) { outcomes <- fn(ctx) }(fn)
	}
	for range functions {
		if err := <-outcomes; err != nil && result.Failure == "" {
			result.Failure = err.Error()
			cancel()
		}
	}
	if result.Failure == "" && ctx.Err() != nil {
		result.Failure = "workload_deadline"
	}
	result.WorkloadCompletedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	return
}

func percentile(values []float64, target int) float64 {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	index := (len(ordered)*target+99)/100 - 1
	if index < 0 {
		index = 0
	}
	return ordered[index]
}

func millis(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

func writeRecoveryIDs(path string, ids []string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(ids)
}

func writeReport(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(value)
}

func main() {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ack-slot-probe: invalid bounded live arguments")
		os.Exit(2)
	}
	config, err := skirk.LoadConfig(options.config)
	if err != nil || config.Route.Mode != "google_front_pinned" || config.Route.GoogleIP == "" ||
		config.Drive.FolderID == "" || config.Drive.Space == "appDataFolder" {
		fmt.Fprintln(os.Stderr, "ack-slot-probe: requires an isolated pinned-front test folder")
		os.Exit(2)
	}
	p := newProbe(config, options.out, time.Duration(options.readerPauseMS)*time.Millisecond)
	p.pollInterval = time.Duration(options.pollMS) * time.Millisecond
	if options.roundtrip {
		p.roundtrip = true
		result := p.runRoundtrip()
		if err := writeReport(options.out, result); err != nil {
			fmt.Fprintln(os.Stderr, "ack-slot-probe: private result write failed")
			os.Exit(2)
		}
		fmt.Printf("{\"ok\":%t,\"roundtrips\":%d,\"quota_units_upper_bound\":%d,\"cleanup_verified\":%t,\"failure\":%q}\n",
			result.OK, result.Roundtrips, result.QuotaUnitsUpperBound, result.CleanupVerified, result.Failure)
		if !result.OK {
			os.Exit(1)
		}
		return
	}
	result := p.run()
	if err := writeReport(options.out, result); err != nil {
		fmt.Fprintln(os.Stderr, "ack-slot-probe: private result write failed")
		os.Exit(2)
	}
	fmt.Printf("{\"ok\":%t,\"frames_received\":%d,\"frames_acknowledged\":%d,\"gets\":%d,\"cleanup_verified\":%t,\"failure\":%q}\n",
		result.OK, result.FramesReceived, result.FramesAcknowledged, result.Gets, result.CleanupVerified, result.Failure)
	if !result.OK {
		os.Exit(1)
	}
}
