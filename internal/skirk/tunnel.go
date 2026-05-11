package skirk

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	inlineDataThreshold           = 64 * 1024
	mediumDataThreshold           = 8 * 1024
	nameInlineDataLimit           = 1400
	interactiveCoalesceDelay      = 25 * time.Millisecond
	mediumCoalesceDelay           = 120 * time.Millisecond
	bulkCoalesceDelay             = 300 * time.Millisecond
	controlBatchSize              = 8
	controlBatchDelay             = 25 * time.Millisecond
	deferredCleanupDelay          = 5 * time.Second
	deferredCleanupFlushThreshold = 2048
	idleOpenPollInterval          = 1 * time.Second
	openPollWarmWindow            = 45 * time.Second
)

type Tunnel struct {
	Data                BlobStore
	Control             BlobStore
	Secret              string
	SessionID           [16]byte
	ChunkSize           int
	Concurrency         int
	UploadConcurrency   int
	DownloadConcurrency int
	Profile             string
	RouteProxy          string
	role                string
	limiterMu           sync.Mutex
	uploadLimiter       *adaptiveLimiter
	downloadLimiter     *adaptiveLimiter
	watcherMu           sync.Mutex
	watchers            map[byte]*controlWatcher
	lastActivityNS      int64
	PollInterval        time.Duration
	CleanupProcessed    bool
	Logger              *log.Logger
}

