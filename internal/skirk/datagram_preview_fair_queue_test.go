package skirk

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestUDPPreviewFairQueueMixedAssociationPressure(t *testing.T) {
	legacy := &pilotMailbox{publicIPv4: true, batchBytes: pilotWidePlain, queue: make(chan pilotPacket, pilotQueuePackets)}
	fair := &pilotMailbox{publicIPv4: true, batchBytes: pilotWidePlain, fairQueue: newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)}
	target := netip.MustParseAddrPort("8.8.8.8:3478") // Policy fixture only. No sockets use this address.
	for _, arm := range []*pilotMailbox{legacy, fair} {
		for range 12 {
			arm.offer(2<<32|1, target, make([]byte, 140))
		}
		for range 160 {
			arm.offer(1<<32|1, target, make([]byte, 1200))
			arm.offer(3<<32|1, target, make([]byte, 1200))
		}
	}
	if len(legacy.queue) != pilotQueuePackets || fair.fairQueue.len() != pilotQueuePackets {
		t.Fatal("queues did not retain the same aggregate packet budget")
	}
	var legacySmall int
	for len(legacy.queue) != 0 {
		if uint32((<-legacy.queue).flow>>32) == 2 {
			legacySmall++
		}
	}
	var delivered [4]int
	initial := -1
	for i := range pilotQueuePackets {
		packet, ok, _ := fair.fairQueue.take(time.Now(), func(pilotPacket) bool { return true })
		if !ok {
			t.Fatalf("fair queue idled at packet %d with work remaining", i)
		}
		association := uint32(packet.flow >> 32)
		if association == 2 && initial == -1 {
			initial = i
		}
		delivered[association]++
	}
	if legacySmall != 0 || delivered[2] != 12 || initial > 2 || delivered[1] != 58 || delivered[3] != 58 ||
		fair.ingressDropped.Load() != legacy.ingressDropped.Load() || fair.fairQueue.bytes != 0 {
		t.Fatalf("mixed pressure: FIFO small=%d fair counts=%v first-small-index=%d drops=%d/%d", legacySmall, delivered, initial, fair.ingressDropped.Load(), legacy.ingressDropped.Load())
	}
}

func TestUDPPreviewFairQueueByteLimitPackingAndCleanup(t *testing.T) {
	const small, large = 100, 1200
	q := newPilotFairQueue(8, 2*(large+pilotPacketHeader)+small+pilotPacketHeader)
	now := time.Now()
	add := func(id uint64, length int, sent time.Time) int {
		dropped, _ := q.offer(pilotPacket{flow: id<<32 | 1, sent: sent, body: make([]byte, length)})
		return dropped
	}
	add(1, large, now.Add(-time.Millisecond))
	add(1, large, now)
	add(2, small, now)
	if dropped := add(1, large, now); dropped != 1 || q.bytes != q.maxBytes || q.packets != 3 {
		t.Fatalf("byte-only limit not enforced: drops=%d bytes=%d packets=%d", dropped, q.bytes, q.packets)
	}
	got, ok, stale := q.take(now, func(packet pilotPacket) bool { return len(packet.body) == small })
	if !ok || stale != 0 || got.flow>>32 != 2 {
		t.Fatal("a packet that did not fit blocked a different association")
	}
	got, ok, _ = q.take(now, func(pilotPacket) bool { return true })
	if !ok || got.sent != now || q.closeAssociation(1) != 1 || q.packets != 0 || q.bytes != 0 || len(q.groups) != 0 {
		t.Fatal("oldest-overload eviction or exact association cleanup failed")
	}
	add(3, small, now.Add(-pilotLifetime))
	add(4, small, now)
	got, ok, stale = q.take(now, func(pilotPacket) bool { return true })
	if !ok || stale != 1 || got.flow>>32 != 4 || q.packets != 0 || q.bytes != 0 {
		t.Fatal("stale packet consumed budget or held a fresh packet")
	}
}

func TestUDPPreviewFairQueueMixedPacketsCoexistWithBulkTCP(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, target := stableUDPTestPeer(t)
	meta := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	_, _, socks := startUDPPreviewTestPeers(t, ctx, "matching shared peers", target, NewMemoryStore(), meta)
	origin, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	request := bytes.Repeat([]byte("deterministic bulk payload"), (1<<20)/26)
	accepted := make(chan error, 1)
	originDone := make(chan error, 1)
	go func() {
		peer, err := origin.Accept()
		accepted <- err
		if err != nil {
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(14 * time.Second))
		got := make([]byte, len(request))
		if _, err := io.ReadFull(peer, got); err != nil || !bytes.Equal(got, request) {
			originDone <- io.ErrUnexpectedEOF
			return
		}
		originDone <- writeAll(peer, got)
	}()
	tcp, err := DialViaSOCKS5(ctx, "socks5://"+socks, origin.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	_ = tcp.SetDeadline(time.Now().Add(14 * time.Second))
	tcpDone := make(chan error, 1)
	go func() { tcpDone <- writeAll(tcp, request) }()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("bulk origin did not receive the connection")
	}
	mailbox := &pilotMailbox{allowed: map[netip.AddrPort]bool{target: true}, publicIPv4: true,
		batchBytes: pilotWidePlain, fairQueue: newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)}
	for range 32 {
		mailbox.offer(2<<32|1, target, make([]byte, 140))
		for range 160 {
			mailbox.offer(1<<32|1, target, make([]byte, 1200))
		}
		batch, size := mailbox.collectFairBatch(pilotWidePlain)
		if len(batch) == 0 || size > pilotWidePlain || !containsFairQueueAssociation(batch, 2) {
			t.Fatal("burst prevented small association from joining a bounded batch")
		}
	}
	response := make([]byte, len(request))
	if _, err := io.ReadFull(tcp, response); err != nil || !bytes.Equal(response, request) {
		t.Fatalf("bulk TCP response changed under isolated mixed workload: %v", err)
	}
	if writeErr, originErr := <-tcpDone, <-originDone; writeErr != nil || originErr != nil {
		t.Fatalf("bulk TCP transfer failed: client=%v exit=%v", writeErr, originErr)
	}
}

func containsFairQueueAssociation(packets []pilotPacket, id uint32) bool {
	for _, packet := range packets {
		if uint32(packet.flow>>32) == id {
			return true
		}
	}
	return false
}
