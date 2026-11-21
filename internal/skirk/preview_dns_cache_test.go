package skirk

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func dnsCacheTestReply(query dnsmessage.Message) dnsmessage.Message {
	q := query.Questions[0]
	name := dnsmessage.MustNewName("address.example.")
	query.Response = true
	query.Answers = []dnsmessage.Resource{
		{Header: dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 3}, Body: &dnsmessage.CNAMEResource{CNAME: name}},
		{Header: dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, TTL: 20}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}},
	}
	return query
}

func TestClientDNSCacheCoalescesAndExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		gate := make(chan struct{})
		resolver := newPreviewDNSPool(t.Context(), func(ctx context.Context) (net.Conn, error) {
			local, peer := net.Pipe()
			go func() {
				defer peer.Close()
				for {
					packet, err := readPreviewDNSFrame(peer, previewDNSMaxQuery)
					if err != nil {
						return
					}
					request, err := unpackPreviewDNSQuery(packet)
					if err != nil {
						return
					}
					requests.Add(1)
					<-gate
					reply := dnsCacheTestReply(request)
					wire, err := reply.Pack()
					if err != nil || writePreviewDNSFrame(peer, wire) != nil {
						return
					}
				}
			}()
			return local, nil
		})
		resolver.cache = newPreviewDNSCache()
		query := func(ctx context.Context, id uint16) dnsmessage.Message {
			q := previewDNSFixture(id, dnsmessage.TypeA)
			wire, _ := q.Pack()
			out, err := resolver.respond(ctx, wire, id%2 == 0)
			var msg dnsmessage.Message
			if err != nil || msg.Unpack(out) != nil || msg.ID != id {
				t.Errorf("DNS response to %d: %v", id, err)
			}
			return msg
		}
		const clients = 20
		replies := make(chan dnsmessage.Message, clients)
		for n := range clients {
			go func() { replies <- query(t.Context(), uint16(n)) }()
		}
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		canceled := make(chan dnsmessage.Message, 1)
		go func() { canceled <- query(ctx, 100) }()
		synctest.Wait()
		cancel()
		if msg := <-canceled; msg.RCode != dnsmessage.RCodeServerFailure {
			t.Fatal("canceled waiter received an answer")
		}
		close(gate)
		for range clients {
			if msg := <-replies; msg.RCode != dnsmessage.RCodeSuccess || len(msg.Answers) != 2 {
				t.Fatal("uncanceled waiter missed its answer")
			}
		}
		if requests.Load() != 1 {
			t.Fatalf("concurrent lookups made %d remote requests", requests.Load())
		}
		time.Sleep(2 * time.Second)
		msg := query(t.Context(), 101)
		if len(msg.Answers) != 2 || msg.Answers[0].Header.TTL != 1 || msg.Answers[1].Header.TTL != 18 || requests.Load() != 1 {
			t.Fatalf("cached TTLs or request count wrong: %+v, %d requests", msg.Answers, requests.Load())
		}
		time.Sleep(time.Second)
		msg = query(t.Context(), 102)
		if len(msg.Answers) != 2 || msg.Answers[0].Header.TTL != 3 || requests.Load() != 2 {
			t.Fatal("CNAME expiry did not force a fresh remote lookup")
		}
		resolver.closeIdle()
	})
}

func TestClientDNSCanceledQueryRetiresUnacknowledgedOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secret, err := RandomSecret()
		if err != nil {
			t.Fatal(err)
		}
		mux, err := newDriveMux(&Tunnel{
			Data: NewMemoryStore(), Secret: secret, SessionID: [16]byte{1}, Transport: "muxv5",
			ClientID: "test-client", RunID: "test-run", Concurrency: 4,
			ChunkSize: 64 << 10, PollInterval: time.Second,
		}, "client", DirectionUp, DirectionDown)
		if err != nil {
			t.Fatal(err)
		}
		defer mux.closeAll()
		// No upload worker acknowledges the OPEN. Keep the tunnel alive while
		// canceling just its DNS request, as when an app abandons a lookup.
		resolver := newPreviewDNS(t.Context(), mux)
		ctx, cancel := context.WithCancel(t.Context())
		q := previewDNSFixture(123, dnsmessage.TypeA)
		wire, _ := q.Pack()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = resolver.respond(ctx, wire, true)
		}()
		synctest.Wait()
		if mux.active.Load() != 1 {
			t.Fatal("fixture did not stage a DNS OPEN")
		}
		cancel()
		<-done
		synctest.Wait()
		if mux.active.Load() != 0 {
			t.Fatal("abandoned DNS OPEN still keeps the mux active")
		}
	})
}

