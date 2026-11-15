package skirk

import (
	"context"
	"strings"
	"time"
)

func (*fsHybridPriorityStore) MetadataOnlyPriorityEnabled() bool { return false }
func (*fsHybridPriorityStore) MetadataSlotEnabled() bool         { return false }

func (s *fsHybridPriorityStore) QuotaSnapshot() DriveQuotaSnapshot {
	if base, ok := s.base.(interface{ QuotaSnapshot() DriveQuotaSnapshot }); ok {
		return base.QuotaSnapshot()
	}
	return DriveQuotaSnapshot{Ops: map[string]DriveQuotaOpSnapshot{}}
}
func (s *fsHybridPriorityStore) QuotaBackoffSnapshot() DriveQuotaBackoffSnapshot {
	if base, ok := s.base.(interface {
		QuotaBackoffSnapshot() DriveQuotaBackoffSnapshot
	}); ok {
		return base.QuotaBackoffSnapshot()
	}
	return DriveQuotaBackoffSnapshot{}
}
func (s *fsHybridPriorityStore) WaitForDriveQuota(ctx context.Context, op string) error {
	call, done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if base, ok := s.base.(driveQuotaWaitStore); ok {
		return base.WaitForDriveQuota(call, op)
	}
	return nil
}

func (s *fsHybridPriorityStore) filterPage(contains []string, result ObjectListInfo) ObjectListInfo {
	var objects []ObjectInfo
	seen := make(map[string]struct{})
	prefix := muxDirPrefix(s.cfg.Session, s.state.incoming.direction, s.cfg.ClientID, s.cfg.RunID)
	for _, info := range result.Objects {
		if !strings.HasPrefix(info.Name, prefix) || !fsHybridContains(info.Name, contains) {
			continue
		}
		if strings.Contains(info.Name, "/p0/") {
			s.signal(s.discoverWake)
			continue
		}
		objects = append(objects, info)
		seen[info.ID] = struct{}{}
	}
	s.mu.Lock()
	for _, entry := range s.state.rxNames {
		if !entry.done && fsHybridContains(entry.info.Name, contains) {
			if _, ok := seen[entry.info.ID]; !ok {
				objects = append(objects, entry.info)
				seen[entry.info.ID] = struct{}{}
			}
		}
	}
	s.mu.Unlock()
	result.Objects = objects
	return result
}

func (s *fsHybridPriorityStore) ListFresh(ctx context.Context, prefix string, since time.Time) ([]ObjectInfo, error) {
	result, err := s.ListFreshStatus(ctx, prefix, since)
	return result.Objects, err
}
func (s *fsHybridPriorityStore) ListFreshStatus(ctx context.Context, prefix string, since time.Time) (ObjectListInfo, error) {
	if fsHybridPriorityQuery([]string{prefix}) {
		s.signal(s.discoverWake)
		return s.virtualPage([]string{prefix}), nil
	}
	base, ok := s.base.(FreshListStatusStore)
	if !ok {
		return s.ListFreshContainsPageStatus(ctx, []string{prefix}, since, "", 4)
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectListInfo{}, err
	}
	defer done()
	result, err := base.ListFreshStatus(call, prefix, since)
	if err != nil {
		return result, err
	}
	return s.filterPage([]string{prefix}, result), nil
}
func (s *fsHybridPriorityStore) ListFreshPageStatus(ctx context.Context, prefix string, since time.Time, token string) (ObjectListInfo, error) {
	if fsHybridPriorityQuery([]string{prefix}) {
		s.signal(s.discoverWake)
		return s.virtualPage([]string{prefix}), nil
	}
	base, ok := s.base.(FreshListPageStatusStore)
	if !ok {
		return s.ListFreshContainsPageStatus(ctx, []string{prefix}, since, token, 1)
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectListInfo{}, err
	}
	defer done()
	result, err := base.ListFreshPageStatus(call, prefix, since, token)
	if err != nil {
		return result, err
	}
	return s.filterPage([]string{prefix}, result), nil
}
func (s *fsHybridPriorityStore) ListFreshAdaptiveMuxPageStatus(ctx context.Context, prefix string, since time.Time, token string, maxPages int) (ObjectListInfo, error) {
	if fsHybridPriorityQuery([]string{prefix}) {
		s.signal(s.discoverWake)
		return s.virtualPage([]string{prefix}), nil
	}
	base, ok := s.base.(AdaptiveMuxListPageStatusStore)
	if !ok {
		return s.ListFreshContainsPageStatus(ctx, []string{prefix}, since, token, maxPages)
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectListInfo{}, err
	}
	defer done()
	result, err := base.ListFreshAdaptiveMuxPageStatus(call, prefix, since, token, maxPages)
	if err != nil {
		return result, err
	}
	return s.filterPage([]string{prefix}, result), nil
}

func (s *fsHybridPriorityStore) GetMuxObjectMetadataByID(ctx context.Context, id string) (ObjectInfo, bool, error) {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return ObjectInfo{}, false, errFSHybridClosed
	}
	if entry := s.state.rxIDs[id]; entry != nil {
		info, live := entry.info, !entry.done
		s.mu.Unlock()
		return info, live, nil
	}
	if entry := s.state.rxPhysical[id]; entry != nil {
		info, live := entry.info, !entry.done
		s.mu.Unlock()
		return info, live, nil
	}
	s.mu.Unlock()
	base, ok := s.base.(MuxObjectMetadataStore)
	if !ok {
		return ObjectInfo{}, false, nil
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	defer done()
	info, found, err := base.GetMuxObjectMetadataByID(call, id)
	if err != nil || !found {
		return info, found, err
	}
	if !strings.HasPrefix(info.Name, muxDirPrefix(s.cfg.Session, s.state.incoming.direction, s.cfg.ClientID, s.cfg.RunID)) {
		return ObjectInfo{}, false, nil
	}
	if strings.Contains(info.Name, "/p0/") {
		// Do not let exact-ID repair bypass shared priority identity admission.
		s.signal(s.discoverWake)
		return ObjectInfo{}, false, nil
	}
	return info, true, nil
}

func (s *fsHybridPriorityStore) GetObjectRangeByID(ctx context.Context, id string, start, end int64) ([]byte, ObjectRangeInfo, error) {
	if strings.HasPrefix(id, "firestore-priority:") {
		return nil, ObjectRangeInfo{}, errFSHybridBounds
	}
	base, ok := s.base.(RangeObjectStore)
	if !ok {
		return nil, ObjectRangeInfo{}, errFSHybridBounds
	}
	call, done, err := s.begin(ctx)
	if err != nil {
		return nil, ObjectRangeInfo{}, err
	}
	defer done()
	return base.GetObjectRangeByID(call, id, start, end)
}
