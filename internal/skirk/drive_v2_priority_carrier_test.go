package skirk

// Drive-native research only. No CLI defaults, profile grants or Cloud owner
// credentials are changed. The live fixture must own fresh files and share its
// global Drive quota guard with this carrier and the unchanged bulk store.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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

var errDriveV2CASUnsupported = errors.New("Drive v2 conditional metadata write fence unavailable")

const driveV2PriorityCipherMax = 16 << 10

type driveV2PriorityConfig struct {
	PrivateDir, OwnID, PeerID, OwnName, PeerName, QuotaProject string
	Route                                                      RouteConfig
	Token                                                      func(context.Context) (string, error)
	// Required campaign admission hook. Charge the same atomic Drive guard as
	// bulk exactly once, either here or through guarded BOTH HTTP pools. The
	// live harness uses transport gates and this hook checks admission only;
	// charging both layers would double-count. Fake tests have no Google I/O.
	BeforeDispatch                                     func(context.Context, string, string, int) error
	PollInterval, IdleInterval, ActiveHold, Duration   time.Duration
	MaxCalls, MaxQuotaUnitsUpper, MaxRequestBytesUpper int64
}

var driveV2PriorityName = regexp.MustCompile(`^skirk-v2-priority-[a-f0-9]{32}-(up|down)$`)

