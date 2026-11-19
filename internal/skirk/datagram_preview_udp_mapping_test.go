package skirk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewStableMappingRequiresPublicPolicy(t *testing.T) {
	config := TunnelConfig{UDPPreviewStableUDP: true, UDPPreviewTargets: []string{"127.0.0.1:3478"}}
	if _, err := previewUDPBatchBytes(config); err == nil {
		t.Fatal("stable mapping accepted a loopback-only preview")
	}
	config.UDPPreviewPublicIPv4 = true
	if _, err := previewUDPBatchBytes(config); err != nil {
		t.Fatal(err)
	}
}

func TestUDPPreviewStableLeaseQuietWindow(t *testing.T) {
	for _, direction := range []string{"receive", "publish"} {
		t.Run(direction, func(t *testing.T) {
			t.Setenv("SKIRK_ACCESS_TOKEN", "")
			synctest.Test(t, func(t *testing.T) {
				preview := publicPreviewForLifetimeTest(t)
				preview.stableUDP = true
				ctx, cancel, activity := preview.leaseContext(t.Context())
				defer cancel()
				mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{1}, true, preview.allowed)
				if err != nil {
					t.Fatal(err)
				}
				mailbox.publicIPv4, mailbox.separateUploads, mailbox.activity = true, true, activity
				mailbox.deliver = func(context.Context, pilotPacket) {}
				time.Sleep(75 * time.Second)
				if ctx.Err() != nil {
					t.Fatal("lease did not survive beyond the prior preview's quiet interval")
				}
				packet := pilotPacket{flow: 1, sent: time.Now(), target: netip.MustParseAddrPort("8.8.8.8:3478"), body: []byte("fixture")}
				if direction == "receive" {
					readLeaseTestPacket(t, ctx, mailbox, 1, packet)
				} else {
					mailbox.publish(ctx, 1, []pilotPacket{packet})
				}
				time.Sleep(120*time.Second - time.Millisecond)
				if ctx.Err() != nil || mailbox.delivered.Load()+mailbox.published.Load() != 1 {
					t.Fatal("authenticated traffic did not restart the quiet interval")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if !errors.Is(context.Cause(ctx), errUDPPreviewIdle) {
					t.Fatal("quiet association did not expire after 120 seconds")
				}
			})
		})
	}
}

func TestUDPPreviewStableMappingReusesPortAndFiltersReplies(t *testing.T) {
	first, firstTarget := stableUDPTestPeer(t)
	second, secondTarget := stableUDPTestPeer(t)
	unsolicited, unsolicitedTarget := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(firstTarget, secondTarget, unsolicitedTarget)
	mapping, _ := stableUDPTestMapping(t, mailbox)
	ctx := t.Context()

	mapping.send(ctx, pilotPacket{flow: 1, target: firstTarget, body: []byte("first request")})
	firstSource := stableUDPRead(t, first, "first request")
	mapping.send(ctx, pilotPacket{flow: 2, target: secondTarget, body: []byte("second request")})
	secondSource := stableUDPRead(t, second, "second request")
	if firstSource != secondSource {
		t.Fatalf("exit endpoint changed with destination: %v then %v", firstSource, secondSource)
	}

	stableUDPWrite(t, unsolicited, firstSource, []byte("unsolicited reply from an unsent port"))
	stableUDPWrite(t, first, firstSource, make([]byte, 8192))
	stableUDPWrite(t, first, firstSource, []byte("first reply"))
	stableUDPWrite(t, second, firstSource, []byte("second reply"))
	seen := make(map[uint64]bool)
	for range 2 {
		select {
		case reply := <-mailbox.queue:
			switch string(reply.body) {
			case "first reply":
				if reply.flow != 1 || reply.target != firstTarget {
					t.Fatal("first reply was assigned to the wrong flow")
				}
			case "second reply":
				if reply.flow != 2 || reply.target != secondTarget {
					t.Fatal("second reply was assigned to the wrong flow")
				}
			default:
				t.Fatalf("inadmissible reply reached the mailbox: %d bytes", len(reply.body))
			}
			if seen[reply.flow] {
				t.Fatal("one flow received both replies")
			}
			seen[reply.flow] = true
		case <-time.After(time.Second):
			t.Fatal("valid UDP reply did not reach the mailbox")
		}
	}
	stableUDPWait(t, func() bool { return mailbox.rejected.Load() == 2 })
	if mailbox.originSent.Load() != 2 || mailbox.originReceived.Load() != 2 || len(mailbox.queue) != 0 {
		t.Fatal("only successfully written requests and solicited replies should count")
	}
}

