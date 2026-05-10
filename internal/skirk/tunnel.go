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
	"time"
)

const (
	inlineDataThreshold           = 64 * 1024
	readCoalesceDelay             = 25 * time.Millisecond
	controlBatchSize              = 8
	controlBatchDelay             = 25 * time.Millisecond
	deferredCleanupDelay          = 2 * time.Minute
	deferredCleanupFlushThreshold = 2048
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
	RouteMode           string
	RouteProxy          string
	role                string
	limiterMu           sync.Mutex
	uploadLimiter       chan struct{}
	downloadLimiter     chan struct{}
	PollInterval        time.Duration
	CleanupProcessed    bool
	Logger              *log.Logger
}

func NewTunnel(data BlobStore, control BlobStore, cfg *Config) (*Tunnel, error) {
	sid, err := ParseSessionID(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	return &Tunnel{
		Data:                data,
		Control:             control,
		Secret:              cfg.Secret,
		SessionID:           sid,
		ChunkSize:           cfg.Tunnel.ChunkSize,
		Concurrency:         cfg.Tunnel.Concurrency,
		UploadConcurrency:   cfg.Tunnel.UploadConcurrency,
		DownloadConcurrency: cfg.Tunnel.DownloadConcurrency,
		Profile:             cfg.Tunnel.Profile,
		RouteMode:           cfg.Route.Mode,
		RouteProxy:          cfg.Route.Proxy,
		PollInterval:        cfg.PollInterval(),
		CleanupProcessed:    cfg.Tunnel.CleanupProcessed,
		Logger:              log.Default(),
	}, nil
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
	type state struct {
		conn net.Conn
	}
	conns := map[string]*state{}
	seen := map[string]bool{}
	prefix := streamControlDirPrefix(t.SessionID, DirectionUp)
	listOpenControls := func(ctx context.Context) ([]ObjectInfo, error) {
		if store, ok := t.Control.(ContainsListStore); ok {
			return store.ListContains(ctx, []string{prefix, ".OPEN"})
		}
		return t.Control.List(ctx, prefix)
	}
	changeStore, useChanges := t.changeStore()
	changeToken := ""
	if useChanges {
		var err error
		changeToken, err = changeStore.StartChangeToken(ctx)
		if err != nil {
			useChanges = false
			if t.Logger != nil {
				t.Logger.Printf("exit change feed unavailable, using list polling: %v", err)
			}
		}
	}
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	seedInfos, err := listOpenControls(ctx)
	if err == nil {
		sort.Slice(seedInfos, func(i, j int) bool { return seedInfos[i].Name < seedInfos[j].Name })
		for _, info := range seedInfos {
			if seen[info.Name] {
				continue
			}
			raw, err := t.Control.Get(ctx, info.Name)
			if err != nil {
				continue
			}
			var event ControlPayload
			if err := json.Unmarshal(raw, &event); err != nil {
				seen[info.Name] = true
				continue
			}
			seen[info.Name] = true
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
				t.serveExitConn(ctx, event.ConnID, remote)
			}
		}
	}
	poll := func() []ObjectInfo {
		if useChanges {
			infos, next, err := changeStore.ListChanges(ctx, changeToken)
			if err == nil {
				changeToken = next
				filtered := infos[:0]
				for _, info := range infos {
					if strings.HasPrefix(info.Name, prefix) {
						filtered = append(filtered, info)
					}
				}
				return filtered
			}
			if t.Logger != nil {
				t.Logger.Printf("exit changes failed, falling back to list once: %v", err)
			}
		}
		infos, err := listOpenControls(ctx)
		if err != nil {
			if t.Logger != nil {
				t.Logger.Printf("exit control list failed: %v", err)
			}
			return nil
		}
		return infos
	}
	for {
		select {
		case <-ctx.Done():
			for _, s := range conns {
				_ = s.conn.Close()
			}
			return nil
		case <-ticker.C:
			infos := poll()
			sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
			for _, info := range infos {
				if !strings.HasSuffix(info.Name, ".OPEN") {
					continue
				}
				if seen[info.Name] {
					continue
				}
				raw, err := t.Control.Get(ctx, info.Name)
				if err != nil {
					continue
				}
				var event ControlPayload
				if err := json.Unmarshal(raw, &event); err != nil {
					seen[info.Name] = true
					continue
				}
				seen[info.Name] = true
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
					t.serveExitConn(ctx, event.ConnID, remote)
				}
			}
		}
	}
}

