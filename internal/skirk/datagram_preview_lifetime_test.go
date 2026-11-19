package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewPublicLeaseStaysOpenForTraffic(t *testing.T) {
	for _, direction := range []string{"receive", "publish"} {
		t.Run(direction, func(t *testing.T) {
			t.Setenv("SKIRK_ACCESS_TOKEN", "")
			synctest.Test(t, func(t *testing.T) {
				preview := publicPreviewForLifetimeTest(t)
				ctx, cancel, activity := preview.leaseContext(t.Context())
				defer cancel()
				mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{1}, true, preview.allowed)
				if err != nil {
					t.Fatal(err)
				}
				mailbox.publicIPv4, mailbox.separateUploads, mailbox.activity = true, true, activity
				mailbox.deliver = func(context.Context, pilotPacket) {}
				target := netip.MustParseAddrPort("8.8.8.8:3478")
				for seq := uint64(1); seq <= 7; seq++ {
					time.Sleep(20 * time.Second)
					if err := ctx.Err(); err != nil {
						t.Fatalf("active lease expired after %s: %v", time.Duration(seq)*20*time.Second, err)
					}
					packet := pilotPacket{flow: 1, sent: time.Now(), target: target, body: []byte("fixture")}
					if direction == "receive" {
						readLeaseTestPacket(t, ctx, mailbox, seq, packet)
					} else {
						mailbox.publish(ctx, seq, []pilotPacket{packet})
					}
				}
				if got := mailbox.delivered.Load() + mailbox.published.Load(); got != 7 {
					t.Fatalf("valid datagrams = %d, want 7", got)
				}
				time.Sleep(udpPreviewPublicIdle - time.Millisecond)
				if err := ctx.Err(); err != nil {
					t.Fatalf("lease expired before the idle window: %v", err)
				}
				readLeaseTestPacket(t, ctx, mailbox, 8, pilotPacket{
					flow: 1, sent: time.Now(), target: netip.MustParseAddrPort("100.64.0.1:3478"),
				})
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if ctx.Err() == nil || mailbox.rejected.Load() != 1 {
					t.Fatalf("rejected traffic renewed the idle lease: error=%v rejected=%d", ctx.Err(), mailbox.rejected.Load())
				}
			})
		})
	}
}

func TestUDPPreviewPublicExitTeardown(t *testing.T) {
	for _, reason := range []string{"idle", "parent shutdown", "peer disconnect"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("SKIRK_ACCESS_TOKEN", "")
			synctest.Test(t, func(t *testing.T) {
				preview := publicPreviewForLifetimeTest(t)
				var logs bytes.Buffer
				preview.tunnel.Logger = log.New(&logs, "", 0)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				peer, exit := net.Pipe()
				defer peer.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer exit.Close()
					preview.serveExit(ctx, muxStreamKey{ClientID: "fixture-client", RunID: "fixture-run"}, exit, [exitProbeNonceBytes]byte{2})
				}()
				reply := make([]byte, len(udpPreviewReply)+2*exitProbeNonceBytes)
				if _, err := io.ReadFull(peer, reply); err != nil {
					t.Fatal(err)
				}
				switch reason {
				case "idle":
					time.Sleep(udpPreviewPublicIdle)
				case "parent shutdown":
					cancel()
				case "peer disconnect":
					_ = peer.Close()
				}
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("exit teardown did not finish")
				}
				if len(preview.slots) != 0 {
					t.Fatal("exit did not release its lease slot")
				}
				closeReason, age := "mux_or_peer_closed", int64(0)
				switch reason {
				case "idle":
					closeReason, age = "idle", udpPreviewPublicIdle.Milliseconds()
				case "parent shutdown":
					closeReason = "shutdown"
				}
				want := fmt.Sprintf("udp preview exit closed accepted=0 origin_sent=0 origin_received=0 published=0 expired=0 dropped=0 rejected=0 provider_errors=0 partial_lists=0 batch_bytes=8192 published_batches=0 received_batches=0 published_plain_bytes=0 accepted_payload_bytes=0 ingress_dropped=0 hev_reply_dropped=0 flow_cap=0 lease_cap_total=0 origin_dial_errors=0 origin_write_errors=0 hev_write_errors=0 reason=%s lease_age_ms=%d\n", closeReason, age)
				withMediaCounters := strings.TrimSuffix(want, "\n") + " media_uploaded=0 media_fetched=0 local_queue_expired=0 incoming_certainly_stale=0 invalid_timing=0\n"
				const timingLine = "udp timing role=exit epoch=1 control_ms=0 rate_bound_ppm=1000\n"
				if !strings.HasPrefix(logs.String(), timingLine) {
					t.Fatalf("missing bounded timing diagnostic %q", logs.String())
				}
				if got := strings.TrimPrefix(logs.String(), timingLine); got != withMediaCounters {
					t.Fatalf("unexpected exit summary %q", got)
				}
			})
		})
	}
}

func TestUDPPreviewPublicHEVReceivesAfterUplinkPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		preview := publicPreviewForLifetimeTest(t)
		ctx, cancel, _ := preview.leaseContext(t.Context())
		defer cancel()
		app, control := net.Pipe()
		defer app.Close()
		target := netip.MustParseAddrPort("8.8.8.8:3478")
		mailbox := &pilotMailbox{publicIPv4: true, queue: make(chan pilotPacket, 1)}
		flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow)}
		relay := &pilotClientRelay{publicIPv4: true, open: func() (*pilotMailbox, *pilotFlowTable, func(), error) {
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
		if err := writeAll(app, buildUDPInTCPFrame(target.Addr().String(), int(target.Port()), []byte("uplink"))); err != nil {
			t.Fatal(err)
		}
		packet := <-mailbox.queue
		time.Sleep(pilotHEVIdle + time.Second)
		packet.body = []byte("downlink")
		flows.returnToClient(ctx, packet)
		got, err := readUDPInTCPFrame(app)
		if err != nil || string(got.Payload) != "downlink" {
			t.Fatalf("downlink failed after uplink pause: %v", err)
		}
		cancel()
		<-done
	})
}

type leaseTestLoopbackConn struct{ net.Conn }

func (c leaseTestLoopbackConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func publicPreviewForLifetimeTest(t *testing.T) *datagramPreview {
	t.Helper()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{0x01}}
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
	drive := NewDriveStore(client, "owned-test-token", DriveConfig{FolderID: "isolated-drive-folder"})
	return &datagramPreview{tunnel: tunnel, drive: drive, http: client, publicIPv4: true, slots: make(chan struct{}, udpPreviewLeases)}
}

func readLeaseTestPacket(t *testing.T, ctx context.Context, mailbox *pilotMailbox, seq uint64, packet pilotPacket) {
	t.Helper()
	wire, err := Seal(mailbox.recvKey, mailbox.tunnel.SessionID, mailbox.recvDir, seq, pilotMarshal([]pilotPacket{packet}), false)
	if err != nil {
		t.Fatal(err)
	}
	mailbox.read(ctx, pilotObject{
		ID: "owned-metadata-object", Name: fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), seq),
		Description: base64.RawStdEncoding.EncodeToString(wire),
	})
}
