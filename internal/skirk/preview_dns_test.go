package skirk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestPreviewExitDNSConfiguration(t *testing.T) {
	for _, tc := range []struct {
		label, address, name, errorField string
		enabled                          bool
		want                             previewDNSResolver
	}{
		{label: "default", want: previewDNSResolver{address: previewDNSUpstream, tlsName: previewDNSName}},
		{label: "IPv6", address: "[::1]:8853", name: "resolver.example.", enabled: true,
			want: previewDNSResolver{address: "[::1]:8853", tlsName: "resolver.example"}},
		{label: "only endpoint", address: "127.0.0.1:853", enabled: true, errorField: "exit_dns_tls_name"},
		{label: "only name", name: "resolver.example", enabled: true, errorField: "exit_dns_address"},
		{label: "disabled", address: "127.0.0.1:853", name: "resolver.example", errorField: "udp_preview_remote_dns"},
		{label: "hostname dial", address: "resolver.example:853", name: "resolver.example", enabled: true, errorField: "exit_dns_address"},
		{label: "zero port", address: "127.0.0.1:0", name: "resolver.example", enabled: true, errorField: "exit_dns_address"},
		{label: "multicast", address: "[ff02::1]:853", name: "resolver.example", enabled: true, errorField: "exit_dns_address"},
		{label: "unspecified", address: "[::ffff:0.0.0.0]:853", name: "resolver.example", enabled: true, errorField: "exit_dns_address"},
		{label: "IP-only TLS name", address: "127.0.0.1:853", name: "127.0.0.1", enabled: true, errorField: "exit_dns_tls_name"},
		{label: "wildcard name", address: "127.0.0.1:853", name: "*.resolver.example", enabled: true, errorField: "exit_dns_tls_name"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			config := Config{
				Secret: "local-exit-dns-config-fixture-only",
				Drive:  DriveConfig{FolderID: "isolated-drive-folder"},
				Tunnel: TunnelConfig{
					ExitDNSAddress: tc.address, ExitDNSTLSName: tc.name,
					UDPPreviewRemoteDNS: tc.enabled, UDPPreviewPublicIPv4: tc.enabled,
				},
			}
			config.ApplyDefaults()
			store := NewDriveStore(NewGoogleHTTPClient(RouteConfig{Mode: "direct"}), "", config.Drive)
			err := config.Validate()
			if tc.errorField != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorField) {
					t.Fatalf("Validate() = %v, expected error mentioning %s", err, tc.errorField)
				}
				if _, err := NewTunnel(store, &config); err == nil || !strings.Contains(err.Error(), tc.errorField) {
					t.Fatalf("NewTunnel() = %v, expected error mentioning %s", err, tc.errorField)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tunnel, err := NewTunnel(store, &config)
			if err != nil {
				t.Fatal(err)
			}
			if tunnel.exitDNSUpstream != tc.want {
				t.Fatalf("resolver = %+v, want %+v", tunnel.exitDNSUpstream, tc.want)
			}
		})
	}
}

