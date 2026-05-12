package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	muxMagic             = "SKM3"
	muxVersion           = byte(3)
	muxFrameHeaderSize   = 21
	muxFrameOpen         = byte(1)
	muxFrameData         = byte(2)
	muxFrameFIN          = byte(3)
	muxFrameRST          = byte(4)
	muxFramePing         = byte(5)
	muxLaneCount         = 4
	muxMaxFrames         = 512
	muxMinBatch          = 64 * 1024
	muxMaxBatch          = 8 * 1024 * 1024
	muxInlineFirst       = 16 * 1024
	muxPendingFrameLimit = 4096
)

type muxFrame struct {
	Kind     byte
	StreamID uint64
	Seq      uint64
	Payload  []byte
}

type muxObjectMeta struct {
	Name string
	ID   string
	Lane int
	Seq  uint64
}

type driveMux struct {
	t       *Tunnel
	role    string
	sendDir byte
	recvDir byte
	epoch   string

	lanes []*muxLane

	streamsMu sync.Mutex
	streams   map[uint64]*muxStream
	pendingMu sync.Mutex
	pending   map[uint64][]muxFrame
	active    atomic.Int64

	seenMu sync.Mutex
	seen   map[string]struct{}

	recvWake  chan struct{}
	startedAt time.Time
}

type muxLane struct {
	mux    *driveMux
	idx    int
	key    []byte
	send   chan muxFrame
	upload chan []muxFrame
	seq    uint64
}

type muxStream struct {
	id   uint64
	mux  *driveMux
	conn net.Conn

	inbound chan []byte
	done    chan struct{}
	once    sync.Once

	mu             sync.Mutex
	localReadDone  bool
	remoteReadDone bool
	sendSeq        atomic.Uint64
	recvExpected   uint64
	recvPending    map[uint64]muxFrame
}

func newDriveMux(t *Tunnel, role string, sendDir, recvDir byte) (*driveMux, error) {
	epoch, err := randomMuxEpoch()
	if err != nil {
		return nil, err
	}
	m := &driveMux{
		t:         t,
		role:      role,
		sendDir:   sendDir,
		recvDir:   recvDir,
		epoch:     epoch,
		streams:   map[uint64]*muxStream{},
		pending:   map[uint64][]muxFrame{},
		seen:      map[string]struct{}{},
		recvWake:  make(chan struct{}, 1),
		startedAt: time.Now().UTC().Add(-5 * time.Second),
	}
	for i := 0; i < muxLaneCount; i++ {
		key, err := DeriveMuxLaneKey(t.Secret, t.SessionID, sendDir, i)
		if err != nil {
			return nil, err
		}
		m.lanes = append(m.lanes, &muxLane{
			mux:    m,
			idx:    i,
			key:    key,
			send:   make(chan muxFrame, 4096),
			upload: make(chan []muxFrame, 16),
		})
	}
	return m, nil
}

func (t *Tunnel) serveMuxClient(ctx context.Context, listen string) error {
	t.role = "client"
	mux, err := t.getClientMux(ctx)
	if err != nil {
		return err
	}
	server := SOCKSServer{
		Listen: listen,
		Logger: t.Logger,
		Handler: func(connCtx context.Context, target string, conn net.Conn) {
			if err := mux.openClientStream(connCtx, target, conn); err != nil && t.Logger != nil {
				t.Logger.Printf("client stream target=%s failed: %s", targetFingerprint(target), errorSummary(err))
			}
		},
	}
	return server.Serve(ctx)
}

func (t *Tunnel) getClientMux(ctx context.Context) (*driveMux, error) {
	t.muxMu.Lock()
	defer t.muxMu.Unlock()
	if t.clientMux != nil {
		return t.clientMux, nil
	}
	mux, err := newDriveMux(t, "client", DirectionUp, DirectionDown)
	if err != nil {
		return nil, err
	}
	mux.start(ctx)
	t.clientMux = mux
	return mux, nil
}

