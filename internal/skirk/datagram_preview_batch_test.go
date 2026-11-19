package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewBatchPolicy(t *testing.T) {
	for _, tc := range []struct {
		batchBytes int
		publicIPv4 bool
		want       int
	}{
		{0, false, pilotMaxPlain},
		{pilotMaxPlain, false, pilotMaxPlain},
		{pilotMaxPlain, true, pilotMaxPlain},
		{pilotWidePlain, true, pilotWidePlain},
		{pilotWidePlain, false, 0},
		{32768, true, 0},
		{-1, true, 0},
	} {
		got, err := previewUDPBatchBytes(TunnelConfig{UDPPreviewBatchBytes: tc.batchBytes, UDPPreviewPublicIPv4: tc.publicIPv4})
		if got != tc.want || (err != nil) != (tc.want == 0) {
			t.Fatalf("batch=%d public=%v got=%d err=%v want=%d", tc.batchBytes, tc.publicIPv4, got, err, tc.want)
		}
	}

	base := publicPreviewForLifetimeTest(t)
	cfg := &Config{Tunnel: TunnelConfig{UDPPreviewPublicIPv4: true}, Drive: DriveConfig{FolderID: base.drive.folderID}}
	base.tunnel.Data = base.drive
	var policies [3][32]byte
	for i, size := range []int{0, pilotMaxPlain, pilotWidePlain} {
		cfg.Tunnel.UDPPreviewBatchBytes = size
		preview, err := newDatagramPreview(base.tunnel, cfg)
		if err != nil {
			t.Fatal(err)
		}
		policies[i], err = preview.proof("client", "run", [exitProbeNonceBytes]byte{1}, [exitProbeNonceBytes]byte{2})
		if err != nil {
			t.Fatal(err)
		}
	}
	if policies[0] != policies[1] || policies[0] == policies[2] {
		t.Fatal("optional batch size changed the default proof or failed to bind 16 KiB")
	}
}

func TestUDPPreviewWideBatchesInOnePublishTick(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		preview := publicPreviewForLifetimeTest(t)
		mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{1}, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		mailbox.publicIPv4, mailbox.separateUploads, mailbox.batchBytes = true, true, pilotWidePlain
		for _, packet := range wideLeaseTestPackets(time.Now()) {
			mailbox.offer(packet.flow, packet.target, packet.body)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		time.Sleep(pilotPublishTick + time.Millisecond)
		synctest.Wait()
		if batches, plain := mailbox.publishedBatches.Load(), mailbox.publishedPlainBytes.Load(); batches != 1 || plain != pilotWidePlain {
			t.Fatalf("one publish tick wrote %d batches and %d plaintext bytes, queue=%d offered=%d errors=%d rejected=%d",
				batches, plain, len(mailbox.queue), mailbox.offered.Load(), mailbox.errors.Load(), mailbox.rejected.Load())
		}
	})
}

func TestUDPPreviewWideDenseList(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{pilotMaxListFiles, pilotMaxListFiles + 1} {
			preview := publicPreviewForLifetimeTest(t)
			mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{1}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			mailbox.publicIPv4, mailbox.batchBytes = true, pilotWidePlain
			packets := wideLeaseTestPackets(time.Now())
			plain := pilotMarshal(packets)
			if _, ok := pilotUnmarshal(plain, pilotMaxPlain, pilotMaxBatch, false); ok {
				t.Fatal("default receiver accepted a 16 KiB batch")
			}
			objects := make([]pilotObject, 0, count)
			for seq := count; seq > 0; seq-- {
				wire, err := Seal(mailbox.recvKey, mailbox.tunnel.SessionID, mailbox.recvDir, uint64(seq), plain, false)
				if err != nil {
					t.Fatal(err)
				}
				objects = append(objects, pilotObject{
					ID:          fmt.Sprintf("owned-object-%03d-%s", seq, strings.Repeat("x", 116)),
					Name:        fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), seq),
					Description: base64.RawStdEncoding.EncodeToString(wire),
				})
			}
			body, err := json.Marshal(map[string]any{"files": objects})
			if err != nil || len(body) > pilotMaxListBytes {
				t.Fatalf("dense encrypted listing size=%d err=%v", len(body), err)
			}
			mailbox.http = &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/drive/v3/files" || req.URL.Query().Get("pageSize") != "40" {
					return nil, fmt.Errorf("unexpected poll request")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
			})}}
			mailbox.deliver = func(context.Context, pilotPacket) {}
			ctx, cancel := context.WithCancel(t.Context())
			go mailbox.poll(ctx)
			synctest.Wait()
			time.Sleep(pilotPollTick)
			synctest.Wait()
			cancel()
			if count == pilotMaxListFiles {
				if mailbox.receivedBatches.Load() != uint64(count) || mailbox.delivered.Load() != uint64(count*len(packets)) {
					t.Fatalf("dense poll received %d batches and %d packets", mailbox.receivedBatches.Load(), mailbox.delivered.Load())
				}
			} else if mailbox.receivedBatches.Load() != 0 || mailbox.errors.Load() != 1 {
				t.Fatal("listing exceeded the 40-result limit")
			}
		}
	})
}

func TestUDPPreviewBatchHEVReplyHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		batchBytes, mediaBytes int
		burst, wantQueued      int
	}{
		{"default metadata", pilotMaxPlain, 0, 2 * pilotMaxBatch, pilotMaxFlows},
		{"16 KiB metadata", pilotWidePlain, 0, 2 * pilotMaxBatch, 2 * pilotMaxBatch},
		{"128 KiB media", pilotWidePlain, pilotMediaWidePlain, 2 * pilotMediaWideBatch, 2 * pilotMediaWideBatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				app, control := net.Pipe()
				defer app.Close()
				target := netip.MustParseAddrPort("8.8.8.8:3478")
				mailbox := &pilotMailbox{publicIPv4: true, batchBytes: tc.batchBytes, queue: make(chan pilotPacket, 1)}
				flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow)}
				relay := &pilotClientRelay{publicIPv4: true, batchBytes: tc.batchBytes, mediaBytes: tc.mediaBytes, open: func() (*pilotMailbox, *pilotFlowTable, func(), error) {
					return mailbox, flows, func() {}, nil
				}}
				done := make(chan struct{})
				go func() {
					defer close(done)
					pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{control})
				}()
				var reply [10]byte
				if _, err := io.ReadFull(app, reply[:]); err != nil || reply[1] != 0 {
					t.Fatalf("HEV association failed: %v", err)
				}
				if err := writeAll(app, buildUDPInTCPFrame(target.Addr().String(), int(target.Port()), []byte{0})); err != nil {
					t.Fatal(err)
				}
				packet := <-mailbox.queue
				flows.returnToClient(ctx, packet)
				synctest.Wait() // Android has not yet drained the first reply.
				for i := 1; i <= tc.burst; i++ {
					packet.body = []byte{byte(i)}
					flows.returnToClient(ctx, packet)
				}
				wantDropped := uint64(tc.burst - tc.wantQueued)
				if mailbox.hevReplyDropped.Load() != wantDropped || mailbox.ingressDropped.Load() != 0 || mailbox.dropped.Load() != wantDropped {
					t.Fatalf("unexpected HEV burst counter: hev=%d ingress=%d total=%d want=%d", mailbox.hevReplyDropped.Load(), mailbox.ingressDropped.Load(), mailbox.dropped.Load(), wantDropped)
				}
				for i := 0; i <= tc.wantQueued; i++ {
					got, err := readUDPInTCPFrame(app)
					if err != nil || !bytes.Equal(got.Payload, []byte{byte(i)}) {
						t.Fatalf("HEV reply %d failed: %v", i, err)
					}
				}
				cancel()
				<-done
			})
		})
	}
}

func wideLeaseTestPackets(now time.Time) []pilotPacket {
	packets := make([]pilotPacket, 16)
	target := netip.MustParseAddrPort("8.8.8.8:3478")
	remaining := pilotWidePlain - len(pilotDatagramMagic) - len(packets)*pilotPacketHeader
	for i := range packets {
		size := remaining / (len(packets) - i)
		remaining -= size
		packets[i] = pilotPacket{flow: 1, sent: now, target: target, body: bytes.Repeat([]byte{byte(i)}, size)}
	}
	return packets
}
