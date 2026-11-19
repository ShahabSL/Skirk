package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

type cooldownFixture struct {
	name          string
	packets       int
	step          time.Duration
	bursts        int
	firstDeadline bool
	freshStall    bool
	outage        bool
	mixed         bool
	wallFault     bool
	wallOutage    bool
}

type cooldownObservation struct {
	offered, published, dropped, expired, unconfirmed int
	attempts, cooldown, probes, rotations, deadlines  int
	p95                                               time.Duration
	quietOffered, quietPublished, quietDropped        int
	quietP95                                          time.Duration
	confirmedBy1s, confirmedBy2s, confirmedBy3s       int
}

func TestPreviewReceiveBurstQueueLimits(t *testing.T) {
	baseline := newPilotMediaFairQueue(pilotMediaMaxPlain)
	if baseline.maxPackets != pilotQueuePackets || baseline.maxBytes != pilotFairQueueBytes {
		t.Fatal("standard media changed the established ingress budget")
	}
	wide := newPilotMediaFairQueue(pilotMediaWidePlain)
	if wide.maxPackets != 256 || wide.maxBytes != 262136 {
		t.Fatalf("unexpected wide-media budget: %d packets, %d bytes", wide.maxPackets, wide.maxBytes)
	}
	const first = uint64(1)<<32 | 1
	const second = uint64(2)<<32 | 1
	start := time.Now()
	for _, bodyLen := range []int{100, 1200} {
		queue := newPilotMediaFairQueue(pilotMediaWidePlain)
		packetsPerBatch := min(pilotMediaWideBatch, (pilotMediaWidePlain-len(pilotDatagramMagic))/(pilotPacketHeader+bodyLen))
		for _, flow := range []uint64{first, second} {
			for range packetsPerBatch {
				if dropped, stale := queue.offer(pilotPacket{flow: flow, sent: start, body: make([]byte, bodyLen)}); dropped != 0 || stale != 0 {
					t.Fatalf("two receive batches did not fit with %d-byte bodies", bodyLen)
				}
			}
		}
		want := 2 * packetsPerBatch
		if queue.len() != want {
			t.Fatalf("queued %d packets, want %d", queue.len(), want)
		}
		// When the old entries fill the count or byte budget, fresh arrivals
		// displace existing backlog without exceeding either bound.
		overflows := 0
		for range 4 {
			dropped, stale := queue.offer(pilotPacket{flow: first, sent: start, body: make([]byte, bodyLen)})
			overflows += dropped
			if stale != 0 || queue.packets > queue.maxPackets || queue.bytes > queue.maxBytes {
				t.Fatalf("queue escaped its hard bound with %d-byte bodies", bodyLen)
			}
		}
		if overflows == 0 || queue.closeAssociation(1) == 0 || queue.len() == 0 {
			t.Fatal("pressure or association close lost isolation")
		}
		_, ok, stale := queue.take(start.Add(pilotLifetime), func(pilotPacket) bool { return true })
		if ok || stale == 0 || queue.len() != 0 {
			t.Fatal("widened queue retained stale entries")
		}
	}
}

func TestPreviewFirstRecoveryProbeOnlyConsumesBinaryAndHonorsQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, quota := range []bool{false, true} {
		name := "small_batch_keeps_probe"
		if quota {
			name = "global_provider_hold"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotUploadTestProvider{stallPool: 1, limitMetadata: quota}
				mailbox := newAdaptiveTestMailbox(t, provider)
				a := mailbox.adaptiveUploads
				a.tryEarlyRecovery = true
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go mailbox.publishLoop(ctx)
				synctest.Wait()
				offerPilotUploadTestPackets(mailbox, 40, 1200)
				time.Sleep(pilotPublishTick + 2*time.Second + time.Millisecond)
				synctest.Wait()
				if a.mediaPoolResets.Load() != 1 {
					t.Fatal("first media pool did not drain")
				}
				offerPilotUploadTestPackets(mailbox, 8, 160)
				time.Sleep(pilotPublishTick)
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				if len(attempts) != 2 || attempts[1].pool != 0 || a.firstRecoveryProbeDispatches.Load() != 0 {
					t.Fatal("metadata consumed the first binary recovery probe")
				}
				offerPilotUploadTestPackets(mailbox, 40, 1200)
				time.Sleep(pilotPublishTick)
				synctest.Wait()
				attempts, _, _, _ = provider.snapshot()
				if quota {
					if len(attempts) != 2 || a.firstRecoveryProbeDispatches.Load() != 0 || a.quotaHoldTicks.Load() == 0 {
						t.Fatal("recovery bypassed Drive quota backoff")
					}
				} else if len(attempts) != 3 || attempts[2].pool != 2 || attempts[2].plain > pilotUploadRecoveryPlain || a.firstRecoveryProbeDispatches.Load() != 1 {
					t.Fatal("first recovery was not one bounded create on a fresh pool")
				}
			})
		})
	}
}

