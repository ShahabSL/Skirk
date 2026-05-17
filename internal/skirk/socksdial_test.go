package skirk

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSOCKSClientDialer(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	socks := SOCKSServer{
		Listen: "127.0.0.1:0",
		Handler: func(ctx context.Context, target string, conn net.Conn) {
			remote, err := net.Dial("tcp", target)
			if err != nil {
				return
			}
			defer remote.Close()
			go io.Copy(remote, conn)
			io.Copy(conn, remote)
		},
	}
	listener, err := net.Listen("tcp", socks.Listen)
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		socks.Listen = addr
		_ = socks.Serve(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	conn, err := dialViaSOCKS5(context.Background(), "socks5h://"+addr, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}
}

func TestSOCKSRejectsMappedDNSOverTLSProbe(t *testing.T) {
	called := make(chan struct{}, 1)
	socks := SOCKSServer{
		Listen: "127.0.0.1:0",
		Handler: func(ctx context.Context, target string, conn net.Conn) {
			called <- struct{}{}
		},
	}
	listener, err := net.Listen("tcp", socks.Listen)
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		socks.Listen = addr
		_ = socks.Serve(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, methodReply); err != nil {
		t.Fatal(err)
	}
	request := []byte{0x05, socksCommandConnect, 0x00, 0x01, 198, 18, 0, 2, 0, 0}
	binary.BigEndian.PutUint16(request[8:10], 853)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0x04 {
		t.Fatalf("expected mapped DNS-over-TLS probe to be rejected with host unreachable, got 0x%02x", reply[1])
	}
	select {
	case <-called:
		t.Fatal("handler should not be called for mapped DNS-over-TLS probes")
	default:
	}
}

func TestSOCKSUDPInTCPRejectsNonDNSPackets(t *testing.T) {
	socks := SOCKSServer{Listen: "127.0.0.1:0"}
	listener, err := net.Listen("tcp", socks.Listen)
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		socks.Listen = addr
		_ = socks.Serve(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, methodReply); err != nil {
		t.Fatal(err)
	}
	request := []byte{0x05, socksCommandUDPInTCP, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("udp-in-tcp reply = 0x%02x, want success before per-packet refusal", reply[1])
	}
	if _, err := conn.Write(buildUDPInTCPFrame("example.com", 443, []byte("quic"))); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); err == nil || n != 0 {
		t.Fatalf("non-DNS udp-in-tcp packet kept session open: n=%d err=%v", n, err)
	}
}

func TestSOCKSDNSAAAAQueriesReturnNoAnswers(t *testing.T) {
	response, err := answerDNSQuery(dnsQuestion("example.com", 28))
	if err != nil {
		t.Fatal(err)
	}
	if len(response) < 12 {
		t.Fatalf("short dns response: %d bytes", len(response))
	}
	if rcode := response[3] & 0x0f; rcode != 0 {
		t.Fatalf("AAAA response rcode = %d, want noerror/nodata", rcode)
	}
	if answers := binary.BigEndian.Uint16(response[6:8]); answers != 0 {
		t.Fatalf("AAAA answers = %d, want 0", answers)
	}
}

func TestDialViaHTTPConnectProxy(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	proxyTargets := make(chan string, 1)
	proxy := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				t.Errorf("method = %s, want CONNECT", r.Method)
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			proxyTargets <- r.Host
			remote, err := net.DialTimeout("tcp", r.Host, 2*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer is not hijackable")
				http.Error(w, "not hijackable", http.StatusInternalServerError)
				remote.Close()
				return
			}
			client, _, err := hijacker.Hijack()
			if err != nil {
				remote.Close()
				return
			}
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() {
				defer client.Close()
				defer remote.Close()
				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(remote, client)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(client, remote)
					done <- struct{}{}
				}()
				<-done
			}()
		}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = proxy.Serve(listener) }()
	defer proxy.Close()

	conn, err := DialViaProxy(context.Background(), "http://"+listener.Addr().String(), echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q, want pong", buf)
	}
	select {
	case target := <-proxyTargets:
		if target != echo.Addr().String() {
			t.Fatalf("proxy target = %q, want %q", target, echo.Addr().String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not see CONNECT target")
	}
}

func dnsQuestion(name string, qtype uint16) []byte {
	msg := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	msg = binary.BigEndian.AppendUint16(msg, 1)
	return msg
}