func NewTunnel(data BlobStore, control BlobStore, cfg *Config) (*Tunnel, error) {
	sid, err := ParseSessionID(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{
		Data:                data,
		Control:             control,
		Secret:              cfg.Secret,
		SessionID:           sid,
		ChunkSize:           cfg.Tunnel.ChunkSize,
		Concurrency:         cfg.Tunnel.Concurrency,
		UploadConcurrency:   cfg.Tunnel.UploadConcurrency,
		DownloadConcurrency: cfg.Tunnel.DownloadConcurrency,
		Profile:             cfg.Tunnel.Profile,
		RouteProxy:          cfg.Route.Proxy,
		PollInterval:        cfg.PollInterval(),
		CleanupProcessed:    cfg.Tunnel.CleanupProcessed,
		Logger:              log.Default(),
	}
	t.markActivity()
	return t, nil
}

func (t *Tunnel) ServeClient(ctx context.Context, listen string) error {
	t.role = "client"
	server := SOCKSServer{
		Listen: listen,
		Logger: t.Logger,
		Handler: func(connCtx context.Context, target string, conn net.Conn) {
			if err := t.handleClientConn(connCtx, target, conn); err != nil && t.Logger != nil {
				t.Logger.Printf("client connection %s failed: %v", target, err)
			}
		},
	}
	return server.Serve(ctx)
}

func (t *Tunnel) handleClientConn(ctx context.Context, target string, local net.Conn) error {
	connID, err := randomConnID()
	if err != nil {
		return err
	}
	if err := t.sendEvent(ctx, DirectionUp, connID, 0, "OPEN", "", target, 0, false, ""); err != nil {
		return err
	}
	type pumpResult struct {
		downstream bool
		err        error
	}
	errCh := make(chan pumpResult, 2)
	go func() { errCh <- pumpResult{err: t.pumpReaderToMailbox(ctx, local, DirectionUp, connID, 1)} }()
	go func() {
		errCh <- pumpResult{downstream: true, err: t.pumpMailboxToWriter(ctx, local, DirectionDown, connID, 1)}
	}()
	for {
		result := <-errCh
		if result.downstream || result.err != nil {
			_ = local.Close()
			return result.err
		}
		// A clean upstream EOF means the client finished sending bytes. Keep the
		// local connection open so the downstream response can still arrive.
	}
}

func (t *Tunnel) ServeExit(ctx context.Context) error {
	t.role = "exit"
	startedAt := time.Now().UTC().Add(-5 * time.Second)
	type state struct {
		conn net.Conn
	}
	conns := map[string]*state{}
	seen := map[string]bool{}
	closedConns := make(chan string, 1024)
	prefix := streamControlDirPrefix(t.SessionID, DirectionUp)
	listOpenControls := func(ctx context.Context) ([]ObjectInfo, error) {
		if store, ok := t.Control.(ContainsListStore); ok {
			return store.ListContains(ctx, []string{prefix, ".OPEN"})
		}
		return t.Control.List(ctx, prefix)
	}
	seedInfos, err := listOpenControls(ctx)
	if err == nil {
		sort.Slice(seedInfos, func(i, j int) bool { return seedInfos[i].Name < seedInfos[j].Name })
		for _, info := range seedInfos {
			if seen[info.Name] {
				continue
			}
			if !controlIsFresh(info, startedAt) {
				seen[info.Name] = true
				continue
			}
			var event ControlPayload
			if parsed, ok := t.parseOpenControlInfo(info.Name); ok {
				event = parsed
			} else {
				raw, err := t.Control.Get(ctx, info.Name)
				if err != nil {
					continue
				}
				if err := json.Unmarshal(raw, &event); err != nil {
					seen[info.Name] = true
					continue
				}
			}
			seen[info.Name] = true
			t.markActivity()
			if t.CleanupProcessed {
				_ = t.deleteControl(ctx, info.Name, info.ID)
			}
			if event.Event == "OPEN" {
				remote, err := net.DialTimeout("tcp", event.Target, 30*time.Second)
				if err != nil {
					_ = t.sendEvent(ctx, DirectionDown, event.ConnID, 0, "RST", "", "", 0, true, err.Error())
					continue
				}
				conns[event.ConnID] = &state{conn: remote}
				t.serveExitConn(ctx, event.ConnID, remote, func() {
					select {
					case closedConns <- event.ConnID:
					default:
					}
				})
			}
		}
	}
	poll := func() []ObjectInfo {
		infos, err := listOpenControls(ctx)
		if err != nil {
			if t.Logger != nil {
				t.Logger.Printf("exit control list failed: %v", err)
			}
			return nil
		}
		return infos
	}
	timer := time.NewTimer(t.openPollInterval(len(conns)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, s := range conns {
				_ = s.conn.Close()
			}
			return nil
		case connID := <-closedConns:
			delete(conns, connID)
		case <-timer.C:
			infos := poll()
			sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
			for _, info := range infos {
				if !controlNameIsOpen(info.Name) {
					continue
				}
				if seen[info.Name] {
					continue
				}
				if !controlIsFresh(info, startedAt) {
					seen[info.Name] = true
					continue
				}
				var event ControlPayload
				if parsed, ok := t.parseOpenControlInfo(info.Name); ok {
					event = parsed
				} else {
					raw, err := t.Control.Get(ctx, info.Name)
					if err != nil {
						continue
					}
					if err := json.Unmarshal(raw, &event); err != nil {
						seen[info.Name] = true
						continue
					}
				}
				seen[info.Name] = true
				t.markActivity()
				if t.CleanupProcessed {
					_ = t.deleteControl(ctx, info.Name, info.ID)
				}
				switch event.Event {
				case "OPEN":
					remote, err := net.DialTimeout("tcp", event.Target, 30*time.Second)
					if err != nil {
						_ = t.sendEvent(ctx, DirectionDown, event.ConnID, 0, "RST", "", "", 0, true, err.Error())
						continue
					}
					conns[event.ConnID] = &state{conn: remote}
					t.serveExitConn(ctx, event.ConnID, remote, func() {
						select {
						case closedConns <- event.ConnID:
						default:
						}
					})
				}
			}
			timer.Reset(t.openPollInterval(len(conns)))
		}
	}
}

func (t *Tunnel) serveExitConn(ctx context.Context, connID string, conn net.Conn, done func()) {
	var doneOnce sync.Once
	markDone := func() {
		if done != nil {
			doneOnce.Do(done)
		}
	}
	go func() {
		defer markDone()
		if err := t.pumpReaderToMailbox(ctx, conn, DirectionDown, connID, 1); err != nil && t.Logger != nil {
			t.Logger.Printf("exit downstream pump %s: %v", connID, err)
		}
		_ = conn.Close()
	}()
	go func() {
		defer markDone()
		err := t.pumpMailboxToWriter(ctx, conn, DirectionUp, connID, 1)
		if err != nil {
			if t.Logger != nil {
				t.Logger.Printf("exit upstream pump %s: %v", connID, err)
			}
			_ = conn.Close()
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
}

func (t *Tunnel) openPollInterval(activeConns int) time.Duration {
	if activeConns > 0 || t.recentActivity() || t.PollInterval >= idleOpenPollInterval {
		return t.PollInterval
	}
	return idleOpenPollInterval
}

func (t *Tunnel) logPumpSummary(label string, direction byte, connID string, chunks int, bytes int64, duration time.Duration, err error) {
	if t.Logger == nil || (chunks == 0 && bytes == 0 && err == nil) {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	t.Logger.Printf("%s direction=%s conn=%s status=%s chunks=%d bytes=%d duration=%s error=%s", label, directionName(direction), connID, status, chunks, bytes, duration.Round(time.Millisecond), errorSummary(err))
}

func errorSummary(err error) string {
	if err == nil {
		return "none"
	}
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	return text
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

func controlNameIsOpen(name string) bool {
	base := controlBaseName(name)
	return strings.Contains(base, ".OPEN")
}

func (t *Tunnel) markActivity() {
	atomic.StoreInt64(&t.lastActivityNS, time.Now().UnixNano())
}

func (t *Tunnel) recentActivity() bool {
	last := atomic.LoadInt64(&t.lastActivityNS)
	return last > 0 && time.Since(time.Unix(0, last)) <= openPollWarmWindow
}

func (t *Tunnel) pumpReaderToMailbox(ctx context.Context, reader io.Reader, direction byte, connID string, firstSeq uint64) (err error) {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type uploadJob struct {
		seq  uint64
		data []byte
	}
	type uploadResult struct {
		event  ControlPayload
		direct bool
		err    error
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	uploadWorkers := t.uploadWorkerCount()
	jobs := make(chan uploadJob, uploadWorkers)
	results := make(chan uploadResult, uploadWorkers*2)
	errCh := make(chan error, uploadWorkers+2)
	var wg sync.WaitGroup
	for i := 0; i < uploadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				sealed, err := Seal(key, t.SessionID, direction, job.seq, job.data, false)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				release, err := t.acquireUploadSlot(workerCtx)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				event, direct, err := t.prepareDataEvent(workerCtx, direction, connID, job.seq, sealed, len(job.data))
				release(err)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				results <- uploadResult{event: event, direct: direct}
			}
		}()
	}
	aggDone := make(chan struct{})
	go func() {
		defer close(aggDone)
		batch := make([]ControlPayload, 0, controlBatchSize)
		timer := time.NewTimer(controlBatchDelay)
		if !timer.Stop() {
			<-timer.C
		}
		timerActive := false
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			if err := t.sendDataBatchEvent(workerCtx, direction, connID, batch); err != nil {
				errCh <- err
				cancel()
				return false
			}
			batch = batch[:0]
			return true
		}
		for {
			select {
			case result, ok := <-results:
				if !ok {
					if timerActive && !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					flush()
					return
				}
				if result.err != nil {
					errCh <- result.err
					cancel()
					return
				}
				if result.direct {
					continue
				}
				batch = append(batch, result.event)
				if len(batch) >= controlBatchSize {
					if timerActive && !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timerActive = false
					if !flush() {
						return
					}
					continue
				}
				if !timerActive {
					timer.Reset(controlBatchDelay)
					timerActive = true
				}
			case <-timer.C:
				timerActive = false
				if !flush() {
					return
				}
			case <-workerCtx.Done():
				return
			}
		}
	}()
	buffer := make([]byte, t.ChunkSize)
	seq := firstSeq
	chunks := 0
	var bytesSent int64
	started := time.Now()
	defer func() {
		t.logPumpSummary("mailbox upload", direction, connID, chunks, bytesSent, time.Since(started), err)
	}()
	for {
		n, readErr := readChunk(reader, buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			select {
			case jobs <- uploadJob{seq: seq, data: data}:
				seq++
				chunks++
				bytesSent += int64(n)
			case err := <-errCh:
				close(jobs)
				wg.Wait()
				return err
			case <-workerCtx.Done():
				close(jobs)
				wg.Wait()
				return workerCtx.Err()
			}
		}
		if readErr == io.EOF {
			close(jobs)
			wg.Wait()
			close(results)
			<-aggDone
			select {
			case err := <-errCh:
				return err
			default:
			}
			return t.sendEvent(ctx, direction, connID, seq, "FIN", "", "", 0, true, "")
		}
		if readErr != nil {
			cancel()
			close(jobs)
			wg.Wait()
			close(results)
			<-aggDone
			_ = t.sendEvent(ctx, direction, connID, seq, "RST", "", "", 0, true, readErr.Error())
			return readErr
		}
	}
}

func (t *Tunnel) pumpMailboxToWriter(ctx context.Context, writer io.Writer, direction byte, connID string, firstSeq uint64) (err error) {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	started := time.Now()
	chunksWritten := 0
	var bytesWritten int64
	defer func() {
		t.logPumpSummary("mailbox download", direction, connID, chunksWritten, bytesWritten, time.Since(started), err)
	}()
	type dataResult struct {
		seq         uint64
		object      string
		fileID      string
		controlFile bool
		plaintext   []byte
		err         error
	}
	cleanup := t.newDeferredCleanup()
	defer cleanup.FlushAsync()
	controlInfos, unsubscribe := t.subscribeControls(ctx, direction, connID)
	defer unsubscribe()
	pending := map[uint64]ControlPayload{}
	inflight := map[uint64]bool{}
	ready := map[uint64]dataResult{}
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	expected := firstSeq
	concurrency := t.downloadWorkerCount()
	results := make(chan dataResult, concurrency*2)
	hasFIN := false
	var finSeq uint64
	var remoteReset error
	startDownloads := func() {
		for len(inflight) < concurrency {
			started := false
			for seq := expected; seq < expected+uint64(concurrency*4); seq++ {
				event, ok := pending[seq]
				if !ok || inflight[seq] {
					continue
				}
				inflight[seq] = true
				started = true
				go func(event ControlPayload) {
					release, err := t.acquireDownloadSlot(ctx)
					if err != nil {
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, controlFile: event.ControlFile, err: err}
						return
					}
					sealed, err := t.getEventData(ctx, event)
					release(err)
					if err != nil {
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, controlFile: event.ControlFile, err: err}
						return
					}
					env, plaintext, err := OpenEnvelope(key, sealed)
					if err != nil || env.Direction != direction || env.Sequence != event.Sequence || SessionString(env.SessionID) != event.SessionID {
						if err == nil {
							err = fmt.Errorf("envelope metadata mismatch for %s", event.DriveObject)
						}
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, controlFile: event.ControlFile, err: err}
						return
					}
					results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, controlFile: event.ControlFile, plaintext: plaintext}
				}(event)
				break
			}
			if !started {
				break
			}
		}
	}
	writeReady := func() (bool, error) {
		for {
			result, ok := ready[expected]
			if !ok {
				break
			}
			if err := writeAll(writer, result.plaintext); err != nil {
				return false, err
			}
			chunksWritten++
			bytesWritten += int64(len(result.plaintext))
			if result.controlFile {
				cleanup.Control(result.object, result.fileID)
			} else if result.object != "" || result.fileID != "" {
				cleanup.Data(result.object, result.fileID)
			}
			delete(ready, expected)
			delete(pending, expected)
			expected++
		}
		if hasFIN && expected >= finSeq {
			return true, nil
		}
		return false, nil
	}
	processInfos := func(infos []ObjectInfo) {
		enqueue := func(event ControlPayload) {
			switch event.Event {
			case "DATA":
				if event.Sequence < expected {
					return
				}
				pending[event.Sequence] = event
			case "FIN":
				hasFIN = true
				finSeq = event.Sequence
			case "RST":
				if event.Error != "" {
					remoteReset = fmt.Errorf("remote reset: %s", event.Error)
				} else {
					remoteReset = fmt.Errorf("remote reset")
				}
			}
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		for _, info := range infos {
			if event, ok := t.parseListedControlInfo(info, direction); ok {
				if !event.ControlFile {
					cleanup.Control(info.Name, info.ID)
				}
				if event.Sequence >= expected {
					enqueue(event)
				}
				continue
			}
			raw, err := t.Control.Get(ctx, info.Name)
			if err != nil {
				continue
			}
			var event ControlPayload
			if err := json.Unmarshal(raw, &event); err != nil {
				continue
			}
			cleanup.Control(info.Name, info.ID)
			if event.Event == "BATCH" {
				for _, item := range event.Batch {
					enqueue(item)
				}
				continue
			}
			enqueue(event)
		}
	}
	if remoteReset != nil {
		return remoteReset
	}
	startDownloads()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case info, ok := <-controlInfos:
			if !ok {
				return ctx.Err()
			}
			processInfos([]ObjectInfo{info})
			if remoteReset != nil {
				return remoteReset
			}
			startDownloads()
			done, err := writeReady()
			if done || err != nil {
				return err
			}
		case result := <-results:
			delete(inflight, result.seq)
			if result.err != nil {
				return result.err
			}
			ready[result.seq] = result
			done, err := writeReady()
			if done || err != nil {
				return err
			}
			startDownloads()
		case <-ticker.C:
			startDownloads()
			done, err := writeReady()
			if done || err != nil {
				return err
			}
		}
	}
}

