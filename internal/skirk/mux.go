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
	muxMagic                     = "SKM4"
	muxVersion                   = byte(4)
	muxBatchHeaderSize           = 7
	muxFrameHeaderSize           = 21
	muxFrameOpen                 = byte(1)
	muxFrameData                 = byte(2)
	muxFrameFIN                  = byte(3)
	muxFrameRST                  = byte(4)
	muxLaneCount                 = 4
	muxMaxFrames                 = 512
	muxMinBatch                  = 64 * 1024
	muxMaxBatch                  = 4 * 1024 * 1024
	muxNormalFairBatch           = 1 * 1024 * 1024
	muxNormalBulkBatch           = 4 * 1024 * 1024
	muxInlineFirst               = 16 * 1024
	muxPendingFrameLimit         = 4096
	muxUrgentFrameQueue          = 1024
	muxNormalFrameQueue          = 128
	muxNormalFrameQueueHard      = muxNormalFrameQueue * 4
	muxNormalStreamQueue         = 16
	muxNormalLaneQueueBytes      = 64 * 1024 * 1024
	muxNormalStreamQueueBytes    = 16 * 1024 * 1024
	muxUrgentUploadQueue         = 32
	muxNormalUploadQueue         = 1
	muxStreamInbound             = 64
	muxStreamInboundPause        = muxStreamInbound * 3 / 4
	muxReceiveQueue              = 8192
	muxNormalReceiveQueueBytes   = 64 * 1024 * 1024
	muxNormalReceiveGlobalBytes  = 256 * 1024 * 1024
	muxPendingStreamBytes        = 64 * 1024 * 1024
	muxPendingGlobalBytes        = 256 * 1024 * 1024
	muxStreamPendingFrames       = 4096
	muxStreamPendingBytes        = 64 * 1024 * 1024
	muxStreamPauseFrames         = 64
	muxStreamPauseBytes          = 8 * 1024 * 1024
	muxProcessMaxRetries         = 8
	muxUploadMaxRetries          = 8
	muxStartupCatchup            = 30 * time.Second
	muxListLookback              = 30 * time.Second
	muxClosedStreamTTL           = 2 * time.Minute
	muxRetryDelayMax             = 5 * time.Second
	muxPriorityTinyData          = 4 * 1024
	muxPriorityDataChunk         = inlineDataThreshold
	muxInitialPriorityFrames     = 4
	muxIdlePriorityGap           = 2 * time.Second
	muxIdlePriorityChunks        = 2
	muxNormalStreamInflight      = 6
	muxNormalStreamInflightBytes = 12 * muxNormalFairBatch
	muxNormalSmallBatchDelay     = 50 * time.Millisecond
	muxPriorityDownloadHedge     = 1500 * time.Millisecond
	muxReceiveGapRepairInterval  = 2 * time.Second
)

type muxFrame struct {
	Kind         byte
	ClientID     string
	RunID        string
	StreamID     uint64
	Seq          uint64
	Payload      []byte
	PriorityHint bool
}

type muxObjectMeta struct {
	Name            string
	ID              string
	ClientID        string
	RunID           string
	Epoch           string
	StreamID        uint64
	Lane            int
	Seq             uint64
	Priority        bool
	Plane           string
	PlainBytes      int
	FrameMinSeq     uint64
	FrameMaxSeq     uint64
	FrameRangeKnown bool
	Updated         time.Time
	Attempts        int
}

func (m muxObjectMeta) key() muxStreamKey {
	return muxStreamKey{ClientID: m.ClientID, RunID: m.RunID, StreamID: m.StreamID}
}

func (m muxObjectMeta) normalReceiveBytes() int {
	if m.PlainBytes > 0 {
		return m.PlainBytes
	}
	return muxMinBatch
}

type muxStreamKey struct {
	ClientID string
	RunID    string
	StreamID uint64
}

type driveMux struct {
	t         *Tunnel
	role      string
	sendDir   byte
	recvDir   byte
	epoch     string
	transport string

	lanes []*muxLane

	streamsMu         sync.Mutex
	streams           map[muxStreamKey]*muxStream
	opening           map[muxStreamKey]struct{}
	closed            map[muxStreamKey]time.Time
	pendingMu         sync.Mutex
	pending           map[muxStreamKey][]muxFrame
	pendingBytes      map[muxStreamKey]int
	pendingTotalBytes int
	active            atomic.Int64

	seenMu sync.Mutex
	seen   map[string]struct{}
	queued map[string]struct{}

	listMu        sync.Mutex
	listSince     time.Time
	listPageToken string

	recvWake              chan struct{}
	recvUrgent            chan muxObjectMeta
	recvNormalReady       chan muxStreamKey
	recvNormalMu          sync.Mutex
	recvNormalFlows       map[muxStreamKey][]muxObjectMeta
	recvNormalQueuedBytes map[muxStreamKey]int
	recvNormalQueuedTotal int
	recvNormalActive      map[muxStreamKey]int
	recvNormalActiveBytes map[muxStreamKey]int
	recvNormalSent        map[muxStreamKey]bool
	recvGapLastRepair     map[muxStreamKey]time.Time
	cleanupQueue          chan cleanupTask
	startedAt             time.Time
}

type muxLane struct {
	mux                 *driveMux
	idx                 int
	urgent              chan muxFrame
	urgentUpload        chan []muxFrame
	upload              chan []muxFrame
	normalWake          chan struct{}
	normalMu            sync.Mutex
	normalQueues        map[muxStreamKey][]muxFrame
	normalQueueBytes    map[muxStreamKey]int
	normalQueueFirstAt  map[muxStreamKey]time.Time
	normalQueuedStreams map[muxStreamKey]bool
	normalOrder         []muxStreamKey
	normalQueuedFrames  int
	normalQueuedBytes   int
	seq                 uint64
}

type muxStream struct {
	id       uint64
	clientID string
	runID    string
	mux      *driveMux
	conn     net.Conn

	inbound chan []byte
	done    chan struct{}
	once    sync.Once

	deliverMu        sync.Mutex
	mu               sync.Mutex
	localReadDone    bool
	remoteReadDone   bool
	lastSendAt       time.Time
	sendSeq          atomic.Uint64
	sendPayloadBytes atomic.Int64
	recvExpected     uint64
	recvPending      map[uint64]muxFrame
	recvPendingBytes int
}

func newDriveMux(t *Tunnel, role string, sendDir, recvDir byte) (*driveMux, error) {
	epoch, err := randomMuxEpoch()
	if err != nil {
		return nil, err
	}
	startedAt := time.Now().UTC().Add(-muxStartupCatchup)
	m := &driveMux{
		t:                     t,
		role:                  role,
		sendDir:               sendDir,
		recvDir:               recvDir,
		epoch:                 epoch,
		transport:             normalizeMuxTransport(t.Transport),
		streams:               map[muxStreamKey]*muxStream{},
		opening:               map[muxStreamKey]struct{}{},
		closed:                map[muxStreamKey]time.Time{},
		pending:               map[muxStreamKey][]muxFrame{},
		pendingBytes:          map[muxStreamKey]int{},
		seen:                  map[string]struct{}{},
		queued:                map[string]struct{}{},
		listSince:             startedAt,
		recvWake:              make(chan struct{}, 1),
		recvUrgent:            make(chan muxObjectMeta, muxReceiveQueue),
		recvNormalReady:       make(chan muxStreamKey, muxReceiveQueue),
		recvNormalFlows:       map[muxStreamKey][]muxObjectMeta{},
		recvNormalQueuedBytes: map[muxStreamKey]int{},
		recvNormalActive:      map[muxStreamKey]int{},
		recvNormalActiveBytes: map[muxStreamKey]int{},
		recvNormalSent:        map[muxStreamKey]bool{},
		recvGapLastRepair:     map[muxStreamKey]time.Time{},
		cleanupQueue:          make(chan cleanupTask, muxReceiveQueue),
		startedAt:             startedAt,
	}
	for i := 0; i < muxLaneCount; i++ {
		m.lanes = append(m.lanes, newMuxLane(m, i))
	}
	return m, nil
}

func newMuxLane(m *driveMux, idx int) *muxLane {
	return &muxLane{
		mux:                 m,
		idx:                 idx,
		urgent:              make(chan muxFrame, muxUrgentFrameQueue),
		urgentUpload:        make(chan []muxFrame, muxUrgentUploadQueue),
		upload:              make(chan []muxFrame, muxNormalUploadQueue),
		normalWake:          make(chan struct{}, 1),
		normalQueues:        map[muxStreamKey][]muxFrame{},
		normalQueueBytes:    map[muxStreamKey]int{},
		normalQueueFirstAt:  map[muxStreamKey]time.Time{},
		normalQueuedStreams: map[muxStreamKey]bool{},
	}
}

func normalizeMuxTransport(transport string) string {
	return "muxv4"
}

