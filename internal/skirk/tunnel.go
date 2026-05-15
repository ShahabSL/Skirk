package skirk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	inlineDataThreshold           = 64 * 1024
	mediumDataThreshold           = 8 * 1024
	bulkStreamCoalesceAfter       = 256 * 1024
	initialOpenDataWait           = 15 * time.Millisecond
	interactiveCoalesceDelay      = 5 * time.Millisecond
	mediumCoalesceDelay           = 50 * time.Millisecond
	bulkCoalesceDelay             = 75 * time.Millisecond
	forcedBulkCoalesceDelay       = 100 * time.Millisecond
	interactiveCoalesceMaxAge     = 15 * time.Millisecond
	mediumCoalesceMaxAge          = 75 * time.Millisecond
	bulkCoalesceMaxAge            = 250 * time.Millisecond
	forcedBulkCoalesceMaxAge      = 1 * time.Second
	deferredCleanupDelay          = 5 * time.Second
	deferredCleanupFlushThreshold = 128
	idleOpenPollInterval          = 1 * time.Second
	openPollWarmWindow            = 45 * time.Second
	directDriveSlowThreshold      = 5 * time.Second
	proxyDriveSlowThreshold       = 10 * time.Second
	limiterBulkByteThreshold      = 1 * 1024 * 1024
	limiterDirectBulkBytesPerSec  = 2 * 1024 * 1024
	limiterProxyBulkBytesPerSec   = 1 * 1024 * 1024
	limiterBackoffCooldown        = 2 * time.Second
	limiterMinNormalSlots         = 2
	autoClientUploadWorkers       = 8
	autoClientProxyUploadWorkers  = 4
	autoExitUploadWorkers         = 16
	autoExitUploadMaxWorkers      = 32
	autoClientUploadWindow        = 4
	autoClientProxyUploadWindow   = 2
	autoExitUploadWindow          = 8
	autoExitExplicitUploadWindow  = 16
	exitFamilyPreferenceTimeout   = 2 * time.Second
	cleanupQuietWindow            = 2 * time.Second
	cleanupMaxForegroundDelay     = 2 * time.Minute
	exitDialTimeout               = 30 * time.Second
	burstSlowListThreshold        = 3 * time.Second
	burstCooldownAfterSlow        = 20 * time.Second
)

type Tunnel struct {
	Data                 BlobStore
	Secret               string
	SessionID            [16]byte
	ClientID             string
	RunID                string
	ChunkSize            int
	Transport            string
	Concurrency          int
	UploadConcurrency    int
	DownloadConcurrency  int
	Profile              string
	RouteProxy           string
	ExitProxy            string
	ExitIPFamily         string
	BurstPoll            bool
	BurstPollInterval    time.Duration
	BurstPollWindow      time.Duration
	Observe              bool
	role                 string
	activeStreams        atomic.Int64
	limiterMu            sync.Mutex
	uploadLimiter        *adaptiveLimiter
	downloadLimiter      *adaptiveLimiter
	muxMu                sync.Mutex
	clientMux            *driveMux
	lastActivityNS       int64
	lastUploadNS         int64
	burstDisabledUntilNS int64
	PollInterval         time.Duration
	CleanupProcessed     bool
	Logger               *log.Logger
}