func (t *Tunnel) sendEvent(ctx context.Context, direction byte, connID string, seq uint64, eventType, driveObject, target string, bytes int, final bool, errorText string) error {
	t.markActivity()
	if errorText == "" {
		switch eventType {
		case "OPEN":
			if target != "" {
				return t.Control.Put(ctx, streamOpenControlName(t.SessionID, direction, connID, seq, target), []byte("{}"))
			}
		case "FIN":
			return t.Control.Put(ctx, streamControlName(t.SessionID, direction, connID, seq, eventType), []byte("{}"))
		case "RST":
			return t.Control.Put(ctx, streamControlName(t.SessionID, direction, connID, seq, eventType), []byte("{}"))
		}
	}
	event := ControlPayload{
		Version:     1,
		Event:       eventType,
		SessionID:   SessionString(t.SessionID),
		ConnID:      connID,
		Direction:   directionName(direction),
		Sequence:    seq,
		DriveObject: driveObject,
		Target:      target,
		Bytes:       bytes,
		Final:       final,
		Error:       errorText,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return t.Control.Put(ctx, streamControlName(t.SessionID, direction, connID, seq, eventType), raw)
}

func (t *Tunnel) prepareDataEvent(ctx context.Context, direction byte, connID string, seq uint64, sealed []byte, bytes int) (ControlPayload, bool, error) {
	if len(sealed) <= inlineDataThreshold {
		return t.dataEvent(direction, connID, seq, "", "", base64.StdEncoding.EncodeToString(sealed), bytes), false, nil
	}
	name := streamFileDataControlName(t.SessionID, direction, connID, seq, bytes)
	info, err := t.putControlData(ctx, name, sealed)
	if err != nil {
		return ControlPayload{}, false, err
	}
	return t.dataEvent(direction, connID, seq, info.Name, info.ID, "", bytes), true, nil
}

func (t *Tunnel) dataEvent(direction byte, connID string, seq uint64, driveObject, driveFileID, inlineData string, bytes int) ControlPayload {
	return ControlPayload{
		Version:     1,
		Event:       "DATA",
		SessionID:   SessionString(t.SessionID),
		ConnID:      connID,
		Direction:   directionName(direction),
		Sequence:    seq,
		DriveObject: driveObject,
		DriveFileID: driveFileID,
		InlineData:  inlineData,
		Bytes:       bytes,
	}
}

func (t *Tunnel) sendDataBatchEvent(ctx context.Context, direction byte, connID string, events []ControlPayload) error {
	if len(events) == 0 {
		return nil
	}
	t.markActivity()
	if len(events) == 1 {
		return t.sendDataEvent(ctx, direction, connID, events[0])
	}
	batch := append([]ControlPayload(nil), events...)
	event := ControlPayload{
		Version:   1,
		Event:     "BATCH",
		SessionID: SessionString(t.SessionID),
		ConnID:    connID,
		Direction: directionName(direction),
		Sequence:  batch[0].Sequence,
		Batch:     batch,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return t.Control.Put(ctx, streamBatchControlName(t.SessionID, direction, connID, batch[0].Sequence, batch[len(batch)-1].Sequence), raw)
}

func (t *Tunnel) sendDataEvent(ctx context.Context, direction byte, connID string, event ControlPayload) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	name := streamControlName(t.SessionID, direction, connID, event.Sequence, "DATA")
	if event.InlineData != "" && len(event.InlineData) <= nameInlineDataLimit {
		sealed, err := base64.StdEncoding.DecodeString(event.InlineData)
		if err == nil {
			name = streamInlineDataControlName(t.SessionID, direction, connID, event.Sequence, event.Bytes, sealed)
			return t.Control.Put(ctx, name, []byte("{}"))
		}
	}
	if event.DriveFileID != "" && event.InlineData == "" {
		name = streamDataControlName(t.SessionID, direction, connID, event.Sequence, event.Bytes, event.DriveFileID)
	}
	return t.Control.Put(ctx, name, raw)
}

func (t *Tunnel) getEventData(ctx context.Context, event ControlPayload) ([]byte, error) {
	if event.InlineData != "" {
		data, err := base64.StdEncoding.DecodeString(event.InlineData)
		if err != nil {
			return nil, fmt.Errorf("inline data decode failed: %w", err)
		}
		return data, nil
	}
	if event.ControlFile {
		return t.getControlData(ctx, event.DriveObject, event.DriveFileID)
	}
	return t.getData(ctx, event.DriveObject, event.DriveFileID)
}

func (t *Tunnel) putControlData(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if store, ok := t.Control.(ObjectPutStore); ok {
		return store.PutObject(ctx, name, data)
	}
	if err := t.Control.Put(ctx, name, data); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: name}, nil
}