func (t *Tunnel) serveMuxClient(ctx context.Context, listen string) error {
	t.role = "client"
	if strings.TrimSpace(t.ClientID) == "" || strings.TrimSpace(t.RunID) == "" {
		return errors.New("client id and run id are required for client transport")
	}
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
		for i := 0; i < m.priorityUploadWorkersPerLane(); i++ {
			go lane.runUploadLoop(ctx, true)
		}
		for i := 0; i < m.uploadWorkersPerLane(); i++ {
			go lane.runUploadLoop(ctx, false)
		}
	}
	priorityWorkers, normalWorkers := m.receiveWorkerCounts()
	for i := 0; i < priorityWorkers; i++ {
		go m.runReceiveWorker(ctx, true)
	}
	for i := 0; i < normalWorkers; i++ {
		go m.runReceiveWorker(ctx, false)
	}
	go m.runCleanupLoop(ctx)
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
	stream := m.registerStream(streamID, m.t.ClientID, m.t.RunID, local)
	m.startWriter(stream)
	if err := m.sendFrame(ctx, muxFrame{Kind: muxFrameOpen, ClientID: stream.clientID, RunID: stream.runID, StreamID: streamID, Payload: encodeMuxOpenPayload(target, initial)}); err != nil {
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

func (m *driveMux) openExitStream(ctx context.Context, frame muxFrame) {
	key := frame.key()
	if !m.claimExitOpen(key) {
		if m.t.Logger != nil {
			m.t.Logger.Printf("mux duplicate open ignored stream=%016x client=%s run=%s", frame.StreamID, frame.ClientID, frame.RunID)
		}
		return
	}
	target, initial, err := decodeMuxOpenPayload(frame.Payload)
	if err != nil {
		m.terminalCloseStreamKey(ctx, key, "bad_open", []byte("bad_open"), true)
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
		m.terminalCloseStreamKey(ctx, key, "dial_failed", []byte(sanitizeTransportErrorText(err.Error())), true)
		return
	}
	if m.isClosedStream(key) {
		_ = remote.Close()
		m.terminalCloseStreamKey(ctx, key, "closed_before_open", nil, false)
		return
	}
	stream, ok := m.registerStreamIfOpen(frame.StreamID, frame.ClientID, frame.RunID, remote)
	if !ok {
		_ = remote.Close()
		m.terminalCloseStreamKey(ctx, key, "open_rejected", nil, false)
		return
	}
	m.finishExitOpenClaim(key, false)
	m.startWriter(stream)
	if len(initial) > 0 {
		if err := writeAll(remote, initial); err != nil {
			m.terminalCloseStreamKey(ctx, key, "initial_write_failed", []byte(sanitizeTransportErrorText(err.Error())), true)
			return
		}
	}
	m.flushPendingFrames(ctx, stream)
	go m.readLoop(ctx, stream)
}

func (m *driveMux) claimExitOpen(key muxStreamKey) bool {
	now := time.Now()
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	if m.streams == nil {
		m.streams = map[muxStreamKey]*muxStream{}
	}
	if m.opening == nil {
		m.opening = map[muxStreamKey]struct{}{}
	}
	if m.closed == nil {
		m.closed = map[muxStreamKey]time.Time{}
	}
	if _, ok := m.streams[key]; ok {
		return false
	}
	if _, ok := m.opening[key]; ok {
		return false
	}
	if until, ok := m.closed[key]; ok {
		if now.Before(until) {
			return false
		}
		delete(m.closed, key)
	}
	m.opening[key] = struct{}{}
	return true
}

func (m *driveMux) finishExitOpenClaim(key muxStreamKey, rememberClosed bool) {
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	if m.opening != nil {
		delete(m.opening, key)
	}
	if rememberClosed {
		m.rememberClosedStreamLocked(key, time.Now())
	}
}

func (m *driveMux) rememberClosedStream(key muxStreamKey) {
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	if m.opening != nil {
		delete(m.opening, key)
	}
	m.rememberClosedStreamLocked(key, time.Now())
}

func (m *driveMux) terminalCloseStreamKey(ctx context.Context, key muxStreamKey, reason string, rstPayload []byte, sendRST bool) {
	if m == nil {
		return
	}
	if m.t != nil && m.t.Logger != nil && reason != "" {
		m.t.Logger.Printf("mux terminal close role=%s stream=%016x reason=%s", m.role, key.StreamID, reason)
	}
	m.rememberClosedStream(key)
	m.dropPendingFrames(key)
	m.dropQueuedNormalMuxObjects(key)
	if stream := m.streamByKey(key); stream != nil {
		stream.close()
	}
	if sendRST {
		m.sendRSTBestEffort(ctx, key, rstPayload)
	}
}

func (m *driveMux) registerStream(id uint64, clientID, runID string, conn net.Conn) *muxStream {
	stream, _ := m.registerStreamInternal(id, clientID, runID, conn, false)
	return stream
}

func (m *driveMux) registerStreamIfOpen(id uint64, clientID, runID string, conn net.Conn) (*muxStream, bool) {
	return m.registerStreamInternal(id, clientID, runID, conn, true)
}

func (m *driveMux) registerStreamInternal(id uint64, clientID, runID string, conn net.Conn, rejectClosed bool) (*muxStream, bool) {
	stream := &muxStream{
		id:           id,
		clientID:     clientID,
		runID:        runID,
		mux:          m,
		conn:         conn,
		inbound:      make(chan []byte, muxStreamInbound),
		done:         make(chan struct{}),
		recvExpected: 1,
		recvPending:  map[uint64]muxFrame{},
	}
	m.streamsMu.Lock()
	if m.streams == nil {
		m.streams = map[muxStreamKey]*muxStream{}
	}
	key := stream.key()
	if rejectClosed {
		now := time.Now()
		if until, ok := m.closed[key]; ok {
			if now.Before(until) {
				m.streamsMu.Unlock()
				return nil, false
			}
			delete(m.closed, key)
		}
	}
	m.streams[key] = stream
	m.streamsMu.Unlock()
	m.active.Add(1)
	m.t.activeStreams.Add(1)
	return stream, true
}

func (s *muxStream) key() muxStreamKey {
	return muxStreamKey{ClientID: s.clientID, RunID: s.runID, StreamID: s.id}
}

func (f muxFrame) key() muxStreamKey {
	return muxStreamKey{ClientID: f.ClientID, RunID: f.RunID, StreamID: f.StreamID}
}

func (m *driveMux) unregisterStream(stream *muxStream) {
	m.streamsMu.Lock()
	key := stream.key()
	if m.streams[key] == stream {
		delete(m.streams, key)
		m.rememberClosedStreamLocked(key, time.Now())
	}
	m.streamsMu.Unlock()
	m.dropPendingFrames(key)
	m.dropQueuedNormalMuxObjects(key)
	m.active.Add(-1)
	m.t.activeStreams.Add(-1)
}

func (m *driveMux) dropQueuedNormalMuxObjects(key muxStreamKey) {
	m.recvNormalMu.Lock()
	metas := m.removeQueuedNormalMuxObjectsLocked(key)
	m.recvNormalMu.Unlock()
	m.discardQueuedMuxObjects(metas)
}

func (m *driveMux) removeQueuedNormalMuxObjectsLocked(key muxStreamKey) []muxObjectMeta {
	metas := append([]muxObjectMeta(nil), m.recvNormalFlows[key]...)
	for _, meta := range metas {
		m.removeNormalReceiveQueuedLocked(key, meta)
	}
	delete(m.recvNormalFlows, key)
	if m.recvNormalActive[key] == 0 {
		delete(m.recvNormalActive, key)
		delete(m.recvNormalActiveBytes, key)
	}
	delete(m.recvNormalSent, key)
	return metas
}

func (m *driveMux) discardQueuedMuxObjects(metas []muxObjectMeta) {
	for _, meta := range metas {
		m.discardQueuedMuxObject(meta)
	}
}

func (m *driveMux) discardQueuedMuxObject(meta muxObjectMeta) {
	if meta.Name == "" {
		return
	}
	m.markSeen(meta.Name)
	m.enqueueCleanup(cleanupTask{name: meta.Name, id: meta.ID})
}

func (m *driveMux) rememberClosedStreamLocked(key muxStreamKey, now time.Time) {
	if m.closed == nil {
		m.closed = map[muxStreamKey]time.Time{}
	}
	if len(m.closed) > 100000 {
		m.closed = map[muxStreamKey]time.Time{}
	}
	m.closed[key] = now.Add(muxClosedStreamTTL)
}

func (m *driveMux) isClosedStream(key muxStreamKey) bool {
	now := time.Now()
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	if until, ok := m.closed[key]; ok {
		if now.Before(until) {
			return true
		}
		delete(m.closed, key)
	}
	return false
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

func (m *driveMux) priorityUploadWorkersPerLane() int {
	if len(m.lanes) == 0 {
		return 1
	}
	workers := m.t.uploadWorkerCount()
	perLane := (workers + len(m.lanes) - 1) / len(m.lanes)
	switch {
	case perLane >= 8:
		return 2
	case perLane >= 2:
		return 1
	default:
		return 1
	}
}

func (m *driveMux) stream(frame muxFrame) *muxStream {
	return m.streamByKey(frame.key())
}

func (m *driveMux) streamByKey(key muxStreamKey) *muxStream {
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	return m.streams[key]
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
		n, err := readChunkWithPolicy(stream.conn, buffer, stream.bulkSendCoalescing())
		if n > 0 {
			if sendErr := m.sendDataPayload(ctx, stream, buffer[:n]); sendErr != nil {
				stream.close()
				return
			}
			stream.noteSentPayloadBytes(n)
		}
		if err == nil {
			continue
		}
		if err == io.EOF || strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
			_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameFIN, ClientID: stream.clientID, RunID: stream.runID, StreamID: stream.id, Seq: stream.nextSendSeq()})
			stream.markLocalReadDone()
			return
		}
		_ = m.sendFrame(ctx, muxFrame{Kind: muxFrameRST, ClientID: stream.clientID, RunID: stream.runID, StreamID: stream.id, Payload: []byte(sanitizeTransportErrorText(err.Error()))})
		stream.close()
		return
	}
}

func (m *driveMux) sendDataPayload(ctx context.Context, stream *muxStream, payload []byte) error {
	priorityChunks := stream.beginSendDataBurst(time.Now())
	for len(payload) > 0 {
		seq := stream.nextSendSeq()
		chunkSize := len(payload)
		if seq <= muxInitialPriorityFrames && chunkSize > muxPriorityDataChunk {
			chunkSize = muxPriorityDataChunk
		}
		priorityHint := false
		if priorityChunks > 0 {
			priorityHint = true
			priorityChunks--
			if chunkSize > muxPriorityDataChunk {
				chunkSize = muxPriorityDataChunk
			}
		}
		if !priorityHint && chunkSize > muxNormalFairBatch {
			chunkSize = muxNormalFairBatch
		}
		chunk := append([]byte(nil), payload[:chunkSize]...)
		if err := m.sendFrame(ctx, muxFrame{Kind: muxFrameData, ClientID: stream.clientID, RunID: stream.runID, StreamID: stream.id, Seq: seq, Payload: chunk, PriorityHint: priorityHint}); err != nil {
			return err
		}
		payload = payload[chunkSize:]
	}
	return nil
}

func (s *muxStream) beginSendDataBurst(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle := s.lastSendAt.IsZero() || now.Sub(s.lastSendAt) >= muxIdlePriorityGap
	s.lastSendAt = now
	if idle {
		return muxIdlePriorityChunks
	}
	return 0
}

func (s *muxStream) nextSendSeq() uint64 {
	return s.sendSeq.Add(1)
}

func (s *muxStream) noteSentPayloadBytes(n int) {
	if n > 0 {
		s.sendPayloadBytes.Add(int64(n))
	}
}

func (s *muxStream) bulkSendCoalescing() bool {
	return s.sendPayloadBytes.Load() >= bulkStreamCoalesceAfter
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
					stream.close()
					m.sendRSTBestEffort(context.Background(), stream.key(), []byte(sanitizeTransportErrorText(err.Error())))
					return
				}
				signalCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				m.signalNormalMuxObjectIfReady(signalCtx, stream.key())
				cancel()
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
	if halfCloser, ok := s.conn.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
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
		if s.conn != nil {
			_ = s.conn.Close()
		}
		close(s.done)
		if s.mux != nil {
			s.mux.unregisterStream(s)
		}
	})
}

func (m *driveMux) sendRSTBestEffort(ctx context.Context, key muxStreamKey, payload []byte) {
	if m == nil || len(m.lanes) == 0 {
		return
	}
	if len(payload) == 0 {
		payload = []byte("rst")
	}
	frame := m.normalizeFrameNamespace(muxFrame{Kind: muxFrameRST, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: payload})
	if frame.ClientID == "" || frame.RunID == "" {
		return
	}
	lane := m.lanes[m.frameLane(frame)]
	if lane == nil {
		return
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case lane.urgent <- frame:
		if m.t != nil {
			m.t.markActivity()
		}
	case <-ctx.Done():
	case <-timer.C:
		if m.t != nil && m.t.Logger != nil {
			m.t.Logger.Printf("mux rst drop role=%s stream=%016x reason=urgent_queue_full", m.role, key.StreamID)
		}
	}
}

