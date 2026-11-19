package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

// Existing one-object fixtures skip the queue and dispatch directly.
func (p *pilotMailbox) publish(ctx context.Context, seq uint64, packets []pilotPacket) error {
	size := len(pilotDatagramMagic)
	for _, packet := range packets {
		size += pilotPacketHeader + len(packet.body)
	}
	batch := pilotPublication{packets: packets, size: size, binary: p.media && size > p.batchBytes}
	tails := make(chan []pilotPacket, 1)
	err := p.publishBatch(ctx, seq, &batch, tails)
	if len(tails) > 0 {
		return errors.New("multi-object fixture needs the publish loop")
	}
	return err
}

func activatePublicationEpoch(t *testing.T, p *pilotMailbox, epoch uint64) {
	t.Helper()
	if p.timing == nil {
		p.timing = newPilotTimingState()
	}
	now := time.Now()
	if err := p.timing.install(epoch, now, now); err != nil {
		t.Fatal(err)
	}
	if err := p.timing.activate(epoch); err != nil {
		t.Fatal(err)
	}
}

func TestUDPPreviewExpiryDuringUploadAdmission(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, tc := range []struct {
		name                           string
		old, oldSize, fresh, freshSize int
		objects, binaryObjects         int
		firstAmbiguous                 bool
	}{
		{"metadata", 1, 8, 63, 8, 1, 0, false},
		{"binary becomes metadata", 1, 1400, 63, 190, 1, 0, false},
		{"binary becomes two metadata objects", 9, 1300, 90, 65, 2, 0, false},
		{"binary remains binary", 1, 8, 80, 260, 1, 1, false},
		{"ambiguous first upload of split batch", 9, 1300, 90, 65, 2, 0, true},
		{"all expired", 2, 8, 0, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var wires [][]byte
				provider := &pilotUploadTestProvider{packetIDs: func(wire []byte) []uint32 {
					wires = append(wires, bytes.Clone(wire))
					return nil
				}}
				failedPackets, failedObjects := 0, 0
				if tc.firstAmbiguous {
					provider.delays = map[uint64]pilotUploadTestDelay{1: {delay: time.Millisecond, deadline: true}}
					failedPackets, failedObjects = pilotMaxBatch, 1
				}
				p := newAdaptiveTestMailbox(t, provider)
				p.queue = nil
				p.mediaBytes = pilotMediaWidePlain
				p.fairQueue = newPilotMediaFairQueue(p.mediaPlainLimit())
				p.publishMode = pilotPublishFullBinary
				p.separateUploads = false
				p.tunnel.uploadLimiter = newAdaptiveLimiter(1, 1, time.Second, "expiry-test", nil)
				target := netip.MustParseAddrPort("127.0.0.1:41280")
				p.allowed = map[netip.AddrPort]bool{target: true}
				activatePublicationEpoch(t, p, 1)

				receiver, backend := mediaReaderTestMailbox(t, 0, pilotMediaWidePlain)
				receiver.recvKey, receiver.recvDir, receiver.prefix = p.sendKey, p.sendDir, p.prefix
				receiver.allowed = p.allowed
				activatePublicationEpoch(t, receiver, 1)
				var ids []int
				receiver.deliver = func(_ context.Context, packet pilotPacket) {
					if packet.target != target || packet.flow != 2<<32|1 || len(packet.body) != tc.freshSize ||
						!bytes.Equal(packet.body, bytes.Repeat(packet.body[:1], tc.freshSize)) || packet.timingEpoch != 1 {
						t.Errorf("unexpected delivered packet, flow=%d bytes=%d epoch=%d", packet.flow, len(packet.body), packet.timingEpoch)
						return
					}
					ids = append(ids, int(packet.body[0]))
				}
				ctx, stop := context.WithCancel(t.Context())
				defer stop()
				origin := time.Now()
				release, err := p.tunnel.acquireUploadSlotBytes(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				for range tc.old {
					p.offer(1<<32|1, target, make([]byte, tc.oldSize))
				}
				time.Sleep(pilotLifetime - pilotPublishTick - time.Millisecond)
				go p.publishLoop(ctx)
				synctest.Wait()
				time.Sleep(pilotPublishTick - time.Millisecond)
				for i := 1; i <= tc.fresh; i++ {
					p.offer(2<<32|1, target, bytes.Repeat([]byte{byte(i)}, tc.freshSize))
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if p.seq != 1 || p.fairQueue.len() != 0 {
					t.Fatalf("expected one complete selection before expiry, seq=%d queued=%d", p.seq, p.fairQueue.len())
				}
				time.Sleep(2 * time.Millisecond)
				release(nil, 0)
				synctest.Wait()
				time.Sleep(2 * pilotPublishTick)
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				if len(attempts) != tc.objects || p.localQueueExpired.Load() != uint64(tc.old) || p.dropped.Load() != uint64(failedPackets) ||
					p.published.Load() != uint64(tc.fresh-failedPackets) || p.errors.Load() != uint64(failedObjects) || p.mediaUploaded.Load() != uint64(tc.binaryObjects) {
					t.Fatalf("uploads=%d expired=%d healthy drops=%d published=%d errors=%d binary=%d", len(attempts),
						p.localQueueExpired.Load(), p.dropped.Load(), p.published.Load(), p.errors.Load(), p.mediaUploaded.Load())
				}
				if p.adaptiveUploads.metadata.published.Load() != uint64(tc.objects-tc.binaryObjects-failedObjects) ||
					p.adaptiveUploads.media.published.Load() != uint64(tc.binaryObjects) {
					t.Fatal("completion attributed to the wrong transport")
				}

				var objects []pilotObject
				for i, a := range attempts {
					seq := i + 1
					if a.name != fmt.Sprintf("%s%020d", p.dirPrefix(p.sendDir), seq) {
						t.Fatalf("unexpected name or repeated sequence at upload %d", seq)
					}
					if i > 0 && a.at.Sub(origin) != pilotLifetime-time.Millisecond+pilotPublishTick {
						t.Fatalf("suffix after %v, expected an ordinary tick", a.at.Sub(attempts[i-1].at))
					}
					object := pilotObject{ID: fmt.Sprintf("object-%d", seq), Name: a.name}
					if a.pool == 0 {
						object.Description = base64.RawStdEncoding.EncodeToString(wires[i])
					} else {
						object.Size = strconv.Itoa(len(wires[i]))
						backend.bodies[seq] = wires[i]
					}
					objects = append(objects, object)
				}
				// A lost create response can still mean the object exists. The
				// receiver must see every original record at most once even then.
				receiver.readPage(ctx, objects)
				synctest.Wait()
				receiver.readPage(ctx, objects)
				synctest.Wait()
				var expected []int
				for i := 1; i <= tc.fresh; i++ {
					expected = append(expected, i)
				}
				gets, _, _ := backend.snapshot()
				if !slices.Equal(ids, expected) || receiver.rejected.Load() != 0 || len(gets) != tc.binaryObjects {
					t.Fatalf("received=%v rejected=%d media GETs=%d", ids, receiver.rejected.Load(), len(gets))
				}
			})
		})
	}
}

