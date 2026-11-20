package skirk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestPreviewDNSReservedMuxPreservesReplies(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var upstreamDials atomic.Int32
	resolver := newPreviewDNSMuxFixtureResolver(ctx, &upstreamDials)
	var genericDials atomic.Int32
	client := newPreviewDNS(ctx, startPreviewDNSMuxFixture(t, ctx, resolver, &genericDials))
	for _, id := range []uint16{91, 92, 93} {
		requirePreviewDNSMuxAnswer(t, ctx, client, id)
	}
	if genericDials.Load() != 0 || upstreamDials.Load() != 1 || previewDNSExitCount(resolver) != 1 {
		t.Fatalf("wrong resolver path, generic=%d upstream=%d mux=%d", genericDials.Load(), upstreamDials.Load(), previewDNSExitCount(resolver))
	}
}

func TestPreviewDNSReservedMuxPressureReclaimsParkedClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var upstreamDials, genericDials atomic.Int32
	resolver := newPreviewDNSMuxFixtureResolver(ctx, &upstreamDials)
	muxes := startPreviewDNSMuxClients(t, ctx, resolver, &genericDials, previewDNSExitLimit+1)
	clients := make([]*previewDNS, len(muxes))
	for index, mux := range muxes {
		clients[index] = newPreviewDNS(ctx, mux)
	}
	for index, client := range clients {
		requirePreviewDNSMuxAnswer(t, ctx, client, uint16(index+1))
	}
	// The oldest idle peer reconnects through normal bounded retry.
	requirePreviewDNSMuxAnswer(t, ctx, clients[0], 100)
	if genericDials.Load() != 0 || upstreamDials.Load() != 1 {
		t.Fatalf("wrong resolver path, generic=%d upstream=%d", genericDials.Load(), upstreamDials.Load())
	}
	if got := previewDNSExitCount(resolver); got != previewDNSExitLimit {
		t.Fatalf("expected %d exit DNS pipes after reuse, got %d", previewDNSExitLimit, got)
	}
}

func TestPreviewDNSReservedMuxRetiresAfterEitherSideCloses(t *testing.T) {
	for _, closer := range []string{"client", "exit"} {
		t.Run(closer, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var upstreamDials, genericDials atomic.Int32
			resolver := newPreviewDNSMuxFixtureResolver(ctx, &upstreamDials)
			mux := startPreviewDNSMuxFixture(t, ctx, resolver, &genericDials)
			client := newPreviewDNS(ctx, mux)
			requirePreviewDNSMuxAnswer(t, ctx, client, 42)

			if closer == "client" {
				client.closeIdle()
			} else {
				resolver.exits.mu.Lock()
				for pipe := range resolver.exits.pipes {
					_ = pipe.Close()
				}
				resolver.exits.mu.Unlock()
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && (mux.active.Load() != 0 || previewDNSExitCount(resolver) != 0) {
				time.Sleep(time.Millisecond)
			}
			if active, retained := mux.active.Load(), previewDNSExitCount(resolver); active != 0 || retained != 0 {
				t.Fatalf("closed DNS mux should not keep polling or occupying a slot: client_active=%d exit_retained=%d", active, retained)
			}
		})
	}
}

func newPreviewDNSMuxFixtureResolver(ctx context.Context, upstreamDials *atomic.Int32) *previewDNS {
	resolver := newPreviewDNSPool(ctx, func(context.Context) (net.Conn, error) {
		upstreamDials.Add(1)
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			stop := context.AfterFunc(ctx, func() { _ = remote.Close() })
			defer stop()
			for {
				raw, err := readPreviewDNSFrame(remote, previewDNSMaxQuery)
				if err != nil {
					return
				}
				query, err := unpackPreviewDNSQuery(raw)
				if err != nil {
					return
				}
				query.Response = true
				query.RecursionAvailable = true
				query.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 37},
					Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 1, 0xd, 0xb8, 15: 1}},
				}}
				wire, err := query.Pack()
				if err != nil || writePreviewDNSFrame(remote, wire) != nil {
					return
				}
			}
		}()
		return local, nil
	})
	resolver.exits = newPreviewDNSExitPool(previewDNSExitLimit)
	return resolver
}