func TestPreviewCooldownPressureReproduction(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, workload := range []cooldownFixture{
		{name: "healthy", packets: 35, step: 350 * time.Millisecond, bursts: 36},
		{name: "burst_first_deadline", packets: 35, step: 350 * time.Millisecond, bursts: 36, firstDeadline: true},
		{name: "paced_first_deadline", packets: 1, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true},
		{name: "mixed_call_burst_first_deadline", packets: 35, step: 10 * time.Millisecond, bursts: 1260, firstDeadline: true, mixed: true},
		{name: "unhealthy_fresh_pool", packets: 35, step: 350 * time.Millisecond, bursts: 36, firstDeadline: true, freshStall: true},
		{name: "persistent_provider_outage", packets: 35, step: 350 * time.Millisecond, bursts: 36, firstDeadline: true, outage: true},
	} {
		t.Run(workload.name, func(t *testing.T) {
			observations := make(map[string]cooldownObservation)
			for _, variant := range []struct {
				name            string
				probe, headroom bool
			}{
				{name: "frozen"},
				{name: "early_probe", probe: true},
				{name: "receive_headroom", headroom: true},
				{name: "combined", probe: true, headroom: true},
			} {
				t.Run(variant.name, func(t *testing.T) {
					observations[variant.name] = observeCooldownFixture(t, workload, variant.probe, variant.headroom)
				})
			}
			baseline, early, headroom, combined := observations["frozen"], observations["early_probe"],
				observations["receive_headroom"], observations["combined"]
			switch {
			case !workload.firstDeadline:
				if early != baseline || headroom != baseline || combined != baseline || baseline.dropped != 0 || baseline.published != baseline.offered {
					t.Fatalf("the healthy publisher changed: %+v", observations)
				}
			case workload.freshStall, workload.outage:
				if early.probes != 1 || combined.probes != 1 || early.deadlines < 2 || combined.deadlines < 2 || early.rotations < 2 || combined.rotations < 2 {
					t.Fatalf("early recovery must allow one attempt in a bad fresh pool before normal backoff: %+v", observations)
				}
			default:
				if baseline.deadlines != 1 || baseline.dropped == 0 || early.deadlines != 1 || early.probes != 1 ||
					early.dropped >= baseline.dropped || headroom.dropped >= baseline.dropped || combined.dropped > min(early.dropped, headroom.dropped) {
					t.Fatalf("recovery variants failed to reduce queued loss: %+v", observations)
				}
			}
		})
	}
}

