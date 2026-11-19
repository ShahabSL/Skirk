package skirk

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func holdPilotQuota(d *DriveStore, seconds string) {
	d.backoff.Observe("create", &HTTPResult{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {seconds}}}, nil)
}

func TestPilotDiscoveryWaitsThroughQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		var firstBudget time.Duration
		provider := &pilotDiscoveryTestProvider{answer: func([]string) pilotDiscoveryPage { return pilotDiscoveryPage{} }}
		provider.before = func(req *http.Request, _ []string) error {
			if firstBudget == 0 {
				deadline, _ := req.Context().Deadline()
				firstBudget = time.Until(deadline)
			}
			return nil
		}
		mailbox := pilotDiscoveryTestMailboxes(t, provider, 1)[0]
		holdPilotQuota(mailbox.drive, "8")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.poll(ctx)
		// A separate store sharing the same transport can still poll.
		healthy := pilotDiscoveryTestMailboxes(t, provider, 1)[0]
		if _, err := healthy.listDiscovery(ctx, []string{healthy.dirPrefix(healthy.recvDir)}); err != nil {
			t.Fatal(err)
		}
		firstBudget = 0
		time.Sleep(3 * time.Second)
		holdPilotQuota(mailbox.drive, "8")
		time.Sleep(8*time.Second - time.Millisecond)
		synctest.Wait()
		if calls, failures := len(provider.snapshot()), mailbox.errors.Load(); calls != 1 || failures != 0 {
			t.Fatalf("before extended cooldown ended: calls=%d (one independent), mailbox errors=%d", calls, failures)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls := len(provider.snapshot()); calls < 2 || firstBudget != pilotDiscoveryDeadline {
			t.Fatalf("at cooldown expiry: calls=%d, resumed request budget=%s, want %s", calls, firstBudget, pilotDiscoveryDeadline)
		}
		cancel()
		synctest.Wait()
	})
}

func TestPilotDiscoveryGroupQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, cancelEarly := range []bool{false, true} {
		name := "resume"
		if cancelEarly {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotDiscoveryTestProvider{answer: func([]string) pilotDiscoveryPage { return pilotDiscoveryPage{} }}
				mailboxes := pilotDiscoveryTestMailboxes(t, provider, 2)
				holdPilotQuota(mailboxes[0].drive, "8")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var group []*pilotDiscoveryReceiver
				for _, mailbox := range mailboxes {
					group = append(group, &pilotDiscoveryReceiver{ctx: t.Context(), mailbox: mailbox,
						prefix: mailbox.dirPrefix(mailbox.recvDir), pages: make(chan pilotDiscoveryPage, 2)})
				}
				done := make(chan bool, 1)
				go func() { done <- pollPilotDiscoveryGroup(ctx, group) }()
				time.Sleep(3 * time.Second)
				synctest.Wait()
				if len(done) != 0 || len(provider.snapshot()) != 0 {
					t.Fatal("combined poll ended or reached the provider during a known cooldown")
				}
				wantCalls := 1
				if cancelEarly {
					cancel()
					wantCalls = 0
				} else {
					time.Sleep(5 * time.Second)
				}
				synctest.Wait()
				select {
				case split := <-done:
					if split {
						t.Fatal("time spent waiting for quota incorrectly split a healthy group")
					}
				default:
					t.Fatal("poll did not finish promptly")
				}
				if len(provider.snapshot()) != wantCalls {
					t.Fatalf("provider calls=%d, want %d", len(provider.snapshot()), wantCalls)
				}
				for _, r := range group {
					if len(r.pages) != 0 {
						t.Fatal("receiver received a synthetic failure page")
					}
				}
			})
		})
	}
}

func TestPilotDiscoveryCountsProviderTimeoutAfterQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotDiscoveryTestProvider{before: func(req *http.Request, _ []string) error {
			<-req.Context().Done()
			return req.Context().Err()
		}}
		mailbox := pilotDiscoveryTestMailboxes(t, provider, 1)[0]
		holdPilotQuota(mailbox.drive, "8")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.poll(ctx)
		time.Sleep(8*time.Second + pilotDiscoveryDeadline - time.Millisecond)
		synctest.Wait()
		if failures := mailbox.errors.Load(); failures != 0 || len(provider.snapshot()) != 1 {
			t.Fatalf("before the actual RPC timed out: failures=%d, calls=%d", failures, len(provider.snapshot()))
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if failures := mailbox.errors.Load(); failures != 1 {
			t.Fatalf("actual provider deadline: failures=%d, want 1", failures)
		}
	})
}

