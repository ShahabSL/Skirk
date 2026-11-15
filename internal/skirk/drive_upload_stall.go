package skirk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const driveUploadDuplicateDefaultBudget = 16 * 1024 * 1024
const driveUploadCommitProbeTimeout = 2 * time.Second

var errDriveUploadDuplicateBudget = errors.New("upload duplicate byte budget exhausted before request")

type driveUploadCommitProbeKey struct{}
type driveUploadStallScopeKey struct{}
type driveUploadStallScope struct {
	observer *driveUploadStallObserver
	charge   func(int64) error
}
type driveUploadDuplicateCharge struct {
	at    time.Time
	bytes int64
}

// A timestamp belongs to one actual HTTP attempt. Auth refresh and metadata
// requests cannot arm this observer; a new POST attempt resets the old state.
type driveUploadStallObserver struct {
	mu         sync.Mutex
	generation uint64
	wroteAt    time.Time
	headers    bool
	changed    chan struct{}
}

func newDriveUploadStallObserver() *driveUploadStallObserver {
	return &driveUploadStallObserver{changed: make(chan struct{}, 1)}
}
func (s *driveUploadStallObserver) notifyLocked() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *driveUploadStallObserver) begin() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	s.wroteAt, s.headers = time.Time{}, false
	s.notifyLocked()
	return s.generation
}
func (s *driveUploadStallObserver) update(generation uint64, wrote bool, headers bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return
	}
	if headers {
		s.headers = true
		s.wroteAt = time.Time{}
	} else if !s.headers {
		if wrote {
			s.wroteAt = time.Now()
		} else {
			s.wroteAt = time.Time{}
		}
	}
	s.notifyLocked()
}
func (s *driveUploadStallObserver) remaining(delay time.Duration) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headers || s.wroteAt.IsZero() {
		return 0, false
	}
	return time.Until(s.wroteAt.Add(delay)), true
}
func (s *driveUploadStallObserver) stalled(delay time.Duration) bool {
	remaining, armed := s.remaining(delay)
	return armed && remaining <= 0
}

type driveStallHTTPAttempt struct {
	requests   atomic.Int32
	observer   *driveUploadStallObserver
	generation uint64
}

func startDriveStallHTTPAttempt(ctx context.Context, method, path string, bodyBytes int) (context.Context, *driveStallHTTPAttempt, error) {
	scope, ok := ctx.Value(driveUploadStallScopeKey{}).(driveUploadStallScope)
	if !ok || method != http.MethodPost || !strings.HasPrefix(path, "/upload/drive/v3/files?") {
		return ctx, nil, nil
	}
	if scope.charge != nil {
		if err := scope.charge(int64(bodyBytes)); err != nil {
			return ctx, nil, err
		}
	}
	a := &driveStallHTTPAttempt{observer: scope.observer}
	if a.observer != nil {
		a.generation = a.observer.begin()
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			if a.observer != nil {
				a.observer.update(a.generation, false, false)
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			a.requests.Add(1)
			if a.observer != nil {
				a.observer.update(a.generation, info.Err == nil, false)
			}
		},
		GotFirstResponseByte: a.gotHeaders,
	}), a, nil
}
func (a *driveStallHTTPAttempt) gotHeaders() {
	if a.observer != nil {
		a.observer.update(a.generation, false, true)
	}
}
func (a *driveStallHTTPAttempt) count() int {
	n := int(a.requests.Load())
	if n < 1 {
		return 1
	}
	return n
}

func (d *DriveStore) reserveUploadDuplicateBytes(now time.Time, bytes int64) error {
	d.uploadStallBudgetMu.Lock()
	defer d.uploadStallBudgetMu.Unlock()
	budget := d.uploadStallHedgeBudget
	if budget == 0 {
		budget = driveUploadDuplicateDefaultBudget
	}
	var retained []driveUploadDuplicateCharge
	var used int64
	for _, charge := range d.uploadStallCharges {
		if now.Sub(charge.at) < time.Minute {
			retained = append(retained, charge)
			used += charge.bytes
		}
	}
	d.uploadStallCharges = retained
	if bytes <= 0 || bytes > budget || used > budget-bytes {
		if d.Logger != nil {
			d.Logger.Printf("drive upload stall action=duplicate_budget_denied body_bytes=%d rolling_bytes=%d budget_bytes=%d", bytes, used, budget)
		}
		return errDriveUploadDuplicateBudget
	}
	d.uploadStallCharges = append(d.uploadStallCharges, driveUploadDuplicateCharge{now, bytes})
	if d.Logger != nil {
		d.Logger.Printf("drive upload stall action=duplicate_reserved body_bytes=%d rolling_bytes=%d budget_bytes=%d", bytes, used+bytes, budget)
	}
	return nil
}

func (d *DriveStore) readUploadCommitProof(ctx context.Context, client *GoogleHTTPClient, id, name string, size int, digest string) (ObjectInfo, bool, error) {
	ctx = context.WithValue(ctx, driveUploadCommitProbeKey{}, true)
	ctx = withGoogleResponseLimit(ctx, 64*1024)
	result, err := d.requestWithHTTPMode(ctx, client, http.MethodGet, "/drive/v3/files/"+url.PathEscape(id)+"?fields=id,name,size,sha256Checksum", nil, nil, true)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	if result.Status == http.StatusNotFound {
		return ObjectInfo{}, false, nil
	}
	if err := require2xx(result, "upload commit proof"); err != nil {
		return ObjectInfo{}, false, err
	}
	var item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Size   string `json:"size"`
		SHA256 string `json:"sha256Checksum"`
	}
	if err := json.Unmarshal(result.Body, &item); err != nil {
		return ObjectInfo{}, false, err
	}
	if item.ID != id || item.Name != name || item.Size != fmt.Sprint(size) {
		return ObjectInfo{}, false, errors.New("upload commit proof identity differs")
	}
	if item.SHA256 == "" {
		return ObjectInfo{}, false, nil
	}
	if !strings.EqualFold(item.SHA256, digest) {
		return ObjectInfo{}, false, errors.New("upload commit proof content differs")
	}
	return ObjectInfo{ID: id, Name: name, Size: int64(size)}, true, nil
}