func (t *Tunnel) serveExitConn(ctx context.Context, connID string, conn net.Conn) {
	go func() {
		if err := t.pumpReaderToMailbox(ctx, conn, DirectionDown, connID, 1); err != nil && t.Logger != nil {
			t.Logger.Printf("exit downstream pump %s: %v", connID, err)
		}
		_ = conn.Close()
	}()
	go func() {
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

func (t *Tunnel) pumpReaderToMailbox(ctx context.Context, reader io.Reader, direction byte, connID string, firstSeq uint64) error {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type uploadJob struct {
		seq  uint64
		data []byte
	}
	type uploadResult struct {
		event ControlPayload
		err   error
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
				dataName := streamDataName(t.SessionID, direction, connID, job.seq)
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
				event, err := t.prepareDataEvent(workerCtx, direction, connID, job.seq, dataName, sealed, len(job.data))
				release()
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				results <- uploadResult{event: event}
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
			if t.Logger != nil && bytesSent > 0 {
				t.Logger.Printf("mailbox pump direction=%s conn=%s chunks=%d bytes=%d duration=%s", directionName(direction), connID, chunks, bytesSent, time.Since(started).Round(time.Millisecond))
			}
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

func (t *Tunnel) pumpMailboxToWriter(ctx context.Context, writer io.Writer, direction byte, connID string, firstSeq uint64) error {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type dataResult struct {
		seq       uint64
		object    string
		fileID    string
		plaintext []byte
		err       error
	}
	seen := map[string]bool{}
	cleanup := t.newDeferredCleanup()
	defer cleanup.FlushAsync()
	pending := map[uint64]ControlPayload{}
	inflight := map[uint64]bool{}
	ready := map[uint64]dataResult{}
	prefix := streamControlPrefix(t.SessionID, direction, connID)
	changeStore, useChanges := t.changeStore()
	changeToken := ""
	if useChanges {
		var err error
		changeToken, err = changeStore.StartChangeToken(ctx)
		if err != nil {
			useChanges = false
		}
	}
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
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, err: err}
						return
					}
					sealed, err := t.getEventData(ctx, event)
					release()
					if err != nil {
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, err: err}
						return
					}
					env, plaintext, err := OpenEnvelope(key, sealed)
					if err != nil || env.Direction != direction || env.Sequence != event.Sequence || SessionString(env.SessionID) != event.SessionID {
						if err == nil {
							err = fmt.Errorf("envelope metadata mismatch for %s", event.DriveObject)
						}
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, err: err}
						return
					}
					results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, plaintext: plaintext}
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
			if _, err := writer.Write(result.plaintext); err != nil {
				return false, err
			}
			if result.object != "" || result.fileID != "" {
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
			if seen[info.Name] {
				continue
			}
			if event, ok := t.parseDataControlInfo(info.Name, direction); ok {
				seen[info.Name] = true
				cleanup.Control(info.Name, info.ID)
				if event.Sequence >= expected {
					pending[event.Sequence] = event
				}
				continue
			}
			raw, err := t.Control.Get(ctx, info.Name)
			if err != nil {
				continue
			}
			var event ControlPayload
			if err := json.Unmarshal(raw, &event); err != nil {
				seen[info.Name] = true
				continue
			}
			seen[info.Name] = true
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
	poll := func() []ObjectInfo {
		if useChanges {
			infos, next, err := changeStore.ListChanges(ctx, changeToken)
			if err == nil {
				changeToken = next
				filtered := infos[:0]
				for _, info := range infos {
					if strings.HasPrefix(info.Name, prefix) {
						filtered = append(filtered, info)
					}
				}
				return filtered
			}
		}
		infos, err := t.Control.List(ctx, prefix)
		if err != nil {
			return nil
		}
		return infos
	}
	if seedInfos, err := t.Control.List(ctx, prefix); err == nil {
		processInfos(seedInfos)
	} else {
		processInfos(poll())
	}
	if remoteReset != nil {
		return remoteReset
	}
	startDownloads()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
			infos := poll()
			processInfos(infos)
			if remoteReset != nil {
				return remoteReset
			}
			startDownloads()
			done, err := writeReady()
			if done || err != nil {
				return err
			}
		}
	}
}

