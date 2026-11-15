package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

type relayResearchRecord struct {
	info              ObjectInfo
	hash              [32]byte
	data              []byte
	total, sent       int
	complete, deleted bool
}
type relayResearchReply struct {
	hash     [32]byte
	done     chan struct{}
	wire     []byte
	err      error
	finished bool
}
type relayResearchChannel struct {
	next, completed, receipt uint64
	active                   bool
	replies                  map[uint64]*relayResearchReply
}
type relayResearchStats struct {
	UpBytes, DownBytes                                                   int64
	UpObjects, DownObjects, Assembling, CachedReplies, LiveWorkers       int
	AcceptedUp, AcceptedDown, DeletedUp, DeletedDown, Requests, Replayed uint64
}
type relayResearchBroker struct {
	mu                        sync.Mutex
	ctx                       context.Context
	cancel                    context.CancelFunc
	wg                        sync.WaitGroup
	closed                    bool
	cfg                       relayResearchConfig
	codec                     *relayResearchCodec
	upStore                   *MemoryStore
	downStore                 *MemoryStore
	up, down                  map[string]*relayResearchRecord
	upNames, downNames        map[string]string
	downOrder                 []string
	downCursor, priorityBurst int
	channels                  map[byte]*relayResearchChannel
	changed                   chan struct{}
	stats                     relayResearchStats
	validateUp                func(ObjectInfo, []byte) error
	notifyUp                  func(ObjectInfo)
}

func newRelayResearchBroker(parent context.Context, key []byte, epoch [16]byte, cfg relayResearchConfig) (*relayResearchBroker, error) {
	var err error
	if cfg, err = cfg.normalized(); err != nil {
		return nil, err
	}
	codec, err := newRelayResearchCodec(key, epoch, cfg.FrameBytes)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	b := &relayResearchBroker{ctx: ctx, cancel: cancel, cfg: cfg, codec: codec, upStore: NewMemoryStore(), downStore: NewMemoryStore(), up: map[string]*relayResearchRecord{}, down: map[string]*relayResearchRecord{}, upNames: map[string]string{}, downNames: map[string]string{}, channels: map[byte]*relayResearchChannel{}, changed: make(chan struct{})}
	for _, kind := range []byte{rrUp, rrPull} {
		b.channels[kind] = &relayResearchChannel{next: 1, replies: map[uint64]*relayResearchReply{}}
	}
	return b, nil
}
func (b *relayResearchBroker) signalLocked() { close(b.changed); b.changed = make(chan struct{}) }
func (b *relayResearchBroker) setUpHooks(validate func(ObjectInfo, []byte) error, notify func(ObjectInfo)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.validateUp, b.notifyUp = validate, notify
}
func (b *relayResearchBroker) snapshot() relayResearchStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats
	s.UpObjects, s.DownObjects = len(b.up), len(b.down)
	for _, v := range b.up {
		if !v.complete {
			s.Assembling++
		}
	}
	for _, c := range b.channels {
		s.CachedReplies += len(c.replies)
	}
	return s
}
func (b *relayResearchBroker) Close() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		b.cancel()
		b.signalLocked()
	}
	b.mu.Unlock()
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, store := range []*MemoryStore{b.upStore, b.downStore} {
		store.mu.Lock()
		store.objects = map[string][]byte{}
		store.ids = map[string]string{}
		store.mu.Unlock()
	}
	b.up = map[string]*relayResearchRecord{}
	b.down = map[string]*relayResearchRecord{}
	b.upNames = map[string]string{}
	b.downNames = map[string]string{}
	b.downOrder = nil
	b.stats.UpBytes, b.stats.DownBytes = 0, 0
	for _, ch := range b.channels {
		ch.replies = map[uint64]*relayResearchReply{}
	}
}