func TestPreviewExitDNSAuthenticatesConfiguredResolver(t *testing.T) {
	// Reuse Go's localhost TLS test certificate for the raw DNS fixture.
	certificateSource := httptest.NewTLSServer(nil)
	certificate := certificateSource.TLS.Certificates[0]
	trusted := x509.NewCertPool()
	trusted.AddCert(certificateSource.Certificate())
	certificateSource.Close()
	serverNames := make(chan string, 4)
	upstream, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- hello.ServerName
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		for {
			connection, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				defer context.AfterFunc(ctx, func() { _ = connection.Close() })()
				wire, err := readPreviewDNSFrame(connection, previewDNSMaxQuery)
				if err != nil {
					return
				}
				query, err := unpackPreviewDNSQuery(wire)
				if err != nil {
					return
				}
				query.Response = true
				query.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 37},
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
				}}
				if reply, err := query.Pack(); err == nil {
					_ = writePreviewDNSFrame(connection, reply)
				}
			}()
		}
	}()
	front := &testPreviewUpstream{tcpTarget: upstream.Addr().String()}
	proxyURL, proxyErrors := front.start(t, ctx)
	for _, tc := range []struct {
		label, name, proxy string
		roots              *x509.CertPool
		want               dnsmessage.RCode
	}{
		{label: "direct", name: "example.com", roots: trusted, want: dnsmessage.RCodeSuccess},
		{label: "authenticated SOCKS5 exit", name: "example.com", proxy: proxyURL, roots: trusted, want: dnsmessage.RCodeSuccess},
		{label: "mismatched certificate behind proxy", name: "wrong.invalid", proxy: proxyURL, roots: trusted, want: dnsmessage.RCodeServerFailure},
		{label: "unknown authority", name: "example.com", roots: x509.NewCertPool(), want: dnsmessage.RCodeServerFailure},
	} {
		t.Run(tc.label, func(t *testing.T) {
			config, err := configuredExitPreviewDNS(TunnelConfig{
				ExitDNSAddress: upstream.Addr().String(), ExitDNSTLSName: tc.name, UDPPreviewRemoteDNS: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			secure := config.tlsConfig()
			secure.RootCAs = tc.roots
			resolver := newPreviewDNSPool(t.Context(), func(ctx context.Context) (net.Conn, error) {
				return config.dial(ctx, tc.proxy, secure)
			})
			request := previewDNSFixture(42, dnsmessage.TypeA)
			wire, err := request.Pack()
			if err != nil {
				t.Fatal(err)
			}
			reply, err := resolver.respond(ctx, wire, true)
			if err != nil {
				t.Fatal(err)
			}
			var message dnsmessage.Message
			if err := message.Unpack(reply); err != nil {
				t.Fatal(err)
			}
			if message.ID != 42 || message.RCode != tc.want || (len(message.Answers) == 1) != (tc.want == dnsmessage.RCodeSuccess) {
				t.Fatalf("reply code=%v answers=%d, want code=%v", message.RCode, len(message.Answers), tc.want)
			}
			select {
			case name := <-serverNames:
				if name != tc.name {
					t.Errorf("TLS SNI = %q, want %q", name, tc.name)
				}
			case <-ctx.Done():
				t.Fatal("resolver never received TLS ClientHello")
			}
		})
	}
	if front.connects.Load() != 2 || front.associates.Load() != 0 {
		t.Fatal("expected both configured proxy requests to use only SOCKS CONNECT")
	}
	select {
	case err := <-proxyErrors:
		t.Fatal(err)
	default:
	}
}

func TestPreviewDNSPreservesRecordsAndReusesConnection(t *testing.T) {
	var dials atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dns := &previewDNS{
		ctx: ctx, idle: make(chan net.Conn, 2), active: make(chan struct{}, 4),
		dial: func(context.Context) (net.Conn, error) {
			dials.Add(1)
			conn, resolver := net.Pipe()
			go func() {
				defer resolver.Close()
				defer context.AfterFunc(ctx, func() { _ = resolver.Close() })()
				for {
					raw, err := readPreviewDNSFrame(resolver, previewDNSMaxQuery)
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
						Header: dnsmessage.ResourceHeader{
							Name: query.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 37,
						},
						Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 1, 0xd, 0xb8, 15: 1}},
					}}
					wire, err := query.Pack()
					if err != nil || writePreviewDNSFrame(resolver, wire) != nil {
						return
					}
				}
			}()
			return conn, nil
		},
	}
	for _, id := range []uint16{11, 12} {
		request := previewDNSFixture(id, dnsmessage.TypeAAAA)
		wire, err := request.Pack()
		if err != nil {
			t.Fatal(err)
		}
		reply, err := dns.respond(ctx, wire, true)
		if err != nil {
			t.Fatal(err)
		}
		var message dnsmessage.Message
		if err := message.Unpack(reply); err != nil {
			t.Fatal(err)
		}
		if message.ID != id || len(message.Answers) != 1 ||
			message.Answers[0].Header.Type != dnsmessage.TypeAAAA || message.Answers[0].Header.TTL != 37 {
			t.Fatal("DNS answer identity, type, or TTL was lost")
		}
	}
	if dials.Load() != 1 {
		t.Fatal("DNS exchange did not reuse its secure stream")
	}
}