func NewTunnel(data BlobStore, cfg *Config) (*Tunnel, error) {
	sid, err := ParseSessionID(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{
		Data:                data,
		Secret:              cfg.Secret,
		SessionID:           sid,
		ClientID:            strings.TrimSpace(cfg.Client.ID),
		RunID:               strings.TrimSpace(cfg.Client.RunID),
		ChunkSize:           cfg.Tunnel.ChunkSize,
		Transport:           strings.TrimSpace(cfg.Tunnel.Transport),
		Concurrency:         cfg.Tunnel.Concurrency,
		UploadConcurrency:   cfg.Tunnel.UploadConcurrency,
		DownloadConcurrency: cfg.Tunnel.DownloadConcurrency,
		Profile:             cfg.Tunnel.Profile,
		RouteProxy:          cfg.Route.Proxy,
		ExitProxy:           strings.TrimSpace(cfg.Tunnel.ExitProxy),
		ExitIPFamily:        strings.TrimSpace(cfg.Tunnel.ExitIPFamily),
		BurstPoll:           cfg.Tunnel.BurstPoll,
		BurstPollInterval:   time.Duration(cfg.Tunnel.BurstPollMS) * time.Millisecond,
		BurstPollWindow:     time.Duration(cfg.Tunnel.BurstPollWindowMS) * time.Millisecond,
		Observe:             cfg.Tunnel.Observe || envBool("SKIRK_OBSERVE"),
		PollInterval:        cfg.PollInterval(),
		CleanupProcessed:    cfg.Tunnel.CleanupProcessed,
		Logger:              log.Default(),
	}
	t.markActivity()
	return t, nil
}

func (t *Tunnel) ServeClient(ctx context.Context, listen string) error {
	return t.serveMuxClient(ctx, listen)
}

func (t *Tunnel) ServeExit(ctx context.Context) error {
	return t.serveMuxExit(ctx)
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func errorSummary(err error) string {
	if err == nil {
		return "none"
	}
	return sanitizeTransportErrorText(err.Error())
}

func sanitizeTransportErrorText(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch {
	case lower == "":
		return ""
	case strings.Contains(lower, "context canceled"):
		return "context_canceled"
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "timeout"):
		return "timeout"
	case strings.Contains(lower, "connection refused"):
		return "connection_refused"
	case strings.Contains(lower, "connection reset"):
		return "connection_reset"
	case strings.Contains(lower, "broken pipe"):
		return "broken_pipe"
	case strings.Contains(lower, "no such host"):
		return "dns_failure"
	case strings.Contains(lower, "network is unreachable"):
		return "network_unreachable"
	case strings.Contains(lower, "use of closed network connection"):
		return "closed"
	case strings.Contains(lower, "remote reset"):
		return "remote_reset"
	default:
		return "transport_error"
	}
}

func targetFingerprint(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:6])
}

func controlIsFresh(info ObjectInfo, startedAt time.Time) bool {
	if strings.TrimSpace(info.Updated) == "" {
		return true
	}
	updated, err := time.Parse(time.RFC3339Nano, info.Updated)
	if err != nil {
		return true
	}
	return !updated.Before(startedAt)
}

func (t *Tunnel) markActivity() {
	atomic.StoreInt64(&t.lastActivityNS, time.Now().UnixNano())
}

func (t *Tunnel) markUpload() {
	now := time.Now()
	atomic.StoreInt64(&t.lastUploadNS, now.UnixNano())
	atomic.StoreInt64(&t.lastActivityNS, now.UnixNano())
}

func (t *Tunnel) markSlowList(duration time.Duration) {
	if !t.BurstPoll || duration < burstSlowListThreshold {
		return
	}
	atomic.StoreInt64(&t.burstDisabledUntilNS, time.Now().Add(burstCooldownAfterSlow).UnixNano())
}

func (t *Tunnel) burstPollActive(now time.Time) bool {
	if t == nil || !t.BurstPoll || t.BurstPollInterval <= 0 || t.BurstPollWindow <= 0 {
		return false
	}
	if disabledUntil := atomic.LoadInt64(&t.burstDisabledUntilNS); disabledUntil > 0 && now.Before(time.Unix(0, disabledUntil)) {
		return false
	}
	lastUpload := atomic.LoadInt64(&t.lastUploadNS)
	if lastUpload <= 0 {
		return false
	}
	if now.Sub(time.Unix(0, lastUpload)) > t.BurstPollWindow {
		return false
	}
	return t.activeStreams.Load() > 0 || t.recentActivity()
}

func (t *Tunnel) recentActivity() bool {
	last := atomic.LoadInt64(&t.lastActivityNS)
	return last > 0 && time.Since(time.Unix(0, last)) <= openPollWarmWindow
}

func (t *Tunnel) deleteData(ctx context.Context, name, fileID string) error {
	release, err := t.acquireUploadSlot(ctx, false)
	if err != nil {
		return err
	}
	defer func() {
		if release != nil {
			release(nil)
		}
	}()
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			err = store.DeleteID(ctx, fileID)
			release(err)
			release = nil
			return err
		}
	}
	err = t.Data.Delete(ctx, name)
	release(err)
	release = nil
	return err
}

func (t *Tunnel) dialExitTarget(ctx context.Context, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, exitDialTimeout)
	defer cancel()
	if proxy := strings.TrimSpace(t.ExitProxy); proxy != "" && !bypassExitProxy(target) {
		return DialViaProxy(ctx, proxy, target)
	}
	return dialDirectExitTarget(ctx, target, t.ExitIPFamily)
}

