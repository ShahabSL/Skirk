package skirk

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

func creditModeName(mode pilotPublishMode) string {
	return [...]string{"fixed", "full_binary"}[mode]
}

// These fixtures have no delayed pre-create token/slot admission. Under that
// assumption the observed HTTP starts must obey the same dispatch envelope.
func assertPilotCreditHTTPEnvelope(t *testing.T, attempts []pilotUploadAttempt) {
	t.Helper()
	for i := range attempts {
		for j := i + 2; j < len(attempts); j++ {
			if attempts[j].at.Sub(attempts[i].at) < time.Duration(j-i-1)*pilotPublishTick {
				t.Fatalf("HTTP starts exceeded cap2/rate4 envelope between%d and%d", i, j)
			}
		}
	}
}

func TestPublisherCreditsBoundaryMatrix(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, phase := range []time.Duration{0, 100 * time.Millisecond} {
		for _, mode := range []pilotPublishMode{pilotPublishFixed, pilotPublishFullBinary} {
			t.Run(fmt.Sprintf("%s/phase_%s", creditModeName(mode), phase), func(t *testing.T) {
				result := runReaderBurstProbe(t, false, 4, 104, 100*time.Millisecond, 20*time.Millisecond, phase, mode)
				t.Logf("result=%+v", result)
				if mode == pilotPublishFullBinary && result.dropped != 0 {
					t.Fatal("full binary credits failed to drain bounded catch-up")
				}
			})
		}
	}
}

func TestPublisherCreditsPressureMatrix(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, workload := range []cooldownFixture{
		{name: "healthy_100hz", packets: 1, step: 10 * time.Millisecond, bursts: 1260},
		{name: "recovery_100hz", packets: 1, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true},
		{name: "mixed_small_flow", packets: 35, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true, mixed: true},
		{name: "fresh_pool_stall", packets: 1, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true, freshStall: true},
		{name: "persistent_outage", packets: 1, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true, outage: true},
		{name: "wall_recovery_100hz", packets: 1, step: 10 * time.Millisecond, bursts: 1260, wallFault: true},
		{name: "wall_persistent_100hz", packets: 1, step: 10 * time.Millisecond, bursts: 1260, wallOutage: true},
		{name: "wall_mixed_small_flow", packets: 35, step: 10 * time.Millisecond, bursts: 1260, wallFault: true, mixed: true},
	} {
		for _, mode := range []pilotPublishMode{pilotPublishFixed, pilotPublishFullBinary} {
			t.Run(workload.name+"/"+creditModeName(mode), func(t *testing.T) {
				result := observeCooldownFixture(t, workload, true, true, mode)
				t.Logf("matrix=%+v", result)
				if result.probes > 1 {
					t.Fatal("more than one first recovery probe")
				}
			})
		}
	}
}