func (m *driveMux) sendFrame(ctx context.Context, frame muxFrame) error {
	if len(m.lanes) == 0 {
		return errors.New("mux has no lanes")
	}
	frame = m.normalizeFrameNamespace(frame)
	if frame.ClientID == "" || frame.RunID == "" {
		return errors.New("mux frame client id and run id are required")
	}
	lane := m.lanes[m.frameLane(frame)]
	if muxPriorityFrame(frame) {
		select {
		case lane.urgent <- frame:
			m.t.markActivity()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := lane.enqueueNormalFrame(ctx, frame); err != nil {
		return err
	}
	m.t.markActivity()
	return nil
}

func (l *muxLane) enqueueNormalFrame(ctx context.Context, frame muxFrame) error {
	started := time.Now()
	loggedWait := false
	key := frame.key()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		l.normalMu.Lock()
		streamLen := len(l.normalQueues[key])
		frameBytes := encodedMuxFrameSize(frame)
		streamBytes := l.normalQueueBytes[key]
		queueLimit := muxNormalFrameQueue
		if streamLen == 0 && !l.normalQueuedStreams[key] {
			queueLimit = muxNormalFrameQueueHard
		}
		if l.normalQueuedFrames < queueLimit &&
			l.normalQueuedBytes+frameBytes <= muxNormalLaneQueueBytes &&
			streamLen < muxNormalStreamQueue &&
			streamBytes+frameBytes <= muxNormalStreamQueueBytes {
			if streamLen == 0 && !l.normalQueuedStreams[key] {
				l.normalOrder = append(l.normalOrder, key)
				l.normalQueuedStreams[key] = true
				l.normalQueueFirstAt[key] = time.Now()
			}
			l.normalQueues[key] = append(l.normalQueues[key], frame)
			l.normalQueueBytes[key] = streamBytes + frameBytes
			l.normalQueuedFrames++
			l.normalQueuedBytes += frameBytes
			totalQueued := l.normalQueuedFrames
			totalQueuedBytes := l.normalQueuedBytes
			streamQueuedBytes := streamBytes + frameBytes
			l.normalMu.Unlock()
			l.signalNormalFrames()
			if waited := time.Since(started); waited >= 250*time.Millisecond && l.mux.t.Logger != nil {
				l.mux.t.Logger.Printf("mux enqueue slow role=%s stream=%016x kind=%d frame_seq=%d priority=false lane=%d wait=%s normal_q=%d normal_q_bytes=%d stream_q=%d stream_q_bytes=%d", l.mux.role, frame.StreamID, frame.Kind, frame.Seq, l.idx, waited.Round(time.Millisecond), totalQueued, totalQueuedBytes, streamLen+1, streamQueuedBytes)
			}
			return nil
		}
		totalQueued := l.normalQueuedFrames
		totalQueuedBytes := l.normalQueuedBytes
		l.normalMu.Unlock()
		if !loggedWait && time.Since(started) >= 250*time.Millisecond && l.mux.t.Logger != nil {
			loggedWait = true
			l.mux.t.Logger.Printf("mux enqueue waiting role=%s stream=%016x kind=%d frame_seq=%d priority=false lane=%d normal_q=%d normal_q_bytes=%d stream_q=%d stream_q_bytes=%d", l.mux.role, frame.StreamID, frame.Kind, frame.Seq, l.idx, totalQueued, totalQueuedBytes, streamLen, streamBytes)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *muxLane) signalNormalFrames() {
	select {
	case l.normalWake <- struct{}{}:
	default:
	}
}

func (m *driveMux) normalizeFrameNamespace(frame muxFrame) muxFrame {
	if frame.ClientID == "" {
		frame.ClientID = strings.TrimSpace(m.t.ClientID)
	}
	if frame.RunID == "" {
		frame.RunID = strings.TrimSpace(m.t.RunID)
	}
	return frame
}

func (m *driveMux) frameLane(frame muxFrame) int {
	laneCount := uint64(len(m.lanes))
	if laneCount == 0 {
		return 0
	}
	home := int(frame.StreamID % laneCount)
	return home
}

func (l *muxLane) runBatchLoop(ctx context.Context) {
	go l.runBatchLoopFor(ctx, l.urgent, l.urgentUpload)
	l.runFairNormalBatchLoop(ctx)
}

func (l *muxLane) runBatchLoopFor(ctx context.Context, input <-chan muxFrame, output chan<- []muxFrame) {
	var pending []muxFrame
	for {
		var first muxFrame
		if len(pending) > 0 {
			first = pending[0]
			pending = pending[1:]
		} else {
			select {
			case <-ctx.Done():
				return
			case frame := <-input:
				first = frame
			}
			if first.Kind == 0 {
				continue
			}
		}
		frames := []muxFrame{first}
		bytes := encodedMuxFrameSize(first)
		timer := time.NewTimer(muxFlushDelay(first))
		flush := false
		for !flush && len(frames) < muxMaxFrames && bytes < l.mux.maxBatchBytes() {
			select {
			case frame := <-input:
				if !sameMuxBatchNamespace(first, frame) {
					pending = append(pending, frame)
					flush = true
					continue
				}
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
		case output <- frames:
		case <-ctx.Done():
			return
		}
	}
}

func sameMuxBatchNamespace(a, b muxFrame) bool {
	return a.ClientID == b.ClientID && a.RunID == b.RunID && a.StreamID == b.StreamID
}

func (l *muxLane) runFairNormalBatchLoop(ctx context.Context) {
	for {
		frames, ok := l.takeNormalBatch(ctx)
		if !ok {
			return
		}
		select {
		case l.upload <- frames:
		case <-ctx.Done():
			return
		}
	}
}

func (l *muxLane) takeNormalBatch(ctx context.Context) ([]muxFrame, bool) {
	for {
		l.normalMu.Lock()
		if l.normalQueuedFrames > 0 && len(l.normalOrder) > 0 {
			key := l.normalOrder[0]
			contended := len(l.normalOrder) > 1
			queue := l.normalQueues[key]
			if len(queue) == 0 {
				l.normalOrder = l.normalOrder[1:]
				l.normalQueuedStreams[key] = false
				delete(l.normalQueues, key)
				delete(l.normalQueueBytes, key)
				delete(l.normalQueueFirstAt, key)
				delete(l.normalQueuedStreams, key)
				l.normalMu.Unlock()
				continue
			}
			if l.shouldCorkNormalBatchLocked(key, queue, contended) {
				l.normalMu.Unlock()
				if !l.waitNormalSmallBatch(ctx) {
					return nil, false
				}
				continue
			}
			l.normalOrder = l.normalOrder[1:]
			l.normalQueuedStreams[key] = false
			first := queue[0]
			frames := []muxFrame{first}
			bytes := encodedMuxFrameSize(first)
			consumed := 1
			batchLimit := l.normalBatchLimit(contended)
			for consumed < len(queue) && len(frames) < muxMaxFrames && bytes < batchLimit {
				next := queue[consumed]
				if !sameMuxBatchNamespace(first, next) {
					break
				}
				nextBytes := encodedMuxFrameSize(next)
				if bytes+nextBytes > batchLimit && len(frames) > 0 {
					break
				}
				frames = append(frames, next)
				bytes += nextBytes
				consumed++
			}
			queue = queue[consumed:]
			l.normalQueuedFrames -= consumed
			l.normalQueuedBytes -= bytes
			if l.normalQueuedBytes < 0 {
				l.normalQueuedBytes = 0
			}
			if remainingBytes := l.normalQueueBytes[key] - bytes; remainingBytes > 0 {
				l.normalQueueBytes[key] = remainingBytes
			} else {
				delete(l.normalQueueBytes, key)
			}
			if len(queue) > 0 {
				l.normalQueues[key] = queue
				if !l.normalQueuedStreams[key] {
					l.normalOrder = append(l.normalOrder, key)
					l.normalQueuedStreams[key] = true
				}
			} else {
				delete(l.normalQueues, key)
				delete(l.normalQueueBytes, key)
				delete(l.normalQueueFirstAt, key)
				delete(l.normalQueuedStreams, key)
			}
			remainingFrames := l.normalQueuedFrames
			remainingStreams := len(l.normalOrder)
			l.normalMu.Unlock()
			l.observeNormalSchedule(first, len(frames), bytes, batchLimit, contended, remainingStreams, remainingFrames)
			return frames, true
		}
		l.normalMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-l.normalWake:
		}
	}
}

func (l *muxLane) shouldCorkNormalBatchLocked(key muxStreamKey, queue []muxFrame, contended bool) bool {
	if contended || len(queue) != 1 {
		return false
	}
	frame := queue[0]
	if frame.Kind != muxFrameData || muxPriorityFrame(frame) || encodedMuxFrameSize(frame) >= muxMinBatch {
		return false
	}
	firstAt := l.normalQueueFirstAt[key]
	if firstAt.IsZero() {
		return false
	}
	return time.Since(firstAt) < muxNormalSmallBatchDelay
}

func (l *muxLane) waitNormalSmallBatch(ctx context.Context) bool {
	timer := time.NewTimer(muxNormalSmallBatchDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *muxLane) normalBatchLimit(contended bool) int {
	limit := l.mux.normalBatchBytes()
	if contended && limit > muxNormalFairBatch {
		return muxNormalFairBatch
	}
	return limit
}

func (l *muxLane) observeNormalSchedule(first muxFrame, frames, bytes, batchLimit int, contended bool, remainingStreams, remainingFrames int) {
	if l.mux == nil || l.mux.t == nil || !l.mux.t.Observe || l.mux.t.Logger == nil {
		return
	}
	if !contended && remainingStreams == 0 {
		return
	}
	l.mux.t.Logger.Printf("mux send scheduler role=%s lane=%d stream=%016x frames=%d plain_bytes=%d batch_limit=%d contended=%t remaining_streams=%d remaining_frames=%d", l.mux.role, l.idx, first.StreamID, frames, bytes, batchLimit, contended, remainingStreams, remainingFrames)
}

func (l *muxLane) runUploadLoop(ctx context.Context, priorityOnly bool) {
	for {
		frames, done := l.receiveUploadBatch(ctx, priorityOnly)
		if done {
			return
		}
		for attempt := 1; ; attempt++ {
			if err := l.uploadBatch(ctx, frames); err != nil {
				if ctx.Err() != nil {
					return
				}
				if attempt >= muxUploadMaxRetries {
					l.failUploadBatch(ctx, frames, err, attempt)
					break
				}
				delay := muxRetryDelay(attempt)
				if l.mux.t.Logger != nil {
					bytes := muxBatchPlainBytes(frames)
					l.mux.t.Logger.Printf("mux upload retry role=%s lane=%d priority=%t frames=%d plain_bytes=%d attempt=%d delay=%s error=%s", l.mux.role, l.idx, muxPriorityBatch(frames), len(frames), bytes, attempt, delay, errorSummary(err))
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			break
		}
	}
}

func (l *muxLane) failUploadBatch(ctx context.Context, frames []muxFrame, err error, attempts int) {
	if l == nil || l.mux == nil {
		return
	}
	if l.mux.t != nil && l.mux.t.Logger != nil {
		l.mux.t.Logger.Printf("mux upload terminal failure role=%s lane=%d priority=%t frames=%d plain_bytes=%d attempts=%d error=%s", l.mux.role, l.idx, muxPriorityBatch(frames), len(frames), muxBatchPlainBytes(frames), attempts, errorSummary(err))
	}
	closed := map[muxStreamKey]struct{}{}
	for _, frame := range frames {
		key := frame.key()
		if key.StreamID == 0 {
			continue
		}
		if _, ok := closed[key]; ok {
			continue
		}
		closed[key] = struct{}{}
		l.mux.terminalCloseStreamKey(ctx, key, "mux_upload_failed", []byte("mux_upload_failed"), true)
	}
}

func (l *muxLane) receiveUploadBatch(ctx context.Context, priorityOnly bool) ([]muxFrame, bool) {
	select {
	case frames := <-l.urgentUpload:
		return frames, false
	default:
	}
	if priorityOnly {
		select {
		case <-ctx.Done():
			return nil, true
		case frames := <-l.urgentUpload:
			return frames, false
		}
	}
	select {
	case <-ctx.Done():
		return nil, true
	case frames := <-l.urgentUpload:
		return frames, false
	case frames := <-l.upload:
		return frames, false
	}
}

func muxFlushDelay(frame muxFrame) time.Duration {
	if frame.Kind == muxFrameOpen || frame.Kind == muxFrameFIN || frame.Kind == muxFrameRST {
		return 5 * time.Millisecond
	}
	if frame.Kind == muxFrameData && frame.Seq == 1 {
		return 5 * time.Millisecond
	}
	if frame.Kind == muxFrameData && len(frame.Payload) <= 4096 {
		return 5 * time.Millisecond
	}
	return 25 * time.Millisecond
}

func muxPriorityFrame(frame muxFrame) bool {
	switch frame.Kind {
	case muxFrameOpen, muxFrameFIN, muxFrameRST:
		return true
	case muxFrameData:
		if frame.PriorityHint && len(frame.Payload) <= muxPriorityDataChunk {
			return true
		}
		return frame.Seq <= muxInitialPriorityFrames && len(frame.Payload) <= muxPriorityDataChunk
	default:
		return false
	}
}

func muxPriorityBatch(frames []muxFrame) bool {
	if len(frames) == 0 {
		return false
	}
	for _, frame := range frames {
		if !muxPriorityFrame(frame) {
			return false
		}
	}
	return true
}

func (l *muxLane) uploadBatch(ctx context.Context, frames []muxFrame) error {
	return l.uploadBatchV4(ctx, frames)
}

func (l *muxLane) uploadBatchV4(ctx context.Context, frames []muxFrame) error {
	if len(frames) == 0 {
		return nil
	}
	clientID := frames[0].ClientID
	runID := frames[0].RunID
	if clientID == "" || runID == "" {
		return errors.New("mux upload batch missing client namespace")
	}
	raw, err := encodeMuxBatch(frames)
	if err != nil {
		return err
	}
	seq := atomic.AddUint64(&l.seq, 1)
	key, err := DeriveMuxLaneKeyV4(l.mux.t.Secret, l.mux.t.SessionID, l.mux.sendDir, clientID, runID, l.idx)
	if err != nil {
		return err
	}
	sealed, err := Seal(key, l.mux.t.SessionID, l.mux.sendDir, seq, raw, false)
	if err != nil {
		return err
	}
	priority := muxPriorityBatch(frames)
	minSeq, maxSeq := muxBatchFrameSeqRange(frames)
	name := muxObjectName(l.mux.t.SessionID, l.mux.sendDir, clientID, runID, l.mux.epoch, frames[0].StreamID, l.idx, seq, len(frames), minSeq, maxSeq, len(raw), priority)
	release, err := l.mux.t.acquireUploadSlotBytes(ctx, priority)
	if err != nil {
		return err
	}
	started := time.Now()
	opCtx, cancel := context.WithTimeout(ctx, muxDriveAttemptTimeout(len(sealed), priority, l.mux.t.RouteProxy != ""))
	if store, ok := l.mux.t.Data.(ObjectPutStore); ok {
		_, err = store.PutObject(opCtx, name, sealed)
	} else {
		err = l.mux.t.Data.Put(opCtx, name, sealed)
	}
	cancel()
	release(err, int64(len(sealed)))
	duration := time.Since(started)
	if l.mux.t.Logger != nil && (l.mux.t.Observe || err != nil || duration >= l.mux.t.slowDriveThreshold()) {
		l.mux.t.Logger.Printf("mux upload role=%s lane=%d seq=%d priority=%t stream=%016x frames=%d frame_seq_min=%d frame_seq_max=%d plain_bytes=%d sealed_bytes=%d urgent_q=%d normal_q=%d urgent_upload_q=%d normal_upload_q=%d duration=%s error=%s", l.mux.role, l.idx, seq, priority, frames[0].StreamID, len(frames), minSeq, maxSeq, len(raw), len(sealed), len(l.urgent), l.normalQueueLen(), len(l.urgentUpload), len(l.upload), duration.Round(time.Millisecond), errorSummary(err))
	}
	if err == nil {
		l.mux.t.markUpload()
		l.mux.wakeReceiver()
	}
	return err
}

func (l *muxLane) normalQueueLen() int {
	l.normalMu.Lock()
	defer l.normalMu.Unlock()
	return l.normalQueuedFrames
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
			resetTicker(ticker, delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.recvWake:
		case <-ticker.C:
			if delay != m.t.PollInterval {
				resetTicker(ticker, m.t.PollInterval)
			}
		}
	}
}

func resetTicker(ticker *time.Ticker, delay time.Duration) {
	if delay <= 0 {
		return
	}
	ticker.Stop()
	select {
	case <-ticker.C:
	default:
	}
	ticker.Reset(delay)
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
	prefix := m.recvPrefix()
	return m.pollMuxObjectsFromPrefix(ctx, prefix, m.discoverySource(), m.listRecvMuxObjects, parseMuxObjectInfo, m.hasListFreshPageToken, m.rewindListSinceForMetas)
}

func (m *driveMux) pollMuxObjectsFromPrefix(ctx context.Context, prefix, source string, listFn func(context.Context, string) ([]ObjectInfo, error), parseFn func(ObjectInfo) (muxObjectMeta, bool), hasMoreFn func() bool, rewindFn func([]muxObjectMeta)) bool {
	started := time.Now()
	infos, err := listFn(ctx, prefix)
	listDuration := time.Since(started)
	m.t.markSlowList(listDuration)
	if err != nil {
		if m.t.Logger != nil && ctx.Err() == nil {
			m.t.Logger.Printf("mux list direction=%s failed: %v", directionName(m.recvDir), err)
		}
		return false
	}
	metas := make([]muxObjectMeta, 0, len(infos))
	for _, info := range infos {
		meta, ok := parseFn(info)
		if !ok || meta.Name == "" {
			continue
		}
		if !controlIsFresh(info, m.startedAt) {
			m.markSeen(meta.Name)
			continue
		}
		if m.isKnown(meta.Name) {
			continue
		}
		metas = append(metas, meta)
	}
	metas = orderMuxMetas(metas)
	if m.t.Observe && m.t.Logger != nil {
		m.t.Logger.Printf("mux poll role=%s direction=%s source=%s prefix=%s infos=%d metas=%d seen=%d duration=%s", m.role, directionName(m.recvDir), source, muxShortName(prefix), len(infos), len(metas), len(infos)-len(metas), listDuration.Round(time.Millisecond))
	}
	if len(metas) == 0 {
		return hasMoreFn()
	}
	enqueued := 0
	var failed []muxObjectMeta
	for _, meta := range metas {
		if m.enqueueMuxObject(ctx, meta) {
			enqueued++
			continue
		}
		if !m.isKnown(meta.Name) {
			failed = append(failed, meta)
		}
	}
	if len(failed) > 0 && rewindFn != nil {
		rewindFn(failed)
	}
	return enqueued > 0 || hasMoreFn()
}

func (m *driveMux) recvPrefix() string {
	if m.role == "exit" {
		return muxDirPrefix(m.t.SessionID, m.recvDir, "", "")
	}
	return muxDirPrefix(m.t.SessionID, m.recvDir, m.t.ClientID, m.t.RunID)
}

func (m *driveMux) listRecvMuxObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if store, ok := m.t.Data.(FreshListPageStatusStore); ok {
		since, pageToken := m.listFreshCursor()
		result, err := store.ListFreshPageStatus(ctx, prefix, since, pageToken)
		if err != nil && pageToken != "" && isDrivePageTokenRejected(err) {
			m.setListFreshPageToken("")
			if m.t != nil && m.t.Logger != nil {
				m.t.Logger.Printf("mux list page token rejected role=%s direction=%s prefix=%s error=%s", m.role, directionName(m.recvDir), muxShortName(prefix), errorSummary(err))
			}
			result, err = store.ListFreshPageStatus(ctx, prefix, since, "")
		}
		if err == nil && result.Truncated && result.NextPageToken != "" {
			m.setListFreshPageToken(result.NextPageToken)
			if m.t != nil && m.t.Logger != nil {
				m.t.Logger.Printf("mux list fresh truncated role=%s direction=%s prefix=%s infos=%d since=%s next_page=%t", m.role, directionName(m.recvDir), muxShortName(prefix), len(result.Objects), since.Format(time.RFC3339Nano), result.NextPageToken != "")
			}
		} else if err == nil && (result.Truncated || result.Incomplete) {
			m.setListFreshPageToken("")
			if m.t != nil && m.t.Logger != nil {
				m.t.Logger.Printf("mux list fresh partial role=%s direction=%s prefix=%s infos=%d since=%s truncated=%t incomplete=%t", m.role, directionName(m.recvDir), muxShortName(prefix), len(result.Objects), since.Format(time.RFC3339Nano), result.Truncated, result.Incomplete)
			}
		} else if err == nil {
			m.setListFreshPageToken("")
			m.advanceListSince(result.Objects)
		}
		return result.Objects, err
	}
	if store, ok := m.t.Data.(FreshListStatusStore); ok {
		result, err := store.ListFreshStatus(ctx, prefix, m.listFreshSince())
		if err == nil && !result.Truncated {
			m.advanceListSince(result.Objects)
		}
		if err == nil && (result.Truncated || result.Incomplete) && m.t != nil && m.t.Logger != nil {
			m.t.Logger.Printf("mux list fresh partial role=%s direction=%s prefix=%s infos=%d since=%s truncated=%t incomplete=%t", m.role, directionName(m.recvDir), muxShortName(prefix), len(result.Objects), m.listFreshSince().Format(time.RFC3339Nano), result.Truncated, result.Incomplete)
		}
		return result.Objects, err
	}
	if store, ok := m.t.Data.(FreshListStore); ok {
		infos, err := store.ListFresh(ctx, prefix, m.listFreshSince())
		if err == nil {
			m.advanceListSince(infos)
		}
		return infos, err
	}
	return m.listMuxObjectsByPrefix(ctx, prefix)
}

func (m *driveMux) listFreshSince() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if m.listSince.IsZero() {
		return m.startedAt
	}
	return m.listSince
}

func (m *driveMux) listFreshCursor() (time.Time, string) {
	if m == nil {
		return time.Time{}, ""
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	since := m.listSince
	if since.IsZero() {
		since = m.startedAt
	}
	return since, m.listPageToken
}

func (m *driveMux) setListFreshPageToken(pageToken string) {
	if m == nil {
		return
	}
	m.listMu.Lock()
	m.listPageToken = strings.TrimSpace(pageToken)
	m.listMu.Unlock()
}

func (m *driveMux) hasListFreshPageToken() bool {
	if m == nil {
		return false
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	return strings.TrimSpace(m.listPageToken) != ""
}

func (m *driveMux) advanceListSince(infos []ObjectInfo) {
	if m == nil || len(infos) == 0 {
		return
	}
	next := m.nextListSince(infos)
	if next.IsZero() {
		return
	}
	m.listMu.Lock()
	if m.listSince.IsZero() || next.After(m.listSince) {
		m.listSince = next
	}
	m.listPageToken = ""
	m.listMu.Unlock()
}

func (m *driveMux) rewindListSinceForMetas(metas []muxObjectMeta) {
	m.rewindListSinceForMetasLocked(metas, &m.listMu, &m.listSince, &m.listPageToken)
}

func (m *driveMux) rewindReceiveListForMeta(meta muxObjectMeta) {
	m.rewindListSinceForMetas([]muxObjectMeta{meta})
}

func (m *driveMux) rewindListSinceForMetasLocked(metas []muxObjectMeta, mu *sync.Mutex, since *time.Time, pageToken *string) {
	if m == nil || len(metas) == 0 || mu == nil || since == nil || pageToken == nil {
		return
	}
	target := m.listSinceTargetForMetas(metas)
	if target.IsZero() {
		return
	}
	mu.Lock()
	if since.IsZero() || target.Before(*since) {
		*since = target
	}
	*pageToken = ""
	mu.Unlock()
}

func (m *driveMux) listSinceTargetForMetas(metas []muxObjectMeta) time.Time {
	var oldest time.Time
	for _, meta := range metas {
		if meta.Updated.IsZero() {
			continue
		}
		if oldest.IsZero() || meta.Updated.Before(oldest) {
			oldest = meta.Updated
		}
	}
	if oldest.IsZero() {
		return m.startedAt
	}
	target := oldest.Add(-muxListLookback)
	if !m.startedAt.IsZero() && target.Before(m.startedAt) {
		return m.startedAt
	}
	return target
}

func (m *driveMux) nextListSince(infos []ObjectInfo) time.Time {
	var newest time.Time
	for _, info := range infos {
		updated := parseObjectUpdated(info)
		if updated.After(newest) {
			newest = updated
		}
	}
	if newest.IsZero() {
		return time.Time{}
	}
	next := newest.Add(-muxListLookback)
	if !m.startedAt.IsZero() && next.Before(m.startedAt) {
		next = m.startedAt
	}
	return next
}

func parseObjectUpdated(info ObjectInfo) time.Time {
	if strings.TrimSpace(info.Updated) == "" {
		return time.Time{}
	}
	updated, err := time.Parse(time.RFC3339Nano, info.Updated)
	if err != nil {
		return time.Time{}
	}
	return updated
}

func (m *driveMux) discoverySource() string {
	if _, ok := m.t.Data.(FreshListStore); ok {
		return "prefix_fast"
	}
	return "prefix"
}

func (m *driveMux) listMuxObjectsByPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	return m.t.Data.List(ctx, prefix)
}

func fairOrderMuxMetas(metas []muxObjectMeta) []muxObjectMeta {
	if len(metas) <= 1 {
		return metas
	}
	groups := map[muxStreamKey][]muxObjectMeta{}
	var keys []muxStreamKey
	seen := map[muxStreamKey]struct{}{}
	for _, meta := range metas {
		key := muxStreamKey{ClientID: meta.ClientID, RunID: meta.RunID, StreamID: meta.StreamID}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], meta)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ClientID != keys[j].ClientID {
			return keys[i].ClientID < keys[j].ClientID
		}
		if keys[i].RunID != keys[j].RunID {
			return keys[i].RunID < keys[j].RunID
		}
		return keys[i].StreamID < keys[j].StreamID
	})
	for _, key := range keys {
		items := groups[key]
		sort.Slice(items, func(i, j int) bool {
			if items[i].Lane != items[j].Lane {
				return items[i].Lane < items[j].Lane
			}
			return items[i].Seq < items[j].Seq
		})
		groups[key] = items
	}
	out := make([]muxObjectMeta, 0, len(metas))
	for len(out) < len(metas) {
		for _, key := range keys {
			items := groups[key]
			if len(items) == 0 {
				continue
			}
			out = append(out, items[0])
			groups[key] = items[1:]
		}
	}
	return out
}

func orderMuxMetas(metas []muxObjectMeta) []muxObjectMeta {
	if len(metas) <= 1 {
		return metas
	}
	var priority []muxObjectMeta
	var normal []muxObjectMeta
	for _, meta := range metas {
		if meta.Priority {
			priority = append(priority, meta)
		} else {
			normal = append(normal, meta)
		}
	}
	priority = fairOrderMuxMetasByTime(priority, true)
	normal = fairOrderMuxMetasByTime(normal, false)
	out := make([]muxObjectMeta, 0, len(metas))
	out = append(out, priority...)
	out = append(out, normal...)
	return out
}

func fairOrderMuxMetasByTime(metas []muxObjectMeta, newestFirst bool) []muxObjectMeta {
	if len(metas) <= 1 {
		return metas
	}
	groups := map[muxStreamKey][]muxObjectMeta{}
	var keys []muxStreamKey
	seen := map[muxStreamKey]struct{}{}
	for _, meta := range metas {
		key := muxStreamKey{ClientID: meta.ClientID, RunID: meta.RunID, StreamID: meta.StreamID}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], meta)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ClientID != keys[j].ClientID {
			return keys[i].ClientID < keys[j].ClientID
		}
		if keys[i].RunID != keys[j].RunID {
			return keys[i].RunID < keys[j].RunID
		}
		return keys[i].StreamID < keys[j].StreamID
	})
	for _, key := range keys {
		items := groups[key]
		sort.Slice(items, func(i, j int) bool {
			if !newestFirst {
				if lessMuxObjectForStream(items[i], items[j]) {
					return true
				}
				if lessMuxObjectForStream(items[j], items[i]) {
					return false
				}
			}
			if !items[i].Updated.IsZero() && !items[j].Updated.IsZero() && !items[i].Updated.Equal(items[j].Updated) {
				if newestFirst {
					return items[i].Updated.After(items[j].Updated)
				}
				return items[i].Updated.Before(items[j].Updated)
			}
			if items[i].Lane != items[j].Lane {
				return items[i].Lane < items[j].Lane
			}
			return items[i].Seq < items[j].Seq
		})
		groups[key] = items
	}
	out := make([]muxObjectMeta, 0, len(metas))
	for len(out) < len(metas) {
		for _, key := range keys {
			items := groups[key]
			if len(items) == 0 {
				continue
			}
			out = append(out, items[0])
			groups[key] = items[1:]
		}
	}
	return out
}