func TestPreviewDNSSilentPooledStreamLeavesTimeForFreshRetry(t *testing.T) {
	var dials atomic.Int32
	resolver := newPreviewDNSPool(t.Context(), func(context.Context) (net.Conn, error) {
		dials.Add(1)
		client, peer := net.Pipe()
		go func() {
			defer peer.Close()
			raw, err := readPreviewDNSFrame(peer, previewDNSMaxQuery)
			if err != nil {
				return
			}
			request, err := unpackPreviewDNSQuery(raw)
			if err != nil {
				return
			}
			request.Response = true
			request.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{
					Name: request.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 30,
				},
				Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 1, 0xd, 0xb8, 15: 1}},
			}}
			if reply, err := request.Pack(); err == nil {
				_ = writePreviewDNSFrame(peer, reply)
			}
		}()
		return client, nil
	})
	reused, silent := net.Pipe()
	defer silent.Close()
	firstQuery := make(chan error, 1)
	go func() {
		// The old stream consumes the DNS request but never returns a response.
		_, err := readPreviewDNSFrame(silent, previewDNSMaxQuery)
		firstQuery <- err
	}()
	resolver.release(reused)
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	request := previewDNSFixture(21, dnsmessage.TypeAAAA)
	query, err := request.Pack()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := resolver.respond(ctx, query, true)
	if err != nil {
		t.Fatal(err)
	}
	var answer dnsmessage.Message
	if err := answer.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	if answer.RCode != dnsmessage.RCodeSuccess || len(answer.Answers) != 1 || dials.Load() != 1 {
		t.Fatalf("quiet pooled stream should retry on one fresh dial: code=%v answers=%d dials=%d",
			answer.RCode, len(answer.Answers), dials.Load())
	}
	if err := <-firstQuery; err != nil {
		t.Fatal(err)
	}
}

func TestPreviewDNSRejectsMismatchAndTruncatesUDP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, test := range []struct {
		name      string
		wrongID   bool
		code      dnsmessage.RCode
		truncated bool
	}{
		{name: "mismatch", wrongID: true, code: dnsmessage.RCodeServerFailure},
		{name: "large", truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dns := &previewDNS{
				ctx: ctx, idle: make(chan net.Conn, 2), active: make(chan struct{}, 4),
				dial: func(context.Context) (net.Conn, error) {
					conn, resolver := net.Pipe()
					go func() {
						defer resolver.Close()
						raw, err := readPreviewDNSFrame(resolver, previewDNSMaxQuery)
						if err != nil {
							return
						}
						message, err := unpackPreviewDNSQuery(raw)
						if err != nil {
							return
						}
						message.Response = true
						if test.wrongID {
							message.ID++
						} else {
							for i := 0; i < 8; i++ {
								message.Answers = append(message.Answers, dnsmessage.Resource{
									Header: dnsmessage.ResourceHeader{
										Name: message.Questions[0].Name, Class: dnsmessage.ClassINET,
									},
									Body: &dnsmessage.TXTResource{TXT: []string{string(make([]byte, 100))}},
								})
							}
						}
						if reply, err := message.Pack(); err == nil {
							_ = writePreviewDNSFrame(resolver, reply)
						}
					}()
					return conn, nil
				},
			}
			request := previewDNSFixture(7, dnsmessage.TypeTXT)
			wire, err := request.Pack()
			if err != nil {
				t.Fatal(err)
			}
			reply, err := dns.respond(ctx, wire, true)
			if err != nil {
				t.Fatal(err)
			}
			var message dnsmessage.Message
			if err := message.Unpack(reply); err != nil {
				t.Fatal(err)
			}
			if message.ID != 7 || message.RCode != test.code || message.Truncated != test.truncated || len(message.Answers) != 0 {
				t.Fatalf("incorrect DNS result header: %+v", message.Header)
			}
		})
	}
}

func previewDNSFixture(id uint16, qtype dnsmessage.Type) dnsmessage.Message {
	return dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName("fixture.example."), Type: qtype, Class: dnsmessage.ClassINET,
		}},
	}
}
