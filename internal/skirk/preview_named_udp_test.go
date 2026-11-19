package skirk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestPreviewNamedUDPFrontendRoundTrip(t *testing.T) {
	for _, command := range []byte{socksCommandUDPAssociate, socksCommandUDPInTCP} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			app, control := net.Pipe()
			defer app.Close()
			_ = app.SetDeadline(time.Now().Add(5 * time.Second))
			mailbox := &pilotMailbox{publicIPv4: true, queue: make(chan pilotPacket, 2)}
			flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow)}
			relay := &pilotClientRelay{namedUDP: true, publicIPv4: true,
				open: func() (*pilotMailbox, *pilotFlowTable, func(), error) { return mailbox, flows, func() {}, nil },
				remoteDNS: func(_ context.Context, query []byte, _ bool) ([]byte, error) {
					return namedTestReply(t, query, namedTestAnswer("game.example.", "8.8.8.8")), nil
				},
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if command == socksCommandUDPAssociate {
					pilotAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{control})
				} else {
					pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{control})
				}
			}()
			defer func() { cancel(); app.Close(); <-done }()
			var handshake [10]byte
			if _, err := io.ReadFull(app, handshake[:]); err != nil || handshake[1] != 0 {
				t.Fatalf("handshake: %v", err)
			}
			var udp *net.UDPConn
			if command == socksCommandUDPAssociate {
				var err error
				udp, err = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(handshake[4:8]), Port: int(handshake[8])<<8 | int(handshake[9])})
				if err != nil {
					t.Fatal(err)
				}
				defer udp.Close()
				_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
			}
			for i, host := range []string{"game.example", "GAME.example."} {
				var err error
				if udp != nil {
					if i > 0 {
						oversized := make([]byte, previewDNSMaxQuery)
						if _, err := udp.Write(buildSOCKSUDPDatagram(host, 443, oversized)); err != nil {
							t.Fatal(err)
						}
					}
					_, err = udp.Write(buildSOCKSUDPDatagram(host, 443, []byte("uplink")))
				} else {
					err = writeAll(app, buildUDPInTCPFrame(host, 443, []byte("uplink")))
				}
				if err != nil {
					t.Fatal(err)
				}
				var packet pilotPacket
				select {
				case packet = <-mailbox.queue:
				case <-time.After(5 * time.Second):
					t.Fatal("hostname packet was not queued")
				}
				if packet.target.String() != "8.8.8.8:443" || string(packet.body) != "uplink" {
					t.Fatalf("packet = %+v", packet)
				}
				packet.body = []byte("downlink")
				flows.returnToClient(ctx, packet)
				var reply socksUDPPacket
				if udp != nil {
					var buffer [256]byte
					n, readErr := udp.Read(buffer[:])
					if readErr != nil {
						t.Fatal(readErr)
					}
					reply, err = parseSOCKSUDPDatagram(buffer[:n])
				} else {
					reply, err = readUDPInTCPFrame(app)
				}
				if err != nil || reply.Host != "8.8.8.8" || reply.Port != 443 || string(reply.Payload) != "downlink" {
					t.Fatalf("reply = %+v, error = %v", reply, err)
				}
			}
			flows.mu.Lock()
			count := len(flows.client)
			flows.mu.Unlock()
			if count != 1 {
				t.Fatalf("duplicate numeric flow admissions: %d", count)
			}
		})
	}
}

func namedTestAnswer(name, address string) dnsmessage.Resource {
	ip := netip.MustParseAddr(address)
	rr := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: 0}}
	if ip.Is4() {
		rr.Header.Type = dnsmessage.TypeA
		rr.Body = &dnsmessage.AResource{A: ip.As4()}
	} else {
		rr.Header.Type = dnsmessage.TypeAAAA
		rr.Body = &dnsmessage.AAAAResource{AAAA: ip.As16()}
	}
	return rr
}

func namedTestAlias(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, Type: dnsmessage.TypeCNAME},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

func namedTestReply(t *testing.T, query []byte, answers ...dnsmessage.Resource) []byte {
	t.Helper()
	var reply dnsmessage.Message
	if err := reply.Unpack(query); err != nil {
		t.Fatal(err)
	}
	reply.Response = true
	reply.Answers = answers
	wire, err := reply.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestPreviewNamedUDPUsesFullResolverReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	const host = "fixture.example."
	answers := make([]dnsmessage.Resource, 48)
	for i := range answers {
		answers[i] = namedTestAnswer(host, fmt.Sprintf("2001:4860::%x", i+1))
	}
	dns := newPreviewDNSPool(ctx, func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			for {
				query, err := readPreviewDNSFrame(server, previewDNSMaxQuery)
				if err != nil {
					return
				}
				response := namedTestReply(t, query, answers...)
				if err := writePreviewDNSFrame(server, response); err != nil {
					return
				}
			}
		}()
		return client, nil
	})
	relay := &pilotClientRelay{namedUDP: true, publicIPv6: true, remoteDNS: dns.respond}
	query, err := previewNamedQuery(host, dnsmessage.TypeAAAA)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	// A DNS datagram for the same question still gets a truncated response.
	response, err := relay.answerDNS(ctx, wire)
	if err != nil {
		t.Fatal(err)
	}
	var message dnsmessage.Message
	if err := message.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if !message.Truncated || len(response) > previewDNSUDPMax {
		t.Fatalf("UDP reply was not truncated: %d bytes, TC=%t", len(response), message.Truncated)
	}
	target, ok := relay.allowedTarget(ctx, socksUDPPacket{Host: host, Port: 443})
	if !ok || target != netip.MustParseAddrPort("[2001:4860::1]:443") {
		t.Fatalf("named UDP did not use the full DNS reply: %v, %v", target, ok)
	}
}