func dialDirectExitTarget(ctx context.Context, target, family string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: exitDialTimeout, KeepAlive: 30 * time.Second}
	primary, fallback := exitDialNetworks(target, family)
	if primary != "" {
		attemptCtx, cancel := context.WithTimeout(ctx, exitFamilyPreferenceTimeout)
		conn, err := dialer.DialContext(attemptCtx, primary, target)
		cancel()
		if err == nil {
			return conn, nil
		}
		if fallback == "" || !shouldFallbackFromFamilyDial(err) {
			return nil, err
		}
	}
	if fallback == "" {
		fallback = "tcp"
	}
	return dialer.DialContext(ctx, fallback, target)
}

func exitDialNetworks(target, family string) (string, string) {
	switch strings.TrimSpace(family) {
	case "ipv4_only":
		return "tcp4", ""
	case "prefer_ipv6":
		if targetSupportsIPFamilyPreference(target, "ipv6") {
			return "tcp6", "tcp"
		}
		return "", "tcp"
	case "ipv6_only":
		return "tcp6", ""
	case "auto":
		return "", "tcp"
	default:
		if targetSupportsIPFamilyPreference(target, "ipv4") {
			return "tcp4", "tcp"
		}
		return "", "tcp"
	}
}

func targetSupportsIPFamilyPreference(target, family string) bool {
	trimmed := strings.TrimSpace(target)
	host, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		ip := net.ParseIP(trimmed)
		return ip == nil || ipMatchesFamily(ip, family)
	}
	ip := net.ParseIP(host)
	return ip == nil || ipMatchesFamily(ip, family)
}

func ipMatchesFamily(ip net.IP, family string) bool {
	if ip == nil {
		return true
	}
	if family == "ipv4" {
		return ip.To4() != nil
	}
	return ip.To4() == nil
}

func shouldFallbackFromFamilyDial(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no suitable address") ||
		strings.Contains(text, "network is unreachable") ||
		strings.Contains(text, "address family not supported") ||
		strings.Contains(text, "no such host")
}