func (t *Tunnel) serveMuxExit(ctx context.Context) error {
	t.role = "exit"
	mux, err := newDriveMux(t, "exit", DirectionDown, DirectionUp)
	if err != nil {
		return err
	}
	mux.start(ctx)
	<-ctx.Done()
	mux.closeAll()
	return nil
}

func (m *driveMux) start(ctx context.Context) {
	for _, lane := range m.lanes {
		go lane.runBatchLoop(ctx)
		for i := 0; i < m.uploadWorkersPerLane(); i++ {
			go lane.runUploadLoop(ctx)
		}
	}
	go m.runReceiveLoop(ctx)
}

func (m *driveMux) openClientStream(ctx context.Context, target string, local net.Conn) error {
	streamID, err := randomStreamID()
	if err != nil {
		return err
	}
	initial, err := readInitialClientData(local, muxInlineFirst, initialOpenDataWait)
	if err != nil {
		return err
	}
	stream := m.registerStream(streamID, local)
	m.startWriter(stream)
	if err := m.sendFrame(ctx, muxFrame{Kind: muxFrameOpen, StreamID: streamID, Payload: encodeMuxOpenPayload(target, initial)}); err != nil {
		stream.close()
		return err
	}
	go m.readLoop(ctx, stream)
	select {
	case <-ctx.Done():
		stream.close()
		return ctx.Err()
	case <-stream.done:
		return nil
	}
}

func (m *driveMux) openExitStream(ctx context.Context, streamID uint64, payload []byte) {
	target, initial, err := decodeMuxOpenPayload(payload)
	if err != nil {
		_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameRST, StreamID: streamID, Payload: []byte("bad_open")})
		return
	}
	started := time.Now()
	remote, err := m.t.dialExitTarget(ctx, target)
	dialDuration := time.Since(started)
	if m.t.Logger != nil {
		if err != nil || dialDuration >= time.Second {
			m.t.Logger.Printf("exit dial target=%s proxy=%s duration=%s error=%s", targetFingerprint(target), firstNonEmptyString(m.t.ExitProxy, "none"), dialDuration.Round(time.Millisecond), errorSummary(err))
		}
	}
	if err != nil {
		_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameRST, StreamID: streamID, Payload: []byte(sanitizeTransportErrorText(err.Error()))})
		return
	}
	stream := m.registerStream(streamID, remote)
	m.startWriter(stream)
	if len(initial) > 0 {
		if err := writeAll(remote, initial); err != nil {
			_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameRST, StreamID: streamID, Payload: []byte(sanitizeTransportErrorText(err.Error()))})
			stream.close()
			return
		}
	}
	m.flushPendingFrames(ctx, stream)
	go m.readLoop(ctx, stream)
}

func (m *driveMux) registerStream(id uint64, conn net.Conn) *muxStream {
	stream := &muxStream{
		id:           id,
		mux:          m,
		conn:         conn,
		inbound:      make(chan []byte, 256),
		done:         make(chan struct{}),
		recvExpected: 1,
		recvPending:  map[uint64]muxFrame{},
	}
	m.streamsMu.Lock()
	m.streams[id] = stream
	m.streamsMu.Unlock()
	m.active.Add(1)
	m.t.activeStreams.Add(1)
	return stream
}

func (m *driveMux) unregisterStream(id uint64) {
	m.streamsMu.Lock()
	delete(m.streams, id)
	m.streamsMu.Unlock()
	m.active.Add(-1)
	m.t.activeStreams.Add(-1)
}

func (m *driveMux) uploadWorkersPerLane() int {
	if len(m.lanes) == 0 {
		return 1
	}
	workers := m.t.uploadWorkerCount()
	perLane := (workers + len(m.lanes) - 1) / len(m.lanes)
	if perLane < 1 {
		return 1
	}
	if perLane > 16 {
		return 16
	}
	return perLane
}

func (m *driveMux) stream(id uint64) *muxStream {
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	return m.streams[id]
}

func (m *driveMux) closeAll() {
	m.streamsMu.Lock()
	streams := make([]*muxStream, 0, len(m.streams))
	for _, stream := range m.streams {
		streams = append(streams, stream)
	}
	m.streamsMu.Unlock()
	for _, stream := range streams {
		stream.close()
	}
}

