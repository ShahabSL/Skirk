package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func startSOCKSUDPProxyTestControl(t *testing.T, serve func(net.Conn) error) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		done <- serve(conn)
	}()
	return listener.Addr().String(), done
}

func expectSOCKSUDPProxyBytes(conn net.Conn, want []byte) error {
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("unexpected SOCKS5 test handshake bytes")
	}
	return nil
}

func mockSOCKSUDPProxyHandshake(conn net.Conn, auth bool, reply []byte) error {
	method := byte(0)
	if auth {
		method = 2
	}
	if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 1, method}); err != nil {
		return err
	}
	if err := writeAll(conn, []byte{5, method}); err != nil {
		return err
	}
	if auth {
		if err := expectSOCKSUDPProxyBytes(conn, []byte{1, 3, 'u', ':', 'r', 3, 'p', '@', 's'}); err != nil {
			return err
		}
		if err := writeAll(conn, []byte{1, 0}); err != nil {
			return err
		}
	}
	if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	return writeAll(conn, reply)
}

func TestSOCKSUDPProxyRoundTripAndControlLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, network, relayIP, advertisedIP string
		auth, cancelClient                   bool
	}{
		{"IPv4 wildcard relay and control EOF", "udp4", "127.0.0.1", "0.0.0.0", false, false},
		{"hostname relay and client context", "udp4", "127.0.0.1", "localhost", false, true},
		{"IPv6 relay and authenticated context", "udp6", "::1", "::1", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay, err := net.ListenUDP(tc.network, &net.UDPAddr{IP: net.ParseIP(tc.relayIP)})
			if err != nil && tc.network == "udp6" {
				t.Skipf("IPv6 loopback unavailable: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			intruder, err := net.ListenUDP(tc.network, &net.UDPAddr{IP: net.ParseIP(tc.relayIP)})
			if err != nil {
				t.Fatal(err)
			}
			defer intruder.Close()
			_ = relay.SetDeadline(time.Now().Add(time.Second))
			port := relay.LocalAddr().(*net.UDPAddr).Port
			end := make(chan struct{})
			defer close(end)
			address, done := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
				reply := append([]byte{5, 0, 0}, buildSOCKSAddress(tc.advertisedIP, port)...)
				if err := mockSOCKSUDPProxyHandshake(conn, tc.auth, reply); err != nil {
					return err
				}
				if tc.cancelClient {
					var extra [1]byte
					_, err := conn.Read(extra[:])
					if !errors.Is(err, io.EOF) {
						return fmt.Errorf("client cancellation did not close control: %w", err)
					}
					return nil
				}
				<-end
				return conn.Close()
			})

			proxyURL := &url.URL{Scheme: "socks5h", Host: address}
			if tc.auth {
				proxyURL.User = url.UserPassword("u:r", "p@s")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			proxy, err := dialSOCKSUDPProxy(ctx, proxyURL.String())
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			if err := proxy.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			_ = proxy.udp.SetReadDeadline(time.Now().Add(time.Second))
			var data [2048]byte
			for _, target := range []netip.AddrPort{netip.MustParseAddrPort("192.0.2.3:4242"), netip.MustParseAddrPort("[2001:db8::3]:4242")} {
				payload := []byte("probe datagram")
				if n, err := proxy.WriteToUDPAddrPort(payload, target); err != nil || n != len(payload) {
					t.Fatalf("send payload bytes=%d, err=%v", n, err)
				}
				n, source, err := relay.ReadFromUDPAddrPort(data[:])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data[:n], buildSOCKSUDPDatagram(target.Addr().String(), int(target.Port()), payload)) {
					t.Fatal("outbound relay frame did not match requested address and payload")
				}
				spoof := buildSOCKSUDPDatagram(target.Addr().String(), int(target.Port()), []byte("wrong relay"))
				if _, err := intruder.WriteToUDPAddrPort(spoof, source); err != nil {
					t.Fatal(err)
				}
				fragment, reserved := bytes.Clone(spoof), bytes.Clone(spoof)
				fragment[2], reserved[0] = 1, 1
				for _, raw := range [][]byte{
					fragment, reserved, spoof[:5], buildSOCKSUDPDatagram("localhost", 4242, payload),
					buildSOCKSUDPDatagram(target.Addr().String(), 0, payload),
					buildSOCKSUDPDatagram(target.Addr().String(), int(target.Port()), payload),
				} {
					if _, err := relay.WriteToUDPAddrPort(raw, source); err != nil {
						t.Fatal(err)
					}
				}
				n, got, err := proxy.ReadFromUDPAddrPort(data[:])
				if err != nil || got != target || !bytes.Equal(data[:n], payload) {
					t.Fatalf("invalid or spoofed relay reply escaped filtering: peer=%v bytes=%d err=%v", got, n, err)
				}
			}
			_ = proxy.udp.SetReadDeadline(time.Time{})
			readErr := make(chan error, 1)
			go func() {
				_, _, err := proxy.ReadFromUDPAddrPort(data[:])
				readErr <- err
			}()
			if tc.cancelClient {
				cancel()
			} else {
				// Signal the test server to close the established TCP channel.
				end <- struct{}{}
			}
			select {
			case got := <-readErr:
				if !errors.Is(got, net.ErrClosed) {
					t.Fatalf("UDP socket survived control teardown: %v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("UDP read was not unblocked by association teardown")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSOCKSUDPProxyRejectsUnsupportedOrMalformedHandshake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply []byte
		want  error
	}{
		{"UDP command refused", []byte{5, 7, 0, 1}, errSOCKSUDPUnsupported},
		{"invalid version", []byte{4, 0, 0, 1}, nil},
		{"reserved byte set", []byte{5, 0, 1, 1}, nil},
		{"unsupported address type", []byte{5, 0, 0, 2}, nil},
		{"truncated IPv6 reply", []byte{5, 0, 0, 4, 0}, nil},
		{"zero relay port", []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, done := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
				return mockSOCKSUDPProxyHandshake(conn, false, tc.reply)
			})
			proxy, err := dialSOCKSUDPProxy(t.Context(), "socks5://"+address)
			if proxy != nil || err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("unexpected result for malformed upstream: proxy=%t err=%v", proxy != nil, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := dialSOCKSUDPProxy(t.Context(), "http://127.0.0.1:1"); !errors.Is(err, errSOCKSUDPUnsupported) {
		t.Fatalf("unexpected unsupported HTTP UDP proxy result: %v", err)
	}
	const secret = "never-reveal-these-credentials"
	for _, target := range []string{"socks5://" + secret + ":%XY@127.0.0.1:1", "socks5://" + secret + "@127.0.0.1:1"} {
		if _, err := dialSOCKSUDPProxy(t.Context(), target); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("invalid proxy URL was accepted or exposed credentials")
		}
	}
}

func TestSOCKSUDPProxyCancelDuringHandshake(t *testing.T) {
	started := make(chan struct{})
	address, server := startSOCKSUDPProxyTestControl(t, func(conn net.Conn) error {
		if err := expectSOCKSUDPProxyBytes(conn, []byte{5, 1, 0}); err != nil {
			return err
		}
		close(started)
		var extra [1]byte
		_, err := conn.Read(extra[:])
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("handshake cancellation did not close control: %w", err)
		}
		return nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := dialSOCKSUDPProxy(ctx, "socks5://"+address)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("proxy did not begin handshake")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handshake returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed to cancel blocked upstream handshake")
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}
