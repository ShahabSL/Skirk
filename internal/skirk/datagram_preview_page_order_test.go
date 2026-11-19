package skirk

import (
	"context"
	"encoding/base64"
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

var pageOrderTarget = netip.MustParseAddrPort("127.0.0.1:3478")

func pageOrderObject(t *testing.T, p *pilotMailbox, id string, seq uint64, marker byte, media bool) (pilotObject, []byte) {
	t.Helper()
	flow := uint64(marker%2+1)<<32 | 1
	packets := []pilotPacket{{flow: flow, target: pageOrderTarget, sent: time.Now(), body: []byte{marker}}}
	if media {
		packets = pilotMediaTestPackets(time.Now(), pageOrderTarget, 24<<10, 24)
		for i := range packets {
			packets[i].flow = flow
			packets[i].body[0] = marker
		}
	}
	wire, err := Seal(p.recvKey, p.tunnel.SessionID, p.recvDir, seq, pilotMarshal(packets), false)
	if err != nil {
		t.Fatal(err)
	}
	object := pilotObject{ID: id, Name: fmt.Sprintf("%s%020d", p.dirPrefix(p.recvDir), seq)}
	if media {
		object.Size = strconv.Itoa(len(wire))
	} else {
		object.Description = base64.RawStdEncoding.EncodeToString(wire)
	}
	return object, wire
}

func TestUDPPreviewReadPageOrdersReadyDescriptions(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("media_enabled_%t", mixed), func(t *testing.T) {
			p, _ := mediaReaderTestMailbox(t, 0, pilotMediaWidePlain)
			p.media = mixed
			p.publicIPv4 = false
			p.allowed = map[netip.AddrPort]bool{pageOrderTarget: true}
			var got []byte
			var flows []uint64
			p.deliver = func(_ context.Context, packet pilotPacket) {
				got = append(got, packet.body[0])
				flows = append(flows, packet.flow)
			}
			makeDescription := func(id string, seq uint64, marker byte) pilotObject {
				object, _ := pageOrderObject(t, p, id, seq, marker, false)
				return object
			}
			low := makeDescription("low", 1, 1)
			middle := makeDescription("middle", 3, 2)
			high := makeDescription("high", 70, 3)
			replay := low
			replay.ID = "duplicate-low"
			wrongWire := middle
			wrongWire.Name = fmt.Sprintf("%s%020d", p.dirPrefix(p.recvDir), 2)
			badDesc := makeDescription("malformed-description", 6, 4)
			badDesc.Description = "!"
			wrongScope := middle
			wrongScope.Name = "unrelated/prefix/00000000000000000004"
			wrongID := makeDescription("no-id", 5, 5)
			wrongID.ID = ""

			// Reversing newest-create first puts high first here. Its
			// publisher sequence is more than one replay window past low.
			p.readPage(t.Context(), []pilotObject{badDesc, wrongScope, replay, low, middle, wrongWire, wrongID, high})
			if want := []byte{1, 2, 3}; !slices.Equal(got, want) {
				t.Fatalf("ready page delivery %v, want %v", got, want)
			}
			if len(flows) != 3 || flows[0] == flows[1] || flows[0] != flows[2] {
				t.Fatalf("did not cover independent flows in one ready page: %v", flows)
			}
			if p.rejected.Load() != 2 || p.receivedBatches.Load() != 3 {
				t.Fatalf("auth or replay behavior changed: rejects=%d accepted_batches=%d", p.rejected.Load(), p.receivedBatches.Load())
			}
			next := makeDescription("next", 72, 5)
			p.readPage(t.Context(), []pilotObject{replay, high, next})
			if want := []byte{1, 2, 3, 5}; !slices.Equal(got, want) {
				t.Fatalf("repeated page delivered prior objects: %v", got)
			}
			var claimed []string
			for len(p.cleanup) != 0 {
				claimed = append(claimed, <-p.cleanup)
			}
			if want := []string{"low", "middle", "high", "next"}; !slices.Equal(claimed, want) {
				t.Fatalf("cleanup escaped exact validated fresh objects: %v", claimed)
			}
		})
	}
}

func TestUDPPreviewReadPageStillCompletesMediaIndependently(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		p, provider := mediaReaderTestMailbox(t, 0, pilotMediaWidePlain)
		p.publicIPv4 = false
		p.allowed = map[netip.AddrPort]bool{pageOrderTarget: true}
		desc3, _ := pageOrderObject(t, p, "desc-3", 3, 3, false)
		desc4, _ := pageOrderObject(t, p, "desc-4", 4, 4, false)
		media1, wire1 := pageOrderObject(t, p, "object-1", 1, 1, true)
		media2, wire2 := pageOrderObject(t, p, "object-2", 2, 2, true)
		duplicate := media1
		duplicate.ID = "object-3"
		provider.bodies[1], provider.bodies[2], provider.bodies[3] = wire1, wire2, wire1
		provider.delay = func(req *http.Request, get mediaReaderTestGet) error {
			delay := 50 * time.Millisecond
			if get.id == 1 {
				delay = 200 * time.Millisecond
			}
			select {
			case <-req.Context().Done():
				return req.Context().Err()
			case <-time.After(delay):
				return nil
			}
		}
		var mu sync.Mutex
		var delivery []byte
		p.deliver = func(_ context.Context, packet pilotPacket) {
			mu.Lock()
			delivery = append(delivery, packet.body[0])
			mu.Unlock()
		}
		assertDelivered := func(want ...byte) {
			t.Helper()
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(delivery, want) {
				t.Fatalf("delivery %v, want %v", delivery, want)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		now := time.Now()
		p.readPage(ctx, []pilotObject{desc3, media1, duplicate, media2, desc4})
		synctest.Wait()
		assertDelivered(3, 4)
		if gets, _, maxActive := provider.snapshot(); len(gets) != 2 || maxActive != 2 || !time.Now().Equal(now) {
			t.Fatal("ordering waited for media or altered its two-slot concurrency")
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		afterSecond := append([]byte{3, 4}, slices.Repeat([]byte{2}, 24)...)
		assertDelivered(afterSecond...)
		time.Sleep(150 * time.Millisecond)
		p.liveCalls.Wait()
		assertDelivered(append(afterSecond, slices.Repeat([]byte{1}, 24)...)...)
		if gets, _, _ := provider.snapshot(); len(gets) != 2 || p.rejected.Load() != 0 || p.receivedBatches.Load() != 4 {
			t.Fatal("mixed-page replay or authentication changed")
		}
	})
}