func bypassExitProxy(target string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil {
		host = strings.TrimSpace(target)
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *Tunnel) uploadWorkerCount() int {
	if t.UploadConcurrency > 0 {
		workers := clampWorkers(t.UploadConcurrency)
		if t.autoProfile() && t.role == "exit" && workers > autoExitUploadMaxWorkers {
			return autoExitUploadMaxWorkers
		}
		return workers
	}
	if t.autoProfile() {
		switch t.role {
		case "client":
			if t.RouteProxy != "" {
				return autoClientProxyUploadWorkers
			}
			return autoClientUploadWorkers
		case "exit":
			return autoExitUploadWorkers
		}
	}
	return clampWorkers(t.Concurrency)
}

func (t *Tunnel) downloadWorkerCount() int {
	if t.DownloadConcurrency > 0 {
		return clampWorkers(t.DownloadConcurrency)
	}
	if t.autoProfile() {
		switch t.role {
		case "client":
			if t.RouteProxy != "" {
				return 16
			}
			return 32
		case "exit":
			return 16
		}
	}
	return clampWorkers(t.Concurrency)
}

func (t *Tunnel) autoProfile() bool {
	return strings.TrimSpace(t.Profile) == "" || strings.TrimSpace(t.Profile) == "auto"
}

func (t *Tunnel) acquireUploadSlot(ctx context.Context, priority bool) (func(error), error) {
	return t.limiter(true).Acquire(ctx, priority)
}

func (t *Tunnel) acquireUploadSlotBytes(ctx context.Context, priority bool) (func(error, int64), error) {
	return t.limiter(true).AcquireBytes(ctx, priority)
}

func (t *Tunnel) acquireDownloadSlotBytes(ctx context.Context, priority bool) (func(error, int64), error) {
	return t.limiter(false).AcquireBytes(ctx, priority)
}

func (t *Tunnel) limiter(upload bool) *adaptiveLimiter {
	t.limiterMu.Lock()
	defer t.limiterMu.Unlock()
	if upload {
		if t.uploadLimiter == nil {
			max := t.uploadWorkerCount()
			t.uploadLimiter = newAdaptiveLimiter(t.initialUploadWindow(max), max, t.slowDriveThreshold(), t.limiterLabel(upload), t.Logger)
		}
		return t.uploadLimiter
	}
	if t.downloadLimiter == nil {
		max := t.downloadWorkerCount()
		t.downloadLimiter = newAdaptiveLimiter(t.initialDownloadWindow(max), max, t.slowDriveThreshold(), t.limiterLabel(upload), t.Logger)
	}
	return t.downloadLimiter
}

func (t *Tunnel) limiterLabel(upload bool) string {
	role := t.role
	if role == "" {
		role = "tunnel"
	}
	if upload {
		return role + "/upload"
	}
	return role + "/download"
}

func (t *Tunnel) slowDriveThreshold() time.Duration {
	if t.RouteProxy != "" {
		return proxyDriveSlowThreshold
	}
	return directDriveSlowThreshold
}

func (t *Tunnel) initialUploadWindow(max int) int {
	if !t.autoProfile() {
		return max
	}
	switch t.role {
	case "client":
		if t.RouteProxy != "" {
			return minInt(autoClientProxyUploadWindow, max)
		}
		return minInt(autoClientUploadWindow, max)
	case "exit":
		if t.UploadConcurrency > 0 {
			return minInt(autoExitExplicitUploadWindow, max)
		}
		return minInt(autoExitUploadWindow, max)
	default:
		return minInt(autoClientUploadWindow, max)
	}
}

func (t *Tunnel) initialDownloadWindow(max int) int {
	if !t.autoProfile() {
		return max
	}
	switch t.role {
	case "client":
		if t.RouteProxy != "" {
			return minInt(8, max)
		}
		return minInt(8, max)
	case "exit":
		return max
	default:
		return minInt(8, max)
	}
}

func clampWorkers(workers int) int {
	if workers < 1 {
		return 1
	}
	if workers > 64 {
		return 64
	}
	return workers
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type adaptiveLimiter struct {
	mu            sync.Mutex
	limit         int
	max           int
	reserve       int
	priorityWait  int
	inFlight      int
	priorityBusy  int
	successes     int
	slowThreshold time.Duration
	bulkBytesPerS int64
	name          string
	logger        *log.Logger
	lastLog       time.Time
	backoffUntil  time.Time
}

func newAdaptiveLimiter(initial, max int, slowThreshold time.Duration, name string, logger *log.Logger) *adaptiveLimiter {
	max = clampWorkers(max)
	if initial < 1 {
		initial = 1
	}
	if initial > max {
		initial = max
	}
	if slowThreshold <= 0 {
		slowThreshold = directDriveSlowThreshold
	}
	bytesPerSecond := int64(limiterDirectBulkBytesPerSec)
	if slowThreshold >= proxyDriveSlowThreshold {
		bytesPerSecond = limiterProxyBulkBytesPerSec
	}
	return &adaptiveLimiter{limit: initial, max: max, reserve: priorityReserve(max), slowThreshold: slowThreshold, bulkBytesPerS: bytesPerSecond, name: name, logger: logger}
}

func priorityReserve(max int) int {
	if max <= 1 {
		return 0
	}
	reserve := max / 8
	if reserve < 1 {
		reserve = 1
	}
	if reserve > 4 {
		reserve = 4
	}
	return reserve
}

func (l *adaptiveLimiter) Acquire(ctx context.Context, priority bool) (func(error), error) {
	release, err := l.AcquireBytes(ctx, priority)
	if err != nil {
		return nil, err
	}
	return func(err error) {
		release(err, 0)
	}, nil
}

func (l *adaptiveLimiter) AcquireBytes(ctx context.Context, priority bool) (func(error, int64), error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	registeredPriority := false
	for {
		l.mu.Lock()
		if priority && !registeredPriority {
			l.priorityWait++
			registeredPriority = true
		}
		if l.canAcquireLocked(priority) {
			if registeredPriority && l.priorityWait > 0 {
				l.priorityWait--
				registeredPriority = false
			}
			l.inFlight++
			if priority {
				l.priorityBusy++
			}
			l.mu.Unlock()
			started := time.Now()
			var once sync.Once
			return func(err error, bytes int64) {
				once.Do(func() {
					l.release(priority, err, time.Since(started), bytes)
				})
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			if registeredPriority {
				l.mu.Lock()
				if l.priorityWait > 0 {
					l.priorityWait--
				}
				l.mu.Unlock()
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *adaptiveLimiter) canAcquireLocked(priority bool) bool {
	if priority {
		if l.inFlight < l.limit {
			return true
		}
		reserve := l.priorityReserveLocked()
		return reserve > 0 && l.priorityBusy < reserve && l.inFlight < l.max
	}
	if l.inFlight >= l.limit {
		return false
	}
	if l.priorityWait > 0 {
		return false
	}
	normalLimit := l.limit - l.priorityReserveLocked()
	if normalLimit < 1 {
		normalLimit = 1
	}
	return l.inFlight < normalLimit
}

func (l *adaptiveLimiter) priorityReserveLocked() int {
	if l.limit <= 1 || l.reserve <= 0 {
		return 0
	}
	reserve := l.reserve
	if reserve > l.limit-1 {
		reserve = l.limit - 1
	}
	return reserve
}

func (l *adaptiveLimiter) release(priority bool, err error, duration time.Duration, bytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	if priority && l.priorityBusy > 0 {
		l.priorityBusy--
	}
	oldLimit := l.limit
	reason := ""
	now := time.Now()
	if err != nil {
		if l.backoffLocked(now, true) {
			reason = "error"
		}
		l.successes = 0
		l.logChangeLocked(oldLimit, reason, duration, bytes)
		return
	}
	slowThreshold := l.effectiveSlowThresholdLocked(priority, bytes)
	if duration >= slowThreshold {
		if l.backoffLocked(now, duration >= 2*slowThreshold) {
			reason = "slow"
		}
		l.successes = 0
		l.logChangeLocked(oldLimit, reason, duration, bytes)
		return
	}
	l.successes++
	threshold := maxInt(2, l.limit*2)
	if l.successes >= threshold && l.limit < l.max {
		l.limit++
		l.successes = 0
		reason = "healthy"
	}
	l.logChangeLocked(oldLimit, reason, duration, bytes)
}

func (l *adaptiveLimiter) backoffLocked(now time.Time, severe bool) bool {
	if !l.backoffUntil.IsZero() && now.Before(l.backoffUntil) {
		return false
	}
	floor := l.minimumLimitLocked()
	if l.limit <= floor {
		return false
	}
	if severe {
		l.limit = maxInt(floor, l.limit/2)
	} else {
		l.limit--
		if l.limit < floor {
			l.limit = floor
		}
	}
	l.backoffUntil = now.Add(limiterBackoffCooldown)
	return true
}

func (l *adaptiveLimiter) effectiveSlowThresholdLocked(priority bool, bytes int64) time.Duration {
	if priority || bytes < limiterBulkByteThreshold || l.bulkBytesPerS <= 0 {
		return l.slowThreshold
	}
	budget := time.Second + time.Duration(bytes*int64(time.Second)/l.bulkBytesPerS)
	if budget < l.slowThreshold {
		return l.slowThreshold
	}
	return budget
}

func (l *adaptiveLimiter) logChangeLocked(oldLimit int, reason string, duration time.Duration, bytes int64) {
	if l.logger == nil || reason == "" || oldLimit == l.limit {
		return
	}
	now := time.Now()
	if now.Sub(l.lastLog) < 2*time.Second {
		return
	}
	l.lastLog = now
	if bytes > 0 {
		l.logger.Printf("drive limiter %s window=%d->%d max=%d reason=%s duration=%s bytes=%d", l.name, oldLimit, l.limit, l.max, reason, duration.Round(time.Millisecond), bytes)
		return
	}
	l.logger.Printf("drive limiter %s window=%d->%d max=%d reason=%s duration=%s", l.name, oldLimit, l.limit, l.max, reason, duration.Round(time.Millisecond))
}

func (l *adaptiveLimiter) minimumLimitLocked() int {
	if l.max <= 1 {
		return 1
	}
	floor := l.reserve + limiterMinNormalSlots
	if floor < 2 {
		return 2
	}
	if floor > l.max {
		return l.max
	}
	return floor
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type cleanupTask struct {
	name string
	id   string
}

type deferredCleanup struct {
	tasks []cleanupTask
	t     *Tunnel
}

func (t *Tunnel) newDeferredCleanup() *deferredCleanup {
	return &deferredCleanup{t: t}
}

func (c *deferredCleanup) Data(name, id string) {
	c.add(cleanupTask{name: name, id: id})
}

func (c *deferredCleanup) add(task cleanupTask) {
	if c == nil || c.t == nil || !c.t.CleanupProcessed || (task.name == "" && task.id == "") {
		return
	}
	c.tasks = append(c.tasks, task)
	if len(c.tasks) >= deferredCleanupFlushThreshold {
		c.flushAsyncAfter(0)
	}
}

func (c *deferredCleanup) flushAsyncAfter(delay time.Duration) {
	if c == nil || c.t == nil || len(c.tasks) == 0 {
		return
	}
	tasks := append([]cleanupTask(nil), c.tasks...)
	c.tasks = c.tasks[:0]
	tunnel := c.t
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		tunnel.waitForCleanupQuiet(context.Background())
		workers := 1
		jobs := make(chan cleanupTask)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for task := range jobs {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					_ = tunnel.deleteData(ctx, task.name, task.id)
					cancel()
				}
			}()
		}
		for _, task := range tasks {
			jobs <- task
		}
		close(jobs)
		wg.Wait()
	}()
}

func (t *Tunnel) waitForCleanupQuiet(ctx context.Context) {
	if t == nil || cleanupMaxForegroundDelay <= 0 {
		return
	}
	deadline := time.NewTimer(cleanupMaxForegroundDelay)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for t.foregroundBusy() {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (t *Tunnel) foregroundBusy() bool {
	if t == nil {
		return false
	}
	if t.activeStreams.Load() > 0 {
		return true
	}
	last := atomic.LoadInt64(&t.lastActivityNS)
	return last > 0 && time.Since(time.Unix(0, last)) < cleanupQuietWindow
}

func readChunk(reader io.Reader, buffer []byte) (int, error) {
	return readChunkWithPolicy(reader, buffer, false)
}

func readChunkWithPolicy(reader io.Reader, buffer []byte, forceBulk bool) (int, error) {
	n, err := reader.Read(buffer)
	if n <= 0 || err != nil || n == len(buffer) {
		return n, err
	}
	deadlineConn, ok := reader.(interface {
		SetReadDeadline(time.Time) error
	})
	if !ok {
		return n, err
	}
	defer deadlineConn.SetReadDeadline(time.Time{})
	started := time.Now()
	for n < len(buffer) {
		delay := coalesceDelayForBytesWithPolicy(n, forceBulk)
		maxAge := coalesceMaxAgeForBytesWithPolicy(n, forceBulk)
		if maxAge > 0 {
			remaining := maxAge - time.Since(started)
			if remaining <= 0 {
				return n, nil
			}
			if delay > remaining {
				delay = remaining
			}
		}
		deadline := time.Now().Add(delay)
		if err := deadlineConn.SetReadDeadline(deadline); err != nil {
			return n, nil
		}
		m, readErr := reader.Read(buffer[n:])
		if m > 0 {
			n += m
		}
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				return n, nil
			}
			return n, readErr
		}
		if m == 0 {
			return n, nil
		}
	}
	return n, nil
}

func coalesceDelayForBytesWithPolicy(n int, forceBulk bool) time.Duration {
	if forceBulk {
		return forcedBulkCoalesceDelay
	}
	if n >= inlineDataThreshold {
		return bulkCoalesceDelay
	}
	if n >= mediumDataThreshold {
		return mediumCoalesceDelay
	}
	return interactiveCoalesceDelay
}

func coalesceMaxAgeForBytesWithPolicy(n int, forceBulk bool) time.Duration {
	if forceBulk {
		return forcedBulkCoalesceMaxAge
	}
	if n >= inlineDataThreshold {
		return bulkCoalesceMaxAge
	}
	if n >= mediumDataThreshold {
		return mediumCoalesceMaxAge
	}
	return interactiveCoalesceMaxAge
}

func readInitialClientData(conn net.Conn, limit int, wait time.Duration) ([]byte, error) {
	if limit <= 0 || wait <= 0 {
		return nil, nil
	}
	buf := make([]byte, limit)
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, nil
	}
	defer conn.SetReadDeadline(time.Time{})
	n, err := conn.Read(buf)
	if n > 0 {
		return append([]byte(nil), buf[:n]...), nil
	}
	if err == nil {
		return nil, nil
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return nil, nil
	}
	if err == io.EOF {
		return nil, err
	}
	return nil, err
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