func lessMuxObjectForStream(a, b muxObjectMeta) bool {
	if a.FrameRangeKnown && b.FrameRangeKnown && a.FrameMinSeq != b.FrameMinSeq {
		return a.FrameMinSeq < b.FrameMinSeq
	}
	if a.FrameRangeKnown != b.FrameRangeKnown {
		return a.FrameRangeKnown
	}
	return false
}

func (m *driveMux) receiveWorkerCounts() (int, int) {
	workers := m.t.downloadWorkerCount()
	if workers < 2 {
		return 0, 1
	}
	priority := workers / 4
	if priority < 2 {
		priority = 2
	}
	if priority > 4 {
		priority = 4
	}
	normal := workers - priority
	if normal < 1 {
		normal = 1
	}
	return priority, normal
}

func (m *driveMux) enqueueMuxObject(ctx context.Context, meta muxObjectMeta) bool {
	if meta.Name == "" || !m.claimQueued(meta.Name) {
		return false
	}
	if meta.Priority {
		select {
		case m.recvUrgent <- meta:
			return true
		case <-ctx.Done():
			m.unclaimQueued(meta.Name)
			return false
		}
	}
	if m.enqueueNormalMuxObject(ctx, meta) {
		return true
	}
	if m.isClosedStream(meta.key()) {
		m.discardQueuedMuxObject(meta)
	} else {
		m.unclaimQueued(meta.Name)
	}
	return false
}

