package skirk

// This adapter exists only in the research test binary. It never changes the
// Drive store or the product's routing and credential behavior.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type relayResearchUpload struct {
	info     ObjectInfo
	hash     [32]byte
	data     []byte
	offset   int
	priority bool
	done     chan struct{}
	finished bool
	err      error
}

type relayResearchDownload struct {
	info            ObjectInfo
	hash            [32]byte
	data            []byte
	next            int
	priority        bool
	ready, queued   bool
	notifying, done bool
	deleting        bool
}

type relayResearchDeletion struct {
	id       string
	done     chan struct{}
	finished bool
	err      error
}

type relayResearchClient struct {
	cfg                                             relayResearchConfig
	codec                                           *relayResearchCodec
	exchange                                        relayResearchExchange
	ctx                                             context.Context
	cancel                                          context.CancelFunc
	wg                                              sync.WaitGroup
	wake                                            chan struct{}
	changed                                         chan struct{}
	mu                                              sync.Mutex
	err                                             error
	up                                              map[string]*relayResearchUpload
	upNames                                         map[string]string
	upQueue                                         []*relayResearchUpload
	upBytes                                         int64
	normalUpBytes                                   int64
	normalUpObjects                                 int
	down                                            map[string]*relayResearchDownload
	downNames                                       map[string]string
	downBytes                                       int64
	normalDownBytes                                 int64
	normalDownObjects                               int
	deletes                                         map[string]*relayResearchDeletion
	validate                                        func(ObjectInfo, []byte) error
	notify                                          func(ObjectInfo) bool
	requests, retries, published, received, deleted uint64
	upWaits, downWaits                              uint64
	packedUp, packedDown                            uint64
}

func newRelayResearchClient(ctx context.Context, key []byte, epoch [16]byte, cfg relayResearchConfig, exchange relayResearchExchange) (*relayResearchClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	if cfg, err = cfg.normalized(); err != nil || exchange == nil {
		return nil, errRelayResearchBounds
	}
	codec, err := newRelayResearchCodec(key, epoch, cfg.FrameBytes)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &relayResearchClient{cfg: cfg, codec: codec, exchange: exchange, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), changed: make(chan struct{}), up: map[string]*relayResearchUpload{}, upNames: map[string]string{},
		down: map[string]*relayResearchDownload{}, downNames: map[string]string{}, deletes: map[string]*relayResearchDeletion{}}
	s.wg.Add(2)
	go s.upPump()
	go s.pullPump()
	return s, nil
}

func relayResearchID(name string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *relayResearchClient) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Called under mu; pending admission waiters do not compete with the UP pump.
func (s *relayResearchClient) changedLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *relayResearchClient) setDownHooks(validate func(ObjectInfo, []byte) error, notify func(ObjectInfo) bool) {
	s.mu.Lock()
	s.validate, s.notify = validate, notify
	s.mu.Unlock()
}

func (s *relayResearchClient) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		for _, v := range s.up {
			if !v.finished {
				v.err, v.finished = err, true
				close(v.done)
			}
		}
		for _, v := range s.deletes {
			if !v.finished {
				v.err, v.finished = err, true
				close(v.done)
			}
		}
		s.changedLocked()
	}
	s.mu.Unlock()
	s.cancel()
	s.signal()
}

func (s *relayResearchClient) Close() {
	s.fail(errRelayResearchClosed)
	s.wg.Wait()
	s.mu.Lock()
	for _, v := range s.up {
		v.data = nil
	}
	for _, v := range s.down {
		v.data = nil
	}
	s.upBytes, s.downBytes, s.normalUpBytes, s.normalDownBytes = 0, 0, 0, 0
	s.mu.Unlock()
}
func (s *relayResearchClient) error() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

func (s *relayResearchClient) snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int64{"up_objects": int64(len(s.up)), "down_objects": int64(len(s.down)),
		"up_bytes": s.upBytes, "down_bytes": s.downBytes, "requests": int64(s.requests),
		"retries": int64(s.retries), "published": int64(s.published), "received": int64(s.received), "deleted": int64(s.deleted), "up_waits": int64(s.upWaits), "down_waits": int64(s.downWaits),
		"normal_up_bytes": s.normalUpBytes, "normal_down_bytes": s.normalDownBytes, "normal_up_objects": int64(s.normalUpObjects), "normal_down_objects": int64(s.normalDownObjects), "packed_up_rpcs": int64(s.packedUp), "packed_down_rpcs": int64(s.packedDown)}
}

