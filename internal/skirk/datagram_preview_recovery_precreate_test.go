package skirk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func recoveryPrecreateSchedule(now time.Time) pilotUploadSchedule {
	return pilotUploadSchedule{
		inFlight: map[uint64]time.Time{11: now}, cooldown: pilotUploadCooldownMin,
		until: now.Add(pilotUploadCooldownMin), smoothed: 700 * time.Millisecond,
		variation: 100 * time.Millisecond, allowEarlyProbe: true, earlyProbeSeq: 11,
	}
}

func TestPreviewEarlyRecoveryPrecreateSkipPreservesSchedule(t *testing.T) {
	for _, skip := range []error{errPilotTimingInvalid, errPilotMediaBackoff} {
		t.Run(skip.Error(), func(t *testing.T) {
			now := time.Now()
			schedule := recoveryPrecreateSchedule(now)
			until, smoothed, variation := schedule.until, schedule.smoothed, schedule.variation
			finished := now.Add(100 * time.Millisecond)
			if schedule.complete(pilotUploadCompletion{seq: 11, binary: true, finished: finished, err: fmt.Errorf("pre-create: %w", skip)}) {
				t.Fatal("local cancellation rotated a healthy media pool")
			}
			if schedule.earlyProbeSeq != 0 || len(schedule.inFlight) != 0 || schedule.drain ||
				schedule.cooldown != pilotUploadCooldownMin || schedule.until != until ||
				schedule.smoothed != smoothed || schedule.variation != variation {
				t.Fatalf("local cancellation changed recovery health: %+v", schedule)
			}
			if limit, reason := schedule.limit(finished, pilotWidePlain, pilotMediaWidePlain); limit != pilotUploadRecoveryPlain || reason != pilotUploadMediaFirstProbe {
				t.Fatal("fresh work could not use the unattempted recovery grant")
			}
		})
	}
}

func TestPreviewEarlyRecoveryProvider429IsNotPrecreateSkip(t *testing.T) {
	var attempts int
	mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
		attempts++
		response := stringResponse(http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`)
		response.Header.Set("Retry-After", "1")
		return response, nil
	})
	name := fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.sendDir), 11)
	err := mailbox.uploadMediaBatch(t.Context(), name, make([]byte, pilotMediaMinWire))
	var apiErr *GoogleAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || errors.Is(err, errPilotMediaBackoff) || attempts != 1 {
		t.Fatalf("provider rejection was confused with a local skip: %v attempts=%d", err, attempts)
	}
	now := time.Now()
	schedule := recoveryPrecreateSchedule(now)
	if schedule.complete(pilotUploadCompletion{seq: 11, binary: true, finished: now, err: err}) || schedule.earlyProbeSeq != 11 || schedule.cooldown != 2*pilotUploadCooldownMin {
		t.Fatal("an actual failed create released its early grant")
	}
	if !mailbox.drive.QuotaBackoffSnapshot().Active {
		t.Fatal("provider quota backoff was not retained")
	}
	if err := mailbox.uploadMediaBatch(t.Context(), name, make([]byte, pilotMediaMinWire)); !errors.Is(err, errPilotMediaBackoff) || attempts != 1 {
		t.Fatalf("the active hold permitted another create: %v attempts=%d", err, attempts)
	}
}

func TestPreviewEarlyRecoveryPrecreatePublisherBoundary(t *testing.T) {
	testPreviewEarlyRecoveryPrecreatePublisherBoundary(t, pilotPublishFixed)
}

func TestPublisherCreditsPrecreateBoundary(t *testing.T) {
	testPreviewEarlyRecoveryPrecreatePublisherBoundary(t, pilotPublishFullBinary)
}

func testPreviewEarlyRecoveryPrecreatePublisherBoundary(t *testing.T, mode pilotPublishMode) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, quota := range []bool{false, true} {
		name := "epoch_invalidated_after_collection"
		if quota {
			name = "quota_hold_after_selection"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotUploadTestProvider{stallPool: 1}
				mailbox := newAdaptiveTestMailbox(t, provider)
				mailbox.publishMode = mode
				mailbox.mediaBytes = pilotMediaWidePlain
				a := mailbox.adaptiveUploads
				a.tryEarlyRecovery = true
				timing := newPilotTimingState()
				var atPublish atomic.Bool
				timing.now = func() time.Time {
					now := time.Now()
					if atPublish.Swap(false) {
						if quota {
							mailbox.drive.backoff.Observe("fixture", &HTTPResult{Status: http.StatusTooManyRequests,
								Header: http.Header{"Retry-After": {"1"}}, Body: []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`)}, nil)
						} else {
							timing.mu.Lock()
							timing.invalidateLocked(now)
							timing.mu.Unlock()
						}
					}
					return now
				}
				install := func(id uint64) {
					t.Helper()
					now := time.Now()
					if err := timing.install(id, now, now); err != nil {
						t.Fatal(err)
					}
					if err := timing.activate(id); err != nil {
						t.Fatal(err)
					}
				}
				install(1)
				mailbox.timing = timing
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go mailbox.publishLoop(ctx)
				synctest.Wait()
				offerPilotUploadTestPackets(mailbox, 40, 1200)
				time.Sleep(pilotPublishTick + 2*time.Second + time.Millisecond)
				synctest.Wait()
				if a.mediaPoolResets.Load() != 1 {
					t.Fatal("initial timeout did not drain and rotate the pool")
				}
				offerPilotUploadTestPackets(mailbox, 40, 1200)
				// FIFO collection uses the supplied time directly. Its next
				// clock callback occurs in publish's final admission gate.
				atPublish.Store(true)
				time.Sleep(pilotPublishTick)
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				if atPublish.Load() || len(attempts) != 1 || a.firstRecoveryProbeDispatches.Load() != 1 {
					t.Fatal("the selected early batch did not stop before HTTP")
				}
				if quota {
					if a.media.quotaSkipped.Load() != 1 {
						t.Fatal("post-selection quota skip was not observed")
					}
				} else {
					if mailbox.invalidTiming.Load() == 0 || a.media.other.Load() != 0 {
						t.Fatal("local timing rejection was not isolated from upload stats")
					}
					install(2)
				}
				offerPilotUploadTestPackets(mailbox, 40, 1200)
				if quota {
					time.Sleep(pilotPublishTick)
					synctest.Wait()
					attempts, _, _, _ = provider.snapshot()
					if len(attempts) != 1 || a.firstRecoveryProbeDispatches.Load() != 1 || a.quotaHoldTicks.Load() == 0 {
						t.Fatal("released grant bypassed the active provider hold")
					}
					time.Sleep(3 * pilotPublishTick)
				} else {
					time.Sleep(pilotPublishTick)
				}
				synctest.Wait()
				attempts, _, _, _ = provider.snapshot()
				if len(attempts) != 2 || attempts[1].pool != 2 || attempts[1].plain > pilotUploadRecoveryPlain ||
					!strings.HasSuffix(attempts[1].name, "00000000000000000003") || a.firstRecoveryProbeDispatches.Load() != 2 {
					t.Fatalf("fresh work lost the unattempted probe or reused its sequence: %+v dispatches=%d", attempts, a.firstRecoveryProbeDispatches.Load())
				}
				cancel()
				synctest.Wait()
			})
		})
	}
}
