package skirk

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

func (s *fsHybridPriorityStore) Put(ctx context.Context, name string, data []byte) error {
	_, err := s.PutObject(ctx, name, data)
	return err
}

func (s *fsHybridPriorityStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if !strings.Contains(name, "/p0/") {
		call, done, err := s.begin(ctx)
		if err != nil {
			return ObjectInfo{}, err
		}
		defer done()
		if put, ok := s.base.(ObjectPutStore); ok {
			return put.PutObject(call, name, data)
		}
		return ObjectInfo{Name: name, Size: int64(len(data))}, s.base.Put(call, name, data)
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return ObjectInfo{}, errFSHybridClosed
	}
	r, fresh, err := s.state.admission(name, data, time.Now().Add(s.cfg.FallbackAfter))
	if err != nil {
		s.mu.Unlock()
		if !errors.Is(err, errFSHybridBusy) {
			s.fail(err)
		}
		return ObjectInfo{}, err
	}
	if fresh {
		if !s.retired && s.state.enqueue(r) {
			s.dirty = true
			s.ackDue = time.Time{}
			s.counters["priority_enqueued"]++
		} else {
			r.deadline = time.Time{}
			s.counters["priority_immutable_only"]++
		}
	}
	s.mu.Unlock()
	s.signal(s.wake)
	s.signal(s.fallbackWake)
	select {
	case <-r.pubDone:
	case <-ctx.Done():
		return ObjectInfo{}, ctx.Err()
	case <-s.ctx.Done():
	}
	s.mu.Lock()
	published, info, lastErr := r.published, r.info, r.fbErr
	s.mu.Unlock()
	if published {
		return info, nil
	}
	if lastErr != nil {
		return ObjectInfo{}, lastErr
	}
	return ObjectInfo{}, errFSHybridClosed
}