func (m *driveMux) runReceiveWorker(ctx context.Context, priorityOnly bool) {
	urgentBudget := muxNormalStreamInflight
	for {
		meta, ok := m.nextMuxObject(ctx, priorityOnly, &urgentBudget)
		if !ok {
			return
		}
		started := time.Now()
		meta, err := m.processMuxObjectWithRetry(ctx, meta)
		if !meta.Priority {
			m.finishNormalMuxObject(ctx, meta)
		}
		if err != nil {
			if meta.Attempts >= muxProcessMaxRetries {
				m.failMuxObject(ctx, meta, err)
				continue
			}
			if m.t.Logger != nil && ctx.Err() == nil {
				m.t.Logger.Printf("mux process requeue object=%s lane=%d seq=%d priority=%t attempt=%d error=%s", muxShortName(meta.Name), meta.Lane, meta.Seq, meta.Priority, meta.Attempts+1, errorSummary(err))
			}
			m.retryMuxObject(ctx, meta)
			continue
		}
		m.markSeen(meta.Name)
		m.enqueueCleanup(cleanupTask{name: meta.Name, id: meta.ID})
		if m.t.Observe && m.t.Logger != nil {
			m.t.Logger.Printf("mux process role=%s direction=%s lane=%d seq=%d priority=%t object=%s duration=%s", m.role, directionName(m.recvDir), meta.Lane, meta.Seq, meta.Priority, muxShortName(meta.Name), time.Since(started).Round(time.Millisecond))
		}
	}
}

func (m *driveMux) enqueueNormalMuxObject(ctx context.Context, meta muxObjectMeta) bool {
	key := meta.key()
	if m.isClosedStream(key) {
		return false
	}
	stream := m.streamByKey(key)
	m.recvNormalMu.Lock()
	if m.isClosedStream(key) {
		m.recvNormalMu.Unlock()
		return false
	}
	if !m.normalReceiveCanQueueLocked(key, meta) {
		m.recvNormalMu.Unlock()
		return false
	}
	items := append(m.recvNormalFlows[key], meta)
	sort.Slice(items, func(i, j int) bool {
		if lessMuxObjectForStream(items[i], items[j]) {
			return true
		}
		if lessMuxObjectForStream(items[j], items[i]) {
			return false
		}
		if items[i].Seq != items[j].Seq {
			return items[i].Seq < items[j].Seq
		}
		if items[i].Lane != items[j].Lane {
			return items[i].Lane < items[j].Lane
		}
		return items[i].Name < items[j].Name
	})
	m.recvNormalFlows[key] = items
	m.addNormalReceiveQueuedLocked(key, meta)
	canSchedule := m.normalReceiveCanScheduleLocked(key, items, stream)
	shouldSignal := canSchedule && !m.recvNormalSent[key]
	gapMeta, gapExpected, gapFrames, gapBytes, gapRepair := muxObjectMeta{}, uint64(0), 0, 0, false
	queued := len(items)
	if shouldSignal {
		m.recvNormalSent[key] = true
	} else if !canSchedule {
		gapMeta, gapExpected, gapFrames, gapBytes, gapRepair = m.receiveGapRepairLocked(key, items, stream)
	}
	m.recvNormalMu.Unlock()
	if gapRepair {
		m.rewindReceiveListForMeta(gapMeta)
		if m.t != nil && m.t.Logger != nil {
			m.t.Logger.Printf("mux receive gap repair role=%s stream=%016x expected_frame=%d next_range=%d-%d pending_frames=%d pending_bytes=%d queued=%d list_rewind=true", m.role, key.StreamID, gapExpected, gapMeta.FrameMinSeq, gapMeta.FrameMaxSeq, gapFrames, gapBytes, queued)
		}
		m.wakeReceiver()
	}
	if !shouldSignal {
		return true
	}
	return m.signalNormalMuxObject(ctx, key)
}

