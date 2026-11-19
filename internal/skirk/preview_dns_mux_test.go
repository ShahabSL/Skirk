package skirk

import (
	"context"
	"errors"
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
	resolver.exits = make(chan struct{}, previewDNSExitLimit)
	var genericDials atomic.Int32
	client := newPreviewDNS(ctx, startPreviewDNSMuxFixture(t, ctx, resolver, &genericDials))
	for _, id := range []uint16{91, 92, 93} {
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
	if genericDials.Load() != 0 || upstreamDials.Load() != 1 || len(resolver.exits) != 1 {
		t.Fatalf("wrong resolver path, generic=%d upstream=%d mux=%d", genericDials.Load(), upstreamDials.Load(), len(resolver.exits))
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
	resolver.exits = make(chan struct{}, 1)
	conn, err := resolver.openExit(ctx, previewDNSMuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if another, err := resolver.openExit(ctx, previewDNSMuxTarget); err == nil {
		_ = another.Close()
		t.Fatal("opened DNS stream above the configured cap")
	}
	if alias, err := resolver.openExit(ctx, "skirk-dns-v1.invalid:80"); err == nil {
		_ = alias.Close()
		t.Fatal("opened noncanonical reserved DNS target")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for len(resolver.exits) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(resolver.exits) != 0 {
		t.Fatal("DNS stream slot leaked after shutdown")
	}
}

func startPreviewDNSMuxFixture(t *testing.T, ctx context.Context, resolver *previewDNS, genericDials *atomic.Int32) *driveMux {
	t.Helper()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	newMux := func(role string, send, receive byte) *driveMux {
		tunnel := &Tunnel{
			Data: store, Secret: secret, SessionID: [16]byte{1}, Transport: "muxv5",
			ClientID: "owned-dns-fixture", RunID: "isolated-fixture", ChunkSize: 64 << 10,
			Concurrency: 4, UploadConcurrency: 4, DownloadConcurrency: 4, PollInterval: 10 * time.Millisecond,
		}
		mux, err := newDriveMux(tunnel, role, send, receive)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mux.closeAll)
		return mux
	}
	client, exit := newMux("client", DirectionUp, DirectionDown), newMux("exit", DirectionDown, DirectionUp)
	exit.exitDNS = resolver
	exit.dialExit = func(context.Context, string) (net.Conn, error) {
		genericDials.Add(1)
		return nil, errors.New("generic dial disabled in fixture")
	}
	exit.start(ctx)
	client.start(ctx)
	return client
}