func TestUDPPreviewPublicationRetainsActiveEpoch(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{}
		p := newAdaptiveTestMailbox(t, provider)
		target := netip.MustParseAddrPort("127.0.0.1:41280")
		p.allowed = map[netip.AddrPort]bool{target: true}
		activatePublicationEpoch(t, p, 1)
		p.offer(1, target, []byte{1})
		activatePublicationEpoch(t, p, 2)
		p.offer(1, target, []byte{2})
		records := []pilotPacket{<-p.queue, <-p.queue}
		p.timing.abandon(1)
		var wire []byte
		provider.packetIDs = func(b []byte) []uint32 { wire = bytes.Clone(b); return nil }
		if err := p.publish(t.Context(), 1, records); err != nil {
			t.Fatal(err)
		}
		_, plain, err := OpenEnvelope(p.sendKey, wire)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := pilotUnmarshal(plain, p.batchBytes, pilotMaxBatch, false)
		if !ok || len(got) != 1 || got[0].timingEpoch != 2 || !bytes.Equal(got[0].body, []byte{2}) ||
			p.invalidTiming.Load() != 1 || p.localQueueExpired.Load() != 0 {
			t.Fatalf("records=%v invalid=%d expired=%d", got, p.invalidTiming.Load(), p.localQueueExpired.Load())
		}
	})
}

func TestUDPPreviewDemotedUploadKeepsBinaryRecoveryState(t *testing.T) {
	now := time.Now()
	for _, err := range []error{nil, errPilotUploadDeadline} {
		s := pilotUploadSchedule{inFlight: map[uint64]time.Time{5: now}, earlyProbeSeq: 5, cooldown: pilotUploadCooldownMin, until: now.Add(time.Second), smoothed: 200 * time.Millisecond}
		rotate := s.complete(pilotUploadCompletion{seq: 5, binary: true, demoted: true, err: err, finished: now.Add(50 * time.Millisecond)})
		if rotate || len(s.inFlight) != 0 || s.earlyProbeSeq != 0 || s.drain || s.cooldown != pilotUploadCooldownMin ||
			s.until != now.Add(time.Second) || s.smoothed != 200*time.Millisecond {
			t.Fatal("metadata result changed binary recovery or kept its slot")
		}
	}
}