func (s *fsHybridPriorityStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if strings.Contains(name, "/p0/") {
		return ObjectInfo{}, errFSHybridBounds
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer done()
	return s.base.(ObjectPutIDStore).PutObjectWithID(call, id, name, data)
}
func (s *fsHybridPriorityStore) GenerateObjectIDs(ctx context.Context, n int) ([]string, error) {
	call, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return s.base.(ObjectIDReserveStore).GenerateObjectIDs(call, n)
}
func (s *fsHybridPriorityStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	if strings.HasPrefix(id, "firestore-priority:") {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
			return nil, errFSHybridClosed
		}
		entry := s.state.rxIDs[id]
		if entry == nil || entry.done {
			return nil, errFSHybridClosed
		}
		return bytes.Clone(entry.sealed), nil
	}
	s.mu.Lock()
	knownPriority := s.state.rxPhysical[id] != nil
	s.mu.Unlock()
	if knownPriority {
		return nil, errFSHybridIntegrity
	} // Discovery must use its normalized virtual ID.
	call, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return s.base.(ObjectIDStore).GetByID(call, id)
}
func (s *fsHybridPriorityStore) GetNormalByID(ctx context.Context, id string) ([]byte, error) {
	if strings.HasPrefix(id, "firestore-priority:") {
		return nil, errFSHybridIntegrity
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if normal, ok := s.base.(NormalObjectIDStore); ok {
		return normal.GetNormalByID(call, id)
	}
	return s.base.(ObjectIDStore).GetByID(call, id)
}
func (s *fsHybridPriorityStore) Get(ctx context.Context, name string) ([]byte, error) {
	if strings.Contains(name, "/p0/") {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
			return nil, errFSHybridClosed
		}
		entry := s.state.rxNames[name]
		if entry == nil || entry.done {
			return nil, errFSHybridBusy
		}
		return bytes.Clone(entry.sealed), nil
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return s.base.Get(call, name)
}
func (s *fsHybridPriorityStore) Delete(ctx context.Context, name string) error {
	if strings.Contains(name, "/p0/") {
		s.mu.Lock()
		entry := s.state.rxNames[name]
		s.mu.Unlock()
		if entry == nil {
			return errFSHybridIntegrity
		}
		return s.DeleteID(ctx, entry.info.ID)
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	return s.base.Delete(call, name)
}

// Delete is physical cleanup only. Failed/canceled mux processing may also
// request cleanup, so it must never manufacture a processing ACK here.
func (s *fsHybridPriorityStore) DeleteID(ctx context.Context, id string) error {
	call, done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if !strings.HasPrefix(id, "firestore-priority:") {
		return s.base.(ObjectIDStore).DeleteID(call, id)
	}
	s.mu.Lock()
	entry := s.state.rxIDs[id]
	if entry == nil || !entry.done {
		s.mu.Unlock()
		s.fail(errFSHybridIntegrity)
		return errFSHybridIntegrity
	}
	var targets []string
	for physical := range entry.physicalIDs {
		if !s.deletedPhysical[physical] {
			targets = append(targets, physical)
		}
	}
	s.mu.Unlock()
	var joined error
	for _, physical := range targets {
		err := s.base.(ObjectIDStore).DeleteID(call, physical)
		if err == nil || isDriveNotFound(err) {
			s.mu.Lock()
			s.deletedPhysical[physical] = true
			s.mu.Unlock()
		} else {
			joined = errors.Join(joined, err)
		}
	}
	s.mu.Lock()
	s.cleanupQueued[id] = false
	pending := false
	for physical := range entry.physicalIDs {
		pending = pending || !s.deletedPhysical[physical]
	}
	s.mu.Unlock()
	if pending {
		s.queuePhysicalCleanup(entry)
	}
	return joined
}

func (s *fsHybridPriorityStore) queuePhysicalCleanup(entry *fsHybridInbound) {
	s.mu.Lock()
	if s.closed || !entry.done || s.cleanup == nil || s.cleanupQueued[entry.info.ID] {
		s.mu.Unlock()
		return
	}
	s.cleanupQueued[entry.info.ID] = true
	callback, info := s.cleanup, entry.info
	s.mu.Unlock()
	callback(info) // Only enqueues an existing deferred cleanup task.
}

func fsHybridContains(name string, contains []string) bool {
	for _, part := range contains {
		if !strings.Contains(name, part) {
			return false
		}
	}
	return true
}
func fsHybridPriorityQuery(contains []string) bool {
	for _, part := range contains {
		if strings.Contains(part, "/p0/") {
			return true
		}
	}
	return false
}
func (s *fsHybridPriorityStore) virtualPage(contains []string) ObjectListInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := ObjectListInfo{Pages: 1}
	for _, entry := range s.state.rxNames {
		if !entry.done && fsHybridContains(entry.info.Name, contains) {
			result.Objects = append(result.Objects, entry.info)
		}
	}
	return result
}

func (s *fsHybridPriorityStore) priorityPage(ctx context.Context, contains []string) (ObjectListInfo, error) {
	s.mu.Lock()
	closed := s.closed || s.ctx.Err() != nil || ctx.Err() != nil
	s.mu.Unlock()
	if closed {
		return ObjectListInfo{}, errFSHybridClosed
	}
	s.signal(s.discoverWake)
	return s.virtualPage(contains), nil
}
func (s *fsHybridPriorityStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	result, err := s.ListFreshContainsPageStatus(ctx, []string{prefix}, time.Time{}, "", 4)
	return result.Objects, err
}
func (s *fsHybridPriorityStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if len(contains) == 0 || len(contains) > 8 {
		return ObjectListInfo{}, errFSHybridBounds
	}
	if fsHybridPriorityQuery(contains) {
		// Core polls p0 before p1 in one goroutine. Never wait for Drive
		// priority prefetch here; its separate owner retains every page tail.
		return s.priorityPage(ctx, contains)
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectListInfo{}, err
	}
	defer done()
	var result ObjectListInfo
	if fresh, ok := s.base.(FreshListContainsPageStatusStore); ok {
		result, err = fresh.ListFreshContainsPageStatus(call, contains, since, pageToken, maxPages)
	} else {
		result.Objects, err = s.base.List(call, contains[0])
		result.Pages = 1
	}
	if err != nil {
		return result, err
	}
	var objects []ObjectInfo
	seen := make(map[string]struct{})
	for _, info := range result.Objects {
		// This finite prototype is exactly one client/run. Never consume or
		// delete another copied profile's mailbox objects.
		if !strings.HasPrefix(info.Name, muxDirPrefix(s.cfg.Session, s.state.incoming.direction, s.cfg.ClientID, s.cfg.RunID)) || !fsHybridContains(info.Name, contains) {
			continue
		}
		if strings.Contains(info.Name, "/p0/") {
			s.signal(s.discoverWake)
			continue
		}
		if _, duplicate := seen[info.ID]; !duplicate {
			objects = append(objects, info)
			seen[info.ID] = struct{}{}
		}
	}
	s.mu.Lock()
	for _, entry := range s.state.rxNames {
		// Unprocessed virtual objects remain discoverable even when a broad
		// Drive since cursor advanced beyond their initial local arrival.
		if entry.done || !fsHybridContains(entry.info.Name, contains) {
			continue
		}
		if _, duplicate := seen[entry.info.ID]; !duplicate {
			objects = append(objects, entry.info)
			seen[entry.info.ID] = struct{}{}
		}
	}
	s.mu.Unlock()
	result.Objects = objects
	return result, nil
}

func (s *fsHybridPriorityStore) bindMux(m *driveMux, ctx context.Context) error {
	if m.t.MetadataSlotV1 || m.t.MetadataSlotDeliveryV1 || m.t.MetadataOnlyPriority {
		return errFSHybridBounds
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return errFSHybridClosed
	}
	if s.bindingCancel != nil {
		s.mu.Unlock()
		return errFSHybridBounds
	}
	// Bind itself and every later callback are fenced by this store's
	// cancellation. A blocked enqueue cannot keep Close waiting on a live mux.
	s.wg.Add(1)
	bound, cancel := context.WithCancel(ctx)
	s.bindingCancel = cancel
	s.bindingStop = context.AfterFunc(s.ctx, cancel)
	s.receive = func(info ObjectInfo) bool {
		meta, ok := parseMuxObjectInfo(info)
		return ok && m.enqueueMuxObject(bound, meta)
	}
	s.cleanup = func(info ObjectInfo) { m.enqueueCleanup(cleanupTask{name: info.Name, id: info.ID}) }
	var entries []*fsHybridInbound
	for _, entry := range s.state.rxNames {
		if !entry.done {
			entries = append(entries, entry)
		}
	}
	s.mu.Unlock()
	defer s.wg.Done()
	for _, entry := range entries {
		s.dispatch(entry)
	}
	return nil
}

func (s *fsHybridPriorityStore) Snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]int64, len(s.counters)+12)
	for key, value := range s.counters {
		result[key] = value
	}
	result["outbound_claims"] = int64(len(s.state.outNames))
	result["inbound_claims"] = int64(len(s.state.rxNames))
	result["pending_records"] = int64(len(s.state.outQueue))
	result["receive_payload_bytes"] = int64(s.state.rxBytes)
	result["send_payload_bytes"] = int64(s.state.outRetained)
	result["processed_prefix"] = int64(s.state.rxProcessed)
	result["peer_processed_prefix"] = int64(s.state.acked)
	result["pending_prefetch"] = int64(len(s.prefetchPending))
	result["queued_prefetch"] = int64(len(s.prefetchJobs))
	result["queued_fallback"] = int64(len(s.fallbackJobs))
	if s.joined {
		result["joined"] = 1
	}
	return result
}

