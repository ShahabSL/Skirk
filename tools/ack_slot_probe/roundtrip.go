package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	roundtripMarker = "skirk-bidi-slot-v1:"
	roundtripCap    = 8192
)

// Each file has exactly one writer. The ACK for the opposite file rides in
// the writer's latest cumulative snapshot, including ACK-only updates.
type roundtripSnapshot struct {
	Generation string  `json:"generation"`
	Direction  string  `json:"direction"`
	Sequence   int     `json:"sequence"`
	Ack        int     `json:"ack"`
	Frames     []frame `json:"frames"`
	Tag        string  `json:"tag"`
}

type roundtripLane struct {
	produced     int
	published    int
	received     int
	acked        int
	ackPublished int
	snapshotSeq  int
	lastVersion  int64
	lastSnapshot int
	staleGets    int
	skipped      int64
}

type roundtripState struct {
	mu         sync.Mutex
	up         roundtripLane
	down       roundtripLane
	requests   [frameCount][]byte
	scheduled  [frameCount]time.Time
	rtts       []float64
	pauseDone  bool
	maxFrames  int
	maxUnacked int
}

type roundtripProbe struct {
	*probe
	state      roundtripState
	generation string
}

type roundtripReport struct {
	SchemaVersion        int     `json:"schema_version"`
	Route                string  `json:"route"`
	Mode                 string  `json:"mode"`
	RequestsScheduled    int     `json:"requests_scheduled"`
	RequestsReceived     int     `json:"requests_received"`
	RepliesReceived      int     `json:"replies_received"`
	RequestsAcknowledged int     `json:"requests_acknowledged"`
	RepliesAcknowledged  int     `json:"replies_acknowledged"`
	Roundtrips           int     `json:"roundtrips"`
	RoundtripP50MS       float64 `json:"roundtrip_scheduled_p50_ms,omitempty"`
	RoundtripP95MS       float64 `json:"roundtrip_scheduled_p95_ms,omitempty"`
	RoundtripMaxMS       float64 `json:"roundtrip_scheduled_max_ms,omitempty"`
	UpPatches            int     `json:"up_patches"`
	DownPatches          int     `json:"down_patches"`
	Gets                 int     `json:"gets"`
	UpStaleGets          int     `json:"up_stale_gets"`
	DownStaleGets        int     `json:"down_stale_gets"`
	UpSkippedVersions    int64   `json:"up_skipped_versions"`
	DownSkippedVersions  int64   `json:"down_skipped_versions"`
	MaxSnapshotFrames    int     `json:"max_snapshot_frames"`
	MaxUnacknowledged    int     `json:"max_unacknowledged"`
	ForcedPauseCompleted bool    `json:"forced_pause_completed"`
	ReaderPauseMS        int     `json:"reader_pause_ms"`
	PollMS               int     `json:"poll_ms"`
	BoundaryBytes        int     `json:"metadata_boundary_bytes"`
	BoundaryVerified     bool    `json:"metadata_boundary_verified"`
	CleanupVerified      bool    `json:"cleanup_verified"`
	CleanupDeletes       int     `json:"cleanup_deletes"`
	CleanupGets          int     `json:"cleanup_gets"`
	QuotaUnitsUpperBound int     `json:"quota_units_upper_bound"`
	WorkloadCompletedUTC string  `json:"workload_completed_utc,omitempty"`
	Failure              string  `json:"failure,omitempty"`
	OK                   bool    `json:"ok"`
}

func roundtripDirection(up bool) (string, byte) {
	if up {
		return "up", 'U'
	}
	return "down", 'V'
}