func (t *Tunnel) getData(ctx context.Context, name, fileID string) ([]byte, error) {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.GetByID(ctx, fileID)
		}
	}
	return t.Data.Get(ctx, name)
}

func (t *Tunnel) getControlData(ctx context.Context, name, fileID string) ([]byte, error) {
	if fileID != "" {
		if store, ok := t.Control.(ObjectIDStore); ok {
			return store.GetByID(ctx, fileID)
		}
	}
	return t.Control.Get(ctx, name)
}

func (t *Tunnel) deleteData(ctx context.Context, name, fileID string) error {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.DeleteID(ctx, fileID)
		}
	}
	return t.Data.Delete(ctx, name)
}

func (t *Tunnel) deleteControl(ctx context.Context, name, fileID string) error {
	if fileID != "" {
		if store, ok := t.Control.(ObjectIDStore); ok {
			return store.DeleteID(ctx, fileID)
		}
	}
	return t.Control.Delete(ctx, name)
}

type controlSubscription struct {
	connID string
	ch     chan ObjectInfo
}

type controlWatcher struct {
	t           *Tunnel
	direction   byte
	register    chan controlSubscription
	unregister  chan string
	subscribers map[string]chan ObjectInfo
	pending     map[string][]ObjectInfo
	seen        map[string]bool
}