func (m *driveMux) signalNormalMuxObject(ctx context.Context, key muxStreamKey) bool {
	select {
	case m.recvNormalReady <- key:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *driveMux) takeNormalMuxObject(ctx context.Context, key muxStreamKey) (muxObjectMeta, bool) {
	var shouldSignal bool
	closed := m.isClosedStream(key)
	stream := m.streamByKey(key)
	m.recvNormalMu.Lock()
	m.recvNormalSent[key] = false
	if closed {
		metas := m.removeQueuedNormalMuxObjectsLocked(key)
		delete(m.recvGapLastRepair, key)
		m.recvNormalMu.Unlock()
		m.discardQueuedMuxObjects(metas)
		return muxObjectMeta{}, false
	}
	if m.recvNormalActive[key] >= muxNormalStreamInflight {
		m.recvNormalMu.Unlock()
		return muxObjectMeta{}, false
	}
	items := m.recvNormalFlows[key]
	if len(items) == 0 {
		delete(m.recvNormalFlows, key)
		if m.recvNormalActive[key] == 0 {
			delete(m.recvNormalActive, key)
			delete(m.recvNormalActiveBytes, key)
			delete(m.recvNormalSent, key)
			delete(m.recvGapLastRepair, key)
		}
		m.recvNormalMu.Unlock()
		return muxObjectMeta{}, false
	}
	if !m.normalReceiveCanScheduleLocked(key, items, stream) {
		active := m.recvNormalActive[key]
		activeBytes := m.recvNormalActiveBytes[key]
		gapMeta, gapExpected, gapFrames, gapBytes, gapRepair := m.receiveGapRepairLocked(key, items, stream)
		m.recvNormalMu.Unlock()
		if gapRepair {
			m.rewindReceiveListForMeta(gapMeta)
			if m.t != nil && m.t.Logger != nil {
				m.t.Logger.Printf("mux receive gap repair role=%s stream=%016x expected_frame=%d next_range=%d-%d pending_frames=%d pending_bytes=%d queued=%d list_rewind=true", m.role, key.StreamID, gapExpected, gapMeta.FrameMinSeq, gapMeta.FrameMaxSeq, gapFrames, gapBytes, len(items))
			}
			m.wakeReceiver()
		}
		if m.t != nil && m.t.Observe && m.t.Logger != nil {
			frames, bytes := m.normalReceiveBacklog(key)
			inbound := 0
			if stream != nil {
				inbound = len(stream.inbound)
			}
			m.t.Logger.Printf("mux receive paused role=%s stream=%016x pending_frames=%d pending_bytes=%d inbound_q=%d active=%d active_bytes=%d", m.role, key.StreamID, frames, bytes, inbound, active, activeBytes)
		}
		return muxObjectMeta{}, false
	}
	meta := items[0]
	if len(items) == 1 {
		delete(m.recvNormalFlows, key)
	} else {
		m.recvNormalFlows[key] = items[1:]
	}
	m.removeNormalReceiveQueuedLocked(key, meta)
	if m.recvNormalActiveBytes == nil {
		m.recvNormalActiveBytes = map[muxStreamKey]int{}
	}
	m.recvNormalActive[key]++
	m.recvNormalActiveBytes[key] += meta.normalReceiveBytes()
	shouldSignal = len(m.recvNormalFlows[key]) > 0 && m.normalReceiveCanScheduleLocked(key, m.recvNormalFlows[key], stream) && !m.recvNormalSent[key]
	if shouldSignal {
		m.recvNormalSent[key] = true
	}
	active := m.recvNormalActive[key]
	activeBytes := m.recvNormalActiveBytes[key]
	queued := len(m.recvNormalFlows[key])
	m.recvNormalMu.Unlock()
	if m.t != nil && m.t.Observe && m.t.Logger != nil && (active == muxNormalStreamInflight || queued > 0) {
		m.t.Logger.Printf("mux receive window role=%s stream=%016x active=%d cap=%d active_bytes=%d byte_cap=%d queued=%d", m.role, key.StreamID, active, muxNormalStreamInflight, activeBytes, muxNormalStreamInflightBytes, queued)
	}
	if shouldSignal && !m.signalNormalMuxObject(ctx, key) {
		m.recvNormalMu.Lock()
		m.recvNormalSent[key] = false
		m.recvNormalMu.Unlock()
	}
	return meta, true
}

func (m *driveMux) finishNormalMuxObject(ctx context.Context, meta muxObjectMeta) {
	key := meta.key()
	stream := m.streamByKey(key)
	m.recvNormalMu.Lock()
	if active := m.recvNormalActive[key]; active > 1 {
		m.recvNormalActive[key] = active - 1
	} else {
		delete(m.recvNormalActive, key)
	}
	if activeBytes := m.recvNormalActiveBytes[key]; activeBytes > meta.normalReceiveBytes() {
		m.recvNormalActiveBytes[key] = activeBytes - meta.normalReceiveBytes()
	} else {
		delete(m.recvNormalActiveBytes, key)
	}
	items := m.recvNormalFlows[key]
	canSchedule := len(items) > 0 && m.normalReceiveCanScheduleLocked(key, items, stream)
	shouldSignal := canSchedule && !m.recvNormalSent[key]
	gapMeta, gapExpected, gapFrames, gapBytes, gapRepair := muxObjectMeta{}, uint64(0), 0, 0, false
	if shouldSignal {
		m.recvNormalSent[key] = true
	} else if len(items) > 0 && !canSchedule {
		gapMeta, gapExpected, gapFrames, gapBytes, gapRepair = m.receiveGapRepairLocked(key, items, stream)
	} else if len(items) == 0 && m.recvNormalActive[key] == 0 && !m.recvNormalSent[key] {
		delete(m.recvNormalFlows, key)
		delete(m.recvNormalActive, key)
		delete(m.recvNormalActiveBytes, key)
		delete(m.recvNormalSent, key)
		delete(m.recvGapLastRepair, key)
	}
	m.recvNormalMu.Unlock()
	if gapRepair {
		m.rewindReceiveListForMeta(gapMeta)
		if m.t != nil && m.t.Logger != nil {
			m.t.Logger.Printf("mux receive gap repair role=%s stream=%016x expected_frame=%d next_range=%d-%d pending_frames=%d pending_bytes=%d queued=%d list_rewind=true", m.role, key.StreamID, gapExpected, gapMeta.FrameMinSeq, gapMeta.FrameMaxSeq, gapFrames, gapBytes, len(items))
		}
		m.wakeReceiver()
	}
	if shouldSignal && !m.signalNormalMuxObject(ctx, key) {
		m.recvNormalMu.Lock()
		m.recvNormalSent[key] = false
		m.recvNormalMu.Unlock()
	}
}

func (m *driveMux) normalReceivePaused(key muxStreamKey) bool {
	reassemblyPaused, inboundPaused := normalReceivePauseStateForStream(m.streamByKey(key))
	return reassemblyPaused || inboundPaused
}

func normalReceivePauseStateForStream(stream *muxStream) (bool, bool) {
	if stream == nil {
		return false, false
	}
	frames, bytes := stream.reassemblyBacklog()
	reassemblyPaused := frames >= muxStreamPauseFrames || bytes >= muxStreamPauseBytes
	inboundPaused := stream != nil && len(stream.inbound) >= muxStreamInboundPause
	return reassemblyPaused, inboundPaused
}

func (m *driveMux) normalReceiveCanScheduleLocked(key muxStreamKey, items []muxObjectMeta, stream *muxStream) bool {
	if len(items) == 0 {
		return false
	}
	if !m.normalReceiveWindowAvailableLocked(key, items[0]) {
		return false
	}
	reassemblyPaused, inboundPaused := normalReceivePauseStateForStream(stream)
	if inboundPaused {
		return false
	}
	if !reassemblyPaused {
		return true
	}
	if m.recvNormalActive[key] > 0 {
		return false
	}
	if stream == nil || !items[0].FrameRangeKnown {
		return false
	}
	expected := stream.expectedRecvSeq()
	return items[0].FrameMinSeq <= expected && expected <= items[0].FrameMaxSeq
}

func (m *driveMux) receiveGapRepairLocked(key muxStreamKey, items []muxObjectMeta, stream *muxStream) (muxObjectMeta, uint64, int, int, bool) {
	if stream == nil || len(items) == 0 || !items[0].FrameRangeKnown {
		return muxObjectMeta{}, 0, 0, 0, false
	}
	reassemblyPaused, inboundPaused := normalReceivePauseStateForStream(stream)
	if !reassemblyPaused || inboundPaused {
		return muxObjectMeta{}, 0, 0, 0, false
	}
	expected := stream.expectedRecvSeq()
	if expected == 0 || expected >= items[0].FrameMinSeq {
		return muxObjectMeta{}, 0, 0, 0, false
	}
	if m.recvGapLastRepair == nil {
		m.recvGapLastRepair = map[muxStreamKey]time.Time{}
	}
	now := time.Now()
	if last := m.recvGapLastRepair[key]; !last.IsZero() && now.Sub(last) < muxReceiveGapRepairInterval {
		return muxObjectMeta{}, 0, 0, 0, false
	}
	m.recvGapLastRepair[key] = now
	frames, bytes := stream.reassemblyBacklog()
	return items[0], expected, frames, bytes, true
}

func (m *driveMux) normalReceiveCanQueueLocked(key muxStreamKey, meta muxObjectMeta) bool {
	bytes := meta.normalReceiveBytes()
	if bytes <= 0 {
		bytes = muxMinBatch
	}
	if m.recvNormalQueuedBytes == nil {
		m.recvNormalQueuedBytes = map[muxStreamKey]int{}
	}
	if m.recvNormalQueuedBytes[key]+bytes > muxNormalReceiveQueueBytes {
		return false
	}
	return m.recvNormalQueuedTotal+bytes <= muxNormalReceiveGlobalBytes
}

func (m *driveMux) addNormalReceiveQueuedLocked(key muxStreamKey, meta muxObjectMeta) {
	bytes := meta.normalReceiveBytes()
	if bytes <= 0 {
		bytes = muxMinBatch
	}
	if m.recvNormalQueuedBytes == nil {
		m.recvNormalQueuedBytes = map[muxStreamKey]int{}
	}
	m.recvNormalQueuedBytes[key] += bytes
	m.recvNormalQueuedTotal += bytes
}

func (m *driveMux) removeNormalReceiveQueuedLocked(key muxStreamKey, meta muxObjectMeta) {
	bytes := meta.normalReceiveBytes()
	if bytes <= 0 {
		bytes = muxMinBatch
	}
	if m.recvNormalQueuedBytes != nil {
		if remaining := m.recvNormalQueuedBytes[key] - bytes; remaining > 0 {
			m.recvNormalQueuedBytes[key] = remaining
		} else {
			delete(m.recvNormalQueuedBytes, key)
		}
	}
	m.recvNormalQueuedTotal -= bytes
	if m.recvNormalQueuedTotal < 0 {
		m.recvNormalQueuedTotal = 0
	}
}

func (m *driveMux) normalReceiveWindowAvailableLocked(key muxStreamKey, meta muxObjectMeta) bool {
	if m.recvNormalActive[key] >= muxNormalStreamInflight {
		return false
	}
	activeBytes := m.recvNormalActiveBytes[key]
	if activeBytes <= 0 {
		return true
	}
	return activeBytes+meta.normalReceiveBytes() <= muxNormalStreamInflightBytes
}

func (m *driveMux) normalReceiveBacklog(key muxStreamKey) (int, int) {
	stream := m.streamByKey(key)
	if stream == nil {
		return 0, 0
	}
	return stream.reassemblyBacklog()
}

func (m *driveMux) signalNormalMuxObjectIfReady(ctx context.Context, key muxStreamKey) {
	stream := m.streamByKey(key)
	m.recvNormalMu.Lock()
	shouldSignal := len(m.recvNormalFlows[key]) > 0 && m.normalReceiveCanScheduleLocked(key, m.recvNormalFlows[key], stream) && !m.recvNormalSent[key]
	if shouldSignal {
		m.recvNormalSent[key] = true
	}
	m.recvNormalMu.Unlock()
	if shouldSignal && !m.signalNormalMuxObject(ctx, key) {
		m.recvNormalMu.Lock()
		m.recvNormalSent[key] = false
		m.recvNormalMu.Unlock()
	}
}

func (m *driveMux) processMuxObjectWithRetry(ctx context.Context, meta muxObjectMeta) (muxObjectMeta, error) {
	if meta.Priority {
		return meta, m.processMuxObject(ctx, meta)
	}
	for {
		err := m.processMuxObject(ctx, meta)
		if err == nil || ctx.Err() != nil {
			return meta, err
		}
		meta.Attempts++
		if meta.Attempts >= muxProcessMaxRetries {
			if m.t.Logger != nil {
				m.t.Logger.Printf("mux process retry budget exhausted object=%s lane=%d seq=%d priority=%t attempts=%d error=%s", muxShortName(meta.Name), meta.Lane, meta.Seq, meta.Priority, meta.Attempts, errorSummary(err))
			}
			return meta, err
		}
		if m.t.Logger != nil {
			m.t.Logger.Printf("mux process retry object=%s lane=%d seq=%d priority=%t attempt=%d error=%s", muxShortName(meta.Name), meta.Lane, meta.Seq, meta.Priority, meta.Attempts, errorSummary(err))
		}
		delay := muxRetryDelay(meta.Attempts)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return meta, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *driveMux) retryMuxObject(ctx context.Context, meta muxObjectMeta) {
	if ctx.Err() != nil {
		return
	}
	if meta.Attempts >= muxProcessMaxRetries {
		m.failMuxObject(ctx, meta, errors.New("mux process retry budget exhausted"))
		return
	}
	meta.Attempts++
	delay := muxRetryDelay(meta.Attempts)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !m.requeueClaimedMuxObject(ctx, meta) {
			m.unclaimQueued(meta.Name)
		}
	}()
}

func (m *driveMux) failMuxObject(ctx context.Context, meta muxObjectMeta, err error) {
	if m == nil {
		return
	}
	key := meta.key()
	if m.t != nil && m.t.Logger != nil && ctx.Err() == nil {
		m.t.Logger.Printf("mux process terminal failure object=%s lane=%d seq=%d priority=%t attempts=%d stream=%016x error=%s", muxShortName(meta.Name), meta.Lane, meta.Seq, meta.Priority, meta.Attempts, meta.StreamID, errorSummary(err))
	}
	m.markSeen(meta.Name)
	m.enqueueCleanup(cleanupTask{name: meta.Name, id: meta.ID})
	m.terminalCloseStreamKey(ctx, key, "mux_process_failed", []byte("mux_process_failed"), true)
}

func muxRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(150*attempt*attempt) * time.Millisecond
	if delay > muxRetryDelayMax {
		return muxRetryDelayMax
	}
	return delay
}