func (m *driveMux) readLoop(ctx context.Context, stream *muxStream) {
	buffer := make([]byte, m.readBufferSize())
	for {
		n, err := readChunk(stream.conn, buffer)
		if n > 0 {
			payload := append([]byte(nil), buffer[:n]...)
			if sendErr := m.sendFrame(ctx, muxFrame{Kind: muxFrameData, StreamID: stream.id, Seq: stream.nextSendSeq(), Payload: payload}); sendErr != nil {
				stream.close()
				return
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF || strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
			_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameFIN, StreamID: stream.id, Seq: stream.nextSendSeq()})
			stream.markLocalReadDone()
			return
		}
		_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameRST, StreamID: stream.id, Payload: []byte(sanitizeTransportErrorText(err.Error()))})
		stream.close()
		return
	}
}

func (s *muxStream) nextSendSeq() uint64 {
	return s.sendSeq.Add(1)
}

func (m *driveMux) startWriter(stream *muxStream) {
	go func() {
		for {
			select {
			case data := <-stream.inbound:
				if data == nil {
					stream.markRemoteReadDone()
					return
				}
				if err := writeAll(stream.conn, data); err != nil {
					_ = m.sendFrame(context.Background(), muxFrame{Kind: muxFrameRST, StreamID: stream.id, Payload: []byte(sanitizeTransportErrorText(err.Error()))})
					stream.close()
					return
				}
			case <-stream.done:
				return
			}
		}
	}()
}

func (s *muxStream) markLocalReadDone() {
	s.mu.Lock()
	s.localReadDone = true
	shouldClose := s.remoteReadDone
	s.mu.Unlock()
	if shouldClose {
		s.close()
	}
}

func (s *muxStream) markRemoteReadDone() {
	if tcp, ok := s.conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	s.mu.Lock()
	s.remoteReadDone = true
	shouldClose := s.localReadDone
	s.mu.Unlock()
	if shouldClose {
		s.close()
	}
}

func (s *muxStream) close() {
	s.once.Do(func() {
		_ = s.conn.Close()
		close(s.done)
		s.mux.unregisterStream(s.id)
	})
}

func (m *driveMux) sendFrame(ctx context.Context, frame muxFrame) error {
	if len(m.lanes) == 0 {
		return errors.New("mux has no lanes")
	}
	lane := m.lanes[m.frameLane(frame)]
	select {
	case lane.send <- frame:
		m.t.markActivity()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *driveMux) frameLane(frame muxFrame) int {
	laneCount := uint64(len(m.lanes))
	if laneCount == 0 {
		return 0
	}
	if frame.Kind == muxFrameData || frame.Kind == muxFrameFIN {
		return int((frame.StreamID + frame.Seq) % laneCount)
	}
	return int(frame.StreamID % laneCount)
}

func (l *muxLane) runBatchLoop(ctx context.Context) {
	for {
		var first muxFrame
		select {
		case <-ctx.Done():
			return
		case first = <-l.send:
		}
		frames := []muxFrame{first}
		bytes := encodedMuxFrameSize(first)
		timer := time.NewTimer(muxFlushDelay(first))
		flush := false
		for !flush && len(frames) < muxMaxFrames && bytes < l.mux.maxBatchBytes() {
			select {
			case frame := <-l.send:
				frameSize := encodedMuxFrameSize(frame)
				frames = append(frames, frame)
				bytes += frameSize
			case <-timer.C:
				flush = true
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		select {
		case l.upload <- frames:
		case <-ctx.Done():
			return
		}
	}
}

func (l *muxLane) runUploadLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frames := <-l.upload:
			if err := l.uploadBatch(ctx, frames); err != nil && l.mux.t.Logger != nil && ctx.Err() == nil {
				bytes := 0
				for _, frame := range frames {
					bytes += encodedMuxFrameSize(frame)
				}
				l.mux.t.Logger.Printf("mux upload lane=%d frames=%d bytes=%d error=%s", l.idx, len(frames), bytes, errorSummary(err))
			}
		}
	}
}

