package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func sharedExitTestManager(t *testing.T, mailbox *pilotMailbox) *pilotSharedUDPExit {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := newPilotSharedUDPExit(ctx, mailbox, pilotSharedMaxAssociations, nil)
	t.Cleanup(func() { cancel(); <-s.done })
	return s
}

func sharedExitTestFlow(association, flow uint32) uint64 {
	return uint64(association)<<32 | uint64(flow)
}

func sharedExitTestGroupCount(s *pilotSharedUDPExit) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.groups)
}

func TestUDPPreviewSharedExitIsolatesSameEndpointAndSourcePorts(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(map[bool]string{false: "ipv4", true: "ipv6"}[ipv6], func(t *testing.T) {
			var peer *net.UDPConn
			var target netip.AddrPort
			if ipv6 {
				peer, target = previewIPv6TestPeer(t)
			} else {
				peer, target = stableUDPTestPeer(t)
			}
			mailbox := stableUDPTestMailbox(target)
			mailbox.publicIPv6 = ipv6
			s := sharedExitTestManager(t, mailbox)
			var sources [2]netip.AddrPort
			for i := range 2 {
				s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(uint32(i+1), 1), target: target, body: []byte("request")})
				sources[i] = stableUDPRead(t, peer, "request")
			}
			if sources[0] == sources[1] {
				t.Fatal("independent associations shared an origin source port")
			}
			for i := range 2 {
				stableUDPWrite(t, peer, sources[i], []byte{byte(i + 1)})
			}
			seen := map[uint64]bool{}
			for range 2 {
				select {
				case packet := <-mailbox.queue:
					if len(packet.body) != 1 || packet.flow != sharedExitTestFlow(uint32(packet.body[0]), 1) || packet.target != target || seen[packet.flow] {
						t.Fatalf("cross-association or duplicate reply: %+v", packet)
					}
					seen[packet.flow] = true
				case <-time.After(time.Second):
					t.Fatal("reply did not reach its association")
				}
			}
			// A second packet to the same peer must keep that association's port.
			s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, body: []byte("again")})
			if source := stableUDPRead(t, peer, "again"); source != sources[0] {
				t.Fatal("association source port changed")
			}
		})
	}
}

func TestUDPPreviewSharedExitBoundsGroupsAndReclaimsClosedGroup(t *testing.T) {
	peer, target := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target)
	s := sharedExitTestManager(t, mailbox)
	for id := uint32(1); id <= pilotSharedMaxAssociations; id++ {
		s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(id, 1), target: target, body: []byte("request")})
		stableUDPRead(t, peer, "request")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(pilotSharedMaxAssociations+1, 1), target: target, body: []byte("over cap")})
	if sharedExitTestGroupCount(s) != pilotSharedMaxAssociations || mailbox.flowCap.Load() != 1 {
		t.Fatal("association cap not enforced")
	}
	if err := s.closeAssociation(1); err != nil {
		t.Fatal(err)
	}
	stableUDPWait(t, func() bool { return sharedExitTestGroupCount(s) == pilotSharedMaxAssociations-1 })
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(pilotSharedMaxAssociations+1, 1), target: target, body: []byte("reclaimed")})
	stableUDPRead(t, peer, "reclaimed")
	if sharedExitTestGroupCount(s) != pilotSharedMaxAssociations {
		t.Fatal("closed group capacity was not reusable")
	}
}

func TestUDPPreviewSharedExitCloseBeforeDataAndRetirementBound(t *testing.T) {
	peer, target := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target)
	s := sharedExitTestManager(t, mailbox)
	if err := s.closeAssociation(7); err != nil {
		t.Fatal(err)
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(7, 1), target: target, body: []byte("late")})
	if sharedExitTestGroupCount(s) != 0 || mailbox.rejected.Load() != 1 {
		t.Fatal("close-before-data resurrected an association")
	}
	s.reap(time.Now().Add(2 * pilotLifetime))
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(7, 1), target: target, body: []byte("newly dated but closed")})
	if sharedExitTestGroupCount(s) != 0 {
		t.Fatal("time expiry resurrected closed association")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(8, 1), target: target, body: []byte("healthy")})
	stableUDPRead(t, peer, "healthy")
	for id := uint32(20); id < 20+3*pilotSharedRetirementWindow; id++ {
		if err := s.closeAssociation(id); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	retained := len(s.retired)
	s.mu.Unlock()
	if retained > pilotSharedRetirementWindow {
		t.Fatal("retirement state grew beyond window")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(7, 1), target: target, body: []byte("old closed")})
	if sharedExitTestGroupCount(s) != 1 {
		t.Fatal("old closed ID was lost from the exact ranges")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(8, 1), target: target, body: []byte("old active")})
	stableUDPRead(t, peer, "old active")
}