func (s *relayResearchClient) Put(ctx context.Context, name string, data []byte) error {
	_, err := s.PutObject(ctx, name, data)
	return err
}

func (s *relayResearchClient) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if ctx.Err() != nil {
		return ObjectInfo{}, ctx.Err()
	}
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || len(data) == 0 || len(data) > rrMaxObject || int64(len(data)) > s.cfg.MaxBytes {
		return ObjectInfo{}, errRelayResearchBounds
	}
	id, hash := relayResearchID(name, data), sha256.Sum256(data)
	priority := strings.Contains(name, "/p0/")
	if !priority && int64(len(data)) > s.cfg.MaxBytes-s.cfg.PriorityReserveBytes {
		return ObjectInfo{}, errRelayResearchBounds
	}
	var v *relayResearchUpload
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return ObjectInfo{}, err
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return ObjectInfo{}, err
		}
		if old, ok := s.upNames[name]; ok && old != id {
			s.mu.Unlock()
			return ObjectInfo{}, errRelayResearchConflict
		}
		v = s.up[id]
		if v != nil {
			if v.hash != hash || v.info.Name != name {
				s.mu.Unlock()
				return ObjectInfo{}, errRelayResearchConflict
			}
			s.mu.Unlock()
			break
		}
		// Immutable claims remain until Close, including after upload data is freed.
		if len(s.up) >= s.cfg.MaxObjects || !priority && s.normalUpObjects >= s.cfg.MaxObjects-s.cfg.PriorityReserveObjects {
			s.mu.Unlock()
			return ObjectInfo{}, errRelayResearchBounds
		}
		if s.upBytes+int64(len(data)) > s.cfg.MaxBytes || !priority && s.normalUpBytes+int64(len(data)) > s.cfg.MaxBytes-s.cfg.PriorityReserveBytes {
			s.upWaits++
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ObjectInfo{}, ctx.Err()
			case <-s.ctx.Done():
				return ObjectInfo{}, s.ctx.Err()
			case <-changed:
			}
			continue
		}
		v = &relayResearchUpload{info: ObjectInfo{Name: name, ID: id, Size: int64(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)},
			hash: hash, data: bytes.Clone(data), priority: priority, done: make(chan struct{})}
		s.up[id], s.upNames[name] = v, id
		s.upQueue = append(s.upQueue, v)
		s.upBytes += int64(len(data))
		if !priority {
			s.normalUpBytes += int64(len(data))
			s.normalUpObjects++
		}
		s.mu.Unlock()
		break
	}
	s.signal()
	// Caller cancellation does not forget an ambiguous publication. The one
	// session-owned pump completes its prepared identity or stops with Close.
	select {
	case <-ctx.Done():
		return ObjectInfo{}, ctx.Err()
	case <-v.done:
		s.mu.Lock()
		info, err := v.info, v.err
		s.mu.Unlock()
		return info, err
	}
}

func (s *relayResearchClient) Get(ctx context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	id := s.downNames[name]
	s.mu.Unlock()
	return s.GetByID(ctx, id)
}
func (s *relayResearchClient) GetByID(ctx context.Context, id string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	v := s.down[id]
	if v == nil || !v.ready || v.done {
		return nil, errors.New("relay object unavailable")
	}
	return bytes.Clone(v.data), nil
}
func (s *relayResearchClient) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var infos []ObjectInfo
	for _, v := range s.down {
		if v.ready && !v.done && !v.deleting && strings.HasPrefix(v.info.Name, prefix) {
			infos = append(infos, v.info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}
func (s *relayResearchClient) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	id := s.downNames[name]
	s.mu.Unlock()
	return s.DeleteID(ctx, id)
}
func (s *relayResearchClient) DeleteID(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	v := s.down[id]
	if v == nil || !v.ready {
		s.mu.Unlock()
		return errors.New("relay delete identity unavailable")
	}
	if v.done {
		s.mu.Unlock()
		return nil
	}
	d := s.deletes[id]
	if d == nil {
		d = &relayResearchDeletion{id: id, done: make(chan struct{})}
		s.deletes[id], v.deleting = d, true
	}
	s.mu.Unlock()
	s.signal()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
		s.mu.Lock()
		err := d.err
		s.mu.Unlock()
		return err
	}
}