func TestPreviewNamedUDPPinsCanonicalNameAndPrefersIPv4(t *testing.T) {
	calls := 0
	relay := &pilotClientRelay{namedUDP: true, publicIPv4: true, publicIPv6: true}
	relay.remoteDNS = func(_ context.Context, query []byte, _ bool) ([]byte, error) {
		calls++
		var request dnsmessage.Message
		if err := request.Unpack(query); err != nil {
			t.Fatal(err)
		}
		if request.Questions[0].Type != dnsmessage.TypeA {
			t.Fatal("queried IPv6 despite allowed IPv4")
		}
		if previewDNSReplyLimit(request) != previewDNSUDPMax {
			t.Fatal("missing bounded EDNS allowance")
		}
		return namedTestReply(t, query, namedTestAlias("game.example.", "edge.example."),
			namedTestAnswer("unrelated.example.", "9.9.9.9"), namedTestAnswer("edge.example.", "10.0.0.1"),
			namedTestAnswer("edge.example.", "8.8.8.8")), nil
	}
	for _, host := range []string{"Game.Example", "game.example."} {
		target, ok := relay.allowedTarget(context.Background(), socksUDPPacket{Host: host, Port: 443})
		if !ok || target.String() != "8.8.8.8:443" {
			t.Fatalf("target = %v, %v", target, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("zero-TTL live flow was resolved again: %d calls", calls)
	}
	_, _ = relay.allowedTarget(context.Background(), socksUDPPacket{Host: "game.example", Port: 444})
	if calls != 2 {
		t.Fatal("distinct port did not get its own admission")
	}
}

func TestPreviewNamedUDPFallsBackToPermittedIPv6(t *testing.T) {
	var kinds []dnsmessage.Type
	relay := &pilotClientRelay{namedUDP: true, publicIPv4: true, publicIPv6: true}
	relay.remoteDNS = func(_ context.Context, query []byte, _ bool) ([]byte, error) {
		var request dnsmessage.Message
		if err := request.Unpack(query); err != nil {
			t.Fatal(err)
		}
		kind := request.Questions[0].Type
		kinds = append(kinds, kind)
		if kind == dnsmessage.TypeA {
			return namedTestReply(t, query, namedTestAnswer("game.example.", "127.0.0.1")), nil
		}
		return namedTestReply(t, query, namedTestAnswer("game.example.", "::ffff:8.8.8.8"),
			namedTestAnswer("game.example.", "fd00::1"), namedTestAnswer("game.example.", "2606:4700:4700::1111")), nil
	}
	target, ok := relay.allowedTarget(context.Background(), socksUDPPacket{Host: "game.example", Port: 443})
	if !ok || target.Addr() != netip.MustParseAddr("2606:4700:4700::1111") || len(kinds) != 2 || kinds[0] != dnsmessage.TypeA || kinds[1] != dnsmessage.TypeAAAA {
		t.Fatalf("target=%v ok=%v queries=%v", target, ok, kinds)
	}
}

func TestPreviewNamedUDPAdmissionBoundsAndNoLocalFallback(t *testing.T) {
	calls := 0
	relay := &pilotClientRelay{namedUDP: true, publicIPv4: true}
	relay.remoteDNS = func(context.Context, []byte, bool) ([]byte, error) {
		calls++
		return nil, errors.New("mock remote outage")
	}
	for i := 0; i < pilotMaxFlows+1; i++ {
		packet := socksUDPPacket{Host: fmt.Sprintf("game%d.example", i), Port: 443}
		for range 2 {
			if _, ok := relay.allowedTarget(context.Background(), packet); ok {
				t.Fatal("failed remote lookup admitted a name")
			}
		}
	}
	if calls != pilotMaxFlows || len(relay.namedTargets) != pilotMaxFlows {
		t.Fatalf("unbounded/repeated admissions: calls=%d entries=%d", calls, len(relay.namedTargets))
	}
	if target, ok := relay.allowedTarget(context.Background(), socksUDPPacket{Host: "8.8.8.8", Port: 443}); !ok || target.String() != "8.8.8.8:443" {
		t.Fatal("full name table affected numeric policy")
	}
}

func TestPreviewNamedUDPRecoversCapacityAfterResolverOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pinned = "active.example"
		const fresh = "new.example"
		relay := &pilotClientRelay{namedUDP: true, publicIPv4: true}
		healthy := true
		queries := 0
		relay.remoteDNS = func(_ context.Context, wire []byte, _ bool) ([]byte, error) {
			queries++
			if !healthy {
				return nil, errors.New("resolver unavailable")
			}
			var request dnsmessage.Message
			if err := request.Unpack(wire); err != nil {
				t.Fatal(err)
			}
			return namedTestReply(t, wire, namedTestAnswer(request.Questions[0].Name.String(), "8.8.8.8")), nil
		}
		lookup := func(host string) bool {
			_, ok := relay.allowedTarget(t.Context(), socksUDPPacket{Host: host, Port: 443})
			return ok
		}
		if !lookup(pinned) {
			t.Fatal("initial destination was not admitted")
		}
		healthy = false
		for i := 1; i < pilotMaxFlows; i++ {
			if lookup(fmt.Sprintf("unavailable%d.example", i)) {
				t.Fatal("resolver error admitted a destination")
			}
		}
		healthy = true
		if lookup(fresh) || queries != pilotMaxFlows {
			t.Fatal("an unexpired negative entry lost its reserved retry interval")
		}
		time.Sleep(previewNamedRetry)
		if !lookup(fresh) {
			t.Fatal("new destination still refused after all failed lookups expired")
		}
		if !lookup(pinned) || queries != pilotMaxFlows+1 || len(relay.namedTargets) > pilotMaxFlows {
			t.Fatalf("active pin or admission bound lost: queries=%d entries=%d", queries, len(relay.namedTargets))
		}
	})
}

