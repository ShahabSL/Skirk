package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestPilotTimingControlFramesAuthenticateAndBoundAllocation(t *testing.T) {
	key := bytes.Repeat([]byte{0x61}, 32)
	frame := pilotControlFrame{kind: pilotControlResetAssociation, body: binary.BigEndian.AppendUint32(nil, 9)}
	wire, err := pilotControlEncode(key, frame)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pilotControlRead(bytes.NewReader(wire), key)
	if err != nil || got.kind != frame.kind || got.epoch != frame.epoch || !bytes.Equal(got.body, frame.body) {
		t.Fatalf("valid control frame changed: %+v %v", got, err)
	}
	for _, index := range []int{0, 1, 8, 11, len(wire) - 1} {
		forged := bytes.Clone(wire)
		forged[index] ^= 1
		if _, err := pilotControlRead(bytes.NewReader(forged), key); err == nil {
			t.Fatalf("forged byte %d was not authenticated", index)
		}
	}
	if _, err := pilotControlRead(bytes.NewReader(wire), bytes.Repeat([]byte{0x62}, 32)); err == nil {
		t.Fatal("control transcript migrated between lease keys")
	}
	if _, err := pilotControlEncode(key, pilotControlFrame{body: make([]byte, pilotControlMaxBody+1)}); err == nil {
		t.Fatal("oversize outgoing control frame accepted")
	}
	var oversized [11]byte
	binary.BigEndian.PutUint16(oversized[9:], pilotControlMaxBody+1)
	if _, err := pilotControlRead(bytes.NewReader(oversized[:]), key); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("oversized header was not refused before reading its body: %v", err)
	}
}

type timingControlTestPair struct {
	client, server *pilotTimingControl
	clientState    *pilotTimingState
	serverState    *pilotTimingState
	resetIDs       chan pilotAssociationReset
	failures       atomic.Uint32
}

func newTimingControlTestPair(t *testing.T, closeAssociation func(uint32) error, onFailure ...func()) *timingControlTestPair {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	clientConn, serverConn := net.Pipe()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{0x19}}
	lease := [16]byte{0x41}
	pair := &timingControlTestPair{
		clientState: newPilotTimingState(),
		serverState: newPilotTimingState(),
		resetIDs:    make(chan pilotAssociationReset, pilotControlAssociationBatch),
	}
	for _, state := range []*pilotTimingState{pair.clientState, pair.serverState} {
		now := time.Now()
		if err := state.install(1, now, now); err != nil {
			t.Fatal(err)
		}
		if err := state.activate(1); err != nil {
			t.Fatal(err)
		}
	}
	failed := func() {
		pair.failures.Add(1)
		if len(onFailure) != 0 {
			onFailure[0]()
		}
	}
	pair.client, err = newPilotTimingControl(ctx, clientConn, tunnel, lease, pair.clientState, true, nil, nil, failed)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	pair.server, err = newPilotTimingControl(ctx, serverConn, tunnel, lease, pair.serverState, false, closeAssociation, pair.resetIDs, failed)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = pair.client.wait()
		_ = pair.server.wait()
	})
	return pair
}

func timingControlActive(state *pilotTimingState) uint64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.active
}

func awaitTimingControlEpoch(t *testing.T, pair *timingControlTestPair, id uint64) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		if timingControlActive(pair.clientState) == id && timingControlActive(pair.serverState) == id {
			return
		}
		select {
		case <-pair.client.done:
			t.Fatalf("client control closed during renewal: %v", pair.client.wait())
		case <-pair.server.done:
			t.Fatalf("server control closed during renewal: %v", pair.server.wait())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timing epoch %d not active: client=%d server=%d", id, timingControlActive(pair.clientState), timingControlActive(pair.serverState))
		}
	}
}