func TestUDPPreviewSharedExitFatalGroupNotifiesCarrierOwner(t *testing.T) {
	peer, target := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failures := make(chan error, 1)
	s := newPilotSharedUDPExit(ctx, mailbox, pilotSharedMaxAssociations, func(err error) { failures <- err; cancel() })
	t.Cleanup(func() { cancel(); <-s.done })
	for id := uint32(1); id <= 2; id++ {
		s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(id, 1), target: target, body: []byte("start")})
		stableUDPRead(t, peer, "start")
	}
	s.mu.Lock()
	broken := s.groups[1]
	healthy := s.groups[2]
	s.mu.Unlock()
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(2, 1), target: target, body: []byte("healthy")})
	source := stableUDPRead(t, peer, "healthy")
	// Two loopback packets can arrive in the same clock tick on Windows.
	// Give the association an older timestamp to observe receive-only renewal.
	before := healthy.lastActivity.Add(-int64(time.Second))
	stableUDPWrite(t, peer, source, []byte("reply"))
	select {
	case <-mailbox.queue:
	case <-time.After(time.Second):
		t.Fatal("healthy group stopped receiving")
	}
	if healthy.lastActivity.Load() <= before {
		t.Fatal("receive-only activity did not refresh group lifetime")
	}
	_ = broken.mapping.socket.Close()
	select {
	case err := <-failures:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("unexpected failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal origin failure not sent to lease owner")
	}
	<-s.done
	if ctx.Err() == nil || sharedExitTestGroupCount(s) != 0 {
		t.Fatal("fatal carrier retained associations")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(3, 1), target: target, body: []byte("after terminal close")})
	if sharedExitTestGroupCount(s) != 0 {
		t.Fatal("terminal manager admitted a new association")
	}
}

func TestUDPPreviewSharedExitIdleReclamationAndInvalidFlows(t *testing.T) {
	peer, target := stableUDPTestPeer(t)
	_, otherTarget := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target, otherTarget)
	s := sharedExitTestManager(t, mailbox)
	for _, flow := range []uint64{0, 1, sharedExitTestFlow(1, 0)} {
		s.send(t.Context(), pilotPacket{flow: flow, target: target})
	}
	if sharedExitTestGroupCount(s) != 0 || mailbox.rejected.Load() != 3 {
		t.Fatal("invalid association/flow was admitted")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, body: []byte("start")})
	stableUDPRead(t, peer, "start")
	s.reap(time.Now().Add(udpPreviewStableIdle + time.Millisecond))
	stableUDPWait(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.groups[1] != nil && s.groups[1].mapping == nil })
	if s.ctx.Err() != nil {
		t.Fatal("idle association reclamation closed the carrier")
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, body: []byte("awake")})
	stableUDPRead(t, peer, "awake")
	if sharedExitTestGroupCount(s) != 1 {
		t.Fatal("dormant association did not reactivate")
	}
	rejected := mailbox.rejected.Load()
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: otherTarget, body: []byte("retarget old flow")})
	if mailbox.rejected.Load() != rejected+1 {
		t.Fatal("idle reactivation lost immutable flow binding")
	}
}

func TestUDPPreviewSharedControlFraming(t *testing.T) {
	var ids []uint32
	closeAssociation := func(id uint32) error {
		if id == 0 {
			return errors.New("zero")
		}
		ids = append(ids, id)
		return nil
	}
	frames := binary.BigEndian.AppendUint32(nil, 1)
	frames = binary.BigEndian.AppendUint32(frames, 9)
	if err := servePilotSharedUDPControl(bytes.NewReader(frames), closeAssociation); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 9 {
		t.Fatal("close frame boundaries were lost")
	}
	if err := servePilotSharedUDPControl(bytes.NewReader([]byte{0, 0}), closeAssociation); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("accepted truncated close")
	}
	if err := servePilotSharedUDPControl(bytes.NewReader([]byte{0, 0, 0, 0}), closeAssociation); err == nil {
		t.Fatal("accepted zero association")
	}
}
