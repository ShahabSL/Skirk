package skirk

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

const (
	pilotUploadRecoveryPlain = 32 << 10
	pilotUploadCooldownMin   = 2 * time.Second
	pilotUploadCooldownMax   = 32 * time.Second
	// The measured healthy binary RPC p95 was about 850 ms. Give that
	// path a margin before reserving spare capacity, even after a fast run.
	pilotUploadHealthMin = 900 * time.Millisecond
	pilotUploadHealthMax = 1500 * time.Millisecond
)

var errPilotUploadDeadline = errors.New("UDP create request deadline")

type pilotUploadFallback uint8

const (
	pilotUploadMediaSlow pilotUploadFallback = iota + 1
	pilotUploadMediaCooldown
	pilotUploadMediaDrain
	pilotUploadMediaProbe
	pilotUploadMediaFirstProbe
)

type pilotUploadCompletion struct {
	seq      uint64
	binary   bool
	demoted  bool // The reserved binary slot sent metadata after expiry shrank the batch.
	err      error
	finished time.Time
}

// Owned by the publisher loop. Two binary RPCs may overlap while both are
// healthy. On a timeout, no binary work joins the draining pool.
type pilotUploadSchedule struct {
	inFlight        map[uint64]time.Time
	cooldown        time.Duration
	until           time.Time
	drain           bool
	smoothed        time.Duration
	variation       time.Duration
	allowEarlyProbe bool
	earlyProbeSeq   uint64
}

func (s *pilotUploadSchedule) limit(now time.Time, metadata, media int) (int, pilotUploadFallback) {
	switch {
	case s.drain:
		return metadata, pilotUploadMediaDrain
	case now.Before(s.until):
		// The first timeout opens one bounded probe once its old pool drains.
		// Reserve the other shared slot for metadata while the probe runs.
		if s.allowEarlyProbe && s.cooldown == pilotUploadCooldownMin && s.earlyProbeSeq == 0 && len(s.inFlight) == 0 {
			return min(media, pilotUploadRecoveryPlain), pilotUploadMediaFirstProbe
		}
		return metadata, pilotUploadMediaCooldown
	case s.cooldown > 0:
		if len(s.inFlight) != 0 {
			return metadata, pilotUploadMediaProbe
		}
		return pilotUploadRecoveryPlain, 0
	}
	threshold := s.healthThreshold()
	for _, start := range s.inFlight {
		if now.Sub(start) >= threshold {
			return metadata, pilotUploadMediaSlow
		}
	}
	return media, 0
}

func (s *pilotUploadSchedule) healthThreshold() time.Duration {
	return min(pilotUploadHealthMax, max(pilotUploadHealthMin, s.smoothed+3*s.variation))
}

func (s *pilotUploadSchedule) complete(done pilotUploadCompletion) (rotate bool) {
	start := s.inFlight[done.seq]
	delete(s.inFlight, done.seq)
	wasEarlyProbe := done.seq == s.earlyProbeSeq
	if done.demoted {
		if wasEarlyProbe {
			s.earlyProbeSeq = 0
		}
	} else if wasEarlyProbe && (errors.Is(done.err, errPilotTimingInvalid) || errors.Is(done.err, errPilotMediaBackoff)) {
		// These paths stop before the create attempt. Preserve its recovery
		// opportunity without treating local admission as media health.
		s.earlyProbeSeq = 0
	} else if errors.Is(done.err, errPilotUploadDeadline) {
		if !s.drain {
			s.cooldown = min(pilotUploadCooldownMax, max(pilotUploadCooldownMin, s.cooldown*2))
			s.drain = true
		}
		until := done.finished.Add(s.cooldown)
		if until.After(s.until) {
			s.until = until
		}
	} else if done.err == nil && !s.drain {
		s.cooldown, s.until = 0, time.Time{}
		s.earlyProbeSeq = 0
		if sample := done.finished.Sub(start); sample > 0 {
			if s.smoothed == 0 {
				s.smoothed, s.variation = sample, sample/2
			} else {
				difference := max(sample-s.smoothed, s.smoothed-sample)
				s.variation += (difference - s.variation) / 4
				s.smoothed += (sample - s.smoothed) / 8
			}
		}
	} else if wasEarlyProbe && !errors.Is(done.err, errPilotMediaBackoff) && !errors.Is(done.err, context.Canceled) {
		// An unsuccessful first probe does not open additional early attempts.
		// Let any provider-requested backoff take precedence in the publisher.
		s.cooldown = min(pilotUploadCooldownMax, 2*s.cooldown)
		s.until = done.finished.Add(s.cooldown)
	}
	if s.drain && len(s.inFlight) == 0 {
		s.drain = false
		return true
	}
	return false
}

type pilotAdaptiveUploads struct {
	// The loop rotates this after every old binary worker returns. The
	// preview's metadata, poll and media download workers use a separate pool.
	mediaHTTP                                                                              atomic.Pointer[GoogleHTTPClient]
	freshMediaHTTP                                                                         func(previous *GoogleHTTPClient) *GoogleHTTPClient
	mediaPoolResets, slotWaitTicks, quotaHoldTicks, mediaBusyBatches, mediaCooldownBatches atomic.Uint64
	mediaOverlapBatches, mediaSlowBatches, mediaDrainBatches, mediaProbeBatches            atomic.Uint64
	mediaHealthThresholdMS                                                                 atomic.Uint64
	firstRecoveryProbeDispatches                                                           atomic.Uint64
	tryEarlyRecovery                                                                       bool
	metadata, media                                                                        pilotUploadStats
}