func (b *roundtripProbe) encodeSnapshot(up bool, sequence, ack, first, last int) (string, error) {
	if sequence < 1 || ack < 0 || ack > frameCount || first < 1 || last > frameCount || first > last+1 {
		return "", errors.New("snapshot_range_invalid")
	}
	direction, domain := roundtripDirection(up)
	value := roundtripSnapshot{Generation: b.generation, Direction: direction, Sequence: sequence,
		Ack: ack, Frames: make([]frame, 0, max(0, last-first+1))}
	for number := first; number <= last; number++ {
		payload := b.state.requests[number-1]
		value.Frames = append(value.Frames, frame{Sequence: number,
			Payload: base64.RawURLEncoding.EncodeToString(payload),
			Tag:     tag(b.key, domain, number, payload)})
	}
	bare, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	value.Tag = tag(b.key, domain, sequence, bare)
	wire, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	description := roundtripMarker + base64.RawURLEncoding.EncodeToString(wire)
	if len(description) > roundtripCap {
		return "", errors.New("snapshot_size_cap")
	}
	return description, nil
}

func (b *roundtripProbe) decodeSnapshot(up bool, description string) (roundtripSnapshot, error) {
	var value roundtripSnapshot
	if !strings.HasPrefix(description, roundtripMarker) {
		return value, errors.New("snapshot_marker_invalid")
	}
	wire, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(description, roundtripMarker))
	if err != nil || json.Unmarshal(wire, &value) != nil {
		return value, errors.New("snapshot_encoding_invalid")
	}
	direction, domain := roundtripDirection(up)
	if value.Generation != b.generation || value.Direction != direction || value.Sequence < 1 ||
		value.Ack < 0 || value.Ack > frameCount || len(value.Frames) > frameCount {
		return value, errors.New("snapshot_identity_invalid")
	}
	mac := value.Tag
	value.Tag = ""
	bare, err := json.Marshal(value)
	if err != nil || !hmac.Equal([]byte(mac), []byte(tag(b.key, domain, value.Sequence, bare))) {
		return value, errors.New("snapshot_authentication_invalid")
	}
	value.Tag = mac
	previous := 0
	for _, item := range value.Frames {
		if item.Sequence < 1 || item.Sequence > frameCount || item.Sequence <= previous ||
			(previous > 0 && item.Sequence != previous+1) {
			return value, errors.New("frame_sequence_invalid")
		}
		payload, err := base64.RawURLEncoding.DecodeString(item.Payload)
		if err != nil || len(payload) != frameBytes ||
			!hmac.Equal(payload, b.state.requests[item.Sequence-1]) ||
			!hmac.Equal([]byte(item.Tag), []byte(tag(b.key, domain, item.Sequence, payload))) {
			return value, errors.New("frame_integrity_invalid")
		}
		previous = item.Sequence
	}
	return value, nil
}

func (b *roundtripProbe) produce(ctx context.Context) error {
	start := time.Now()
	for sequence := 1; sequence <= frameCount; sequence++ {
		scheduled := start.Add(time.Duration(sequence-1) * interval)
		if !sleep(ctx, time.Until(scheduled)) {
			return errors.New("producer_canceled")
		}
		b.state.mu.Lock()
		b.state.scheduled[sequence-1] = scheduled
		b.state.up.produced = sequence
		b.state.mu.Unlock()
	}
	b.state.mu.Lock()
	b.state.mu.Unlock()
	return nil
}

func (b *roundtripProbe) write(ctx context.Context, fileID string, up bool) error {
	for {
		b.state.mu.Lock()
		lane, reverse := &b.state.up, &b.state.down
		kind, client := "data", b.dataWriter
		if !up {
			lane, reverse = reverse, lane
			kind, client = "ack", b.ackWriter
		}
		first, last, ack := lane.acked+1, lane.produced, reverse.received
		finished := lane.acked == frameCount && reverse.received == frameCount && lane.ackPublished == reverse.received
		pending := last > lane.published || ack > lane.ackPublished
		sequence := lane.snapshotSeq + 1
		b.state.mu.Unlock()
		if finished {
			return nil
		}
		if !pending {
			if !sleep(ctx, 25*time.Millisecond) {
				return errors.New("slot_writer_canceled")
			}
			continue
		}
		description, err := b.encodeSnapshot(up, sequence, ack, first, last)
		if err != nil {
			return err
		}
		if err := b.patch(ctx, client, kind, fileID, description); err != nil {
			return err
		}
		b.state.mu.Lock()
		lane.snapshotSeq = sequence
		lane.published = last
		lane.ackPublished = ack
		if count := max(0, last-first+1); count > b.state.maxFrames {
			b.state.maxFrames = count
		}
		if unacked := last - lane.acked; unacked > b.state.maxUnacked {
			b.state.maxUnacked = unacked
		}
		b.state.mu.Unlock()
	}
}