func muxDriveAttemptTimeout(bytes int, priority bool, proxied bool) time.Duration {
	base := 20 * time.Second
	maximum := 30 * time.Second
	if priority {
		base = 6 * time.Second
		maximum = 10 * time.Second
	}
	if proxied {
		base *= 2
		maximum *= 2
	}
	if bytes < 0 {
		bytes = 0
	}
	mib := (bytes + (1024 * 1024) - 1) / (1024 * 1024)
	timeout := base + time.Duration(mib)*4*time.Second
	if timeout > maximum {
		return maximum
	}
	return timeout
}

func muxBatchPlainBytes(frames []muxFrame) int {
	bytes := 0
	for _, frame := range frames {
		bytes += encodedMuxFrameSize(frame)
	}
	return bytes
}

func muxBatchFrameSeqRange(frames []muxFrame) (uint64, uint64) {
	var minSeq uint64
	var maxSeq uint64
	for _, frame := range frames {
		if frame.Seq == 0 {
			continue
		}
		if minSeq == 0 || frame.Seq < minSeq {
			minSeq = frame.Seq
		}
		if frame.Seq > maxSeq {
			maxSeq = frame.Seq
		}
	}
	return minSeq, maxSeq
}

func (m *driveMux) requeueClaimedMuxObject(ctx context.Context, meta muxObjectMeta) bool {
	if meta.Priority {
		select {
		case m.recvUrgent <- meta:
			return true
		case <-ctx.Done():
			return false
		}
	}
	return m.enqueueNormalMuxObject(ctx, meta)
}

func (m *driveMux) nextMuxObject(ctx context.Context, priorityOnly bool, urgentBudget *int) (muxObjectMeta, bool) {
	if urgentBudget == nil {
		value := muxNormalStreamInflight
		urgentBudget = &value
	}
	for {
		if !priorityOnly && *urgentBudget <= 0 {
			select {
			case key := <-m.recvNormalReady:
				if meta, ok := m.takeNormalMuxObject(ctx, key); ok {
					*urgentBudget = muxNormalStreamInflight
					return meta, true
				}
			default:
			}
			*urgentBudget = muxNormalStreamInflight
		}

		select {
		case meta := <-m.recvUrgent:
			*urgentBudget = *urgentBudget - 1
			return meta, true
		default:
		}

		if priorityOnly {
			select {
			case <-ctx.Done():
				return muxObjectMeta{}, false
			case meta := <-m.recvUrgent:
				*urgentBudget = *urgentBudget - 1
				return meta, true
			}
		}

		select {
		case <-ctx.Done():
			return muxObjectMeta{}, false
		case meta := <-m.recvUrgent:
			*urgentBudget = *urgentBudget - 1
			return meta, true
		case key := <-m.recvNormalReady:
			if meta, ok := m.takeNormalMuxObject(ctx, key); ok {
				*urgentBudget = muxNormalStreamInflight
				return meta, true
			}
		}
	}
}

func (m *driveMux) enqueueCleanup(task cleanupTask) {
	if m == nil || m.t == nil || !m.t.CleanupProcessed || (task.name == "" && task.id == "") {
		return
	}
	select {
	case m.cleanupQueue <- task:
	default:
		if m.t.Logger != nil {
			m.t.Logger.Printf("mux cleanup queue full object=%s", muxShortName(task.name))
		}
	}
}

func (m *driveMux) runCleanupLoop(ctx context.Context) {
	if m == nil || m.t == nil || !m.t.CleanupProcessed {
		return
	}
	ticker := time.NewTicker(deferredCleanupDelay)
	defer ticker.Stop()
	var tasks []cleanupTask
	flush := func(force bool) {
		if len(tasks) == 0 {
			return
		}
		if !force && m.t.foregroundBusy() && len(tasks) < deferredCleanupFlushThreshold {
			return
		}
		cleanup := m.t.newDeferredCleanup()
		for _, task := range tasks {
			cleanup.Data(task.name, task.id)
		}
		tasks = tasks[:0]
		cleanup.flushAsyncAfter(0)
	}
	for {
		select {
		case <-ctx.Done():
			flush(true)
			return
		case task := <-m.cleanupQueue:
			tasks = append(tasks, task)
			if len(tasks) >= deferredCleanupFlushThreshold {
				flush(false)
			}
		case <-ticker.C:
			flush(false)
		}
	}
}

func (m *driveMux) processMuxObject(ctx context.Context, meta muxObjectMeta) error {
	if meta.Lane < 0 || meta.Lane >= muxLaneCount {
		return fmt.Errorf("invalid mux lane %d", meta.Lane)
	}
	key, err := DeriveMuxLaneKeyV4(m.t.Secret, m.t.SessionID, m.recvDir, meta.ClientID, meta.RunID, meta.Lane)
	if err != nil {
		return err
	}
	sealed, err := m.downloadMuxObject(ctx, meta)
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
		frame.ClientID = meta.ClientID
		frame.RunID = meta.RunID
		m.handleFrame(ctx, frame)
	}
	m.t.markActivity()
	return nil
}