func (s *fsHybridPriorityStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		if s.bindingCancel != nil {
			s.bindingCancel()
		}
		if s.bindingStop != nil {
			s.bindingStop()
		}
		s.mu.Unlock()
		s.closeErr = s.carrier.Close()
		s.wg.Wait()
		// No writer/base operation can create a further physical copy. Retain
		// all metadata tombstones until GC; late mux callbacks are closed noops.
		s.mu.Lock()
		// Every owner is joined before queued descriptors are discarded. A
		// canceled owner is not a resource-cleanup proof; immutable identities
		// and generated physical IDs remain below for exact external cleanup.
		for len(s.prefetchJobs) > 0 {
			<-s.prefetchJobs
		}
		for len(s.prefetchAdmission) > 0 {
			<-s.prefetchAdmission
		}
		for len(s.fallbackJobs) > 0 {
			<-s.fallbackJobs
		}
		clear(s.prefetchPending)
		s.state.outQueue = nil
		for _, r := range s.state.outNames {
			r.sealed = nil
			r.record.Sealed = nil
			r.fbQueued = false
		}
		for _, entry := range s.state.rxNames {
			entry.sealed = nil
		}
		s.state.outRetained, s.state.rxBytes = 0, 0
		s.joined = true
		s.mu.Unlock()
		// Provider cleanup is a separate, explicitly budgeted fixture phase.
		// A context deadline is not a request/quota reserve; Close makes no
		// unbudgeted deletion calls and must not be reported as resource cleanup.
	})
	return s.closeErr
}

type fsHybridCleanupTarget struct{ ID, Name string }

func (s *fsHybridPriorityStore) CleanupTargets() ([]fsHybridCleanupTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.joined {
		return nil, errFSHybridBusy
	}
	owned := make(map[string]string)
	for _, r := range s.state.outNames {
		if r.physicalID != "" {
			owned[r.physicalID] = r.name
		}
	}
	for id, entry := range s.state.rxPhysical {
		if !s.deletedPhysical[id] {
			owned[id] = entry.info.Name
		}
	}
	result := make([]fsHybridCleanupTarget, 0, len(owned))
	for id, name := range owned {
		result = append(result, fsHybridCleanupTarget{ID: id, Name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