// HTTP caller cancellation does not cancel an already accepted computation.
// Its exact reply is retained for retry, even when the first response is lost.
func (b *relayResearchBroker) exchange(ctx context.Context, wire []byte) ([]byte, error) {
	p, err := b.codec.open(wire)
	if err != nil {
		return nil, err
	}
	if p.Kind != rrUp && p.Kind != rrPull || p.Status != 0 || len(p.Committed) > 0 || len(p.Deleted) > 0 || p.Receipt >= p.Seq || p.Kind == rrPull && (len(p.Pieces) > 0 || len(p.Deletes) > 0) {
		return nil, errRelayResearchBounds
	}
	hash := sha256.Sum256(wire)
	b.mu.Lock()
	if b.closed || b.ctx.Err() != nil {
		b.mu.Unlock()
		return nil, errRelayResearchClosed
	}
	b.stats.Requests++
	ch := b.channels[p.Kind]
	if p.Receipt > ch.completed {
		b.mu.Unlock()
		return nil, errRelayResearchSequence
	}
	if p.Receipt > ch.receipt {
		ch.receipt = p.Receipt
		for seq, r := range ch.replies {
			if seq <= p.Receipt && r.finished {
				delete(ch.replies, seq)
			}
		}
	}
	r, exists := ch.replies[p.Seq]
	if exists {
		if r.hash != hash {
			b.mu.Unlock()
			return nil, errRelayResearchConflict
		}
		b.stats.Replayed++
	} else {
		if p.Seq != ch.next || ch.active {
			b.mu.Unlock()
			return nil, errRelayResearchSequence
		}
		if len(ch.replies) >= 2 {
			b.mu.Unlock()
			return nil, errRelayResearchBounds
		}
		r = &relayResearchReply{hash: hash, done: make(chan struct{})}
		ch.replies[p.Seq] = r
		ch.next++
		ch.active = true
		b.stats.LiveWorkers++
		b.wg.Add(1)
		go b.compute(p, r)
	}
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return append([]byte(nil), r.wire...), r.err
	}
}
func (b *relayResearchBroker) compute(p relayResearchPacket, r *relayResearchReply) {
	defer b.wg.Done()
	reply := relayResearchPacket{Kind: p.Kind | rrResponse, Seq: p.Seq}
	var err error
	if p.Kind == rrUp {
		for _, id := range p.Deletes {
			if err = b.deleteDown(id); err != nil {
				break
			}
			reply.Deleted = append(reply.Deleted, id)
		}
		if err == nil {
			for _, piece := range p.Pieces {
				var committed bool
				committed, err = b.acceptUp(piece)
				if err != nil {
					break
				}
				if committed {
					reply.Committed = append(reply.Committed, piece.ID)
				}
			}
		}
	} else {
		reply.Pieces, err = b.pull()
	}
	if err != nil {
		switch {
		case errors.Is(err, errRelayResearchBounds):
			reply.Status = rrStatusBounds
		case errors.Is(err, errRelayResearchClosed) || errors.Is(err, context.Canceled):
			reply.Status = rrStatusClosed
		default:
			reply.Status = rrStatusConflict
		}
		reply.Pieces = nil
		reply.Committed = nil
		reply.Deleted = nil
	}
	sealed, sealErr := b.codec.seal(reply)
	b.mu.Lock()
	r.wire, r.err, r.finished = sealed, sealErr, true
	ch := b.channels[p.Kind]
	ch.active = false
	ch.completed = p.Seq
	b.stats.LiveWorkers--
	close(r.done)
	b.mu.Unlock()
}
func rrRecordMatches(o *relayResearchRecord, p relayResearchPiece) bool {
	return o.info.Name == p.Name && o.total == p.Total && o.hash == p.Hash
}
func (b *relayResearchBroker) acceptUp(p relayResearchPiece) (bool, error) {
	for {
		b.mu.Lock()
		if b.closed || b.ctx.Err() != nil {
			b.mu.Unlock()
			return false, errRelayResearchClosed
		}
		o := b.up[p.ID]
		if o == nil {
			if b.upNames[p.Name] != "" {
				b.mu.Unlock()
				return false, errRelayResearchConflict
			}
			if len(b.up) >= b.cfg.MaxObjects {
				b.mu.Unlock()
				return false, errRelayResearchBounds
			}
			if int64(p.Total) > b.cfg.MaxBytes {
				b.mu.Unlock()
				return false, errRelayResearchBounds
			}
			if b.stats.UpBytes+int64(p.Total) > b.cfg.MaxBytes {
				wake := b.changed
				b.mu.Unlock()
				select {
				case <-wake:
					continue
				case <-b.ctx.Done():
					return false, errRelayResearchClosed
				}
			}
			o = &relayResearchRecord{info: ObjectInfo{ID: p.ID, Name: p.Name, Size: int64(p.Total)}, hash: p.Hash, total: p.Total, data: make([]byte, 0, p.Total)}
			b.up[p.ID] = o
			b.upNames[p.Name] = p.ID
			b.stats.UpBytes += int64(p.Total)
		}
		if !rrRecordMatches(o, p) {
			b.mu.Unlock()
			return false, errRelayResearchConflict
		}
		if o.complete {
			b.mu.Unlock()
			return true, nil
		}
		if p.Offset < len(o.data) {
			if p.Offset+len(p.Data) > len(o.data) || !bytes.Equal(o.data[p.Offset:p.Offset+len(p.Data)], p.Data) {
				b.mu.Unlock()
				return false, errRelayResearchConflict
			}
			b.mu.Unlock()
			return false, nil
		}
		if p.Offset != len(o.data) {
			b.mu.Unlock()
			return false, errRelayResearchSequence
		}
		o.data = append(o.data, p.Data...)
		if len(o.data) < o.total {
			b.mu.Unlock()
			return false, nil
		}
		if sha256.Sum256(o.data) != o.hash {
			b.mu.Unlock()
			return false, errRelayResearchConflict
		}
		validate, notify, info, data := b.validateUp, b.notifyUp, o.info, o.data
		b.mu.Unlock()
		if validate != nil {
			if err := validate(info, data); err != nil {
				return false, errRelayResearchConflict
			}
		}
		b.mu.Lock()
		if b.closed || b.ctx.Err() != nil {
			b.mu.Unlock()
			return false, errRelayResearchClosed
		}
		if _, err := b.upStore.PutObjectWithID(b.ctx, p.ID, p.Name, data); err != nil {
			b.mu.Unlock()
			return false, err
		}
		o.complete = true
		o.data = nil
		b.stats.AcceptedUp++
		b.signalLocked()
		b.mu.Unlock()
		if notify != nil {
			notify(info)
		}
		return true, nil
	}
}
func (b *relayResearchBroker) deleteUp(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errRelayResearchClosed
	}
	o := b.up[id]
	if o == nil {
		return nil
	}
	if !o.complete {
		return errRelayResearchConflict
	}
	if !o.deleted {
		_ = b.upStore.DeleteID(b.ctx, id)
		o.deleted = true
		b.stats.UpBytes -= int64(o.total)
		b.stats.DeletedUp++
		b.signalLocked()
	}
	return nil
}
func (b *relayResearchBroker) deleteDown(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errRelayResearchClosed
	}
	o := b.down[id]
	if o == nil {
		return nil
	}
	if !o.deleted {
		_ = b.downStore.DeleteID(b.ctx, id)
		o.deleted = true
		o.data = nil
		b.stats.DownBytes -= int64(o.total)
		b.stats.DeletedDown++
		b.signalLocked()
	}
	return nil
}
func (b *relayResearchBroker) putDown(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if len(data) == 0 || len(data) > rrMaxObject || len(id) == 0 || len(id) > 128 || len(name) == 0 || len(name) > 512 {
		return ObjectInfo{}, errRelayResearchBounds
	}
	hash := sha256.Sum256(data)
	for {
		b.mu.Lock()
		if b.closed || b.ctx.Err() != nil {
			b.mu.Unlock()
			return ObjectInfo{}, errRelayResearchClosed
		}
		if o := b.down[id]; o != nil {
			if o.info.Name != name || o.total != len(data) || o.hash != hash {
				b.mu.Unlock()
				return ObjectInfo{}, errRelayResearchConflict
			}
			info := o.info
			b.mu.Unlock()
			return info, nil
		}
		if b.downNames[name] != "" {
			b.mu.Unlock()
			return ObjectInfo{}, errRelayResearchConflict
		}
		if len(b.down) >= b.cfg.MaxObjects || int64(len(data)) > b.cfg.MaxBytes {
			b.mu.Unlock()
			return ObjectInfo{}, errRelayResearchBounds
		}
		if b.stats.DownBytes+int64(len(data)) > b.cfg.MaxBytes {
			wake := b.changed
			b.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return ObjectInfo{}, ctx.Err()
			case <-b.ctx.Done():
				return ObjectInfo{}, errRelayResearchClosed
			}
		}
		info, err := b.downStore.PutObjectWithID(ctx, id, name, data)
		if err != nil {
			b.mu.Unlock()
			return ObjectInfo{}, err
		}
		// MemoryStore owns the immutable copy. The record shares its slice,
		// avoiding a second retained payload; neither owner mutates it.
		b.downStore.mu.Lock()
		stored := b.downStore.objects[name]
		b.downStore.mu.Unlock()
		b.down[id] = &relayResearchRecord{info: info, hash: hash, total: len(data), data: stored, complete: true}
		b.downNames[name] = id
		b.downOrder = append(b.downOrder, id)
		b.stats.DownBytes += int64(len(data))
		b.stats.AcceptedDown++
		b.signalLocked()
		b.mu.Unlock()
		return info, nil
	}
}
func (b *relayResearchBroker) pull() ([]relayResearchPiece, error) {
	timer := time.NewTimer(b.cfg.PollWait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if b.closed || b.ctx.Err() != nil {
			b.mu.Unlock()
			return nil, errRelayResearchClosed
		}
		// At most eight priority fragments can pass a waiting normal fragment.
		wantPriority := b.priorityBurst < 8
		chosen := -1
		for pass := 0; pass < 2 && chosen < 0; pass++ {
			for j := 0; j < len(b.downOrder); j++ {
				i := (b.downCursor + j) % len(b.downOrder)
				o := b.down[b.downOrder[i]]
				if o.deleted || o.sent >= o.total {
					continue
				}
				priority := strings.Contains(o.info.Name, "/p0/")
				if (priority == wantPriority) == (pass == 0) {
					chosen = i
					break
				}
			}
		}
		if chosen >= 0 {
			o := b.down[b.downOrder[chosen]]
			size := min(b.cfg.FrameBytes, o.total-o.sent)
			priority := strings.Contains(o.info.Name, "/p0/")
			p := relayResearchPiece{ID: o.info.ID, Name: o.info.Name, Total: o.total, Offset: o.sent, Hash: o.hash, Data: append([]byte(nil), o.data[o.sent:o.sent+size]...), Priority: priority}
			o.sent += size
			b.downCursor = (chosen + 1) % len(b.downOrder)
			if priority {
				b.priorityBurst++
			} else {
				b.priorityBurst = 0
			}
			b.mu.Unlock()
			return []relayResearchPiece{p}, nil
		}
		wake := b.changed
		b.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C:
			return nil, nil
		case <-b.ctx.Done():
			return nil, errRelayResearchClosed
		}
	}
}