func (m *driveMux) downloadMuxObject(ctx context.Context, meta muxObjectMeta) ([]byte, error) {
	if !meta.Priority {
		return m.downloadMuxObjectOnce(ctx, meta)
	}
	hedgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var won atomic.Bool
	type result struct {
		sealed []byte
		err    error
	}
	results := make(chan result, 2)
	startAttempt := func() {
		go func() {
			sealed, err := m.downloadMuxObjectAttempt(hedgeCtx, meta, &won)
			results <- result{sealed: sealed, err: err}
		}()
	}
	attempts := 1
	completed := 0
	var firstErr error
	startAttempt()
	timer := time.NewTimer(muxPriorityDownloadHedge)
	defer timer.Stop()
	for completed < attempts {
		select {
		case res := <-results:
			completed++
			if res.err == nil {
				won.Store(true)
				cancel()
				return res.sealed, nil
			}
			if firstErr == nil {
				firstErr = res.err
			}
			if attempts == 1 {
				attempts++
				startAttempt()
			}
		case <-timer.C:
			if attempts == 1 {
				attempts++
				startAttempt()
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if firstErr == nil {
		firstErr = errors.New("priority mux download failed")
	}
	return nil, firstErr
}

func (m *driveMux) downloadMuxObjectOnce(ctx context.Context, meta muxObjectMeta) ([]byte, error) {
	return m.downloadMuxObjectAttempt(ctx, meta, nil)
}

func (m *driveMux) downloadMuxObjectAttempt(ctx context.Context, meta muxObjectMeta, hedgeWon *atomic.Bool) ([]byte, error) {
	release, err := m.t.acquireDownloadSlotBytes(ctx, meta.Priority)
	if err != nil {
		return nil, err
	}
	opCtx, cancel := context.WithTimeout(ctx, muxDriveAttemptTimeout(meta.normalReceiveBytes(), meta.Priority, m.t.RouteProxy != ""))
	var sealed []byte
	if meta.ID != "" {
		if store, ok := m.t.Data.(ObjectIDStore); ok {
			sealed, err = store.GetByID(opCtx, meta.ID)
		} else {
			sealed, err = m.t.Data.Get(opCtx, meta.Name)
		}
	} else {
		sealed, err = m.t.Data.Get(opCtx, meta.Name)
	}
	cancel()
	releaseErr := err
	if hedgeWon != nil && hedgeWon.Load() && errors.Is(err, context.Canceled) {
		releaseErr = nil
	}
	release(releaseErr, int64(len(sealed)))
	return sealed, err
}

func (m *driveMux) handleFrame(ctx context.Context, frame muxFrame) {
	switch frame.Kind {
	case muxFrameOpen:
		if m.role == "exit" {
			m.openExitStream(ctx, frame)
		}
	case muxFrameData:
		stream := m.stream(frame)
		if stream == nil {
			m.queuePendingFrame(ctx, frame)
			return
		}
		stream.acceptFrame(ctx, frame)
	case muxFrameFIN:
		if stream := m.stream(frame); stream != nil {
			stream.acceptFrame(ctx, frame)
		} else {
			m.queuePendingFrame(ctx, frame)
		}
	case muxFrameRST:
		if stream := m.stream(frame); stream != nil {
			stream.close()
		} else {
			key := frame.key()
			m.terminalCloseStreamKey(ctx, key, "remote_rst_before_open", nil, false)
		}
	}
}

func (m *driveMux) queuePendingFrame(ctx context.Context, frame muxFrame) {
	if frame.Seq == 0 {
		return
	}
	key := frame.key()
	if m.isClosedStream(key) {
		return
	}
	bytes := pendingFrameBytes(frame)
	var overflow bool
	var overflowReason string
	var overflowKeys []muxStreamKey
	var pendingFrames int
	var pendingBytes int
	var globalBytes int
	m.pendingMu.Lock()
	if m.pending == nil {
		m.pending = map[muxStreamKey][]muxFrame{}
	}
	if m.pendingBytes == nil {
		m.pendingBytes = map[muxStreamKey]int{}
	}
	frames := m.pending[key]
	pendingFrames = len(frames)
	pendingBytes = m.pendingBytes[key]
	switch {
	case pendingFrames >= muxPendingFrameLimit || pendingBytes+bytes > muxPendingStreamBytes:
		overflow = true
		overflowReason = "stream"
		m.dropPendingFramesLocked(key)
		overflowKeys = []muxStreamKey{key}
	case m.pendingTotalBytes+bytes > muxPendingGlobalBytes:
		overflow = true
		overflowReason = "global"
		overflowKeys = m.dropAllPendingFramesLocked()
		if !muxStreamKeyIn(overflowKeys, key) {
			overflowKeys = append(overflowKeys, key)
		}
	default:
		m.pending[key] = append(frames, frame)
		m.pendingBytes[key] = pendingBytes + bytes
		m.pendingTotalBytes += bytes
	}
	globalBytes = m.pendingTotalBytes
	m.pendingMu.Unlock()
	if !overflow {
		return
	}
	if m.t != nil && m.t.Logger != nil {
		m.t.Logger.Printf("mux pending overflow role=%s stream=%016x reason=%s pending_frames=%d pending_bytes=%d frame_bytes=%d global_bytes=%d closed_streams=%d", m.role, key.StreamID, overflowReason, pendingFrames, pendingBytes, bytes, globalBytes, len(overflowKeys))
	}
	for _, closeKey := range overflowKeys {
		m.terminalCloseStreamKey(ctx, closeKey, "mux_pending_overflow", []byte("mux_pending_overflow"), true)
	}
}

func (m *driveMux) dropPendingFrames(key muxStreamKey) {
	m.pendingMu.Lock()
	m.dropPendingFramesLocked(key)
	m.pendingMu.Unlock()
}

func (m *driveMux) dropPendingFramesLocked(key muxStreamKey) []muxFrame {
	frames := append([]muxFrame(nil), m.pending[key]...)
	if m.pending != nil {
		delete(m.pending, key)
	}
	if m.pendingBytes != nil {
		delete(m.pendingBytes, key)
	}
	for _, frame := range frames {
		m.pendingTotalBytes -= pendingFrameBytes(frame)
	}
	if m.pendingTotalBytes < 0 {
		m.pendingTotalBytes = 0
	}
	return frames
}

func (m *driveMux) dropAllPendingFramesLocked() []muxStreamKey {
	keysSeen := map[muxStreamKey]struct{}{}
	keys := make([]muxStreamKey, 0, len(m.pending))
	for key := range m.pending {
		if _, ok := keysSeen[key]; ok {
			continue
		}
		keysSeen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range m.pendingBytes {
		if _, ok := keysSeen[key]; ok {
			continue
		}
		keysSeen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ClientID != keys[j].ClientID {
			return keys[i].ClientID < keys[j].ClientID
		}
		if keys[i].RunID != keys[j].RunID {
			return keys[i].RunID < keys[j].RunID
		}
		return keys[i].StreamID < keys[j].StreamID
	})
	m.pending = map[muxStreamKey][]muxFrame{}
	m.pendingBytes = map[muxStreamKey]int{}
	m.pendingTotalBytes = 0
	return keys
}

func muxStreamKeyIn(keys []muxStreamKey, key muxStreamKey) bool {
	for _, existing := range keys {
		if existing == key {
			return true
		}
	}
	return false
}

func (m *driveMux) flushPendingFrames(ctx context.Context, stream *muxStream) {
	m.pendingMu.Lock()
	frames := m.dropPendingFramesLocked(stream.key())
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

func pendingFrameBytes(frame muxFrame) int {
	bytes := encodedMuxFrameSize(frame)
	if bytes <= 0 {
		return muxFrameHeaderSize
	}
	return bytes
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
	select {
	case <-s.done:
		return
	default:
	}

	s.deliverMu.Lock()
	defer s.deliverMu.Unlock()

	var ready []muxFrame
	closeStream := false
	resumeNormal := false
	pendingFrames := 0
	pendingBytes := 0
	s.mu.Lock()
	if frame.Seq < s.recvExpected {
		s.mu.Unlock()
		return
	}
	if _, exists := s.recvPending[frame.Seq]; !exists {
		nextPendingBytes := s.recvPendingBytes + len(frame.Payload)
		if len(s.recvPending) >= muxStreamPendingFrames || nextPendingBytes > muxStreamPendingBytes {
			closeStream = true
		} else {
			s.recvPending[frame.Seq] = frame
			s.recvPendingBytes = nextPendingBytes
		}
	}
	if !closeStream {
		for {
			next, ok := s.recvPending[s.recvExpected]
			if !ok {
				break
			}
			delete(s.recvPending, s.recvExpected)
			s.recvPendingBytes -= len(next.Payload)
			if s.recvPendingBytes < 0 {
				s.recvPendingBytes = 0
			}
			ready = append(ready, next)
			s.recvExpected++
			if next.Kind == muxFrameFIN {
				break
			}
		}
	}
	pendingFrames = len(s.recvPending)
	pendingBytes = s.recvPendingBytes
	resumeNormal = pendingFrames < muxStreamPauseFrames && pendingBytes < muxStreamPauseBytes
	s.mu.Unlock()
	if closeStream {
		if s.mux != nil && s.mux.t != nil && s.mux.t.Logger != nil {
			s.mux.t.Logger.Printf("mux stream reassembly overflow role=%s stream=%016x pending_frames=%d pending_bytes=%d", s.mux.role, s.id, pendingFrames, pendingBytes)
		}
		s.close()
		return
	}

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
	if resumeNormal && s.mux != nil {
		s.mux.signalNormalMuxObjectIfReady(ctx, s.key())
	}
}

func (s *muxStream) reassemblyBacklog() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recvPending), s.recvPendingBytes
}

func (s *muxStream) expectedRecvSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvExpected
}

func (m *driveMux) isKnown(name string) bool {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	if _, ok := m.seen[name]; ok {
		return true
	}
	_, ok := m.queued[name]
	return ok
}

func (m *driveMux) claimQueued(name string) bool {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	if _, ok := m.seen[name]; ok {
		return false
	}
	if _, ok := m.queued[name]; ok {
		return false
	}
	m.queued[name] = struct{}{}
	return true
}

func (m *driveMux) unclaimQueued(name string) {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	delete(m.queued, name)
}

func (m *driveMux) markSeen(name string) {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	delete(m.queued, name)
	m.seen[name] = struct{}{}
	if len(m.seen) > 200000 {
		m.seen = map[string]struct{}{name: struct{}{}}
	}
}

func isDrivePageTokenRejected(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return (strings.Contains(text, "pagetoken") || strings.Contains(text, "page token")) &&
		(strings.Contains(text, "invalid") || strings.Contains(text, "rejected") || strings.Contains(text, "expired"))
}

func (m *driveMux) readBufferSize() int {
	size := m.t.ChunkSize / 4
	if size < 32*1024 {
		size = 32 * 1024
	}
	maxPayload := m.normalBatchBytes() - muxBatchHeaderSize - muxFrameHeaderSize
	if maxPayload < 32*1024 {
		maxPayload = 32 * 1024
	}
	if size > maxPayload {
		size = maxPayload
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

func (m *driveMux) normalBatchBytes() int {
	size := m.maxBatchBytes()
	if size > muxNormalBulkBatch {
		return muxNormalBulkBatch
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

func muxDirPrefix(sid [16]byte, direction byte, clientID, runID string) string {
	base := fmt.Sprintf("muxv4/%s/%s/", SessionString(sid), directionName(direction))
	clientID = strings.TrimSpace(clientID)
	runID = strings.TrimSpace(runID)
	if clientID == "" {
		return base
	}
	if runID == "" {
		return fmt.Sprintf("%s%s/", base, clientID)
	}
	return fmt.Sprintf("%s%s/%s/", base, clientID, runID)
}

func muxObjectName(sid [16]byte, direction byte, clientID, runID, epoch string, streamID uint64, lane int, seq uint64, frames int, frameMinSeq, frameMaxSeq uint64, bytes int, priority bool) string {
	epoch = strings.TrimSpace(epoch)
	if epoch == "" {
		epoch = "0000000000000000"
	}
	class := "p1"
	if priority {
		class = "p0"
	}
	return fmt.Sprintf("%s%s/%s/s%016x/l%02d/%016x.f%d.r%016x-%016x.b%d", muxDirPrefix(sid, direction, clientID, runID), epoch, class, streamID, lane, seq, frames, frameMinSeq, frameMaxSeq, bytes)
}

func parseMuxObjectInfo(info ObjectInfo) (muxObjectMeta, bool) {
	name := info.Name
	parts := strings.Split(name, "/")
	if len(parts) < 9 || parts[0] != "muxv4" || !strings.HasPrefix(parts[len(parts)-2], "l") {
		return muxObjectMeta{}, false
	}
	classIdx := len(parts) - 3
	var streamID uint64
	if strings.HasPrefix(parts[classIdx], "s") {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(parts[classIdx], "s"), 16, 64)
		if err != nil {
			return muxObjectMeta{}, false
		}
		streamID = parsed
		classIdx--
	}
	if classIdx < 0 {
		return muxObjectMeta{}, false
	}
	class := parts[classIdx]
	if class != "p0" && class != "p1" {
		return muxObjectMeta{}, false
	}
	clientID := parts[3]
	runID := parts[4]
	epoch := parts[5]
	if clientID == "" || runID == "" {
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
	plainBytes := 0
	var frameMinSeq uint64
	var frameMaxSeq uint64
	frameRangeKnown := false
	for _, segment := range strings.Split(base[dot+1:], ".") {
		switch {
		case strings.HasPrefix(segment, "b"):
			parsed, err := strconv.Atoi(strings.TrimPrefix(segment, "b"))
			if err == nil && parsed > 0 {
				plainBytes = parsed
			}
		case strings.HasPrefix(segment, "r"):
			bounds := strings.SplitN(strings.TrimPrefix(segment, "r"), "-", 2)
			if len(bounds) != 2 {
				continue
			}
			minSeq, minErr := strconv.ParseUint(bounds[0], 16, 64)
			maxSeq, maxErr := strconv.ParseUint(bounds[1], 16, 64)
			if minErr == nil && maxErr == nil && minSeq > 0 && maxSeq >= minSeq {
				frameMinSeq = minSeq
				frameMaxSeq = maxSeq
				frameRangeKnown = true
			}
		}
	}
	if plainBytes <= 0 && info.Size > 0 && info.Size <= int64(int(^uint(0)>>1)) {
		plainBytes = int(info.Size)
	}
	var updated time.Time
	if strings.TrimSpace(info.Updated) != "" {
		updated, _ = time.Parse(time.RFC3339Nano, info.Updated)
	}
	return muxObjectMeta{Name: name, ID: info.ID, ClientID: clientID, RunID: runID, Epoch: epoch, StreamID: streamID, Lane: lane, Seq: seq, Priority: class == "p0", PlainBytes: plainBytes, FrameMinSeq: frameMinSeq, FrameMaxSeq: frameMaxSeq, FrameRangeKnown: frameRangeKnown, Updated: updated}, true
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