func muxFlushDelay(frame muxFrame) time.Duration {
	if frame.Kind == muxFrameOpen || frame.Kind == muxFrameFIN || frame.Kind == muxFrameRST || len(frame.Payload) <= 4096 {
		return 5 * time.Millisecond
	}
	if frame.Kind == muxFrameData && frame.Seq == 1 {
		return 5 * time.Millisecond
	}
	return 25 * time.Millisecond
}

func (l *muxLane) uploadBatch(ctx context.Context, frames []muxFrame) error {
	if len(frames) == 0 {
		return nil
	}
	raw, err := encodeMuxBatch(frames)
	if err != nil {
		return err
	}
	seq := atomic.AddUint64(&l.seq, 1)
	sealed, err := Seal(l.key, l.mux.t.SessionID, l.mux.sendDir, seq, raw, false)
	if err != nil {
		return err
	}
	name := muxObjectName(l.mux.t.SessionID, l.mux.sendDir, l.mux.epoch, l.idx, seq, len(frames), len(raw))
	release, err := l.mux.t.acquireUploadSlot(ctx)
	if err != nil {
		return err
	}
	started := time.Now()
	if store, ok := l.mux.t.Data.(ObjectPutStore); ok {
		_, err = store.PutObject(ctx, name, sealed)
	} else {
		err = l.mux.t.Data.Put(ctx, name, sealed)
	}
	release(err)
	if l.mux.t.Logger != nil && (err != nil || time.Since(started) >= l.mux.t.slowDriveThreshold()) {
		l.mux.t.Logger.Printf("mux upload lane=%d seq=%d frames=%d plain_bytes=%d sealed_bytes=%d duration=%s error=%s", l.idx, seq, len(frames), len(raw), len(sealed), time.Since(started).Round(time.Millisecond), errorSummary(err))
	}
	if err == nil {
		l.mux.t.markUpload()
		if l.mux.t.BurstPoll {
			l.mux.wakeReceiver()
		}
	}
	return err
}

func (m *driveMux) runReceiveLoop(ctx context.Context) {
	ticker := time.NewTicker(m.t.PollInterval)
	defer ticker.Stop()
	for {
		processed := m.pollMuxObjects(ctx)
		if processed {
			continue
		}
		delay := m.pollDelay()
		if delay != m.t.PollInterval {
			ticker.Stop()
			ticker.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.recvWake:
		case <-ticker.C:
			if delay != m.t.PollInterval {
				ticker.Reset(m.t.PollInterval)
			}
		}
	}
}

func (m *driveMux) wakeReceiver() {
	select {
	case m.recvWake <- struct{}{}:
	default:
	}
}

func (m *driveMux) pollDelay() time.Duration {
	if m.t.burstPollActive(time.Now()) {
		return m.t.BurstPollInterval
	}
	if m.role == "client" && m.active.Load() == 0 {
		return idleOpenPollInterval
	}
	if m.role == "exit" && m.active.Load() == 0 && !m.t.recentActivity() {
		return idleOpenPollInterval
	}
	return m.t.PollInterval
}

func (m *driveMux) pollMuxObjects(ctx context.Context) bool {
	prefix := muxDirPrefix(m.t.SessionID, m.recvDir)
	started := time.Now()
	infos, err := m.listFreshMuxObjects(ctx, prefix)
	m.t.markSlowList(time.Since(started))
	if err != nil {
		if m.t.Logger != nil && ctx.Err() == nil {
			m.t.Logger.Printf("mux list direction=%s failed: %v", directionName(m.recvDir), err)
		}
		return false
	}
	metas := make([]muxObjectMeta, 0, len(infos))
	for _, info := range infos {
		meta, ok := parseMuxObjectInfo(info)
		if !ok || meta.Name == "" {
			continue
		}
		if !controlIsFresh(info, m.startedAt) {
			m.markSeen(meta.Name)
			continue
		}
		if m.hasSeen(meta.Name) {
			continue
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].Lane != metas[j].Lane {
			return metas[i].Lane < metas[j].Lane
		}
		return metas[i].Seq < metas[j].Seq
	})
	if len(metas) == 0 {
		return false
	}
	return m.processMuxObjects(ctx, metas)
}