func TestClientDNSCacheCanceledLeaderDoesNotCancelOtherCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dials atomic.Int32
		resolver := newPreviewDNSPool(t.Context(), func(ctx context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			local, remote := net.Pipe()
			go func() {
				defer remote.Close()
				query, err := readPreviewDNSFrame(remote, previewDNSMaxQuery)
				if err != nil {
					return
				}
				request, err := unpackPreviewDNSQuery(query)
				if err != nil {
					return
				}
				response := dnsCacheTestReply(request)
				wire, _ := response.Pack()
				_ = writePreviewDNSFrame(remote, wire)
			}()
			return local, nil
		})
		defer resolver.closeIdle()
		resolver.cache = newPreviewDNSCache()
		lookup := func(ctx context.Context, id uint16) <-chan dnsmessage.Message {
			done := make(chan dnsmessage.Message, 1)
			go func() {
				q := previewDNSFixture(id, dnsmessage.TypeA)
				wire, _ := q.Pack()
				response, err := resolver.respond(ctx, wire, true)
				var msg dnsmessage.Message
				if err != nil || msg.Unpack(response) != nil || msg.ID != id {
					t.Errorf("lookup %d: %v", id, err)
				}
				done <- msg
			}()
			return done
		}
		ctx, cancel := context.WithCancel(t.Context())
		first := lookup(ctx, 100)
		synctest.Wait()
		second := lookup(t.Context(), 101)
		synctest.Wait()
		if dials.Load() != 1 {
			t.Fatal("second caller failed to share pending lookup")
		}
		cancel()
		if (<-first).RCode != dnsmessage.RCodeServerFailure || (<-second).RCode != dnsmessage.RCodeSuccess || dials.Load() != 2 {
			t.Fatal("canceling first caller orphaned or canceled the remaining lookup")
		}
	})
}

func TestClientDNSCacheScopeAndPolicy(t *testing.T) {
	query := previewDNSFixture(7, dnsmessage.TypeA)
	edns := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232}, Body: &dnsmessage.OPTResource{}}
	query.Additionals = []dnsmessage.Resource{edns}
	if !cacheableAddressQuestion(query) {
		t.Fatal("plain EDNS address lookup skipped the cache")
	}
	for _, mutate := range []func(*dnsmessage.Message){
		func(m *dnsmessage.Message) { m.CheckingDisabled = true },
		func(m *dnsmessage.Message) { m.AuthenticData = true },
		func(m *dnsmessage.Message) { m.Additionals[0].Header.TTL = 1 << 15 },
		func(m *dnsmessage.Message) {
			m.Additionals[0].Body = &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 10, Data: []byte("cookie")}}}
		},
		func(m *dnsmessage.Message) { m.Questions[0].Type = dnsmessage.TypeTXT },
	} {
		q := previewDNSFixture(7, dnsmessage.TypeA)
		q.Additionals = []dnsmessage.Resource{edns}
		mutate(&q)
		if cacheableAddressQuestion(q) {
			t.Fatal("unsafe query selected for caching")
		}
	}
	now := time.Now()
	for _, mutate := range []func(*dnsmessage.Message){
		func(m *dnsmessage.Message) { m.Truncated = true },
		func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeServerFailure },
		func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeNameError },
		func(m *dnsmessage.Message) { m.Answers = nil },
		func(m *dnsmessage.Message) { m.Answers[0].Header.TTL = 0 },
		func(m *dnsmessage.Message) { m.Answers[0].Header.TTL = 1 << 31 },
		func(m *dnsmessage.Message) { m.Authorities = m.Answers },
		func(m *dnsmessage.Message) { m.AuthenticData = true },
	} {
		msg := dnsCacheTestReply(query)
		mutate(&msg)
		wire, err := msg.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if a := newPreviewDNSAnswer(wire, now); !a.expires.IsZero() {
			t.Fatal("unsafe reply entered positive cache")
		}
	}
	msg := dnsCacheTestReply(query)
	wire, _ := msg.Pack()
	answer := newPreviewDNSAnswer(wire, now)
	binary.BigEndian.PutUint16(wire, 99)
	if answer.expires.Sub(now) != 3*time.Second || binary.BigEndian.Uint16(answer.wire) != 7 {
		t.Fatal("positive entry TTL or buffer ownership")
	}
	cache := newPreviewDNSCache()
	cache.store("soonest", answer)
	answer.expires = now.Add(time.Hour)
	for i := range previewDNSCacheEntries {
		cache.store(fmt.Sprint(i), answer)
	}
	if _, ok := cache.answers["soonest"]; ok || len(cache.answers) != previewDNSCacheEntries || len(newPreviewDNSCache().answers) != 0 {
		t.Fatal("cache eviction or per-client isolation")
	}
}