func TestPreviewNamedUDPRequiresOptInAndValidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		relay  pilotClientRelay
		packet socksUDPPacket
	}{
		{"disabled", pilotClientRelay{publicIPv4: true}, socksUDPPacket{Host: "localhost", Port: 443}},
		{"no remote resolver", pilotClientRelay{namedUDP: true, publicIPv4: true}, socksUDPPacket{Host: "localhost", Port: 443}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.relay.allowedTarget(context.Background(), tc.packet); ok {
				t.Fatal("unexpected admission")
			}
		})
	}
	relay := &pilotClientRelay{namedUDP: true, publicIPv4: true, remoteDNS: func(context.Context, []byte, bool) ([]byte, error) {
		t.Fatal("invalid input queried DNS")
		return nil, nil
	}}
	for _, packet := range []socksUDPPacket{
		{Host: "x..example", Port: 443}, {Host: strings.Repeat("x", 64) + ".example", Port: 443},
		{Host: "game.example\x00", Port: 443}, {Host: "[::1]", Port: 443}, {Host: "game.example", Port: 53},
		{Host: "game.example", Port: 853}, {Host: "game.example", Port: 65536}, {Host: "game.example", Port: 443, Payload: make([]byte, pilotMaxPayload+1)},
	} {
		if _, ok := relay.allowedTarget(context.Background(), packet); ok {
			t.Fatal("unexpected admission")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := relay.allowedTarget(ctx, socksUDPPacket{Host: "game.example", Port: 443}); ok {
		t.Fatal("canceled lookup admitted")
	}
}

func TestPreviewNamedUDPRejectsUnrelatedAndMalformedDNS(t *testing.T) {
	query, err := previewNamedQuery("game.example.", dnsmessage.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		answers []dnsmessage.Resource
		mutate  func(*dnsmessage.Message)
	}{
		{name: "unrelated", answers: []dnsmessage.Resource{namedTestAnswer("other.example.", "8.8.8.8")}},
		{name: "alias loop", answers: []dnsmessage.Resource{namedTestAlias("game.example.", "edge.example."), namedTestAlias("edge.example.", "game.example.")}},
		{name: "conflicting aliases", answers: []dnsmessage.Resource{namedTestAlias("game.example.", "one.example."), namedTestAlias("game.example.", "two.example."), namedTestAnswer("one.example.", "8.8.8.8")}},
		{name: "alias and address", answers: []dnsmessage.Resource{namedTestAlias("game.example.", "edge.example."), namedTestAnswer("game.example.", "8.8.8.8")}},
		{name: "truncated", mutate: func(m *dnsmessage.Message) { m.Truncated = true }},
		{name: "wrong id", mutate: func(m *dnsmessage.Message) { m.ID++ }},
		{name: "servfail", mutate: func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeServerFailure }},
		{name: "wrong question", mutate: func(m *dnsmessage.Message) {
			m.Questions = []dnsmessage.Question{{Name: dnsmessage.MustNewName("other.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := query
			reply.Response = true
			reply.Answers = tc.answers
			if tc.mutate != nil {
				reply.Answers = []dnsmessage.Resource{namedTestAnswer("game.example.", "8.8.8.8")}
				tc.mutate(&reply)
			}
			wire, err := reply.Pack()
			if err != nil {
				t.Fatal(err)
			}
			if got := previewNamedAddresses(query, wire); len(got) != 0 {
				t.Fatalf("admitted %v", got)
			}
		})
	}
}