func observeCooldownFixture(t *testing.T, workload cooldownFixture, enableEarly, enableHeadroom bool, mode ...pilotPublishMode) (result cooldownObservation) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{mediaDelay: 700 * time.Millisecond, metadataDelay: 470 * time.Millisecond}
		if workload.firstDeadline {
			provider.stallMediaFromSeq = 8
		}
		if workload.freshStall {
			provider.stallPool = 2
		}
		if workload.outage {
			provider.stallAllFromSeq = 8
		}
		mailbox := newAdaptiveTestMailbox(t, provider)
		if len(mode) != 0 {
			mailbox.publishMode = mode[0]
		}
		a := mailbox.adaptiveUploads
		a.tryEarlyRecovery = enableEarly
		mailbox.mediaBytes = pilotMediaWidePlain
		mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
		if enableHeadroom {
			mailbox.fairQueue = newPilotMediaFairQueue(mailbox.mediaPlainLimit())
		}
		target := netip.MustParseAddrPort("127.0.0.1:3478")
		mailbox.allowed = map[netip.AddrPort]bool{target: true}
		provider.packetIDs = func(wire []byte) []uint32 {
			_, plain, err := OpenEnvelope(mailbox.sendKey, wire)
			if err != nil {
				return nil
			}
			packets, ok := pilotUnmarshal(plain, pilotMediaWidePlain, pilotMediaPacketLimit(pilotMediaWidePlain), false)
			if !ok {
				return nil
			}
			ids := make([]uint32, len(packets))
			for i, packet := range packets {
				ids[i] = binary.BigEndian.Uint32(packet.body)
			}
			return ids
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		if workload.wallFault || workload.wallOutage {
			provider.wallFaultFrom = start.Add(2 * time.Second)
			provider.wallFaultUntil = start.Add(2600 * time.Millisecond)
			if workload.wallOutage {
				provider.wallFaultUntil = start.Add(time.Hour)
			}
		}
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		const bulkFlow = uint64(1)<<32 | 1
		const quietFlow = uint64(2)<<32 | 1
		body := bytes.Repeat([]byte{0x5a}, 1200)
		quietBody := bytes.Repeat([]byte{0xa5}, 160)
		type issuance struct {
			time  time.Time
			quiet bool
		}
		var issued []issuance
		offer := func(flow uint64, body []byte, quiet bool) {
			binary.BigEndian.PutUint32(body, uint32(len(issued)))
			issued = append(issued, issuance{time.Now(), quiet})
			mailbox.offer(flow, target, body)
		}
		for i := range workload.bursts {
			previous := mailbox.ingressDropped.Load()
			if !workload.mixed || i%35 == 0 {
				for range workload.packets {
					offer(bulkFlow, body, false)
				}
			}
			if workload.mixed && i%5 == 2 {
				offer(quietFlow, quietBody, true)
			}
			if dropped := mailbox.ingressDropped.Load() - previous; dropped != 0 && workload.packets > 1 {
				t.Logf("overflow at=%s dropped=%d queued=%d", time.Since(start), dropped, mailbox.fairQueue.len())
			}
			time.Sleep(workload.step)
			synctest.Wait()
		}
		time.Sleep(7 * time.Second)
		synctest.Wait()
		cancel()
		synctest.Wait()
		attempts, active, maxActive, rotations := provider.snapshot()
		if mailbox.publishMode != pilotPublishFixed {
			assertPilotCreditHTTPEnvelope(t, attempts)
		}
		previous := start
		seen := make(map[uint32]bool)
		var confirmed, quietConfirmed []time.Duration
		for n, attempt := range attempts {
			if (mailbox.publishMode == pilotPublishFixed && attempt.at.Sub(previous) < pilotPublishTick) || len(attempt.packetIDs) == 0 {
				t.Fatalf("create %d exceeded cadence or had a malformed fixture batch", n+1)
			}
			previous = attempt.at
			for _, id := range attempt.packetIDs {
				if seen[id] || int(id) >= len(issued) {
					t.Fatalf("answer repeated or altered the fixture packet ID %d", id)
				}
				seen[id] = true
				if attempt.status == 200 {
					elapsed := attempt.completion.Sub(issued[id].time)
					confirmed = append(confirmed, elapsed)
					if issued[id].quiet {
						quietConfirmed = append(quietConfirmed, elapsed)
					}
				}
			}
			if workload.firstDeadline && workload.packets > 1 {
				t.Logf("create sequence=%d at=%s complete=%s rpc=%s pool=%d plain_bytes=%d status=%d packet_ids=%v",
					n+1, attempt.at.Sub(start), attempt.completion.Sub(start), attempt.completion.Sub(attempt.at),
					attempt.pool, attempt.plain, attempt.status, attempt.packetIDs)
			}
		}
		p95 := func(samples []time.Duration) time.Duration {
			if len(samples) == 0 {
				return 0
			}
			slices.Sort(samples)
			return samples[(len(samples)*95+99)/100-1]
		}
		result = cooldownObservation{
			offered: len(issued), published: int(mailbox.published.Load()), dropped: int(mailbox.ingressDropped.Load()),
			expired: int(mailbox.stale.Load()), unconfirmed: int(a.media.deadlineDroppedPackets.Load() + a.metadata.deadlineDroppedPackets.Load()),
			attempts: len(attempts), cooldown: int(a.mediaCooldownBatches.Load()), probes: int(a.firstRecoveryProbeDispatches.Load()),
			rotations: len(rotations), deadlines: int(a.media.deadlines.Load()), p95: p95(confirmed), quietP95: p95(quietConfirmed), quietPublished: len(quietConfirmed),
		}
		for _, elapsed := range confirmed {
			if elapsed <= time.Second {
				result.confirmedBy1s++
			}
			if elapsed <= 2*time.Second {
				result.confirmedBy2s++
			}
			if elapsed <= 3*time.Second {
				result.confirmedBy3s++
			}
		}
		for id, item := range issued {
			if item.quiet {
				result.quietOffered++
				if !seen[uint32(id)] {
					result.quietDropped++
				}
			}
		}
		if active != 0 || maxActive > 2 || mailbox.fairQueue.len() != 0 || result.published != len(confirmed) || len(seen) != result.published+result.unconfirmed ||
			len(issued)-len(seen) != result.dropped+result.expired {
			t.Fatalf("failed to account for every issued packet under its worker and queue budget: %+v", result)
		}
		t.Logf("observation=%+v max_active=%d", result, maxActive)
		if workload.firstDeadline && workload.packets > 1 {
			missing := make([]int, 0, result.dropped)
			for id := range issued {
				if !seen[uint32(id)] {
					missing = append(missing, id)
				}
			}
			t.Logf("dropped_or_expired_before_dispatch_ids=%v", missing)
		}
	})
	return result
}