type relayResearchExitStore struct{ b *relayResearchBroker }

func (b *relayResearchBroker) exitStore() *relayResearchExitStore { return &relayResearchExitStore{b} }
func (s *relayResearchExitStore) Put(ctx context.Context, name string, data []byte) error {
	_, err := s.PutObject(ctx, name, data)
	return err
}
func (s *relayResearchExitStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	sum := sha256.Sum256(append(append([]byte(name), 0), data...))
	return s.PutObjectWithID(ctx, "relay-down-"+hex.EncodeToString(sum[:]), name, data)
}
func (s *relayResearchExitStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	return s.b.putDown(ctx, id, name, data)
}
func (s *relayResearchExitStore) GenerateObjectIDs(ctx context.Context, n int) ([]string, error) {
	if n < 0 || n > 256 {
		return nil, errRelayResearchBounds
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.b.closed {
		return nil, errRelayResearchClosed
	}
	return s.b.downStore.GenerateObjectIDs(ctx, n)
}
func (s *relayResearchExitStore) Get(ctx context.Context, name string) ([]byte, error) {
	return s.b.upStore.Get(ctx, name)
}
func (s *relayResearchExitStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	return s.b.upStore.GetByID(ctx, id)
}
func (s *relayResearchExitStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	return s.b.upStore.List(ctx, prefix)
}
func (s *relayResearchExitStore) DeleteID(_ context.Context, id string) error {
	return s.b.deleteUp(id)
}
func (s *relayResearchExitStore) Delete(ctx context.Context, name string) error {
	infos, err := s.List(ctx, name)
	if err != nil {
		return err
	}
	for _, i := range infos {
		if i.Name == name {
			return s.DeleteID(ctx, i.ID)
		}
	}
	return nil
}