func TestPublisherCreditsStartupAndQuota(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, mode := range []pilotPublishMode{pilotPublishFullBinary} {
		t.Run(creditModeName(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotUploadTestProvider{}
				p := newAdaptiveTestMailbox(t, provider)
				p.publishMode, p.mediaBytes = mode, pilotMediaWidePlain
				p.queue = nil
				p.fairQueue = newPilotMediaFairQueue(p.mediaPlainLimit())
				target := netip.MustParseAddrPort("127.0.0.1:40000")
				p.allowed = map[netip.AddrPort]bool{target: true}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); p.publishLoop(ctx) }()
				p.offer(1<<32|1, target, make([]byte, 64))
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				want := 0
				if len(attempts) != want {
					t.Fatalf("initial small dispatches=%d want%d", len(attempts), want)
				}
				time.Sleep(pilotPublishTick)
				synctest.Wait()
				p.drive.backoff.Observe("fixture", &HTTPResult{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"8"}}, Body: []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`)}, nil)
				before, _, _, _ := provider.snapshot()
				for range 104 {
					p.offer(1<<32|1, target, make([]byte, 1200))
				}
				time.Sleep(7 * time.Second)
				synctest.Wait()
				after, _, _, _ := provider.snapshot()
				if len(after) != len(before) {
					t.Fatal("credit wake bypassed active quota")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if p.localQueueExpired.Load() != 104 {
					t.Fatal("quota wait renewed old queue lifetime")
				}
				cancel()
				<-done
				p.liveCalls.Wait()
			})
		})
	}
}

func TestPublisherCreditsOrdinaryMetadataCadence(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{metadataDelay: 470 * time.Millisecond}
		p := newAdaptiveTestMailbox(t, provider)
		p.publishMode, p.mediaBytes = pilotPublishFullBinary, pilotMediaWidePlain
		p.queue = nil
		p.fairQueue = newPilotMediaFairQueue(p.mediaPlainLimit())
		target := netip.MustParseAddrPort("127.0.0.1:40000")
		p.allowed = map[netip.AddrPort]bool{target: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); p.publishLoop(ctx) }()
		start := time.Now()
		for range 150 {
			p.offer(1<<32|1, target, []byte{1})
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		attempts, _, peak, _ := provider.snapshot()
		for _, attempt := range attempts {
			if attempt.pool != 0 || attempt.at.Sub(start)%pilotPublishTick != 0 {
				t.Fatalf("metadata dispatched off normal tick: %+v", attempt)
			}
		}
		if p.published.Load() != 150 || peak > 2 {
			t.Fatalf("published=%d peak=%d", p.published.Load(), peak)
		}
		cancel()
		<-done
		p.liveCalls.Wait()
	})
}

func TestPublisherCreditsDispatchEnvelope(t *testing.T) {
	start := time.Now()
	credits := newPilotPublishCredits(start)
	var attempts []pilotUploadAttempt
	for i := range 2000 {
		now := start.Add(time.Duration(i) * time.Millisecond)
		for credits.delay(now) == 0 {
			credits.take(now)
			attempts = append(attempts, pilotUploadAttempt{at: now})
		}
	}
	if len(attempts) != 9 {
		t.Fatalf("dispatches=%d want9", len(attempts))
	}
	assertPilotCreditHTTPEnvelope(t, attempts)
	if credits.delay(start) < time.Millisecond {
		t.Fatal("clock regression minted credit")
	}
	credits.update(start.Add(100 * 365 * 24 * time.Hour))
	if credits.available != 2*pilotPublishTick {
		t.Fatal("idle credit is not capped at2")
	}
}

func TestPartialMediaGroupDelaysAtMostOneOrdinaryTick(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{}
		mailbox := newAdaptiveTestMailbox(t, provider)
		mailbox.publishMode, mailbox.mediaBytes = pilotPublishFullBinary, pilotMediaWidePlain
		mailbox.queue = nil
		mailbox.fairQueue = newPilotMediaFairQueue(mailbox.mediaPlainLimit())
		target := netip.MustParseAddrPort("127.0.0.1:40000")
		mailbox.allowed = map[netip.AddrPort]bool{target: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			mailbox.publishLoop(ctx)
		}()
		defer func() {
			cancel()
			<-done
			mailbox.liveCalls.Wait()
		}()
		synctest.Wait()
		start := time.Now()
		offer := func(group uint64, size int) {
			mailbox.offer(group<<32|1, target, make([]byte, size))
		}
		assertStarts := func(expected ...time.Duration) {
			t.Helper()
			synctest.Wait()
			attempts, _, _, _ := provider.snapshot()
			if len(attempts) != len(expected) {
				t.Fatalf("after %v, got %d HTTP starts, want %v", time.Since(start), len(attempts), expected)
			}
			for i, elapsed := range expected {
				if got := attempts[i].at.Sub(start); got != elapsed {
					t.Fatalf("HTTP start %d at %v, want %v", i, got, elapsed)
				}
			}
		}

		// A full first group publishes between ticks using the existing credits.
		const offset = 20 * time.Millisecond
		time.Sleep(offset)
		for range pilotMediaWidePlain / pilotFairQuantum {
			offer(1, pilotMaxPayload)
		}
		assertStarts(offset)
		// A second association's sparse packet shares this partial group.
		offer(1, 64)
		offer(2, 64)
		time.Sleep(pilotPublishTick - offset)
		assertStarts(offset)
		time.Sleep(pilotPublishTick)
		assertStarts(offset, 2*pilotPublishTick)

		// A lone packet after a group and a new group after a lone packet
		// both flush on their first ordinary tick.
		offer(2, 64)
		time.Sleep(pilotPublishTick)
		assertStarts(offset, 2*pilotPublishTick, 3*pilotPublishTick)
		offer(1, 64)
		offer(2, 64)
		time.Sleep(pilotPublishTick)
		assertStarts(offset, 2*pilotPublishTick, 3*pilotPublishTick, 4*pilotPublishTick)

		// An idle tick consumes the opportunity even if the next burst is dense.
		time.Sleep(pilotPublishTick)
		synctest.Wait()
		offer(1, 64)
		offer(2, 64)
		time.Sleep(pilotPublishTick)
		assertStarts(offset, 2*pilotPublishTick, 3*pilotPublishTick, 4*pilotPublishTick, 6*pilotPublishTick)
	})
}

func TestMediaCoalescingUsesWireSizeAndPacketLimit(t *testing.T) {
	const limit = pilotMediaWidePlain
	roomForOne := limit - len(pilotDatagramMagic) - pilotFairQuantum
	for _, tc := range []struct {
		name           string
		packets, bytes int
		canGrow        bool
	}{
		{"empty", 0, 0, false},
		{"single", 1, 70, false},
		{"room_for_one_full_record", 2, roomForOne, true},
		{"one_byte_short", 2, roomForOne + 1, false},
		{"packet_limit", pilotMediaPacketLimit(limit), 20 << 10, false},
		{"above_packet_limit", pilotMediaPacketLimit(limit) + 1, 20 << 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &pilotFairQueue{packets: tc.packets, bytes: tc.bytes}
			if q.canCoalesce(limit) != tc.canGrow {
				t.Fatalf("canCoalesce(%d packets, %d queued bytes) = %v, want %v", tc.packets, tc.bytes, !tc.canGrow, tc.canGrow)
			}
		})
	}
}