func (cfg driveV2PriorityConfig) normalized() (driveV2PriorityConfig, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.IdleInterval == 0 {
		cfg.IdleInterval = time.Second
	}
	if cfg.ActiveHold == 0 {
		cfg.ActiveHold = 2 * time.Second
	}
	if cfg.Duration == 0 {
		cfg.Duration = 5 * time.Minute
	}
	if cfg.MaxCalls == 0 {
		cfg.MaxCalls = 2048
	}
	if cfg.MaxQuotaUnitsUpper == 0 {
		cfg.MaxQuotaUnitsUpper = 100000
	}
	if cfg.MaxRequestBytesUpper == 0 {
		cfg.MaxRequestBytesUpper = 64 << 20
	}
	if cfg.PollInterval < 20*time.Millisecond || cfg.PollInterval > 5*time.Second || cfg.IdleInterval < cfg.PollInterval || cfg.IdleInterval > 30*time.Second || cfg.ActiveHold < cfg.PollInterval || cfg.ActiveHold > 30*time.Second || cfg.Duration < time.Second || cfg.Duration > 5*time.Minute || cfg.MaxCalls < 16 || cfg.MaxCalls > 4096 || cfg.MaxQuotaUnitsUpper < 2000 || cfg.MaxQuotaUnitsUpper > 150000 || cfg.MaxRequestBytesUpper < 1<<20 || cfg.MaxRequestBytesUpper > 64<<20 {
		return cfg, errFSHybridBounds
	}
	info, err := os.Lstat(cfg.PrivateDir)
	if err != nil || !filepath.IsAbs(cfg.PrivateDir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 || !strings.Contains(filepath.Clean(cfg.PrivateDir)+string(filepath.Separator), string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) {
		return cfg, errFSHybridBounds
	}
	for _, id := range []string{cfg.OwnID, cfg.PeerID} {
		if len(id) < 16 || len(id) > 128 || !isSafeObjectSegment(id) {
			return cfg, errFSHybridBounds
		}
	}
	if cfg.OwnID == cfg.PeerID || cfg.OwnName == cfg.PeerName || !driveV2PriorityName.MatchString(cfg.OwnName) || !driveV2PriorityName.MatchString(cfg.PeerName) || strings.TrimSuffix(strings.TrimSuffix(cfg.OwnName, "-up"), "-down") != strings.TrimSuffix(strings.TrimSuffix(cfg.PeerName, "-up"), "-down") {
		return cfg, errFSHybridBounds
	}
	if cfg.Route.Mode != "google_front_pinned" || cfg.Route.Proxy != "" || net.ParseIP(cfg.Route.GoogleIP) == nil || cfg.Token == nil || cfg.BeforeDispatch == nil || len(cfg.QuotaProject) > 128 || strings.ContainsAny(cfg.QuotaProject, "\r\n\x00") {
		return cfg, errFSHybridBounds
	}
	return cfg, nil
}

type driveV2PriorityStats struct {
	Reserved, Visible, AttemptsUpper, QuotaUnitsUpper, RequestBytesUpper int64
	ResponseBodyBytes, HTTP2, Reads, Writes, NotModified, Preconditions  int64
	Errors, Canceled, GuardDenied, PollActive, PollIdle                  int64
	TokenSourceCalls, TokenErrors                                        int64
	ProbePassed, Joined                                                  int64
}

type driveV2PriorityCarrier struct {
	cfg                                         driveV2PriorityConfig
	ctx                                         context.Context
	cancel                                      context.CancelFunc
	getClient, patchClient                      *GoogleHTTPClient
	mu                                          sync.Mutex
	wg                                          sync.WaitGroup
	closed, verified, probeStarted, watchActive bool
	watchAttempts                               int
	stats                                       driveV2PriorityStats
	writeMu                                     sync.Mutex
	readSlots                                   chan struct{}
	wake                                        chan struct{}
	closeOnce                                   sync.Once
}

func newDriveV2PriorityCarrier(parent context.Context, cfg driveV2PriorityConfig) (*driveV2PriorityCarrier, error) {
	if parent == nil || parent.Err() != nil {
		return nil, errFSHybridClosed
	}
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	marker, err := os.OpenFile(filepath.Join(cfg.PrivateDir, "drive-v2-carrier-started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errFSHybridBounds
	}
	err = json.NewEncoder(marker).Encode(map[string]any{"own_id": cfg.OwnID, "peer_id": cfg.PeerID, "own_name": cfg.OwnName, "peer_name": cfg.PeerName, "max_visible_calls": cfg.MaxCalls, "max_quota_units_upper": cfg.MaxQuotaUnitsUpper})
	if err == nil {
		err = marker.Sync()
	}
	err = errors.Join(err, marker.Close())
	if err != nil {
		return nil, errFSHybridBounds
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	return &driveV2PriorityCarrier{cfg: cfg, ctx: ctx, cancel: cancel, getClient: NewGoogleHTTPClient(cfg.Route), patchClient: NewGoogleHTTPClient(cfg.Route), readSlots: make(chan struct{}, 2), wake: make(chan struct{}, 1)}, nil
}

func (c *driveV2PriorityCarrier) Snapshot() driveV2PriorityStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *driveV2PriorityCarrier) begin(requireVerified bool) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return nil, errFSHybridClosed
	}
	if requireVerified && !c.verified {
		return nil, errDriveV2CASUnsupported
	}
	c.wg.Add(1)
	return c.wg.Done, nil
}

func (c *driveV2PriorityCarrier) path(id string) string {
	return "/drive/v2/files/" + url.PathEscape(id) + "?fields=id,title,description,version,etag"
}