func (t *Tunnel) sendEvent(ctx context.Context, direction byte, connID string, seq uint64, eventType, driveObject, target string, bytes int, final bool, errorText string) error {
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

func (t *Tunnel) prepareDataEvent(ctx context.Context, direction byte, connID string, seq uint64, dataName string, sealed []byte, bytes int) (ControlPayload, error) {
	if len(sealed) <= inlineDataThreshold {
		return t.dataEvent(direction, connID, seq, "", "", base64.StdEncoding.EncodeToString(sealed), bytes), nil
	}
	info, err := t.putData(ctx, dataName, sealed)
	if err != nil {
		return ControlPayload{}, err
	}
	return t.dataEvent(direction, connID, seq, dataName, info.ID, "", bytes), nil
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
	return t.getData(ctx, event.DriveObject, event.DriveFileID)
}

func (t *Tunnel) putData(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if store, ok := t.Data.(ObjectPutStore); ok {
		return store.PutObject(ctx, name, data)
	}
	if err := t.Data.Put(ctx, name, data); err != nil {
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

func (t *Tunnel) changeStore() (ChangeStore, bool) {
	drive, ok := t.Control.(*DriveStore)
	if !ok || !drive.isAppData() {
		return nil, false
	}
	return drive, true
}

func (t *Tunnel) parseDataControlInfo(name string, direction byte) (ControlPayload, bool) {
	base := name
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}
	parts := strings.Split(base, ".")
	if len(parts) != 4 || parts[1] != "DATA" {
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
	idBytes, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return ControlPayload{}, false
	}
	return ControlPayload{
		Version:     1,
		Event:       "DATA",
		SessionID:   SessionString(t.SessionID),
		Direction:   directionName(direction),
		Sequence:    seq,
		DriveFileID: string(idBytes),
		Bytes:       byteCount,
	}, true
}

func (t *Tunnel) uploadWorkerCount() int {
	if t.UploadConcurrency > 0 {
		return clampWorkers(t.UploadConcurrency)
	}
	if t.autoProfile() {
		switch t.role {
		case "client":
			if t.RouteProxy != "" {
				return 1
			}
			return 8
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

func (t *Tunnel) acquireUploadSlot(ctx context.Context) (func(), error) {
	return t.acquireLimiterSlot(ctx, true)
}

func (t *Tunnel) acquireDownloadSlot(ctx context.Context) (func(), error) {
	return t.acquireLimiterSlot(ctx, false)
}

func (t *Tunnel) acquireLimiterSlot(ctx context.Context, upload bool) (func(), error) {
	limiter := t.limiter(upload)
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *Tunnel) limiter(upload bool) chan struct{} {
	t.limiterMu.Lock()
	defer t.limiterMu.Unlock()
	if upload {
		if t.uploadLimiter == nil {
			t.uploadLimiter = make(chan struct{}, t.uploadWorkerCount())
		}
		return t.uploadLimiter
	}
	if t.downloadLimiter == nil {
		t.downloadLimiter = make(chan struct{}, t.downloadWorkerCount())
	}
	return t.downloadLimiter
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
	deadline := time.Now().Add(readCoalesceDelay)
	defer deadlineConn.SetReadDeadline(time.Time{})
	for n < len(buffer) {
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

func streamDataName(sid [16]byte, direction byte, connID string, sequence uint64) string {
	return fmt.Sprintf("%s/%s/%s/%s/%016x.skb", dataPrefix, SessionString(sid), directionName(direction), connID, sequence)
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