func (m *driveMux) listFreshMuxObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if store, ok := m.t.Data.(FreshListStore); ok {
		return store.ListFresh(ctx, prefix, m.startedAt)
	}
	return m.t.Data.List(ctx, prefix)
}

func (m *driveMux) processMuxObjects(ctx context.Context, metas []muxObjectMeta) bool {
	workers := minInt(len(metas), m.t.downloadWorkerCount())
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan muxObjectMeta)
	type result struct {
		meta muxObjectMeta
		err  error
	}
	results := make(chan result, len(metas))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for meta := range jobs {
				if m.hasSeen(meta.Name) {
					results <- result{meta: meta}
					continue
				}
				results <- result{meta: meta, err: m.processMuxObject(ctx, meta)}
			}
		}()
	}
	for _, meta := range metas {
		select {
		case jobs <- meta:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(results)
			return false
		}
	}
	close(jobs)
	wg.Wait()
	close(results)

	processed := false
	var cleanup *deferredCleanup
	if m.t.CleanupProcessed {
		cleanup = m.t.newDeferredCleanup()
	}
	for res := range results {
		if res.err != nil {
			if m.t.Logger != nil && ctx.Err() == nil {
				m.t.Logger.Printf("mux process lane=%d seq=%d object=%s error=%s", res.meta.Lane, res.meta.Seq, muxShortName(res.meta.Name), errorSummary(res.err))
			}
			continue
		}
		if res.meta.Name == "" || m.hasSeen(res.meta.Name) {
			continue
		}
		m.markSeen(res.meta.Name)
		processed = true
		if cleanup != nil {
			cleanup.Data(res.meta.Name, res.meta.ID)
		}
	}
	if cleanup != nil {
		cleanup.FlushAsync()
	}
	return processed
}

func (m *driveMux) processMuxObject(ctx context.Context, meta muxObjectMeta) error {
	if meta.Lane < 0 || meta.Lane >= muxLaneCount {
		return fmt.Errorf("invalid mux lane %d", meta.Lane)
	}
	key, err := DeriveMuxLaneKey(m.t.Secret, m.t.SessionID, m.recvDir, meta.Lane)
	if err != nil {
		return err
	}
	release, err := m.t.acquireDownloadSlot(ctx)
	if err != nil {
		return err
	}
	var sealed []byte
	if meta.ID != "" {
		if store, ok := m.t.Data.(ObjectIDStore); ok {
			sealed, err = store.GetByID(ctx, meta.ID)
		} else {
			sealed, err = m.t.Data.Get(ctx, meta.Name)
		}
	} else {
		sealed, err = m.t.Data.Get(ctx, meta.Name)
	}
	release(err)
	if err != nil {
		return err
	}
	env, raw, err := OpenEnvelope(key, sealed)
	if err != nil {
		return err
	}
	if env.Direction != m.recvDir || env.Sequence != meta.Seq || env.SessionID != m.t.SessionID {
		return errors.New("mux envelope metadata mismatch")
	}
	frames, err := decodeMuxBatch(raw)
	if err != nil {
		return err
	}
	for _, frame := range frames {
		m.handleFrame(ctx, frame)
	}
	m.t.markActivity()
	return nil
}

func (m *driveMux) handleFrame(ctx context.Context, frame muxFrame) {
	switch frame.Kind {
	case muxFrameOpen:
		if m.role == "exit" {
			m.openExitStream(ctx, frame.StreamID, frame.Payload)
		}
	case muxFrameData:
		stream := m.stream(frame.StreamID)
		if stream == nil {
			m.queuePendingFrame(frame)
			return
		}
		stream.acceptFrame(ctx, frame)
	case muxFrameFIN:
		if stream := m.stream(frame.StreamID); stream != nil {
			stream.acceptFrame(ctx, frame)
		} else {
			m.queuePendingFrame(frame)
		}
	case muxFrameRST:
		if stream := m.stream(frame.StreamID); stream != nil {
			stream.close()
		}
	case muxFramePing:
	}
}

