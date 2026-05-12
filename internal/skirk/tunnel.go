package skirk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	inlineDataThreshold           = 64 * 1024
	mediumDataThreshold           = 8 * 1024
	initialOpenDataWait           = 15 * time.Millisecond
	interactiveCoalesceDelay      = 5 * time.Millisecond
	mediumCoalesceDelay           = 50 * time.Millisecond
	bulkCoalesceDelay             = 300 * time.Millisecond
	deferredCleanupDelay          = 5 * time.Second
	deferredCleanupFlushThreshold = 2048
	idleOpenPollInterval          = 1 * time.Second
	openPollWarmWindow            = 45 * time.Second
	directDriveSlowThreshold      = 20 * time.Second
	proxyDriveSlowThreshold       = 35 * time.Second
	cleanupQuietWindow            = 10 * time.Second
	cleanupMaxForegroundDelay     = 2 * time.Minute
	exitDialTimeout               = 30 * time.Second
	burstSlowListThreshold        = 3 * time.Second
	burstCooldownAfterSlow        = 20 * time.Second
)

type Tunnel struct {
	Data                 BlobStore
	Secret               string
	SessionID            [16]byte
	ChunkSize            int
	Concurrency          int
	UploadConcurrency    int
	DownloadConcurrency  int
	Profile              string
	RouteProxy           string
	ExitProxy            string
	BurstPoll            bool
	BurstPollInterval    time.Duration
	BurstPollWindow      time.Duration
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
		ChunkSize:           cfg.Tunnel.ChunkSize,
		Concurrency:         cfg.Tunnel.Concurrency,
		UploadConcurrency:   cfg.Tunnel.UploadConcurrency,
		DownloadConcurrency: cfg.Tunnel.DownloadConcurrency,
		Profile:             cfg.Tunnel.Profile,
		RouteProxy:          cfg.Route.Proxy,
		ExitProxy:           strings.TrimSpace(cfg.Tunnel.ExitProxy),
		BurstPoll:           cfg.Tunnel.BurstPoll,
		BurstPollInterval:   time.Duration(cfg.Tunnel.BurstPollMS) * time.Millisecond,
		BurstPollWindow:     time.Duration(cfg.Tunnel.BurstPollWindowMS) * time.Millisecond,
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

func (t *Tunnel) getData(ctx context.Context, name, fileID string) ([]byte, error) {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.GetByID(ctx, fileID)
		}
	}
	return t.Data.Get(ctx, name)
}

func (t *Tunnel) deleteData(ctx context.Context, name, fileID string) error {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.DeleteID(ctx, fileID)
		}
	}
	return t.Data.Delete(ctx, name)
}

func (t *Tunnel) dialExitTarget(ctx context.Context, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, exitDialTimeout)
	defer cancel()
	if proxy := strings.TrimSpace(t.ExitProxy); proxy != "" {
		return DialViaProxy(ctx, proxy, target)
	}
	dialer := &net.Dialer{Timeout: exitDialTimeout, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, "tcp", target)
}

func (t *Tunnel) uploadWorkerCount() int {
	if t.UploadConcurrency > 0 {
		return clampWorkers(t.UploadConcurrency)
	}
	if t.autoProfile() {
		switch t.role {
		case "client":
			if t.RouteProxy != "" {
				return 8
			}
			return 16
		case "exit":
			return 32
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

func (t *Tunnel) streamDownloadWindow() int {
	workers := t.downloadWorkerCount()
	if t.RouteProxy != "" {
		return minInt(workers, 8)
	}
	return minInt(workers, 16)
}

func (t *Tunnel) autoProfile() bool {
	return strings.TrimSpace(t.Profile) == "" || strings.TrimSpace(t.Profile) == "auto"
}

func (t *Tunnel) acquireUploadSlot(ctx context.Context) (func(error), error) {
	return t.limiter(true).Acquire(ctx)
}

func (t *Tunnel) acquireDownloadSlot(ctx context.Context) (func(error), error) {
	return t.limiter(false).Acquire(ctx)
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
	if t.UploadConcurrency > 0 || !t.autoProfile() {
		return max
	}
	switch t.role {
	case "client":
		if t.RouteProxy != "" {
			return minInt(4, max)
		}
		return minInt(8, max)
	case "exit":
		return minInt(16, max)
	default:
		return minInt(8, max)
	}
}

func (t *Tunnel) initialDownloadWindow(max int) int {
	if t.DownloadConcurrency > 0 || !t.autoProfile() {
		return max
	}
	switch t.role {
	case "client":
		if t.RouteProxy != "" {
			return minInt(8, max)
		}
		return minInt(8, max)
	case "exit":
		return minInt(8, max)
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
	inFlight      int
	successes     int
	slowThreshold time.Duration
	name          string
	logger        *log.Logger
	lastLog       time.Time
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
	return &adaptiveLimiter{limit: initial, max: max, slowThreshold: slowThreshold, name: name, logger: logger}
}

func (l *adaptiveLimiter) Acquire(ctx context.Context) (func(error), error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		l.mu.Lock()
		if l.inFlight < l.limit {
			l.inFlight++
			l.mu.Unlock()
			started := time.Now()
			var once sync.Once
			return func(err error) {
				once.Do(func() {
					l.release(err, time.Since(started))
				})
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *adaptiveLimiter) release(err error, duration time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	oldLimit := l.limit
	reason := ""
	if err != nil {
		if l.limit > 1 {
			l.limit = maxInt(1, l.limit/2)
		}
		l.successes = 0
		reason = "error"
		l.logChangeLocked(oldLimit, reason, duration)
		return
	}
	if duration >= l.slowThreshold {
		if l.limit > 1 {
			l.limit--
		}
		l.successes = 0
		reason = "slow"
		l.logChangeLocked(oldLimit, reason, duration)
		return
	}
	l.successes++
	threshold := maxInt(2, l.limit*2)
	if l.successes >= threshold && l.limit < l.max {
		l.limit++
		l.successes = 0
		reason = "healthy"
	}
	l.logChangeLocked(oldLimit, reason, duration)
}

func (l *adaptiveLimiter) logChangeLocked(oldLimit int, reason string, duration time.Duration) {
	if l.logger == nil || reason == "" || oldLimit == l.limit {
		return
	}
	now := time.Now()
	if now.Sub(l.lastLog) < 2*time.Second {
		return
	}
	l.lastLog = now
	l.logger.Printf("drive limiter %s window=%d->%d max=%d reason=%s duration=%s", l.name, oldLimit, l.limit, l.max, reason, duration.Round(time.Millisecond))
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

func (c *deferredCleanup) FlushAsync() {
	c.flushAsyncAfter(deferredCleanupDelay)
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
		tunnel.waitForCleanupQuiet(context.Background(), cleanupMaxForegroundDelay)
		workers := clampWorkers(tunnel.Concurrency)
		if workers > 4 {
			workers = 4
		}
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

func (t *Tunnel) waitForCleanupQuiet(ctx context.Context, maxWait time.Duration) {
	if t == nil || maxWait <= 0 {
		return
	}
	deadline := time.NewTimer(maxWait)
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
	for n < len(buffer) {
		delay := interactiveCoalesceDelay
		if n >= inlineDataThreshold {
			delay = bulkCoalesceDelay
		} else if n >= mediumDataThreshold {
			delay = mediumCoalesceDelay
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