func newPilotAdaptiveUploads(fresh func(*GoogleHTTPClient) *GoogleHTTPClient) *pilotAdaptiveUploads {
	uploads := &pilotAdaptiveUploads{freshMediaHTTP: fresh}
	uploads.mediaHTTP.Store(fresh(nil))
	return uploads
}

func (a *pilotAdaptiveUploads) resetMediaHTTP() {
	old := a.mediaHTTP.Load()
	a.mediaHTTP.Store(a.freshMediaHTTP(old))
	a.closeHTTP(old)
	a.mediaPoolResets.Add(1)
}

func (a *pilotAdaptiveUploads) closeHTTP(client *GoogleHTTPClient) {
	if fallback, ok := client.client.Transport.(*googleH3FallbackTransport); ok {
		// Close the QUIC pool only after its workers have returned.
		if h3, ok := fallback.h3.(interface{ Close() error }); ok {
			_ = h3.Close()
		}
	}
	client.client.CloseIdleConnections()
}

func newPilotMediaUploadHTTP(shared, previous *GoogleHTTPClient) *GoogleHTTPClient {
	client := NewGoogleHTTPClient(shared.route)
	switch transport := client.client.Transport.(type) {
	case *http2.Transport:
		// A canceled stream may still be unwinding when the old pool is
		// retired. Let its connection close when it becomes idle.
		transport.IdleConnTimeout = 15 * time.Second
	case *googleH3FallbackTransport:
		if h2, ok := transport.h2.(*http2.Transport); ok {
			h2.IdleConnTimeout = 15 * time.Second
		}
		for _, source := range []*GoogleHTTPClient{shared, previous} {
			if source != nil {
				if from, ok := source.client.Transport.(*googleH3FallbackTransport); ok {
					transport.unavailableUntil.Store(max(transport.unavailableUntil.Load(), from.unavailableUntil.Load()))
				}
			}
		}
	}
	return client
}

type pilotUploadStats struct {
	batches, published, deadlines, deadlineDroppedPackets, quotaSkipped, canceled, other atomic.Uint64
	packets, plainBytes, maxPlainBytes, elapsedUS, maxElapsedUS                          atomic.Uint64
}

func pilotUploadMax(maximum *atomic.Uint64, n uint64) {
	for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
	}
}

func (s *pilotUploadStats) record(size, packets int, elapsed time.Duration, err error) {
	s.batches.Add(1)
	s.packets.Add(uint64(packets))
	s.plainBytes.Add(uint64(size))
	pilotUploadMax(&s.maxPlainBytes, uint64(size))
	s.elapsedUS.Add(uint64(elapsed.Microseconds()))
	pilotUploadMax(&s.maxElapsedUS, uint64(elapsed.Microseconds()))
	switch {
	case err == nil:
		s.published.Add(1)
	case errors.Is(err, errPilotUploadDeadline):
		s.deadlines.Add(1)
		s.deadlineDroppedPackets.Add(uint64(packets))
	case errors.Is(err, errPilotMediaBackoff):
		s.quotaSkipped.Add(1)
	case errors.Is(err, context.Canceled):
		s.canceled.Add(1)
	default:
		s.other.Add(1)
	}
}

func (a *pilotAdaptiveUploads) logClose(logger *log.Logger, role string) {
	logger.Printf("udp preview %s upload schedule media_busy_metadata_batches=%d media_cooldown_metadata_batches=%d both_slots_busy_ticks=%d quota_hold_ticks=%d media_pool_resets=%d media_overlap_batches=%d media_slow_metadata_batches=%d media_drain_metadata_batches=%d media_probe_metadata_batches=%d media_health_threshold_ms=%d media_first_recovery_probe_dispatches=%d",
		role, a.mediaBusyBatches.Load(), a.mediaCooldownBatches.Load(), a.slotWaitTicks.Load(), a.quotaHoldTicks.Load(), a.mediaPoolResets.Load(),
		a.mediaOverlapBatches.Load(), a.mediaSlowBatches.Load(), a.mediaDrainBatches.Load(), a.mediaProbeBatches.Load(), a.mediaHealthThresholdMS.Load(), a.firstRecoveryProbeDispatches.Load())
	a.metadata.logClose(logger, role, "metadata")
	a.media.logClose(logger, role, "media")
}

func (s *pilotUploadStats) logClose(logger *log.Logger, role, batchType string) {
	logger.Printf("udp preview %s upload batches kind=%s dispatched=%d published=%d request_deadlines=%d deadline_unconfirmed_packets=%d quota_skips=%d canceled=%d other_failures=%d packets=%d plain_bytes=%d max_plain_bytes=%d elapsed_ms_sum=%.3f elapsed_ms_max=%.3f",
		role, batchType, s.batches.Load(), s.published.Load(), s.deadlines.Load(), s.deadlineDroppedPackets.Load(), s.quotaSkipped.Load(), s.canceled.Load(), s.other.Load(),
		s.packets.Load(), s.plainBytes.Load(), s.maxPlainBytes.Load(), float64(s.elapsedUS.Load())/1000, float64(s.maxElapsedUS.Load())/1000)
}
