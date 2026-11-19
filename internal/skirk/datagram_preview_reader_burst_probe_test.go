package skirk

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The baseline helper reproduces P20's exact GET admission boundary: every
// 350ms list walks oldest first, reserves at most two reads, and forgets the
// unreserved IDs. It uses the production fetch and authenticated accept paths.
func legacyBurstPoll(ctx context.Context, p *pilotMailbox, objects []pilotObject) {
	active := make(map[uint64]bool)
	var mu sync.Mutex
	ticker := time.NewTicker(pilotPollTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, object := range slices.Backward(objects) {
			seq := p.unreadSequence(object)
			if seq == 0 {
				continue
			}
			size, _ := strconv.Atoi(object.Size)
			mu.Lock()
			if active[seq] || len(active) >= 2 {
				mu.Unlock()
				continue
			}
			active[seq] = true
			mu.Unlock()
			p.liveCalls.Go(func() {
				defer func() { mu.Lock(); delete(active, seq); mu.Unlock() }()
				op, cancel := context.WithTimeout(ctx, pilotMediaReadTimeout)
				defer cancel()
				wire, err := p.fetchMediaBatch(op, object.ID, size)
				if err == nil {
					p.mediaFetched.Add(1)
					p.acceptBatch(ctx, object.ID, seq, wire, p.mediaPlainLimit(), pilotMediaPacketLimit(p.mediaPlainLimit()))
				}
			})
		}
	}
}

type readerBurstResult struct {
	accepted, offered, published, dropped, expired uint64
	peakPackets, peakBytes, gets, publications     int
	firstPublished, lastPublished                  time.Duration
}

func runReaderBurstProbe(t *testing.T, legacy bool, objects, packets int, getDelay, publishDelay, phase time.Duration, mode ...pilotPublishMode) readerBurstResult {
	t.Helper()
	var result readerBurstResult
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		upload := &pilotUploadTestProvider{mediaDelay: publishDelay, metadataDelay: publishDelay}
		reply := newAdaptiveTestMailbox(t, upload)
		if len(mode) != 0 {
			reply.publishMode = mode[0]
		}
		reply.mediaBytes = pilotMediaWidePlain
		reply.queue = nil
		reply.fairQueue = newPilotMediaFairQueue(pilotMediaWidePlain)
		up, reads := mediaReaderTestMailbox(t, objects, pilotMediaWidePlain)
		target := netip.MustParseAddrPort("127.0.0.1:40000")
		up.allowed = map[netip.AddrPort]bool{target: true}
		reply.allowed = map[netip.AddrPort]bool{target: true}
		for seq := 1; seq <= objects; seq++ {
			batch := make([]pilotPacket, packets)
			for i := range batch {
				body := make([]byte, 1200)
				binary.BigEndian.PutUint32(body, uint32((seq-1)*packets+i+1))
				// A constructible backlog: one batch produced each250ms
				// before discovery resumes, all within the6s lifetime.
				batch[i] = pilotPacket{flow: 1<<32 | 1, sent: start.Add(-time.Duration(objects-seq) * pilotPublishTick), target: target, body: body}
			}
			wire, err := Seal(up.recvKey, up.tunnel.SessionID, up.recvDir, uint64(seq), pilotMarshal(batch), false)
			if err != nil {
				t.Fatal(err)
			}
			reads.bodies[seq] = wire
			reads.objects[objects-seq].Size = strconv.Itoa(len(wire))
		}
		reads.delay = func(_ *http.Request, _ mediaReaderTestGet) error { time.Sleep(getDelay); return nil }
		up.deliver = func(_ context.Context, packet pilotPacket) {
			// Zero-delay fixed-size echo models an already established local
			// origin. Real delivery, offer, DRR queue, and publisher are used.
			reply.offer(packet.flow, packet.target, packet.body)
			reply.fairQueue.mu.Lock()
			result.peakPackets = max(result.peakPackets, reply.fairQueue.packets)
			result.peakBytes = max(result.peakBytes, reply.fairQueue.bytes)
			reply.fairQueue.mu.Unlock()
		}
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		workers.Go(func() { time.Sleep(phase); reply.publishLoop(ctx) })
		if legacy {
			workers.Go(func() { legacyBurstPoll(ctx, up, reads.objects) })
		} else {
			up.mediaReader.wake = make(chan struct{}, 1)
			workers.Go(func() { up.poll(ctx) })
			workers.Go(func() { up.mediaReadWakeLoop(ctx) })
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		cancel()
		workers.Wait()
		up.liveCalls.Wait()
		reply.liveCalls.Wait()
		result.accepted = up.delivered.Load()
		result.offered = reply.offered.Load()
		result.published = reply.published.Load()
		result.dropped = reply.dropped.Load()
		result.expired = reply.stale.Load()
		gets, _, _ := reads.snapshot()
		result.gets = len(gets)
		attempts, _, _, _ := upload.snapshot()
		result.publications = len(attempts)
		if len(attempts) > 0 {
			result.firstPublished = attempts[0].at.Sub(start)
			result.lastPublished = attempts[len(attempts)-1].at.Sub(start)
		}
		if result.accepted != uint64(objects*packets) || result.offered != result.accepted || result.expired != 0 || result.published+result.dropped != result.offered {
			t.Fatalf("stage accounting inconsistent: %+v", result)
		}
	})
	return result
}

func TestReaderCatchupImmediateEchoBoundaryProbe(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, c := range []struct {
		name                string
		objects, packets    int
		get, publish, phase time.Duration
	}{
		{"four_full_100ms", 4, 104, 100 * time.Millisecond, 20 * time.Millisecond, 0},
		{"four_full_100ms_shifted", 4, 104, 100 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond},
		{"voice_backlog_100ms", 12, 25, 100 * time.Millisecond, 20 * time.Millisecond, 0},
		{"voice_backlog_50ms", 12, 25, 50 * time.Millisecond, 20 * time.Millisecond, 0},
		{"voice_backlog_100ms_slow_reply", 12, 25, 100 * time.Millisecond, 600 * time.Millisecond, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := runReaderBurstProbe(t, true, c.objects, c.packets, c.get, c.publish, c.phase)
			after := runReaderBurstProbe(t, false, c.objects, c.packets, c.get, c.publish, c.phase)
			t.Logf("P20 %+v", before)
			t.Logf("reader %+v", after)
			if c.name == "four_full_100ms" && (before.dropped != 0 || after.dropped != 103) {
				t.Fatal(fmt.Sprintf("expected isolated burst-boundary regression, before=%+v after=%+v", before, after))
			}
		})
	}
}
