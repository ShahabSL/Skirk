package skirk

// Owned-fixture research only. No resources, IAM or product OAuth are created.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type fsHybridGoogleCarrierConfig struct {
	PrivateDir, Database, OwnDocument, PeerDocument, OwnerToken        string
	Route                                                              RouteConfig
	MaxCalls, MaxAttemptsUpper, MaxEncodedBytesUpper, MaxResponseBytes int
	MaxWatchRequests, MaxWatchEvents                                   int
	WatchDuration                                                      time.Duration
}

var fsHybridDatabasePattern = regexp.MustCompile(`^projects/skirk-p/databases/skirk-(front-probe|hybrid-probe)-[a-f0-9]{12}$`)
var fsHybridDocSuffixPattern = regexp.MustCompile(`^/documents/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,128}$`)

func (c fsHybridGoogleCarrierConfig) normalized() (fsHybridGoogleCarrierConfig, error) {
	if c.MaxCalls == 0 {
		c.MaxCalls = 512
	}
	if c.MaxAttemptsUpper == 0 {
		c.MaxAttemptsUpper = 4096
	}
	if c.MaxEncodedBytesUpper == 0 {
		c.MaxEncodedBytesUpper = 128 << 20
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 1 << 20
	}
	if c.MaxWatchRequests == 0 {
		c.MaxWatchRequests = 256
	}
	if c.MaxWatchEvents == 0 {
		c.MaxWatchEvents = 512
	}
	if c.WatchDuration == 0 {
		c.WatchDuration = 5 * time.Minute
	}
	if c.MaxCalls < 32 || c.MaxCalls > 512 || c.MaxAttemptsUpper < 256 || c.MaxAttemptsUpper > 4096 || c.MaxEncodedBytesUpper < 1<<20 || c.MaxEncodedBytesUpper > 128<<20 || c.MaxResponseBytes < 1024 || c.MaxResponseBytes > 1<<20 || c.MaxWatchRequests < 16 || c.MaxWatchRequests > 256 || c.MaxWatchEvents < 1 || c.MaxWatchEvents > 512 || c.WatchDuration < time.Second || c.WatchDuration > 5*time.Minute {
		return c, errFSHybridBounds
	}
	info, err := os.Lstat(c.PrivateDir)
	if err != nil || !filepath.IsAbs(c.PrivateDir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !strings.Contains(filepath.Clean(c.PrivateDir)+string(filepath.Separator), string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) {
		return c, errFSHybridBounds
	}
	if !fsHybridDatabasePattern.MatchString(c.Database) || c.OwnDocument == c.PeerDocument {
		return c, errFSHybridBounds
	}
	for _, document := range []string{c.OwnDocument, c.PeerDocument} {
		if !strings.HasPrefix(document, c.Database) || !fsHybridDocSuffixPattern.MatchString(strings.TrimPrefix(document, c.Database)) {
			return c, errFSHybridBounds
		}
	}
	if c.Route.Mode != "google_front_pinned" || net.ParseIP(c.Route.GoogleIP) == nil || c.Route.Proxy != "" || len(c.OwnerToken) < 1 || len(c.OwnerToken) > 16384 {
		return c, errFSHybridBounds
	}
	for _, r := range c.OwnerToken {
		if r < 0x21 || r > 0x7e {
			return c, errFSHybridBounds
		}
	}
	return c, nil
}

type fsHybridControlError struct{ cause error }

func (e *fsHybridControlError) Error() string { return "Firestore hybrid owned control request failed" }
func (e *fsHybridControlError) Unwrap() error { return e.cause }

type fsHybridCarrierStats struct {
	Calls, AttemptUpper, RequestUpper, ResponseBytes, HTTP2Responses                            int64
	RESTReads, RESTWrites, ListenCalls, CleanupCalls, CanceledCalls, TransportErrors, APIErrors int64
	Events, Documents, RejectedLocal, BudgetRejections                                          int64
	WatchFailures, WatchCancellations                                                           int64
}

type fsHybridGoogleCarrier struct {
	cfg                                     fsHybridGoogleCarrierConfig
	parent                                  context.Context
	deadline                                time.Time
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	client                                  *GoogleHTTPClient
	mu                                      sync.Mutex
	writeMu                                 sync.Mutex
	requestWG                               sync.WaitGroup
	operationWG                             sync.WaitGroup
	requestSlots, cleanupSlots              chan struct{}
	stats                                   fsHybridCarrierStats
	closed, closing, watchStarted, finished bool
	watchStopping                           bool
	watchAttempts                           int
	watchProofClean, watchUnjoined          bool
	watchOwnerCanceled                      bool
	watchDir                                string
	watchDone                               chan struct{}
	watchErr                                error
	watchState                              map[string]any
	bridge                                  *fsHybridWatchBridge
	closeOnce                               sync.Once
	stopOnce                                *sync.Once
	watchStop                               chan struct{}
	closeErr                                error
}

func newFSHybridGoogleCarrier(ctx context.Context, cfg fsHybridGoogleCarrierConfig) (*fsHybridGoogleCarrier, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if ctx == nil || ctx.Err() != nil {
		return nil, errFSHybridClosed
	}
	marker, err := os.OpenFile(filepath.Join(cfg.PrivateDir, "hybrid-carrier-started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errFSHybridBounds
	}
	err = json.NewEncoder(marker).Encode(map[string]any{"pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)})
	if err == nil {
		err = marker.Sync()
	}
	err = errors.Join(err, marker.Close())
	if err != nil {
		return nil, errFSHybridBounds
	}
	for _, name := range []string{"hybrid-watch-config.json", "stop-hybrid-watch.json", "hybrid-watch-result.json", "hybrid-watch.log"} {
		if _, err = os.Lstat(filepath.Join(cfg.PrivateDir, name)); !errors.Is(err, os.ErrNotExist) {
			return nil, errFSHybridBounds
		}
	}
	// A separate instance owns its own HTTP/2 pool. It never reuses Drive's pool.
	deadline := time.Now().Add(cfg.WatchDuration)
	controlCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline.Add(10*time.Second))
	return &fsHybridGoogleCarrier{cfg: cfg, parent: ctx, deadline: deadline, ctx: controlCtx, cancel: cancel, client: NewGoogleHTTPClient(cfg.Route), requestSlots: make(chan struct{}, 8), cleanupSlots: make(chan struct{}, 2)}, nil
}

func (c *fsHybridGoogleCarrier) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	return map[string]int64{"visible_calls": s.Calls, "attempts_upper": s.AttemptUpper, "encoded_request_bytes_upper": s.RequestUpper, "response_body_bytes": s.ResponseBytes, "http2_responses": s.HTTP2Responses, "rest_reads": s.RESTReads, "rest_writes": s.RESTWrites, "listen_calls": s.ListenCalls, "cleanup_calls": s.CleanupCalls, "canceled_calls": s.CanceledCalls, "transport_errors": s.TransportErrors, "api_errors": s.APIErrors, "watch_events": s.Events, "watch_documents": s.Documents, "rejected_local": s.RejectedLocal, "budget_rejections": s.BudgetRejections}
}

func fsHybridServerTimestamp(value string) bool {
	t, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && len(value) <= 64 && t.Nanosecond()%1000 == 0
}

func (c *fsHybridGoogleCarrier) call(ctx context.Context, operation, method, path string, headers map[string]string, body []byte, cleanup bool) (*HTTPResult, error) {
	if ctx == nil {
		return nil, errFSHybridBounds
	}
	if err := ctx.Err(); err != nil {
		return nil, &fsHybridControlError{err}
	}
	slots := c.requestSlots
	if cleanup {
		slots = c.cleanupSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return nil, errFSHybridBusy
	}
	c.mu.Lock()
	limit := c.cfg.MaxCalls - 16
	attemptsLimit := c.cfg.MaxAttemptsUpper - 128
	bytesLimit := c.cfg.MaxEncodedBytesUpper - min(c.cfg.MaxEncodedBytesUpper/4, 16<<20)
	listenLimit := c.cfg.MaxWatchRequests - 8
	if cleanup {
		limit = c.cfg.MaxCalls
		attemptsLimit = c.cfg.MaxAttemptsUpper
		bytesLimit = c.cfg.MaxEncodedBytesUpper
		listenLimit = c.cfg.MaxWatchRequests
	}
	if c.finished || c.closed && !cleanup || c.closing && !cleanup || !cleanup && (c.watchUnjoined || c.parent.Err() != nil || !time.Now().Before(c.deadline)) {
		c.mu.Unlock()
		return nil, errFSHybridClosed
	}
	if c.stats.Calls >= int64(limit) || c.stats.AttemptUpper+8 > int64(attemptsLimit) || c.stats.RequestUpper+8*int64(len(body)) > int64(bytesLimit) || operation == "listen" && c.stats.ListenCalls >= int64(listenLimit) {
		c.stats.BudgetRejections++
		c.mu.Unlock()
		return nil, errFSHybridBounds
	}
	c.stats.Calls++
	c.stats.AttemptUpper += 8
	c.stats.RequestUpper += 8 * int64(len(body))
	switch operation {
	case "read":
		c.stats.RESTReads++
	case "write":
		c.stats.RESTWrites++
	case "listen":
		c.stats.ListenCalls++
	}
	if cleanup {
		c.stats.CleanupCalls++
	}
	c.requestWG.Add(1)
	c.mu.Unlock()
	defer c.requestWG.Done()
	requestCtx, cancel := context.WithTimeout(withGoogleResponseLimit(ctx, int64(c.cfg.MaxResponseBytes)), 15*time.Second)
	defer cancel()
	if !cleanup {
		limited, stop := context.WithDeadline(requestCtx, c.deadline)
		defer stop()
		requestCtx = limited
	}
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	selected := map[string]string{"Authorization": "Bearer " + c.cfg.OwnerToken, "Accept-Encoding": "identity"}
	for k, v := range headers {
		if strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "X-Goog-Api-Client") {
			selected[k] = v
		}
	}
	result, err := c.client.RequestSingleAttempt(requestCtx, method, "firestore.googleapis.com", path, selected, body)
	c.mu.Lock()
	if result != nil {
		c.stats.ResponseBytes += int64(len(result.Body))
		if result.Proto == "HTTP/2.0" {
			c.stats.HTTP2Responses++
		}
	}
	if errors.Is(err, context.Canceled) {
		c.stats.CanceledCalls++
	} else if err != nil {
		c.stats.TransportErrors++
	}
	if result != nil && result.Status >= 400 {
		c.stats.APIErrors++
	}
	c.mu.Unlock()
	if err != nil {
		return result, &fsHybridControlError{err}
	}
	if result == nil || result.Proto != "HTTP/2.0" {
		return result, &fsHybridControlError{errors.New("owned control HTTP2 response required")}
	}
	return result, nil
}

func fsHybridResponseError(result *HTTPResult) error {
	if result != nil && result.Status >= 200 && result.Status < 300 {
		return nil
	}
	status := 0
	if result != nil {
		status = result.Status
	}
	// Never retain an arbitrary server body/message in an error that may be logged.
	reason := http.StatusText(status)
	if result != nil {
		candidate, _ := googleAPIErrorDetails(result.Body)
		switch candidate {
		case "FAILED_PRECONDITION", "ABORTED", "ALREADY_EXISTS", "NOT_FOUND", "PERMISSION_DENIED", "UNAUTHENTICATED", "RESOURCE_EXHAUSTED", "INVALID_ARGUMENT", "UNAVAILABLE", "INTERNAL", "DEADLINE_EXCEEDED", "OUT_OF_RANGE", "UNIMPLEMENTED", "DATA_LOSS", "CANCELLED", "UNKNOWN":
			reason = candidate
		}
	}
	return &GoogleAPIError{Op: "firestore_hybrid", Status: status, Reason: reason}
}

func fsHybridDecodeDocument(raw []byte, expected string) (fsPriorityDocument, error) {
	var result struct {
		Name   string `json:"name"`
		Fields map[string]struct {
			Bytes *string `json:"bytesValue"`
		} `json:"fields"`
		CreateTime string `json:"createTime"`
		UpdateTime string `json:"updateTime"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Name != expected || len(result.Fields) != 1 || !fsHybridServerTimestamp(result.UpdateTime) {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	field, ok := result.Fields["sealed"]
	if !ok || field.Bytes == nil {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	if len(*field.Bytes) > base64.StdEncoding.EncodedLen(fsHybridWireMax) {
		return fsPriorityDocument{}, errFSHybridBounds
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(*field.Bytes)
	if err != nil || len(wire) > fsHybridWireMax || base64.StdEncoding.EncodeToString(wire) != *field.Bytes {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	return fsPriorityDocument{Ciphertext: wire, UpdateTime: result.UpdateTime}, nil
}

func (c *fsHybridGoogleCarrier) read(ctx context.Context, document string) (fsPriorityDocument, error) {
	result, err := c.call(ctx, "read", http.MethodGet, "/v1/"+document, nil, nil, false)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	if err = fsHybridResponseError(result); err != nil {
		return fsPriorityDocument{}, err
	}
	return fsHybridDecodeDocument(result.Body, document)
}
func (c *fsHybridGoogleCarrier) beginOperation(ctx context.Context) error {
	if ctx == nil {
		return errFSHybridBounds
	}
	if err := ctx.Err(); err != nil {
		return &fsHybridControlError{err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing || c.finished || c.watchUnjoined || !time.Now().Before(c.deadline) {
		return errFSHybridClosed
	}
	c.operationWG.Add(1)
	return nil
}
func (c *fsHybridGoogleCarrier) ReadOwn(ctx context.Context) (fsPriorityDocument, error) {
	if err := c.beginOperation(ctx); err != nil {
		return fsPriorityDocument{}, err
	}
	defer c.operationWG.Done()
	return c.read(ctx, c.cfg.OwnDocument)
}
func (c *fsHybridGoogleCarrier) ReadPeer(ctx context.Context) (fsPriorityDocument, error) {
	if err := c.beginOperation(ctx); err != nil {
		return fsPriorityDocument{}, err
	}
	defer c.operationWG.Done()
	return c.read(ctx, c.cfg.PeerDocument)
}

func (c *fsHybridGoogleCarrier) WriteOwn(ctx context.Context, expectedUpdateTime string, wire []byte) (fsPriorityDocument, error) {
	if err := c.beginOperation(ctx); err != nil {
		return fsPriorityDocument{}, err
	}
	defer c.operationWG.Done()
	if !c.writeMu.TryLock() {
		return fsPriorityDocument{}, errFSHybridBusy
	}
	defer c.writeMu.Unlock()
	if !fsHybridServerTimestamp(expectedUpdateTime) || len(wire) == 0 || len(wire) > fsHybridWireMax {
		return fsPriorityDocument{}, errFSHybridBounds
	}
	body, _ := json.Marshal(map[string]any{"fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(wire)}}})
	query := url.Values{"currentDocument.updateTime": []string{expectedUpdateTime}, "updateMask.fieldPaths": []string{"sealed"}}
	result, err := c.call(ctx, "write", http.MethodPatch, "/v1/"+c.cfg.OwnDocument+"?"+query.Encode(), map[string]string{"Content-Type": "application/json"}, body, false)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	if err = fsHybridResponseError(result); err != nil {
		return fsPriorityDocument{}, err
	}
	document, err := fsHybridDecodeDocument(result.Body, c.cfg.OwnDocument)
	if err != nil || !bytes.Equal(document.Ciphertext, wire) {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	return document, nil
}