func (m *driveMux) queuePendingFrame(frame muxFrame) {
	if frame.Seq == 0 {
		return
	}
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	frames := m.pending[frame.StreamID]
	if len(frames) >= muxPendingFrameLimit {
		return
	}
	m.pending[frame.StreamID] = append(frames, frame)
}

func (m *driveMux) flushPendingFrames(ctx context.Context, stream *muxStream) {
	m.pendingMu.Lock()
	frames := m.pending[stream.id]
	delete(m.pending, stream.id)
	m.pendingMu.Unlock()
	if len(frames) == 0 {
		return
	}
	sort.Slice(frames, func(i, j int) bool {
		return frames[i].Seq < frames[j].Seq
	})
	for _, frame := range frames {
		stream.acceptFrame(ctx, frame)
	}
}

func (s *muxStream) acceptFrame(ctx context.Context, frame muxFrame) {
	if frame.Kind == muxFrameRST {
		s.close()
		return
	}
	if frame.Kind != muxFrameData && frame.Kind != muxFrameFIN {
		return
	}
	if frame.Seq == 0 {
		return
	}

	var ready []muxFrame
	s.mu.Lock()
	if frame.Seq < s.recvExpected {
		s.mu.Unlock()
		return
	}
	if _, exists := s.recvPending[frame.Seq]; !exists {
		s.recvPending[frame.Seq] = frame
	}
	for {
		next, ok := s.recvPending[s.recvExpected]
		if !ok {
			break
		}
		delete(s.recvPending, s.recvExpected)
		ready = append(ready, next)
		s.recvExpected++
		if next.Kind == muxFrameFIN {
			break
		}
	}
	s.mu.Unlock()

	for _, next := range ready {
		switch next.Kind {
		case muxFrameData:
			data := append([]byte(nil), next.Payload...)
			select {
			case s.inbound <- data:
			case <-s.done:
				return
			case <-ctx.Done():
				return
			}
		case muxFrameFIN:
			select {
			case s.inbound <- nil:
			case <-s.done:
			case <-ctx.Done():
			}
			return
		}
	}
}

func (m *driveMux) hasSeen(name string) bool {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	_, ok := m.seen[name]
	return ok
}

func (m *driveMux) markSeen(name string) {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	m.seen[name] = struct{}{}
	if len(m.seen) > 200000 {
		m.seen = map[string]struct{}{}
	}
}

func (m *driveMux) readBufferSize() int {
	size := m.t.ChunkSize / 8
	if size < 32*1024 {
		size = 32 * 1024
	}
	if size > 256*1024 {
		size = 256 * 1024
	}
	return size
}

func (m *driveMux) maxBatchBytes() int {
	size := m.t.ChunkSize
	if size < muxMinBatch {
		size = muxMinBatch
	}
	if size > muxMaxBatch {
		size = muxMaxBatch
	}
	return size
}

func encodeMuxBatch(frames []muxFrame) ([]byte, error) {
	if len(frames) > 65535 {
		return nil, errors.New("too many mux frames")
	}
	var buf bytes.Buffer
	buf.WriteString(muxMagic)
	buf.WriteByte(muxVersion)
	var tmp [muxFrameHeaderSize]byte
	binary.BigEndian.PutUint16(tmp[:2], uint16(len(frames)))
	buf.Write(tmp[:2])
	for _, frame := range frames {
		if len(frame.Payload) > int(^uint32(0)) {
			return nil, errors.New("mux frame too large")
		}
		tmp[0] = frame.Kind
		binary.BigEndian.PutUint64(tmp[1:9], frame.StreamID)
		binary.BigEndian.PutUint64(tmp[9:17], frame.Seq)
		binary.BigEndian.PutUint32(tmp[17:21], uint32(len(frame.Payload)))
		buf.Write(tmp[:muxFrameHeaderSize])
		buf.Write(frame.Payload)
	}
	return buf.Bytes(), nil
}