func (t *Tunnel) subscribeControls(ctx context.Context, direction byte, connID string) (<-chan ObjectInfo, func()) {
	watcher := t.getControlWatcher(direction)
	ch := make(chan ObjectInfo, 4096)
	select {
	case watcher.register <- controlSubscription{connID: connID, ch: ch}:
	case <-ctx.Done():
		close(ch)
		return ch, func() {}
	}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			select {
			case watcher.unregister <- connID:
			default:
				go func() { watcher.unregister <- connID }()
			}
		})
	}
}

func (t *Tunnel) getControlWatcher(direction byte) *controlWatcher {
	t.watcherMu.Lock()
	defer t.watcherMu.Unlock()
	if t.watchers == nil {
		t.watchers = map[byte]*controlWatcher{}
	}
	if watcher := t.watchers[direction]; watcher != nil {
		return watcher
	}
	watcher := &controlWatcher{
		t:           t,
		direction:   direction,
		register:    make(chan controlSubscription, 64),
		unregister:  make(chan string, 64),
		subscribers: map[string]chan ObjectInfo{},
		pending:     map[string][]ObjectInfo{},
		seen:        map[string]bool{},
	}
	t.watchers[direction] = watcher
	go watcher.run()
	return watcher
}

func (w *controlWatcher) run() {
	ticker := time.NewTicker(w.t.PollInterval)
	defer ticker.Stop()
	w.poll()
	for {
		select {
		case sub := <-w.register:
			if old := w.subscribers[sub.connID]; old != nil && old != sub.ch {
				close(old)
			}
			w.subscribers[sub.connID] = sub.ch
			w.poll()
			w.flushPending(sub.connID)
		case connID := <-w.unregister:
			if ch := w.subscribers[connID]; ch != nil {
				close(ch)
				delete(w.subscribers, connID)
			}
		case <-ticker.C:
			if len(w.subscribers) > 0 {
				w.poll()
			}
		}
	}
}