// A channel owns its sequence until an authenticated matching reply is fully
// consumed. Every ambiguous transport retry reuses the identical sealed bytes.
func (s *relayResearchClient) call(packet relayResearchPacket) (relayResearchPacket, error) {
	wire, err := s.codec.seal(packet)
	if err != nil {
		return relayResearchPacket{}, err
	}
	for attempt := 0; ; attempt++ {
		if s.ctx.Err() != nil {
			return relayResearchPacket{}, s.ctx.Err()
		}
		s.mu.Lock()
		s.requests++
		if attempt > 0 {
			s.retries++
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.PollWait+15*time.Second)
		response, callErr := s.exchange(ctx, wire)
		cancel()
		if callErr == nil {
			p, e := s.codec.open(response)
			if e != nil {
				return p, e
			}
			if p.Kind != packet.Kind|rrResponse || p.Seq != packet.Seq || p.Receipt != 0 || len(p.Deletes) != 0 {
				return p, errRelayResearchSequence
			}
			switch p.Status {
			case rrStatusOK:
				return p, nil
			case rrStatusBusy:
				return p, nil
			case rrStatusBounds:
				return p, errRelayResearchBounds
			case rrStatusConflict:
				return p, errRelayResearchConflict
			default:
				return p, errRelayResearchClosed
			}
		}
		if errors.Is(callErr, errRelayResearchSequence) || errors.Is(callErr, errRelayResearchConflict) || errors.Is(callErr, errRelayResearchBounds) || errors.Is(callErr, errRelayResearchClosed) {
			return relayResearchPacket{}, callErr
		}
		delay := time.Duration(attempt+1) * 20 * time.Millisecond
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		var unavailable *relayResearchRetryAfter
		if errors.As(callErr, &unavailable) && unavailable.delay > delay {
			delay = unavailable.delay
		}
		select {
		case <-s.ctx.Done():
			return relayResearchPacket{}, s.ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (s *relayResearchClient) upPump() {
	defer s.wg.Done()
	var seq, receipt uint64 = 1, 0
	priorityBurst := 0
	for {
		p := relayResearchPacket{Kind: rrUp, Seq: seq, Receipt: receipt}
		s.mu.Lock()
		for id, d := range s.deletes {
			if !d.finished && len(p.Deletes) < 64 {
				p.Deletes = append(p.Deletes, id)
			}
		}
		sort.Strings(p.Deletes)
		selected := s.selectUpLocked(&priorityBurst, len(p.Deletes) == 0)
		for _, v := range selected {
			n := len(v.data) - v.offset
			if n > s.cfg.FrameBytes {
				n = s.cfg.FrameBytes
			}
			p.Pieces = append(p.Pieces, relayResearchPiece{ID: v.info.ID, Name: v.info.Name, Total: len(v.data), Offset: v.offset, Hash: v.hash,
				Priority: v.priority, Data: v.data[v.offset : v.offset+n]})
		}
		s.mu.Unlock()
		if len(selected) == 0 && len(p.Deletes) == 0 {
			select {
			case <-s.ctx.Done():
				s.fail(s.ctx.Err())
				return
			case <-s.wake:
			}
			continue
		}
		r, err := s.call(p)
		if err == nil && r.Status == rrStatusBusy {
			if len(p.Deletes) != 0 || len(r.Pieces)+len(r.Committed)+len(r.Deleted) != 0 {
				s.fail(errRelayResearchConflict)
				return
			}
			receipt, seq = seq, seq+1
			s.mu.Lock()
			for _, v := range selected {
				s.rotateUpLocked(v, true)
			}
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				s.fail(s.ctx.Err())
				return
			case <-time.After(50 * time.Millisecond):
			case <-s.wake:
			}
			continue
		}
		if err == nil {
			err = s.acceptUpReply(p, r, selected)
		}
		if err != nil {
			s.fail(err)
			return
		}
		receipt, seq = seq, seq+1
	}
}

func (s *relayResearchClient) selectUpLocked(priorityBurst *int, allowPayload bool) []*relayResearchUpload {
	var pri, normal *relayResearchUpload
	for _, v := range s.upQueue {
		if v.finished {
			continue
		}
		if v.priority && pri == nil {
			pri = v
		}
		if !v.priority && normal == nil {
			normal = v
		}
	}
	if !s.cfg.PackPriority {
		// Retain the scalar baseline's burst accounting, including its idle
		// and delete-only iterations, for the controlled profile comparison.
		v := pri
		if v == nil || *priorityBurst >= 4 && normal != nil {
			v = normal
			*priorityBurst = 0
		} else {
			*priorityBurst++
		}
		if v == nil || !allowPayload {
			return nil
		}
		return []*relayResearchUpload{v}
	}
	if !allowPayload {
		return nil
	}
	if pri == nil || *priorityBurst >= 4 && normal != nil {
		if normal != nil {
			*priorityBurst = 0
			return []*relayResearchUpload{normal}
		}
		return nil
	}
	selected := []*relayResearchUpload{pri}
	limit := rrPriorityPackLimit(s.cfg)
	if s.cfg.PackPriority && pri.offset == 0 && len(pri.data) <= limit {
		maxPieces := rrPriorityPackMaxObjects
		if normal != nil {
			maxPieces = min(maxPieces, 4-*priorityBurst)
		}
		total := len(pri.data)
		for _, v := range s.upQueue {
			if v == pri || v.finished || !v.priority || v.offset != 0 {
				continue
			}
			if len(selected) >= maxPieces {
				break
			}
			if total+len(v.data) > limit {
				continue
			}
			selected = append(selected, v)
			total += len(v.data)
		}
	}
	*priorityBurst += len(selected)
	return selected
}

func (s *relayResearchClient) rotateUpLocked(v *relayResearchUpload, keep bool) {
	for i, queued := range s.upQueue {
		if queued == v {
			s.upQueue = append(s.upQueue[:i], s.upQueue[i+1:]...)
			if keep {
				s.upQueue = append(s.upQueue, v)
			}
			return
		}
	}
}

func (s *relayResearchClient) acceptUpReply(p, r relayResearchPacket, selected []*relayResearchUpload) error {
	if len(r.Pieces) != 0 {
		return errRelayResearchConflict
	}
	commits, deleted := map[string]bool{}, map[string]bool{}
	for _, id := range r.Committed {
		if commits[id] {
			return errRelayResearchConflict
		}
		commits[id] = true
	}
	for _, id := range r.Deleted {
		if deleted[id] {
			return errRelayResearchConflict
		}
		deleted[id] = true
	}
	if len(deleted) != len(p.Deletes) {
		return errRelayResearchConflict
	}
	for _, id := range p.Deletes {
		if !deleted[id] {
			return errRelayResearchConflict
		}
	}
	if len(selected) != len(p.Pieces) {
		return errRelayResearchConflict
	}
	expected := map[string]bool{}
	for i, v := range selected {
		piece := p.Pieces[i]
		if piece.ID != v.info.ID {
			return errRelayResearchConflict
		}
		if piece.Offset+len(piece.Data) == piece.Total {
			expected[v.info.ID] = true
		}
	}
	if len(commits) != len(expected) {
		return errRelayResearchConflict
	}
	for id := range expected {
		if !commits[id] {
			return errRelayResearchConflict
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range p.Deletes {
		d, down := s.deletes[id], s.down[id]
		if d == nil || down == nil {
			return errRelayResearchConflict
		}
		if !d.finished {
			d.finished = true
			down.done = true
			s.downBytes -= int64(len(down.data))
			if !down.priority {
				s.normalDownBytes -= int64(len(down.data))
			}
			down.data = nil
			s.deleted++
			close(d.done)
		}
	}
	for i, v := range selected {
		if v.finished {
			continue
		}
		v.offset += len(p.Pieces[i].Data)
		// Rotate after every fragment so an older large object cannot monopolize UP.
		s.rotateUpLocked(v, false)
		if commits[v.info.ID] {
			v.finished = true
			s.upBytes -= int64(len(v.data))
			if !v.priority {
				s.normalUpBytes -= int64(len(v.data))
			}
			v.data = nil
			s.published++
			close(v.done)
		} else {
			s.upQueue = append(s.upQueue, v)
		}
	}
	if len(selected) > 1 {
		s.packedUp++
	}
	s.changedLocked()
	return nil
}

func (s *relayResearchClient) pullPump() {
	defer s.wg.Done()
	var seq, receipt uint64 = 1, 0
	for {
		r, err := s.call(relayResearchPacket{Kind: rrPull, Seq: seq, Receipt: receipt})
		if err == nil && r.Status != rrStatusOK {
			err = errRelayResearchConflict
		}
		if err == nil && (len(r.Committed) != 0 || len(r.Deleted) != 0) {
			err = errRelayResearchConflict
		}
		if err == nil && len(r.Pieces) > 1 {
			err = s.preflightDownGroup(r.Pieces)
		}
		for _, p := range r.Pieces {
			if err == nil {
				err = s.acceptDownPiecePrepared(p, len(r.Pieces) > 1)
			}
		}
		if err != nil {
			s.fail(err)
			return
		}
		if len(r.Pieces) > 1 {
			s.mu.Lock()
			s.packedDown++
			s.mu.Unlock()
		}
		s.announce()
		receipt, seq = seq, seq+1
		if len(r.Pieces) == 0 {
			select {
			case <-s.ctx.Done():
				s.fail(s.ctx.Err())
				return
			case <-time.After(time.Millisecond):
			}
		}
	}
}

func (s *relayResearchClient) preflightDownGroup(pieces []relayResearchPiece) error {
	if !s.cfg.PackPriority || len(pieces) > rrPriorityPackMaxObjects {
		return errRelayResearchConflict
	}
	ids, names := map[string]bool{}, map[string]bool{}
	total := 0
	for _, p := range pieces {
		if p.ID == "" || p.Name == "" || !p.Priority || !rrPriorityName(p.Name) || p.Offset != 0 || p.Total != len(p.Data) || p.Total <= 0 || sha256.Sum256(p.Data) != p.Hash || ids[p.ID] || names[p.Name] {
			return errRelayResearchConflict
		}
		ids[p.ID], names[p.Name] = true, true
		total += p.Total
	}
	if total > rrPriorityPackLimit(s.cfg) {
		return errRelayResearchBounds
	}
	for {
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return err
		}
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			return s.ctx.Err()
		}
		newObjects := 0
		var newBytes int64
		for _, p := range pieces {
			if old := s.down[p.ID]; old != nil {
				if old.info.Name != p.Name || old.info.Size != int64(p.Total) || old.hash != p.Hash || !old.priority || !old.ready && !old.done {
					s.mu.Unlock()
					return errRelayResearchConflict
				}
				continue
			}
			if oldID := s.downNames[p.Name]; oldID != "" && oldID != p.ID {
				s.mu.Unlock()
				return errRelayResearchConflict
			}
			newObjects++
			newBytes += int64(p.Total)
		}
		if len(s.down)+newObjects > s.cfg.MaxObjects || newBytes > s.cfg.MaxBytes {
			s.mu.Unlock()
			return errRelayResearchBounds
		}
		if s.downBytes+newBytes > s.cfg.MaxBytes {
			changed := s.changed
			s.downWaits++
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-changed:
			}
			continue
		}
		validate := s.validate
		s.mu.Unlock()
		if validate != nil {
			for _, p := range pieces {
				if err := validate(ObjectInfo{ID: p.ID, Name: p.Name, Size: int64(p.Total)}, p.Data); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func (s *relayResearchClient) acceptDownPiece(p relayResearchPiece) error {
	return s.acceptDownPiecePrepared(p, false)
}

func (s *relayResearchClient) acceptDownPiecePrepared(p relayResearchPiece, innerValidated bool) error {
	if p.ID == "" || p.Name == "" || p.Total <= 0 || p.Total > rrMaxObject || int64(p.Total) > s.cfg.MaxBytes || p.Offset < 0 || len(p.Data) == 0 || p.Offset > p.Total-len(p.Data) {
		return errRelayResearchBounds
	}
	if p.Priority != strings.Contains(p.Name, "/p0/") {
		return errRelayResearchConflict
	}
	if !p.Priority && int64(p.Total) > s.cfg.MaxBytes-s.cfg.PriorityReserveBytes {
		return errRelayResearchBounds
	}
	var v *relayResearchDownload
	for {
		s.mu.Lock()
		if s.ctx != nil && s.ctx.Err() != nil {
			s.mu.Unlock()
			return s.ctx.Err()
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return err
		}
		v = s.down[p.ID]
		if v != nil {
			break
		}
		if len(s.down) >= s.cfg.MaxObjects || !p.Priority && s.normalDownObjects >= s.cfg.MaxObjects-s.cfg.PriorityReserveObjects {
			s.mu.Unlock()
			return errRelayResearchBounds
		}
		if old := s.downNames[p.Name]; old != "" && old != p.ID {
			s.mu.Unlock()
			return errRelayResearchConflict
		}
		if s.downBytes+int64(p.Total) > s.cfg.MaxBytes || !p.Priority && s.normalDownBytes+int64(p.Total) > s.cfg.MaxBytes-s.cfg.PriorityReserveBytes {
			// The server may have processed a deletion whose reply was lost.
			// Keep this PULL unreceipted while UP recovers that exact reply.
			s.downWaits++
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-changed:
			}
			continue
		}
		v = &relayResearchDownload{info: ObjectInfo{Name: p.Name, ID: p.ID, Size: int64(p.Total), Updated: time.Now().UTC().Format(time.RFC3339Nano)},
			hash: p.Hash, priority: p.Priority, data: make([]byte, p.Total)}
		s.down[p.ID], s.downNames[p.Name] = v, p.ID
		s.downBytes += int64(p.Total)
		if !p.Priority {
			s.normalDownBytes += int64(p.Total)
			s.normalDownObjects++
		}
		break
	}
	if v.info.Name != p.Name || v.info.Size != int64(p.Total) || v.hash != p.Hash || v.priority != p.Priority {
		s.mu.Unlock()
		return errRelayResearchConflict
	}
	if v.done {
		s.mu.Unlock()
		return nil
	}
	end := p.Offset + len(p.Data)
	if p.Offset < v.next {
		if end > v.next || !bytes.Equal(v.data[p.Offset:end], p.Data) {
			s.mu.Unlock()
			return errRelayResearchConflict
		}
		s.mu.Unlock()
		return nil
	}
	if p.Offset != v.next {
		s.mu.Unlock()
		return errRelayResearchConflict
	}
	copy(v.data[p.Offset:end], p.Data)
	v.next = end
	if v.next != len(v.data) {
		s.mu.Unlock()
		return nil
	}
	if sha256.Sum256(v.data) != v.hash {
		s.mu.Unlock()
		return errRelayResearchConflict
	}
	validate, info, data := s.validate, v.info, v.data
	s.mu.Unlock()
	if validate != nil && !innerValidated {
		if err := validate(info, data); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	v.ready = true
	s.received++
	s.mu.Unlock()
	return nil
}

func (s *relayResearchClient) announce() {
	for {
		s.mu.Lock()
		var chosen *relayResearchDownload
		for _, v := range s.down {
			if v.ready && !v.done && !v.deleting && !v.queued && !v.notifying && (chosen == nil || v.priority && !chosen.priority) {
				chosen = v
			}
		}
		notify := s.notify
		if chosen == nil || notify == nil {
			s.mu.Unlock()
			return
		}
		chosen.notifying = true
		info := chosen.info
		s.mu.Unlock()
		accepted := notify(info)
		s.mu.Lock()
		chosen.notifying = false
		chosen.queued = accepted
		s.mu.Unlock()
		if !accepted {
			return
		}
	}
}

func relayResearchMuxHooks(m *driveMux, ctx context.Context) (func(ObjectInfo, []byte) error, func(ObjectInfo) bool) {
	validate := func(info ObjectInfo, sealed []byte) error {
		meta, ok := parseMuxObjectInfo(info)
		if !ok || !strings.HasPrefix(info.Name, muxDirPrefix(m.t.SessionID, m.recvDir, "", "")) || m.role == "client" && (meta.ClientID != m.t.ClientID || meta.RunID != m.t.RunID) {
			return errors.New("relay mux namespace rejected")
		}
		key, err := DeriveMuxLaneKeyV4(m.t.Secret, m.t.SessionID, m.recvDir, meta.ClientID, meta.RunID, meta.Lane)
		if err != nil {
			return err
		}
		env, raw, err := OpenEnvelope(key, sealed)
		if err != nil || env.Direction != m.recvDir || env.Sequence != meta.Seq || env.SessionID != m.t.SessionID {
			return errors.New("relay mux envelope rejected")
		}
		_, err = decodeMuxBatch(raw)
		return err
	}
	notify := func(info ObjectInfo) bool {
		meta, ok := parseMuxObjectInfo(info)
		if !ok {
			return false
		}
		if !controlIsFresh(info, m.startedAt) {
			return false
		}
		return m.enqueueMuxObject(ctx, meta)
	}
	return validate, notify
}

var _ BlobStore = (*relayResearchClient)(nil)
var _ ObjectPutStore = (*relayResearchClient)(nil)
var _ ObjectIDStore = (*relayResearchClient)(nil)