func decodeMuxBatch(data []byte) ([]muxFrame, error) {
	if len(data) < 7 || string(data[:4]) != muxMagic || data[4] != muxVersion {
		return nil, errors.New("bad mux batch header")
	}
	count := int(binary.BigEndian.Uint16(data[5:7]))
	frames := make([]muxFrame, 0, count)
	off := 7
	for i := 0; i < count; i++ {
		if off+muxFrameHeaderSize > len(data) {
			return nil, errors.New("truncated mux frame")
		}
		kind := data[off]
		streamID := binary.BigEndian.Uint64(data[off+1 : off+9])
		seq := binary.BigEndian.Uint64(data[off+9 : off+17])
		size := int(binary.BigEndian.Uint32(data[off+17 : off+21]))
		off += muxFrameHeaderSize
		if size < 0 || off+size > len(data) {
			return nil, errors.New("bad mux frame size")
		}
		frames = append(frames, muxFrame{
			Kind:     kind,
			StreamID: streamID,
			Seq:      seq,
			Payload:  append([]byte(nil), data[off:off+size]...),
		})
		off += size
	}
	if off != len(data) {
		return nil, errors.New("trailing mux batch bytes")
	}
	return frames, nil
}

func encodedMuxFrameSize(frame muxFrame) int {
	return muxFrameHeaderSize + len(frame.Payload)
}

func encodeMuxOpenPayload(target string, initial []byte) []byte {
	targetBytes := []byte(target)
	var buf bytes.Buffer
	var hdr [2]byte
	if len(targetBytes) > 65535 {
		targetBytes = targetBytes[:65535]
	}
	binary.BigEndian.PutUint16(hdr[:], uint16(len(targetBytes)))
	buf.Write(hdr[:])
	buf.Write(targetBytes)
	buf.Write(initial)
	return buf.Bytes()
}

func decodeMuxOpenPayload(payload []byte) (string, []byte, error) {
	if len(payload) < 2 {
		return "", nil, errors.New("open payload too short")
	}
	targetLen := int(binary.BigEndian.Uint16(payload[:2]))
	if targetLen <= 0 || 2+targetLen > len(payload) {
		return "", nil, errors.New("bad open target length")
	}
	target := string(payload[2 : 2+targetLen])
	initial := append([]byte(nil), payload[2+targetLen:]...)
	return target, initial, nil
}

func muxDirPrefix(sid [16]byte, direction byte) string {
	return fmt.Sprintf("muxv3/%s/%s/", SessionString(sid), directionName(direction))
}

func muxObjectName(sid [16]byte, direction byte, epoch string, lane int, seq uint64, frames int, bytes int) string {
	epoch = strings.TrimSpace(epoch)
	if epoch == "" {
		epoch = "0000000000000000"
	}
	return fmt.Sprintf("%s%s/l%02d/%016x.f%d.b%d", muxDirPrefix(sid, direction), epoch, lane, seq, frames, bytes)
}

func parseMuxObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	name := info.Name
	parts := strings.Split(name, "/")
	if len(parts) < 5 || !strings.HasPrefix(parts[len(parts)-2], "l") {
		return muxObjectMeta{}, false
	}
	lane, err := strconv.Atoi(strings.TrimPrefix(parts[len(parts)-2], "l"))
	if err != nil {
		return muxObjectMeta{}, false
	}
	base := parts[len(parts)-1]
	dot := strings.IndexByte(base, '.')
	if dot <= 0 {
		return muxObjectMeta{}, false
	}
	seq, err := strconv.ParseUint(base[:dot], 16, 64)
	if err != nil {
		return muxObjectMeta{}, false
	}
	return muxObjectMeta{Name: name, ID: info.ID, Lane: lane, Seq: seq}, true
}

func muxShortName(name string) string {
	if len(name) <= 32 {
		return name
	}
	return name[len(name)-32:]
}

func randomStreamID() (uint64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	id := binary.BigEndian.Uint64(raw[:])
	if id == 0 {
		id = 1
	}
	return id, nil
}

func randomMuxEpoch() (string, error) {
	id, err := randomStreamID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", id), nil
}