func (d *DriveStore) uploadObjectWithStallHedge(ctx context.Context, primaryHTTP *GoogleHTTPClient, id, name string, data []byte, headers map[string]string, body []byte) (ObjectInfo, error) {
	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	observer := newDriveUploadStallObserver()
	primary := make(chan driveUploadOutcome, 1)
	primaryCtx := context.WithValue(legCtx, driveUploadStallScopeKey{}, driveUploadStallScope{observer: observer})
	upload := func(c context.Context, client *GoogleHTTPClient) driveUploadOutcome {
		info, err := d.uploadObjectWithHTTPMode(c, client, id, name, len(data), headers, body, digest)
		return driveUploadOutcome{info, err}
	}
	go func() { primary <- upload(primaryCtx, primaryHTTP) }()
	awaitPrimary := func() (ObjectInfo, error) { r := <-primary; return r.info, r.err }
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var deadline <-chan time.Time
	for !observer.stalled(d.uploadStallHedgeDelay) {
		select {
		case r := <-primary:
			return r.info, r.err
		case <-ctx.Done():
			cancel()
			return awaitPrimary()
		case <-observer.changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if remaining, armed := observer.remaining(d.uploadStallHedgeDelay); armed {
				if remaining < 0 {
					remaining = 0
				}
				timer.Reset(remaining)
				deadline = timer.C
			} else {
				deadline = nil
			}
		case <-deadline:
		}
	}
	select {
	case r := <-primary:
		return r.info, r.err
	default:
	}
	if ctx.Err() != nil || d.QuotaBackoffSnapshot().Active {
		return awaitPrimary()
	}
	select {
	case d.uploadStallHedgeSlots <- struct{}{}:
	default:
		return awaitPrimary()
	}
	defer func() { <-d.uploadStallHedgeSlots }()
	fresh := d.freshHedgeHTTPClient()
	if fresh == nil {
		return awaitPrimary()
	}
	defer fresh.client.CloseIdleConnections()
	probeCtx, stopProbe := context.WithTimeout(legCtx, driveUploadCommitProbeTimeout)
	defer stopProbe()
	type proofResult struct {
		info      ObjectInfo
		committed bool
		err       error
	}
	proof := make(chan proofResult, 1)
	go func() {
		info, committed, err := d.readUploadCommitProof(probeCtx, fresh, id, name, len(data), digest)
		proof <- proofResult{info, committed, err}
	}()
	var p driveUploadOutcome
	primaryPending := (<-chan driveUploadOutcome)(primary)
	var checked proofResult
probeLoop:
	for {
		select {
		case p = <-primaryPending:
			primaryPending = nil
			if p.err == nil {
				stopProbe()
				<-proof
				return p.info, nil
			}
		case checked = <-proof:
			break probeLoop
		case <-observer.changed:
			if !observer.stalled(d.uploadStallHedgeDelay) {
				stopProbe()
				<-proof
				if primaryPending == nil {
					return p.info, p.err
				}
				return awaitPrimary()
			}
		case <-ctx.Done():
			cancel()
			stopProbe()
			<-proof
			if primaryPending == nil {
				return p.info, p.err
			}
			return awaitPrimary()
		}
	}
	stopProbe()
	if checked.committed {
		cancel()
		if primaryPending != nil {
			<-primaryPending
		}
		if d.Logger != nil {
			d.Logger.Printf("drive upload stall action=commit_proven duplicate_bytes=0")
		}
		return checked.info, nil
	}
	if primaryPending != nil {
		select {
		case p = <-primaryPending:
			primaryPending = nil
		default:
		}
	}
	if primaryPending == nil {
		return p.info, p.err
	}
	if checked.err != nil || !observer.stalled(d.uploadStallHedgeDelay) || ctx.Err() != nil || d.QuotaBackoffSnapshot().Active {
		return awaitPrimary()
	}
	// Headers can race this last check. Suppression is best effort; identical
	// generated IDs and the spare/byte bounds make that race safe.
	secondaryCtx := context.WithValue(legCtx, driveUploadStallScopeKey{}, driveUploadStallScope{charge: func(n int64) error { return d.reserveUploadDuplicateBytes(time.Now(), n) }})
	secondary := make(chan driveUploadOutcome, 1)
	if d.Logger != nil {
		d.Logger.Printf("drive upload stall action=secondary after_write=%s max_extra=1", d.uploadStallHedgeDelay)
	}
	go func() { secondary <- upload(secondaryCtx, fresh) }()
	secondaryPending := (<-chan driveUploadOutcome)(secondary)
	for primaryPending != nil || secondaryPending != nil {
		select {
		case p = <-primaryPending:
			primaryPending = nil
			if p.err == nil {
				cancel()
				if secondaryPending != nil {
					<-secondaryPending
				}
				return p.info, nil
			}
		case r := <-secondaryPending:
			secondaryPending = nil
			if r.err == nil {
				cancel()
				if primaryPending != nil {
					<-primaryPending
				}
				return r.info, nil
			}
		}
	}
	return p.info, p.err
}