func (c *driveV2PriorityCarrier) request(ctx context.Context, method, id string, headers map[string]string, body []byte) (*HTTPResult, error) {
	if ctx == nil {
		return nil, errFSHybridBounds
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if (method != http.MethodGet && method != http.MethodPatch) || id != c.cfg.OwnID && id != c.cfg.PeerID || method == http.MethodPatch && id != c.cfg.OwnID {
		return nil, errFSHybridIntegrity
	}
	for key, value := range headers {
		if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errFSHybridIntegrity
		}
		switch key {
		case "Content-Type", "Cache-Control", "If-Match", "If-None-Match":
		default:
			return nil, errFSHybridIntegrity
		}
	}
	units := int64(5)
	if method == http.MethodPatch {
		units = 50
	}
	c.mu.Lock()
	if c.closed || c.ctx.Err() != nil {
		c.mu.Unlock()
		return nil, errFSHybridClosed
	}
	if c.stats.Reserved >= c.cfg.MaxCalls || c.stats.QuotaUnitsUpper+8*units > c.cfg.MaxQuotaUnitsUpper || c.stats.RequestBytesUpper+8*int64(len(body)) > c.cfg.MaxRequestBytesUpper {
		c.mu.Unlock()
		return nil, errFSHybridBounds
	}
	c.stats.Reserved++
	c.stats.AttemptsUpper += 8
	c.stats.QuotaUnitsUpper += 8 * units
	c.stats.RequestBytesUpper += 8 * int64(len(body))
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	requestCtx, cancel := context.WithTimeout(withGoogleResponseLimit(ctx, 32<<10), 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	path := c.path(id)
	if err := c.cfg.BeforeDispatch(requestCtx, method, path, len(body)); err != nil {
		c.mu.Lock()
		c.stats.GuardDenied++
		c.mu.Unlock()
		return nil, err
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.stats.TokenSourceCalls++
	c.mu.Unlock()
	token, err := c.cfg.Token(requestCtx)
	if err != nil {
		c.mu.Lock()
		c.stats.TokenErrors++
		c.mu.Unlock()
		return nil, err
	}
	if len(token) == 0 || len(token) > 16384 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errFSHybridIntegrity
	}
	selected := map[string]string{"Authorization": "Bearer " + token, "Accept-Encoding": "identity"}
	if c.cfg.QuotaProject != "" {
		selected["X-Goog-User-Project"] = c.cfg.QuotaProject
	}
	for key, value := range headers {
		selected[key] = value
	}
	client := c.getClient
	if method == http.MethodPatch {
		client = c.patchClient
	}
	c.mu.Lock()
	c.stats.Visible++
	if method == http.MethodGet {
		c.stats.Reads++
	} else {
		c.stats.Writes++
	}
	c.mu.Unlock()
	result, err := client.RequestSingleAttempt(requestCtx, method, "www.googleapis.com", path, selected, body)
	// A provider that completes after cancellation still counts as a visible
	// call and must be joined before external cleanup, but cannot make a late
	// caller appear live again. Ambiguous server-side mutation remains possible.
	if err == nil && result != nil && result.Status >= 200 && result.Status < 400 {
		if requestCtx.Err() != nil {
			err = requestCtx.Err()
		} else if c.ctx.Err() != nil {
			err = c.ctx.Err()
		}
	}
	c.mu.Lock()
	if result != nil {
		c.stats.ResponseBodyBytes += int64(len(result.Body))
		if result.Proto == "HTTP/2.0" {
			c.stats.HTTP2++
		}
		if result.Status == 304 {
			c.stats.NotModified++
		}
		if result.Status == 412 {
			c.stats.Preconditions++
		}
	}
	if errors.Is(err, context.Canceled) {
		c.stats.Canceled++
	} else if err != nil || result != nil && result.Status >= 400 && result.Status != 412 {
		c.stats.Errors++
	}
	c.mu.Unlock()
	if err != nil {
		return result, err
	}
	if result == nil || result.Proto != "HTTP/2.0" {
		return result, errFSHybridIntegrity
	}
	return result, nil
}

func (c *driveV2PriorityCarrier) decode(result *HTTPResult, id, name string) (fsPriorityDocument, error) {
	if result == nil || result.Status != 200 {
		return fsPriorityDocument{}, fsHybridResponseError(result)
	}
	var metadata struct{ ID, Title, Description, ETag, Version string }
	dec := json.NewDecoder(bytes.NewReader(result.Body))
	dec.DisallowUnknownFields()
	if dec.Decode(&metadata) != nil || dec.Decode(new(any)) != io.EOF || metadata.ID != id || metadata.Title != name || len(metadata.Description) > base64.RawURLEncoding.EncodedLen(driveV2PriorityCipherMax) {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	wire, err := base64.RawURLEncoding.Strict().DecodeString(metadata.Description)
	if err != nil || base64.RawURLEncoding.EncodeToString(wire) != metadata.Description || len(wire) > driveV2PriorityCipherMax {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	doc := fsPriorityDocument{Ciphertext: wire, FenceKind: fsPriorityDriveV2Fence, OpaqueFence: metadata.ETag}
	if !doc.valid() || strings.HasPrefix(metadata.ETag, "W/") || result.Header.Get("ETag") != "" && result.Header.Get("ETag") != metadata.ETag {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	return doc, nil
}

func (c *driveV2PriorityCarrier) read(ctx context.Context, id, name, tag string) (fsPriorityDocument, bool, error) {
	select {
	case c.readSlots <- struct{}{}:
		defer func() { <-c.readSlots }()
	default:
		return fsPriorityDocument{}, false, errFSHybridBusy
	}
	headers := map[string]string{"Cache-Control": "no-cache"}
	if tag != "" {
		headers["If-None-Match"] = tag
	}
	result, err := c.request(ctx, http.MethodGet, id, headers, nil)
	if err != nil {
		return fsPriorityDocument{}, false, err
	}
	if result.Status == 304 {
		if tag == "" {
			return fsPriorityDocument{}, false, errFSHybridIntegrity
		}
		return fsPriorityDocument{}, true, nil
	}
	doc, err := c.decode(result, id, name)
	return doc, false, err
}

func (c *driveV2PriorityCarrier) patch(ctx context.Context, tag string, wire []byte) (fsPriorityDocument, error) {
	fence := fsPriorityDocument{FenceKind: fsPriorityDriveV2Fence, OpaqueFence: tag}
	if !fence.valid() || len(wire) > driveV2PriorityCipherMax {
		return fsPriorityDocument{}, errFSHybridBounds
	}
	body, _ := json.Marshal(map[string]string{"description": base64.RawURLEncoding.EncodeToString(wire)})
	result, err := c.request(ctx, http.MethodPatch, c.cfg.OwnID, map[string]string{"Content-Type": "application/json", "If-Match": tag}, body)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	doc, err := c.decode(result, c.cfg.OwnID, c.cfg.OwnName)
	if err == nil && (!bytes.Equal(doc.Ciphertext, wire) || doc.OpaqueFence == tag) {
		return fsPriorityDocument{}, errFSHybridIntegrity
	}
	return doc, err
}

// Both direction probes MUST finish before either store starts. A maximum
// opaque body verifies actual description capacity, then is reset to empty.
// An unsupported or ambiguous probe never grants application readiness.
func (c *driveV2PriorityCarrier) ProbeOwnCAS(ctx context.Context) error {
	done, err := c.begin(false)
	if err != nil {
		return err
	}
	defer done()
	c.mu.Lock()
	if c.probeStarted {
		c.mu.Unlock()
		return errFSHybridBounds
	}
	c.probeStarted = true
	c.mu.Unlock()
	if !c.writeMu.TryLock() {
		return errFSHybridBusy
	}
	defer c.writeMu.Unlock()
	old, _, err := c.read(ctx, c.cfg.OwnID, c.cfg.OwnName, "")
	if err != nil {
		return err
	}
	if len(old.Ciphertext) != 0 {
		return errFSHybridIntegrity
	}
	bogus := make([]byte, 16)
	probe := make([]byte, driveV2PriorityCipherMax)
	if _, err = rand.Read(bogus); err != nil {
		return err
	}
	if _, err = rand.Read(probe); err != nil {
		return err
	}
	bogusTag := "\"skirk-impossible-" + hex.EncodeToString(bogus) + "\""
	if bogusTag == old.OpaqueFence {
		return errFSHybridIntegrity
	}
	_, err = c.patch(ctx, bogusTag, nil)
	if !fsHybridPrecondition(err) {
		if err != nil {
			return err
		}
		return errDriveV2CASUnsupported
	}
	next, err := c.patch(ctx, old.OpaqueFence, probe)
	if err != nil {
		return err
	}
	if next.OpaqueFence == old.OpaqueFence || !bytes.Equal(next.Ciphertext, probe) {
		return errDriveV2CASUnsupported
	}
	_, err = c.patch(ctx, old.OpaqueFence, nil)
	if !fsHybridPrecondition(err) {
		if err != nil {
			return err
		}
		return errDriveV2CASUnsupported
	}
	readback, _, err := c.read(ctx, c.cfg.OwnID, c.cfg.OwnName, "")
	if err != nil {
		return err
	}
	if !readback.sameFence(next) || !bytes.Equal(readback.Ciphertext, probe) {
		return errFSHybridIntegrity
	}
	reset, err := c.patch(ctx, next.OpaqueFence, nil)
	if err != nil {
		return err
	}
	if len(reset.Ciphertext) != 0 || reset.sameFence(next) || reset.sameFence(old) {
		return errFSHybridIntegrity
	}
	empty, _, err := c.read(ctx, c.cfg.OwnID, c.cfg.OwnName, "")
	if err != nil {
		return err
	}
	if !empty.sameFence(reset) || len(empty.Ciphertext) != 0 {
		return errFSHybridIntegrity
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return errFSHybridClosed
	}
	c.verified = true
	c.stats.ProbePassed = 1
	return nil
}

func (c *driveV2PriorityCarrier) ReadOwn(ctx context.Context) (fsPriorityDocument, error) {
	done, err := c.begin(true)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	defer done()
	doc, _, err := c.read(ctx, c.cfg.OwnID, c.cfg.OwnName, "")
	return doc, err
}
func (c *driveV2PriorityCarrier) ReadPeer(ctx context.Context) (fsPriorityDocument, error) {
	done, err := c.begin(true)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	defer done()
	doc, _, err := c.read(ctx, c.cfg.PeerID, c.cfg.PeerName, "")
	return doc, err
}
func (c *driveV2PriorityCarrier) WriteOwn(ctx context.Context, tag string, wire []byte) (fsPriorityDocument, error) {
	done, err := c.begin(true)
	if err != nil {
		return fsPriorityDocument{}, err
	}
	defer done()
	if !c.writeMu.TryLock() {
		return fsPriorityDocument{}, errFSHybridBusy
	}
	defer c.writeMu.Unlock()
	doc, err := c.patch(ctx, tag, wire)
	if err == nil {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	return doc, err
}

func (c *driveV2PriorityCarrier) WatchPeer(ctx context.Context, callback func(fsPriorityDocument) error) error {
	if callback == nil || ctx == nil {
		return errFSHybridBounds
	}
	done, err := c.begin(true)
	if err != nil {
		return err
	}
	defer done()
	c.mu.Lock()
	if c.watchActive {
		c.mu.Unlock()
		return errFSHybridBusy
	}
	if c.watchAttempts >= 3 {
		c.mu.Unlock()
		return errFSHybridBounds
	}
	c.watchAttempts++
	c.watchActive = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.watchActive = false; c.mu.Unlock() }()
	tag := ""
	activeUntil := time.Now().Add(c.cfg.ActiveHold)
	for {
		doc, same, err := c.read(ctx, c.cfg.PeerID, c.cfg.PeerName, tag)
		if err != nil {
			return err
		}
		if !same {
			tag = doc.OpaqueFence
			activeUntil = time.Now().Add(c.cfg.ActiveHold)
			if err = callback(doc); err != nil {
				return err
			}
		}
		delay := c.cfg.IdleInterval
		c.mu.Lock()
		if time.Now().Before(activeUntil) {
			delay = c.cfg.PollInterval
			c.stats.PollActive++
		} else {
			c.stats.PollIdle++
		}
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-c.wake:
			timer.Stop()
			activeUntil = time.Now().Add(c.cfg.ActiveHold)
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.ctx.Done():
			timer.Stop()
			return errFSHybridClosed
		}
	}
}

func (c *driveV2PriorityCarrier) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
		c.getClient.client.CloseIdleConnections()
		c.patchClient.client.CloseIdleConnections()
		c.wg.Wait()
		c.mu.Lock()
		c.stats.Joined = 1
		c.mu.Unlock()
	})
	return nil
}