func (w *controlWatcher) poll() {
	prefix := streamControlDirPrefix(w.t.SessionID, w.direction)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	infos, err := w.t.Control.List(ctx, prefix)
	if err != nil {
		if w.t.Logger != nil {
			w.t.Logger.Printf("control list direction=%s failed: %v", directionName(w.direction), err)
		}
		return
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	for _, info := range infos {
		if w.seen[info.Name] {
			continue
		}
		connID := controlConnID(prefix, info.Name)
		if connID == "" {
			w.seen[info.Name] = true
			continue
		}
		w.seen[info.Name] = true
		w.deliver(connID, info)
	}
}

func (w *controlWatcher) deliver(connID string, info ObjectInfo) {
	w.t.markActivity()
	if ch := w.subscribers[connID]; ch != nil {
		ch <- info
		return
	}
	w.pending[connID] = append(w.pending[connID], info)
}

func (w *controlWatcher) flushPending(connID string) {
	ch := w.subscribers[connID]
	if ch == nil {
		return
	}
	pending := w.pending[connID]
	delete(w.pending, connID)
	for _, info := range pending {
		ch <- info
	}
}

func controlConnID(prefix, name string) string {
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return ""
	}
	return rest[:slash]
}

func (t *Tunnel) parseOpenControlInfo(name string) (ControlPayload, bool) {
	base := controlBaseName(name)
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return ControlPayload{}, false
	}
	if parts[1] != "OPEN" {
		return ControlPayload{}, false
	}
	seq, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return ControlPayload{}, false
	}
	targetBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(targetBytes) == 0 {
		return ControlPayload{}, false
	}
	connID := controlConnID(streamControlDirPrefix(t.SessionID, DirectionUp), name)
	if connID == "" {
		return ControlPayload{}, false
	}
	event := ControlPayload{
		Version:   1,
		Event:     "OPEN",
		SessionID: SessionString(t.SessionID),
		ConnID:    connID,
		Direction: directionName(DirectionUp),
		Sequence:  seq,
		Target:    string(targetBytes),
	}
	return event, true
}

