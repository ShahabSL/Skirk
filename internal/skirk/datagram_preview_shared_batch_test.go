package skirk

import (
	"net/netip"
	"testing"
	"time"
)

func TestSharedBinaryBatchWithSmallPacketPrefix(t *testing.T) {
	const meta = 16 << 10
	const wide = pilotMediaWidePlain
	const narrow = pilotMediaMaxPlain
	for _, tc := range []struct {
		name                                string
		n, encodedBytes, limit, wantPackets int
		early, binary                       bool
	}{
		{"partial binary waits for the next tick", 96, 18000, wide, 0, true, false},
		{"partial reaches binary", 96, 18000, wide, 96, false, true},
		{"metadata bound exact", 96, meta, wide, 64, false, false},
		{"metadata bound plus one", 96, meta + 1, wide, 96, false, true},
		{"narrow receiver retains its packet limit", 96, 18000, narrow, 64, false, false},
		{"metadata-only credit", 96, 18000, meta, 64, false, false},
		{"128-packet credit", 128, 19000, wide, 128, true, true},
		{"127 packets wait", 127, 19000, wide, 0, true, false},
		{"129 packets leaves one", 129, 19000, wide, 128, true, true},
		{"128 tiny never early", 128, 12000, wide, 0, true, false},
		{"128 tiny metadata only", 128, 12000, wide, 64, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &pilotMailbox{media: true, mediaBytes: wide, batchBytes: meta, fairQueue: newPilotMediaFairQueue(wide)}
			now := time.Now()
			target := netip.MustParseAddrPort("127.0.0.1:41971")
			recordBytes := tc.encodedBytes - len(pilotDatagramMagic)
			for i := 0; i < tc.n; i++ {
				payloadBytes := recordBytes/tc.n - pilotPacketHeader
				if i < recordBytes%tc.n {
					payloadBytes++
				}
				body := make([]byte, payloadBytes)
				body[0] = byte(i)
				packet := pilotPacket{flow: uint64(i%4+1)<<32 | 1, target: target, sent: now, body: body}
				if dropped, stale := p.fairQueue.offer(packet); dropped != 0 || stale != 0 {
					t.Fatalf("queue dropped %d records and expired %d", dropped, stale)
				}
			}
			var packets []pilotPacket
			var size int
			if tc.early {
				packets, size = p.collectFullFairBatch(tc.limit)
			} else {
				packets, size = p.collectFairBatch(tc.limit)
			}
			if len(packets) != tc.wantPackets || p.fairQueue.len() != tc.n-tc.wantPackets {
				t.Fatalf("consumed %d, remaining %d", len(packets), p.fairQueue.len())
			}
			if len(packets) == 0 {
				return
			}
			if (size > meta) != tc.binary {
				t.Fatalf("size=%d", size)
			}
			raw := pilotMarshal(packets)
			if len(raw) != size {
				t.Fatalf("serialized and selected byte counts differ")
			}
			byteLimit, countLimit := meta, pilotMaxBatch
			if tc.binary {
				byteLimit, countLimit = wide, pilotMediaPacketLimit(wide)
			}
			decoded, ok := pilotUnmarshal(raw, byteLimit, countLimit, false)
			if !ok || len(decoded) != len(packets) {
				t.Fatal("existing receiver rejected selected batch")
			}
			last := map[uint64]int{}
			seen := map[byte]bool{}
			for _, p := range decoded {
				seq := p.body[0]
				if seen[seq] || int(seq)+1 <= last[p.flow] {
					t.Fatal("duplicate or per-association reordering")
				}
				seen[seq] = true
				last[p.flow] = int(seq) + 1
			}
		})
	}
}

func TestSharedBinaryBatchCountsExpiredPacketsOnce(t *testing.T) {
	const wide = pilotMediaWidePlain
	p := &pilotMailbox{media: true, mediaBytes: wide, batchBytes: 16 << 10, fairQueue: newPilotMediaFairQueue(wide)}
	now := time.Now()
	target := netip.MustParseAddrPort("127.0.0.1:41971")
	for i := range 132 {
		sent := now
		if i < 4 {
			sent = now.Add(-2 * pilotLifetime)
		}
		p.fairQueue.offer(pilotPacket{flow: 1<<32 | 1, sent: sent, target: target, body: make([]byte, 150)})
	}
	packets, _ := p.collectFullFairBatch(wide)
	if len(packets) != 128 || p.fairQueue.len() != 0 || p.localQueueExpired.Load() != 4 {
		t.Fatalf("selected=%d remaining=%d expired=%d", len(packets), p.fairQueue.len(), p.localQueueExpired.Load())
	}
}