func (b *roundtripProbe) accept(up bool, value roundtripSnapshot, version int64, observed time.Time) error {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	lane, reverse := &b.state.up, &b.state.down
	if !up {
		lane, reverse = reverse, lane
	}
	if version < 1 || version < lane.lastVersion || value.Sequence < lane.lastSnapshot {
		return errors.New("slot_version_regressed")
	}
	if version == lane.lastVersion {
		lane.staleGets++
	}
	if lane.lastVersion > 0 && version > lane.lastVersion+1 {
		lane.skipped += version - lane.lastVersion - 1
	}
	lane.lastVersion = version
	lane.lastSnapshot = value.Sequence
	if value.Ack > reverse.produced || value.Ack < reverse.acked {
		return errors.New("ack_out_of_range")
	}
	reverse.acked = value.Ack
	if len(value.Frames) > 0 && value.Frames[0].Sequence > lane.received+1 {
		return errors.New("unrecoverable_frame_gap")
	}
	for _, item := range value.Frames {
		if item.Sequence <= lane.received {
			continue
		}
		if item.Sequence != lane.received+1 {
			return errors.New("unrecoverable_frame_gap")
		}
		lane.received++
		if up {
			// The reply is generated only after the request reaches the server.
			b.state.down.produced = lane.received
		} else {
			b.state.rtts = append(b.state.rtts, millis(observed.Sub(b.state.scheduled[item.Sequence-1])))
		}
	}
	return nil
}

func (b *roundtripProbe) read(ctx context.Context, fileID string, up bool) error {
	client := b.dataReader
	if !up {
		client = b.ackReader
	}
	for {
		b.state.mu.Lock()
		if up && !b.state.pauseDone && b.state.up.produced >= pauseAfter && b.state.up.received >= 1 {
			b.state.pauseDone = true
			b.state.mu.Unlock()
			if !sleep(ctx, b.readerPause) {
				return errors.New("reader_pause_canceled")
			}
			b.state.mu.Lock()
		}
		finished := b.state.up.received == frameCount && b.state.down.acked == frameCount
		if !up {
			finished = b.state.down.received == frameCount && b.state.up.acked == frameCount
		}
		b.state.mu.Unlock()
		if finished {
			return nil
		}
		value, status, err := b.get(ctx, client, fileID, false)
		if err != nil {
			return err
		}
		if status == http.StatusOK && strings.HasPrefix(value.Description, roundtripMarker) {
			version, err := strconv.ParseInt(value.Version, 10, 64)
			if err != nil || version < 1 {
				return errors.New("slot_version_invalid")
			}
			snapshot, err := b.decodeSnapshot(up, value.Description)
			if err != nil {
				return err
			}
			if err := b.accept(up, snapshot, version, time.Now()); err != nil {
				return err
			}
		} else if status != http.StatusOK && status != http.StatusNotFound {
			return errors.New("slot_get_status")
		}
		if !sleep(ctx, b.pollInterval) {
			return errors.New("slot_reader_canceled")
		}
	}
}