func (t *Tunnel) parseListedControlInfo(info ObjectInfo, direction byte) (ControlPayload, bool) {
	if event, ok := t.parseDataControlInfo(info, direction); ok {
		return event, true
	}
	base := controlBaseName(info.Name)
	parts := strings.Split(base, ".")
	if len(parts) != 2 {
		return ControlPayload{}, false
	}
	seq, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return ControlPayload{}, false
	}
	switch parts[1] {
	case "FIN":
		return ControlPayload{
			Version:   1,
			Event:     "FIN",
			SessionID: SessionString(t.SessionID),
			Direction: directionName(direction),
			Sequence:  seq,
			Final:     true,
		}, true
	case "RST":
		return ControlPayload{
			Version:   1,
			Event:     "RST",
			SessionID: SessionString(t.SessionID),
			Direction: directionName(direction),
			Sequence:  seq,
			Final:     true,
		}, true
	default:
		return ControlPayload{}, false
	}
}

func (t *Tunnel) parseDataControlInfo(info ObjectInfo, direction byte) (ControlPayload, bool) {
	base := controlBaseName(info.Name)
	parts := strings.Split(base, ".")
	if len(parts) != 4 && len(parts) != 3 {
		return ControlPayload{}, false
	}
	seq, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return ControlPayload{}, false
	}
	byteCount, err := strconv.Atoi(parts[2])
	if err != nil {
		return ControlPayload{}, false
	}
	event := ControlPayload{
		Version:   1,
		Event:     "DATA",
		SessionID: SessionString(t.SessionID),
		Direction: directionName(direction),
		Sequence:  seq,
		Bytes:     byteCount,
	}
	switch parts[1] {
	case "DATA":
		if len(parts) != 4 {
			return ControlPayload{}, false
		}
		idBytes, err := base64.RawURLEncoding.DecodeString(parts[3])
		if err != nil {
			return ControlPayload{}, false
		}
		event.DriveFileID = string(idBytes)
		return event, true
	case "DATAI":
		if len(parts) != 4 {
			return ControlPayload{}, false
		}
		sealed, err := base64.RawURLEncoding.DecodeString(parts[3])
		if err != nil {
			return ControlPayload{}, false
		}
		event.InlineData = base64.StdEncoding.EncodeToString(sealed)
		return event, true
	case "DATAF":
		if len(parts) != 3 {
			return ControlPayload{}, false
		}
		event.DriveObject = info.Name
		event.DriveFileID = info.ID
		event.ControlFile = true
		return event, true
	default:
		return ControlPayload{}, false
	}
}