func TestPilotTimingControlRenewsWithoutClosingAssociations(t *testing.T) {
	closed := make(chan uint32, 2)
	origin, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	reset := make(chan uint32, 2)
	pair := newTimingControlTestPair(t, func(id uint32) error {
		closed <- id
		return nil
	}, func() { _ = origin.Close() })
	pair.client.setAssociationResetHandler(func(id uint32) { reset <- id })
	clientConn, serverConn := pair.client.Conn, pair.server.Conn
	written := make(chan error, 1)
	for id := uint64(2); id <= 3; id++ {
		if id == 3 {
			pair.resetIDs <- pilotAssociationReset{id: 31}
			pair.resetIDs <- pilotAssociationReset{id: 35}
			go func() {
				_, err := pair.client.Write(binary.BigEndian.AppendUint32(nil, 17))
				written <- err
			}()
		}
		pair.client.refresh <- struct{}{}
		awaitTimingControlEpoch(t, pair, id)
	}
	// Association CLOSE and renewal share one writer. Concurrent frames must
	// remain intact, while the existing owner receives the original ID.
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-closed:
		if id != 17 {
			t.Fatalf("association identity changed: %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("association CLOSE did not reach its existing owner")
	}
	for _, want := range []uint32{31, 35} {
		select {
		case id := <-reset:
			if id != want {
				t.Fatalf("association reset ID %d, want %d", id, want)
			}
		case <-time.After(time.Second):
			t.Fatal("exit association reset did not reach the client")
		}
	}
	if pair.client.Conn != clientConn || pair.server.Conn != serverConn || pair.failures.Load() != 0 {
		t.Fatal("timing renewal replaced or closed the control connection")
	}
	peer, err := net.DialUDP("udp4", nil, origin.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = origin.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Write([]byte("same origin socket")); err != nil {
		t.Fatal(err)
	}
	var received [64]byte
	if n, _, err := origin.ReadFromUDP(received[:]); err != nil || string(received[:n]) != "same origin socket" {
		t.Fatalf("origin socket did not survive renewal: %v", err)
	}
	select {
	case id := <-closed:
		t.Fatalf("renewal synthesized association close %d", id)
	default:
	}
}

func TestPilotTimingControlRejectsInvalidAssociationResets(t *testing.T) {
	validBody := binary.BigEndian.AppendUint32(nil, 2)
	for _, tc := range []struct {
		name     string
		fromExit bool
		epoch    uint64
		body     []byte
	}{
		{"wrong role", false, 0, validBody},
		{"wrong epoch", true, 2, validBody},
		{"empty batch", true, 0, nil},
		{"partial ID", true, 0, validBody[:3]},
		{"zero after valid ID", true, 0, binary.BigEndian.AppendUint32(slices.Clone(validBody), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := newTimingControlTestPair(t, func(uint32) error { return nil })
			var callbacks atomic.Uint64
			pair.client.setAssociationResetHandler(func(uint32) { callbacks.Add(1) })
			from, to := pair.client, pair.server
			if tc.fromExit {
				from, to = to, from
			}
			if err := from.send(pilotControlFrame{kind: pilotControlResetAssociation, epoch: tc.epoch, body: tc.body}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-to.done:
				if err := to.wait(); err == nil || err.Error() != "invalid UDP association reset frame" {
					t.Fatalf("invalid frame closed the wrong path: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("invalid reset left the lease running")
			}
			if callbacks.Load() != 0 {
				t.Fatal("invalid reset partially closed an association")
			}
		})
	}
}

func TestPilotTimingControlCoalescesBoundedAssociationResets(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	state := newPilotTimingState()
	now := time.Now()
	if err := state.install(1, now, now); err != nil {
		t.Fatal(err)
	}
	if err := state.activate(1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	local, peer := net.Pipe()
	const extra = 7
	const total = pilotControlAssociationBatch + extra
	resetIDs := make(chan pilotAssociationReset, total)
	var written atomic.Int32
	for id := uint32(1); id <= total; id++ {
		resetIDs <- pilotAssociationReset{id: id, written: func() { written.Add(1) }}
	}
	control, err := newPilotTimingControl(ctx, local, &Tunnel{Secret: secret}, [16]byte{7}, state, false, func(uint32) error { return nil }, resetIDs, nil)
	if err != nil {
		cancel()
		local.Close()
		peer.Close()
		t.Fatal(err)
	}
	defer func() { cancel(); peer.Close(); _ = control.wait() }()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	readBatch := func(start, count int) {
		t.Helper()
		frame, err := pilotControlRead(peer, control.key)
		if err != nil || frame.kind != pilotControlResetAssociation || len(frame.body) != count*4 || !validPilotAssociationFrame(frame) {
			t.Fatalf("did not get a bounded reset batch: count=%d frame=%+v error=%v", count, frame, err)
		}
		for i := range count {
			if id := binary.BigEndian.Uint32(frame.body[4*i:]); id != uint32(start+i+1) {
				t.Fatalf("reset order changed: got ID %d at index %d", id, start+i)
			}
		}
	}
	if written.Load() != 0 {
		t.Fatal("reset queue released a slot before its write succeeded")
	}
	readBatch(0, pilotControlAssociationBatch)
	stableUDPWait(t, func() bool { return written.Load() == pilotControlAssociationBatch })
	readBatch(pilotControlAssociationBatch, extra)
	stableUDPWait(t, func() bool { return written.Load() == total })
}

func TestPilotTimingControlIgnoresObsoleteAuthenticatedFrames(t *testing.T) {
	pair := newTimingControlTestPair(t, func(uint32) error { return nil })
	for _, frame := range []pilotControlFrame{
		{kind: pilotControlReply, epoch: 1},
		{kind: pilotControlUncertain, epoch: 1},
	} {
		if err := pair.server.send(frame); err != nil {
			t.Fatal(err)
		}
	}
	for _, frame := range []pilotControlFrame{
		{kind: pilotControlPrepare, epoch: 1},
		{kind: pilotControlAck, epoch: 1},
	} {
		if err := pair.client.send(frame); err != nil {
			t.Fatal(err)
		}
	}
	pair.client.refresh <- struct{}{}
	awaitTimingControlEpoch(t, pair, 2)
	if pair.failures.Load() != 0 {
		t.Fatal("a delayed obsolete control frame tore down the lease")
	}
}

func TestPilotTimingControlRenewsAfterDiscontinuity(t *testing.T) {
	for _, side := range []string{"client", "server"} {
		t.Run(side, func(t *testing.T) {
			var closes atomic.Uint32
			pair := newTimingControlTestPair(t, func(uint32) error { closes.Add(1); return nil })
			state := pair.clientState
			if side == "server" {
				state = pair.serverState
			}
			now := state.nowTime()
			oldID, oldElapsed, oldReceiveElapsed, ok := state.stamp(now)
			if !ok || oldID != 1 {
				t.Fatal("initial timing epoch not active")
			}
			state.mu.Lock()
			changed := state.observeElapsedLocked(now, time.Millisecond, time.Hour)
			state.mu.Unlock()
			state.signal(changed)
			awaitTimingControlEpoch(t, pair, 2)
			if state.valid(oldID, state.nowTime()) || state.accept(oldID, oldElapsed, oldReceiveElapsed, state.nowTime()) {
				t.Fatal("old packets were relabeled into the renewed epoch")
			}
			if closes.Load() != 0 || pair.failures.Load() != 0 {
				t.Fatal("clock discontinuity closed an established association")
			}
		})
	}
}

func TestPilotTimingBatchRechecksPerPacketAge(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{0x62}}
	endpoint := netip.MustParseAddrPort("127.0.0.1:3478")
	mailbox, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{0x18}, true, map[netip.AddrPort]bool{endpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	origin := time.Now().Add(-10 * time.Second)
	mailbox.timing = newPilotTimingState()
	if err := mailbox.timing.install(1, origin, origin); err != nil {
		t.Fatal(err)
	}
	if err := mailbox.timing.activate(1); err != nil {
		t.Fatal(err)
	}
	nowElapsed := time.Since(origin)
	packets := []pilotPacket{
		{flow: 1, target: endpoint, body: []byte("fresh"), timingEpoch: 1, elapsed: nowElapsed, receiveElapsed: nowElapsed},
		{flow: 2, target: endpoint, body: []byte("expires during first delivery"), timingEpoch: 1, elapsed: nowElapsed - (pilotLifetime - 50*time.Millisecond), receiveElapsed: nowElapsed - (pilotLifetime - 50*time.Millisecond)},
	}
	delivered := 0
	mailbox.deliver = func(context.Context, pilotPacket) {
		delivered++
		if delivered == 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	wire, err := Seal(mailbox.recvKey, tunnel.SessionID, mailbox.recvDir, 1, pilotMarshal(packets), false)
	if err != nil {
		t.Fatal(err)
	}
	mailbox.acceptBatch(t.Context(), "owned-test-metadata", 1, wire, pilotMaxPlain, pilotMaxBatch)
	if delivered != 1 || mailbox.stale.Load() != 1 {
		t.Fatalf("batch reused an earlier receive time: delivered=%d stale=%d", delivered, mailbox.stale.Load())
	}
}
