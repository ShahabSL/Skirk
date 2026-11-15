package skirk

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This owner is separate from mux's sequential p0/p1 poll. One fixed query and
// one retained page tail make pagination/backpressure independent of user data.
func (s *fsHybridPriorityStore) runPriorityDiscovery() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.discoverWake:
		}
		prefix := muxDirPrefix(s.cfg.Session, s.state.incoming.direction, s.cfg.ClientID, s.cfg.RunID)
		token := ""
		for page := 0; page < 4; page++ {
			ctx, done, err := s.begin(s.ctx)
			if err != nil {
				return
			}
			var result ObjectListInfo
			if fresh, ok := s.base.(FreshListContainsPageStatusStore); ok {
				result, err = fresh.ListFreshContainsPageStatus(ctx, []string{prefix, "/p0/"}, time.Time{}, token, 1)
			} else {
				result.Objects, err = s.base.List(ctx, prefix)
				result.Pages = 1
			}
			done()
			if err != nil {
				if s.ctx.Err() == nil {
					s.fail(err)
				}
				return
			}
			if len(result.Objects) > 400 {
				s.fail(errFSHybridBounds)
				return
			}
			// The whole bounded tail remains owned on this goroutine's stack
			// until every descriptor is admitted. No backend token advances past
			// unknown, unretained priority objects.
			for _, info := range result.Objects {
				if !strings.HasPrefix(info.Name, prefix) || !strings.Contains(info.Name, "/p0/") {
					continue
				}
				if err = s.admitPrefetch(info); err != nil {
					if s.ctx.Err() == nil {
						s.fail(err)
					}
					return
				}
			}
			token = result.NextPageToken
			if token == "" {
				break
			}
		}
		// Wakes are coalesced, with an explicit bounded interval after each
		// cycle. There is no extra always-running Google polling timer.
		timer := time.NewTimer(s.cfg.DiscoveryInterval)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *fsHybridPriorityStore) admitPrefetch(info ObjectInfo) error {
	if len(info.Name) == 0 || len(info.Name) > fsHybridNameMax || len(info.ID) == 0 || len(info.ID) > 128 || !isSafeObjectSegment(info.ID) || info.Size < 0 || info.Size > fsHybridPhysicalMax {
		return errFSHybridBounds
	}
	s.mu.Lock()
	if known := s.state.rxPhysical[info.ID]; known != nil {
		if known.info.Name != info.Name {
			s.mu.Unlock()
			return errFSHybridIntegrity
		}
		s.mu.Unlock()
		return nil
	}
	if pending, exists := s.prefetchPending[info.ID]; exists {
		s.mu.Unlock()
		if pending.Name != info.Name {
			return errFSHybridIntegrity
		}
		return nil
	}
	s.mu.Unlock()
	select {
	case s.prefetchAdmission <- struct{}{}:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		<-s.prefetchAdmission
		return errFSHybridClosed
	}
	if known := s.state.rxPhysical[info.ID]; known != nil {
		s.mu.Unlock()
		<-s.prefetchAdmission
		if known.info.Name != info.Name {
			return errFSHybridIntegrity
		}
		return nil
	}
	if pending, exists := s.prefetchPending[info.ID]; exists {
		s.mu.Unlock()
		<-s.prefetchAdmission
		if pending.Name != info.Name {
			return errFSHybridIntegrity
		}
		return nil
	}
	s.prefetchPending[info.ID] = info
	s.mu.Unlock()
	select {
	case s.prefetchJobs <- info:
		return nil
	case <-s.ctx.Done():
		s.mu.Lock()
		delete(s.prefetchPending, info.ID)
		s.mu.Unlock()
		<-s.prefetchAdmission
		return s.ctx.Err()
	}
}

func (s *fsHybridPriorityStore) runPrefetchWorker() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case info := <-s.prefetchJobs:
			err := s.prefetchOne(info)
			s.mu.Lock()
			delete(s.prefetchPending, info.ID)
			s.mu.Unlock()
			<-s.prefetchAdmission
			if err != nil && s.ctx.Err() == nil {
				s.fail(err)
				return
			}
		}
	}
}

func (s *fsHybridPriorityStore) prefetchOne(info ObjectInfo) error {
	var sealed []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.OperationTimeout)
		sealed, err = s.base.(ObjectIDStore).GetByID(ctx, info.ID)
		cancel()
		if err == nil {
			break
		}
		if !fsHybridTransient(err) {
			return err
		}
	}
	if err != nil {
		return err
	}
	if info.Size > 0 && int64(len(sealed)) != info.Size {
		return errFSHybridIntegrity
	}
	for {
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			return errFSHybridClosed
		}
		entry, err := s.state.normalizePhysical(info, sealed, time.Now())
		if err == nil {
			s.counters["physical_priority_discoveries"]++
			done := entry.done
			s.mu.Unlock()
			if done {
				s.queuePhysicalCleanup(entry)
			} else {
				s.dispatch(entry)
			}
			return nil
		}
		s.mu.Unlock()
		if !errors.Is(err, errFSHybridBusy) {
			return err
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.space:
		}
	}
}