func controlBaseName(name string) string {
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		return name[slash+1:]
	}
	return name
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
			t.uploadLimiter = newAdaptiveLimiter(t.initialUploadWindow(max), max)
		}
		return t.uploadLimiter
	}
	if t.downloadLimiter == nil {
		max := t.downloadWorkerCount()
		t.downloadLimiter = newAdaptiveLimiter(t.initialDownloadWindow(max), max)
	}
	return t.downloadLimiter
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
		return max
	case "exit":
		return max
	default:
		return max
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
		return max
	case "exit":
		return max
	default:
		return max
	}
}

func clampWorkers(workers int) int {
	if workers < 1 {
		return 1
	}
	if workers > 32 {
		return 32
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
	mu        sync.Mutex
	limit     int
	max       int
	inFlight  int
	successes int
}

func newAdaptiveLimiter(initial, max int) *adaptiveLimiter {
	max = clampWorkers(max)
	if initial < 1 {
		initial = 1
	}
	if initial > max {
		initial = max
	}
	return &adaptiveLimiter{limit: initial, max: max}
}

func (l *adaptiveLimiter) Acquire(ctx context.Context) (func(error), error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		l.mu.Lock()
		if l.inFlight < l.limit {
			l.inFlight++
			l.mu.Unlock()
			var once sync.Once
			return func(err error) {
				once.Do(func() {
					l.release(err)
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

func (l *adaptiveLimiter) release(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	if err != nil {
		if l.limit > 1 {
			l.limit = maxInt(1, l.limit/2)
		}
		l.successes = 0
		return
	}
	l.successes++
	threshold := maxInt(2, l.limit*2)
	if l.successes >= threshold && l.limit < l.max {
		l.limit++
		l.successes = 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type cleanupTask struct {
	data bool
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
	c.add(cleanupTask{data: true, name: name, id: id})
}

func (c *deferredCleanup) Control(name, id string) {
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
					if task.data {
						_ = tunnel.deleteData(ctx, task.name, task.id)
					} else {
						_ = tunnel.deleteControl(ctx, task.name, task.id)
					}
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

func streamControlDirPrefix(sid [16]byte, direction byte) string {
	return fmt.Sprintf("%s/%s/%s/", controlPrefix, SessionString(sid), directionName(direction))
}

func streamControlPrefix(sid [16]byte, direction byte, connID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/", controlPrefix, SessionString(sid), directionName(direction), connID)
}

func streamControlName(sid [16]byte, direction byte, connID string, sequence uint64, eventType string) string {
	return fmt.Sprintf("%s%016x.%s", streamControlPrefix(sid, direction, connID), sequence, eventType)
}

func streamOpenControlName(sid [16]byte, direction byte, connID string, sequence uint64, target string) string {
	encodedTarget := base64.RawURLEncoding.EncodeToString([]byte(target))
	return fmt.Sprintf("%s%016x.OPEN.%s", streamControlPrefix(sid, direction, connID), sequence, encodedTarget)
}

func streamInlineDataControlName(sid [16]byte, direction byte, connID string, sequence uint64, bytes int, sealed []byte) string {
	encodedData := base64.RawURLEncoding.EncodeToString(sealed)
	return fmt.Sprintf("%s%016x.DATAI.%d.%s", streamControlPrefix(sid, direction, connID), sequence, bytes, encodedData)
}

func streamFileDataControlName(sid [16]byte, direction byte, connID string, sequence uint64, bytes int) string {
	return fmt.Sprintf("%s%016x.DATAF.%d", streamControlPrefix(sid, direction, connID), sequence, bytes)
}

func streamDataControlName(sid [16]byte, direction byte, connID string, sequence uint64, bytes int, fileID string) string {
	encodedID := base64.RawURLEncoding.EncodeToString([]byte(fileID))
	return fmt.Sprintf("%s%016x.DATA.%d.%s", streamControlPrefix(sid, direction, connID), sequence, bytes, encodedID)
}

func streamBatchControlName(sid [16]byte, direction byte, connID string, first, last uint64) string {
	return fmt.Sprintf("%s%016x.BATCH.%016x", streamControlPrefix(sid, direction, connID), first, last)
}

func randomConnID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