func requirePreviewDNSMuxAnswer(t *testing.T, ctx context.Context, client *previewDNS, id uint16) {
	t.Helper()
	request := previewDNSFixture(id, dnsmessage.TypeAAAA)
	wire, err := request.Pack()
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.respond(ctx, wire, id != 92)
	if err != nil {
		t.Fatal(err)
	}
	var reply dnsmessage.Message
	if err := reply.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if reply.ID != id || reply.RCode != dnsmessage.RCodeSuccess || len(reply.Answers) != 1 ||
		reply.Answers[0].Header.Type != dnsmessage.TypeAAAA || reply.Answers[0].Header.TTL != 37 {
		t.Fatalf("exit DNS changed the response header or answers: id=%d code=%v answers=%d", reply.ID, reply.RCode, len(reply.Answers))
	}
}

func TestPreviewDNSReservedMuxFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var genericDials atomic.Int32
	client := newPreviewDNS(ctx, startPreviewDNSMuxFixture(t, ctx, nil, &genericDials))
	query := previewDNSFixture(51, dnsmessage.TypeAAAA)
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.respond(ctx, wire, false)
	if err != nil {
		t.Fatal(err)
	}
	var reply dnsmessage.Message
	if err := reply.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if reply.RCode != dnsmessage.RCodeServerFailure || reply.ID != 51 || genericDials.Load() != 0 || ctx.Err() != nil {
		t.Fatalf("disabled exit did not reject internal DNS before fallback: code=%v dials=%d context=%v", reply.RCode, genericDials.Load(), ctx.Err())
	}
	for _, target := range []string{previewDNSMuxTarget, "SKIRK-DNS-V1.INVALID.:53", "skirk-dns-v1.invalid:80"} {
		if !isPreviewDNSReservedTarget(target) {
			t.Fatalf("DNS namespace alias %q fell through to the generic dial path", target)
		}
	}
}

func TestPreviewDNSReservedMuxStreamLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resolver := newPreviewDNSPool(ctx, func(context.Context) (net.Conn, error) { return nil, errors.New("unexpected upstream") })
	resolver.exits = newPreviewDNSExitPool(1)
	conn, err := resolver.openExit(ctx, previewDNSMuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if another, err := resolver.openExit(ctx, previewDNSMuxTarget); !errors.Is(err, errPreviewDNSCapacity) {
		if another != nil {
			_ = another.Close()
		}
		t.Fatalf("expected DNS capacity error while a fresh stream is opening, got %v", err)
	}
	if _, err := resolver.openExit(ctx, "skirk-dns-v1.invalid:80"); !errors.Is(err, errPreviewDNSTarget) {
		t.Fatalf("expected invalid-target error, got %v", err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for previewDNSExitCount(resolver) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if previewDNSExitCount(resolver) != 0 {
		t.Fatal("DNS stream slot leaked after shutdown")
	}
}

func TestPreviewDNSReservedMuxDoesNotReclaimBusyPipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	waiting := make(chan struct{})
	release := make(chan struct{})
	resolver := newPreviewDNSPool(ctx, func(context.Context) (net.Conn, error) {
		local, peer := net.Pipe()
		go func() {
			defer peer.Close()
			stop := context.AfterFunc(ctx, func() { _ = peer.Close() })
			defer stop()
			for range 2 {
				raw, err := readPreviewDNSFrame(peer, previewDNSMaxQuery)
				if err != nil {
					return
				}
				query, err := unpackPreviewDNSQuery(raw)
				if err != nil {
					return
				}
				if query.ID == 2 {
					close(waiting)
					select {
					case <-release:
					case <-ctx.Done():
						return
					}
				}
				query.Response = true
				wire, err := query.Pack()
				if err != nil || writePreviewDNSFrame(peer, wire) != nil {
					return
				}
			}
		}()
		return local, nil
	})
	resolver.exits = newPreviewDNSExitPool(1)
	conn, err := resolver.openExit(ctx, previewDNSMuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for id := uint16(1); id <= 2; id++ {
		query := previewDNSFixture(id, dnsmessage.TypeAAAA)
		buf, err := query.Pack()
		if err != nil || writePreviewDNSFrame(conn, buf) != nil {
			t.Fatalf("could not send DNS request %d", id)
		}
		if id == 2 {
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("second query did not reach the upstream")
			}
			if competing, err := resolver.openExit(ctx, previewDNSMuxTarget); !errors.Is(err, errPreviewDNSCapacity) {
				if competing != nil {
					_ = competing.Close()
				}
				t.Fatalf("the in-flight DNS query must keep its slot, got %v", err)
			}
			close(release)
		}
		wire, err := readPreviewDNSFrame(conn, previewDNSMaxQuery)
		var reply dnsmessage.Message
		if err != nil || reply.Unpack(wire) != nil || !reply.Response || reply.ID != id || reply.RCode != dnsmessage.RCodeSuccess {
			t.Fatalf("DNS request %d did not keep its response: %v", id, err)
		}
	}
}

func TestPreviewDNSReservedMuxCloseCancelsBlockedUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	waiting := make(chan struct{})
	peerDone := make(chan struct{})
	resolver := newPreviewDNSPool(ctx, func(context.Context) (net.Conn, error) {
		local, peer := net.Pipe()
		go func() {
			defer close(peerDone)
			defer peer.Close()
			stop := context.AfterFunc(ctx, func() { _ = peer.Close() })
			defer stop()
			if _, err := readPreviewDNSFrame(peer, previewDNSMaxQuery); err != nil {
				return
			}
			close(waiting)
			// Hold the response while the remote client closes its pipe.
			_, _ = peer.Read(make([]byte, 1))
		}()
		return local, nil
	})
	resolver.exits = newPreviewDNSExitPool(1)
	conn, err := resolver.openExit(ctx, previewDNSMuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	query := previewDNSFixture(1, dnsmessage.TypeAAAA)
	wire, err := query.Pack()
	if err != nil || writePreviewDNSFrame(conn, wire) != nil {
		t.Fatal("could not send DNS request")
	}
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("query did not reach upstream")
	}
	_ = conn.Close()
	select {
	case <-peerDone:
	case <-ctx.Done():
		t.Fatal("closing the client pipe left the upstream query running")
	}
	for ctx.Err() == nil && (len(resolver.active) != 0 || previewDNSExitCount(resolver) != 0) {
		time.Sleep(time.Millisecond)
	}
	if len(resolver.active) != 0 || previewDNSExitCount(resolver) != 0 {
		t.Fatal("abandoned DNS request did not release both its slots")
	}
}

func previewDNSExitCount(resolver *previewDNS) int {
	resolver.exits.mu.Lock()
	defer resolver.exits.mu.Unlock()
	return len(resolver.exits.pipes)
}

func startPreviewDNSMuxFixture(t *testing.T, ctx context.Context, resolver *previewDNS, genericDials *atomic.Int32) *driveMux {
	t.Helper()
	return startPreviewDNSMuxClients(t, ctx, resolver, genericDials, 1)[0]
}

func startPreviewDNSMuxClients(t *testing.T, ctx context.Context, resolver *previewDNS, genericDials *atomic.Int32, count int) []*driveMux {
	t.Helper()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	newMux := func(role string, index int, send, receive byte) *driveMux {
		tunnel := &Tunnel{
			Data: store, Secret: secret, SessionID: [16]byte{1}, Transport: "muxv5",
			ClientID: fmt.Sprintf("owned-dns-fixture-%d", index), RunID: "isolated-fixture", ChunkSize: 64 << 10,
			Concurrency: 4, UploadConcurrency: 4, DownloadConcurrency: 4, PollInterval: 10 * time.Millisecond,
		}
		mux, err := newDriveMux(tunnel, role, send, receive)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mux.closeAll)
		return mux
	}
	exit := newMux("exit", 0, DirectionDown, DirectionUp)
	exit.exitDNS = resolver
	exit.dialExit = func(context.Context, string) (net.Conn, error) {
		genericDials.Add(1)
		return nil, errors.New("generic dial disabled in fixture")
	}
	exit.start(ctx)
	clients := make([]*driveMux, count)
	for index := range clients {
		clients[index] = newMux("client", index, DirectionUp, DirectionDown)
		clients[index].start(ctx)
	}
	return clients
}