func TestUDPPreviewStableMappingBoundsAdmissionsAndCloses(t *testing.T) {
	var peers []*net.UDPConn
	var targets []netip.AddrPort
	for range pilotMaxFlows + 1 {
		peer, target := stableUDPTestPeer(t)
		peers, targets = append(peers, peer), append(targets, target)
	}
	mailbox := stableUDPTestMailbox(targets...)
	mapping, cancel := stableUDPTestMapping(t, mailbox)
	ctx := t.Context()

	var source netip.AddrPort
	for index, target := range targets[:pilotMaxFlows] {
		request := fmt.Sprintf("request %d", index)
		mapping.send(ctx, pilotPacket{flow: uint64(index + 1), target: target, body: []byte(request)})
		observed := stableUDPRead(t, peers[index], request)
		if index != 0 && observed != source {
			t.Fatal("adding a new flow changed the exit endpoint")
		}
		source = observed
	}
	// One flow cannot change destination, and one destination cannot reuse a
	// different ID because the remote reply carries only its endpoint.
	mapping.send(ctx, pilotPacket{flow: 1, target: targets[1], body: []byte("renumber destination")})
	mapping.send(ctx, pilotPacket{flow: pilotMaxFlows + 1, target: targets[0], body: []byte("renumber flow")})
	mapping.send(ctx, pilotPacket{flow: pilotMaxFlows + 1, target: targets[pilotMaxFlows], body: []byte("too many flows")})
	mapping.send(ctx, pilotPacket{flow: 1, target: netip.MustParseAddrPort("100.64.0.1:3478"), body: []byte("reserved IP")})
	if mailbox.originSent.Load() != pilotMaxFlows || mailbox.flowCap.Load() != 1 || mailbox.rejected.Load() != 3 {
		t.Fatal("flow limit, uniqueness, or destination policy failed")
	}
	stableUDPWrite(t, peers[pilotMaxFlows], source, []byte("overflow reply"))
	stableUDPWait(t, func() bool { return mailbox.rejected.Load() == 4 })

	cancel()
	select {
	case <-mapping.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the reader and socket promptly")
	}
	if err := mapping.socket.SetWriteDeadline(time.Now()); err == nil {
		t.Fatal("reader teardown did not close its UDP socket")
	}
}

func TestUDPPreviewStableMappingFailedSendAdmitsNoPeer(t *testing.T) {
	_, target := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target)
	mapping, _ := stableUDPTestMapping(t, mailbox)
	_ = mapping.socket.Close()
	<-mapping.done
	mapping.send(t.Context(), pilotPacket{flow: 1, target: target, body: []byte("request")})
	if len(mapping.byFlow) != 0 || len(mapping.byEndpoint) != 0 || mailbox.originWriteErrors.Load() != 1 {
		t.Fatal("failed outbound write admitted a peer")
	}
}

func stableUDPTestPeer(t *testing.T) (*net.UDPConn, netip.AddrPort) {
	t.Helper()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	return peer, peer.LocalAddr().(*net.UDPAddr).AddrPort()
}

func stableUDPTestMailbox(targets ...netip.AddrPort) *pilotMailbox {
	allow := make(map[netip.AddrPort]bool, len(targets))
	for _, target := range targets {
		allow[target] = true
	}
	return &pilotMailbox{allowed: allow, publicIPv4: true, queue: make(chan pilotPacket, pilotMaxFlows)}
}

func stableUDPTestMapping(t *testing.T, mailbox *pilotMailbox) (*pilotUDPMapping, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	mapping, err := newPilotUDPMapping(ctx, mailbox, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		<-mapping.done
	})
	return mapping, cancel
}

func stableUDPRead(t *testing.T, peer *net.UDPConn, want string) netip.AddrPort {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [128]byte
	n, source, err := peer.ReadFromUDPAddrPort(data[:])
	if err != nil || string(data[:n]) != want {
		t.Fatalf("peer did not receive %q: %v", want, err)
	}
	return source
}

func stableUDPWrite(t *testing.T, peer *net.UDPConn, target netip.AddrPort, data []byte) {
	t.Helper()
	if _, err := peer.WriteToUDPAddrPort(data, target); err != nil {
		t.Fatal(err)
	}
}

func stableUDPWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("UDP reader did not record a rejected datagram")
		}
		time.Sleep(time.Millisecond)
	}
}