func (p *probe) runRoundtrip() (result roundtripReport) {
	result = roundtripReport{SchemaVersion: 1, Route: "google_front_pinned", Mode: "roundtrip",
		RequestsScheduled: frameCount, ReaderPauseMS: int(p.readerPause / time.Millisecond),
		PollMS: int(p.pollInterval / time.Millisecond), BoundaryBytes: roundtripCap}
	b := &roundtripProbe{probe: p}
	var ids []string
	defer func() {
		cleanup := report{Failure: result.Failure}
		p.cleanup(ids, &cleanup)
		result.Failure = cleanup.Failure
		result.CleanupVerified, result.CleanupDeletes, result.CleanupGets = cleanup.CleanupVerified, cleanup.CleanupDeletes, cleanup.CleanupGets
		p.budgetMu.Lock()
		result.Gets, result.UpPatches, result.DownPatches, result.QuotaUnitsUpperBound = p.gets, p.dataWrites, p.ackWrites, p.units
		p.budgetMu.Unlock()
		b.state.mu.Lock()
		result.RequestsReceived, result.RepliesReceived = b.state.up.received, b.state.down.received
		result.RequestsAcknowledged, result.RepliesAcknowledged = b.state.up.acked, b.state.down.acked
		result.Roundtrips = len(b.state.rtts)
		result.UpStaleGets, result.DownStaleGets = b.state.up.staleGets, b.state.down.staleGets
		result.UpSkippedVersions, result.DownSkippedVersions = b.state.up.skipped, b.state.down.skipped
		result.MaxSnapshotFrames, result.MaxUnacknowledged = b.state.maxFrames, b.state.maxUnacked
		result.ForcedPauseCompleted = b.state.pauseDone
		if len(b.state.rtts) > 0 {
			result.RoundtripP50MS = percentile(b.state.rtts, 50)
			result.RoundtripP95MS = percentile(b.state.rtts, 95)
			result.RoundtripMaxMS = percentile(b.state.rtts, 100)
		}
		b.state.mu.Unlock()
		result.OK = result.Failure == "" && result.BoundaryVerified && result.Roundtrips == frameCount &&
			result.RequestsAcknowledged == frameCount && result.RepliesAcknowledged == frameCount &&
			result.ForcedPauseCompleted && result.CleanupVerified && result.QuotaUnitsUpperBound < 16000
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := rand.Read(p.key[:]); err != nil {
		result.Failure = "random_failed"
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		result.Failure = "random_failed"
		return
	}
	b.generation = hex.EncodeToString(nonce)
	for index := range b.state.requests {
		payload := make([]byte, frameBytes)
		if _, err := rand.Read(payload); err != nil {
			result.Failure = "random_failed"
			return
		}
		b.state.requests[index] = payload
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
	for index, id := range ids {
		name := fmt.Sprintf("bench-drive/bidi-slot/%s/%d", b.generation, index)
		if _, err := p.store.PutObjectWithID(ctx, id, name, []byte("probe")); err != nil {
			result.Failure = "slot_create_failed"
			return
		}
		p.units += 500
	}
	// This is a readback gate for the proposed 8 KiB encoded-description cap.
	boundary := "skirk-bidi-size-v1:" + strings.Repeat("x", roundtripCap-len("skirk-bidi-size-v1:"))
	if err := p.patch(ctx, p.dataWriter, "data", ids[0], boundary); err != nil {
		result.Failure = "boundary_patch_failed"
		return
	}
	value, status, err := p.get(ctx, p.dataReader, ids[0], false)
	if err != nil || status != http.StatusOK || value.Description != boundary {
		result.Failure = "boundary_readback_failed"
		return
	}
	result.BoundaryVerified = true
	functions := []func(context.Context) error{
		b.produce,
		func(ctx context.Context) error { return b.write(ctx, ids[0], true) },
		func(ctx context.Context) error { return b.write(ctx, ids[1], false) },
		func(ctx context.Context) error { return b.read(ctx, ids[0], true) },
		func(ctx context.Context) error { return b.read(ctx, ids[1], false) },
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