func TestPilotDeleteRetainsDequeuedObjectDuringQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox := pilotDiscoveryTestMailboxes(t, &pilotDiscoveryTestProvider{}, 1)[0]
		var calls atomic.Int32
		mailbox.http.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodDelete || req.URL.Path != "/drive/v3/files/test-object" {
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
			calls.Add(1)
			return stringResponse(http.StatusNoContent, ""), nil
		})
		holdPilotQuota(mailbox.drive, "8")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		mailbox.cleanup <- "test-object"
		go mailbox.deleteLoop(ctx)
		time.Sleep(8*time.Second - time.Millisecond)
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("delete reached the provider during cooldown")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("deletes at cooldown expiry=%d, want 1", calls.Load())
		}
	})
}

func TestPilotPublishCountsProviderRejectionOnce(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{limitMetadata: true}
		mailbox := newAdaptiveTestMailbox(t, provider)
		offerPilotUploadTestPackets(mailbox, 2, 160)
		if err := mailbox.publish(t.Context(), 1, []pilotPacket{<-mailbox.queue}); err == nil {
			t.Fatal("expected provider rejection")
		}
		// A second worker can already have left the scheduler when the first
		// worker's provider response puts their shared DriveStore on hold.
		if err := mailbox.publish(t.Context(), 2, []pilotPacket{<-mailbox.queue}); !errors.Is(err, errPilotMediaBackoff) {
			t.Fatalf("expected a local quota skip, got %v", err)
		}
		attempts, _, _, _ := provider.snapshot()
		if len(attempts) != 1 || mailbox.errors.Load() != 1 || mailbox.dropped.Load() != 2 {
			t.Fatalf("physical creates=%d, provider errors=%d, dropped packets=%d, want 1/1/2",
				len(attempts), mailbox.errors.Load(), mailbox.dropped.Load())
		}
	})
}

func TestPilotLegacyPublishReleasesSlotOnNewQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{}
		mailbox := newAdaptiveTestMailbox(t, provider)
		mailbox.adaptiveUploads, mailbox.separateUploads = nil, false
		mailbox.tunnel.uploadLimiter = newAdaptiveLimiter(1, 1, time.Second, "test", nil)
		release, err := mailbox.tunnel.acquireUploadSlotBytes(t.Context(), false)
		if err != nil {
			t.Fatal(err)
		}
		offerPilotUploadTestPackets(mailbox, 1, 160)
		done := make(chan error, 1)
		go func() { done <- mailbox.publish(t.Context(), 1, []pilotPacket{<-mailbox.queue}) }()
		synctest.Wait()
		// Another worker receives a rate limit while the batch awaits its slot.
		holdPilotQuota(mailbox.drive, "8")
		release(nil, 0)
		time.Sleep(25 * time.Millisecond)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, errPilotMediaBackoff) {
				t.Fatalf("expected a quota skip, got %v", err)
			}
		default:
			t.Fatal("metadata worker occupied the shared slot through cooldown")
		}
		attempts, _, _, _ := provider.snapshot()
		if len(attempts) != 0 || mailbox.errors.Load() != 0 || mailbox.dropped.Load() != 1 {
			t.Fatalf("physical creates=%d, provider errors=%d, dropped=%d, want 0/0/1",
				len(attempts), mailbox.errors.Load(), mailbox.dropped.Load())
		}
	})
}

func TestPilotLegacyMetadataRefreshesRejectedTokenOnce(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, alwaysRejected := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			mailbox := newAdaptiveTestMailbox(t, &pilotUploadTestProvider{})
			mailbox.adaptiveUploads = nil
			source := NewAccessTokenSource(AuthConfig{AccessToken: "replacement"}, RouteConfig{Mode: "direct"})
			source.token = "previous"
			source.expiresAt = time.Now().Add(time.Hour)
			mailbox.drive.tokenSource = source
			var calls int
			mailbox.http.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if req.Header.Get("Authorization") != "Bearer previous" {
						t.Fatal("first attempt did not use the cached token")
					}
					return stringResponse(http.StatusUnauthorized, "{}"), nil
				}
				if calls > 2 || req.Header.Get("Authorization") != "Bearer replacement" {
					t.Fatal("expected exactly one retry using the refreshed token")
				}
				if alwaysRejected {
					return stringResponse(http.StatusUnauthorized, "{}"), nil
				}
				return stringResponse(http.StatusOK, `{"id":"owned","name":"owned-name"}`), nil
			})
			err := mailbox.publishDescription(t.Context(), "owned-name", []byte("data"))
			if (err != nil) != alwaysRejected || calls != 2 || mailbox.drive.QuotaSnapshot().Calls != 2 {
				t.Fatalf("always rejected=%t, err=%v, requests=%d, quota=%d", alwaysRejected, err, calls, mailbox.drive.QuotaSnapshot().Calls)
			}
		})
	}
}
