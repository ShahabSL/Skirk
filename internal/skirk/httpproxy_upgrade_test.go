package skirk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func upgradeTestProxy(t *testing.T, handler SOCKSHandler) (*net.TCPConn, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer listener.Close()
		conn, err := listener.Accept()
		if err == nil {
			(&HTTPProxyServer{Handler: handler}).handle(ctx, conn)
		}
	}()
	c, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		cancel()
		_ = listener.Close()
		t.Fatal(err)
	}
	client := c.(*net.TCPConn)
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("HTTP proxy handler did not stop")
		}
	})
	return client, cancel, done
}

func upgradeTestOrigin(t *testing.T, serve func(net.Conn)) (string, SOCKSHandler, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		serve(conn)
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String(), upgradeTestDialHandler(t), done
}

func upgradeTestDialHandler(t *testing.T) SOCKSHandler {
	t.Helper()
	return func(ctx context.Context, target string, conn net.Conn) {
		remote, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
		if err != nil {
			t.Errorf("owned origin dial: %v", err)
			return
		}
		defer remote.Close()
		stop := context.AfterFunc(ctx, func() { _ = remote.Close(); _ = conn.Close() })
		defer stop()
		// Independent transport surrogate: propagate both EOFs like the mux.
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(remote, conn); _ = remote.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
		go func() {
			_, _ = io.Copy(conn, remote)
			if half, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			} else {
				_ = conn.Close()
			}
			done <- struct{}{}
		}()
		<-done
		<-done
	}
}

func upgradeRequest(target, protocol string) string {
	return "GET http://" + target + "/socket HTTP/1.1\r\nHost: " + target + "\r\nConnection: keep-alive, UpGrAdE, X-Hop-Secret\r\nUpgrade: " + protocol + "\r\nX-Hop-Secret: local-only\r\nProxy-Authorization: Basic synthetic-only\r\nProxy-Connection: keep-alive\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
}

func wsTestFrame(op byte, body []byte, masked bool) []byte {
	frame := []byte{0x80 | op, byte(len(body))}
	if !masked {
		return append(frame, body...)
	}
	frame[1] |= 0x80
	mask := []byte{0x31, 0x72, 0xb3, 0xf4}
	frame = append(frame, mask...)
	for i, b := range body {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func readWSTestFrame(r io.Reader, masked bool) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0]&0x80 == 0 || h[0]&0x70 != 0 || (h[1]&0x80 != 0) != masked || h[1]&0x7f > 125 {
		return 0, nil, fmt.Errorf("invalid short WebSocket frame %x", h)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	body := make([]byte, h[1]&0x7f)
	_, err := io.ReadFull(r, body)
	if masked {
		for i := range body {
			body[i] ^= mask[i%4]
		}
	}
	return h[0] & 15, body, err
}

func TestHTTPProxyWebSocketUpgradePreservesBufferedFrames(t *testing.T) {
	target, handler, originDone := upgradeTestOrigin(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			t.Errorf("origin request: %v", err)
			return
		}
		defer req.Body.Close()
		if req.URL.IsAbs() || req.URL.Path != "/socket" || req.Close || req.Header.Get("Upgrade") != "websocket" {
			t.Errorf("invalid forwarded upgrade: uri=%s close=%v headers=%v", req.URL, req.Close, req.Header)
		}
		for _, name := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Hop-Secret"} {
			if req.Header.Get(name) != "" {
				t.Errorf("origin received connection-local %s", name)
			}
		}
		digest := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		header := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(digest[:]) + "\r\n\r\n"
		if err := writeAll(conn, append([]byte(header), wsTestFrame(1, []byte("ready"), false)...)); err != nil {
			t.Errorf("origin handshake: %v", err)
			return
		}
		for _, expected := range []byte{9, 1, 8} {
			op, body, err := readWSTestFrame(r, true)
			if err != nil || op != expected {
				t.Errorf("origin frame opcode=%d want=%d error=%v", op, expected, err)
				return
			}
			if op == 9 {
				op = 10
			}
			if err := writeAll(conn, wsTestFrame(op, body, false)); err != nil {
				t.Errorf("origin frame write: %v", err)
				return
			}
		}
	})
	client, _, _ := upgradeTestProxy(t, handler)
	// The first frame is already in the request reader when HTTP parsing ends.
	if err := writeAll(client, append([]byte(upgradeRequest(target, "websocket")), wsTestFrame(9, []byte("ping"), true)...)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(client)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("WebSocket handshake not preserved: %+v", resp)
	}
	for _, want := range []struct {
		op   byte
		body string
	}{{1, "ready"}, {10, "ping"}} {
		op, body, err := readWSTestFrame(r, false)
		if err != nil || op != want.op || string(body) != want.body {
			t.Fatalf("buffered frame opcode=%d body=%q error=%v", op, body, err)
		}
	}
	for _, frame := range []struct {
		op   byte
		body []byte
	}{{1, []byte("message")}, {8, []byte{3, 232}}} {
		if err := writeAll(client, wsTestFrame(frame.op, frame.body, true)); err != nil {
			t.Fatal(err)
		}
		op, body, err := readWSTestFrame(r, false)
		if err != nil || op != frame.op || !bytes.Equal(body, frame.body) {
			t.Fatalf("WebSocket echo/close opcode=%d body=%x error=%v", op, body, err)
		}
	}
	<-originDone
}

func TestHTTPProxyUpgradeHalfCloseBothDirections(t *testing.T) {
	for _, originFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("origin-first=", originFirst), func(t *testing.T) {
			observed := make(chan string, 1)
			target, handler, _ := upgradeTestOrigin(t, func(conn net.Conn) {
				r := bufio.NewReader(conn)
				if _, err := http.ReadRequest(r); err != nil {
					t.Errorf("origin request: %v", err)
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: example/1\r\n\r\n")
				if originFirst {
					_, _ = io.WriteString(conn, "origin-before-fin")
					_ = conn.(*net.TCPConn).CloseWrite()
				}
				body, err := io.ReadAll(r)
				if err != nil {
					t.Errorf("origin read after upgrade: %v", err)
				}
				observed <- string(body)
				if !originFirst {
					_, _ = io.WriteString(conn, "origin-after-client-fin")
					_ = conn.(*net.TCPConn).CloseWrite()
				}
			})
			client, _, _ := upgradeTestProxy(t, handler)
			_, _ = io.WriteString(client, upgradeRequest(target, "EXAMPLE/1, other/2"))
			r := bufio.NewReader(client)
			resp, err := http.ReadResponse(r, nil)
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("upgrade response=%v error=%v", resp, err)
			}
			if originFirst {
				body, err := io.ReadAll(r)
				if err != nil || string(body) != "origin-before-fin" {
					t.Fatalf("origin EOF did not preserve data: %q %v", body, err)
				}
			}
			if _, err := io.WriteString(client, "client-after-upgrade"); err != nil {
				t.Fatal(err)
			}
			_ = client.CloseWrite()
			if !originFirst {
				body, err := io.ReadAll(r)
				if err != nil || string(body) != "origin-after-client-fin" {
					t.Fatalf("client EOF closed reverse direction: %q %v", body, err)
				}
			}
			select {
			case body := <-observed:
				if body != "client-after-upgrade" {
					t.Fatalf("origin body=%q", body)
				}
			case <-time.After(time.Second):
				t.Fatal("origin never received client EOF")
			}
		})
	}
}

func TestHTTPProxyUpgradeAfterInformationalResponse(t *testing.T) {
	client, _, _ := upgradeTestProxy(t, func(_ context.Context, _ string, conn net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			t.Errorf("origin request: %v", err)
			return
		}
		// Both HTTP headers and upgraded bytes can arrive in one mux DATA frame.
		_, _ = io.WriteString(conn, "HTTP/1.1 103 Early Hints\r\nLink: </style.css>; rel=preload\r\n\r\nHTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: example\r\n\r\n\x00\x81payload")
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
		_, _ = io.Copy(io.Discard, conn)
	})
	_, _ = io.WriteString(client, upgradeRequest("example.test", "example"))
	r := bufio.NewReader(client)
	for _, status := range []int{103, 101} {
		resp, err := http.ReadResponse(r, nil)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("response=%v want=%d error=%v", resp, status, err)
		}
		_ = resp.Body.Close()
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "\x00\x81payload" {
		t.Fatalf("upgraded bytes after 103/101=%q error=%v", got, err)
	}
	_ = client.CloseWrite()
}

func TestHTTPProxyUpgradeValidationAndNonSwitchResponse(t *testing.T) {
	for _, test := range []struct {
		name, requestHeaders, response string
		status                         int
		noOrigin                       bool
	}{
		{"missing-connection", "Upgrade: websocket\r\n", "", 400, true},
		{"missing-protocol", "Connection: Upgrade\r\n", "", 400, true},
		{"invalid-protocol", "Connection: Upgrade\r\nUpgrade: web socket\r\n", "", 400, true},
		{"invalid-protocol-whitespace", "Connection: Upgrade\r\nUpgrade: \u00a0websocket\u00a0\r\n", "", 400, true},
		{"invalid-version", "Connection: Upgrade\r\nUpgrade: example/1/2\r\n", "", 400, true},
		{"invalid-connection-token", "Connection: Upgrade, bad token\r\nUpgrade: websocket\r\n", "", 400, true},
		{"conflicting-close", "Connection: close, Upgrade\r\nUpgrade: websocket\r\n", "", 400, true},
		{"unoffered-protocol", "Connection: Upgrade\r\nUpgrade: websocket\r\n", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: other\r\n\r\n", 502, false},
		{"invalid-response-token", "Connection: Upgrade\r\nUpgrade: websocket\r\n", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: web socket\r\n\r\n", 502, false},
		{"missing-response-connection", "Connection: Upgrade\r\nUpgrade: websocket\r\n", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n", 502, false},
		{"unsolicited-switch", "", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", 502, false},
		{"declined-upgrade", "Connection: Upgrade\r\nUpgrade: websocket\r\n", "HTTP/1.1 403 Forbidden\r\nContent-Length: 2\r\n\r\nno", 403, false},
		{"ordinary-success", "Connection: Upgrade\r\nUpgrade: websocket\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _, done := upgradeTestProxy(t, func(ctx context.Context, _ string, conn net.Conn) {
				if test.noOrigin {
					t.Error("invalid request opened origin stream")
					return
				}
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					t.Errorf("origin read: %v", err)
					return
				}
				_, _ = io.WriteString(conn, test.response)
				<-ctx.Done() // Neither a rejected switch nor a complete body waits for FIN.
			})
			_, _ = io.WriteString(client, "GET http://example.test/ HTTP/1.1\r\nHost: example.test\r\n"+test.requestHeaders+"\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil || resp.StatusCode != test.status {
				t.Fatalf("status=%v want=%d error=%v", resp, test.status, err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if (test.status == 200 || test.status == 403) && !strings.HasSuffix(test.response, string(body)) {
				t.Fatalf("non-switch body changed: %q", body)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("completed response did not cancel upstream")
			}
		})
	}
}

func TestHTTPProxyUpgradeCancellation(t *testing.T) {
	for _, phase := range []string{"request-headers", "request-body", "response-headers", "upgraded-idle", "blocked-write"} {
		t.Run(phase, func(t *testing.T) {
			ready := make(chan struct{})
			client, cancel, done := upgradeTestProxy(t, func(ctx context.Context, _ string, conn net.Conn) {
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				if phase != "response-headers" && phase != "request-body" {
					_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
				}
				close(ready)
				<-ctx.Done()
			})
			if phase != "request-headers" {
				request := upgradeRequest("example.test", "websocket")
				if phase == "request-body" {
					request = strings.TrimSuffix(request, "\r\n") + "Content-Length: 10\r\n\r\n"
				}
				_, _ = io.WriteString(client, request)
				if phase != "response-headers" && phase != "request-body" {
					if _, err := http.ReadResponse(bufio.NewReader(client), nil); err != nil {
						t.Fatal(err)
					}
				}
				<-ready
			}
			writeDone := make(chan struct{})
			if phase == "blocked-write" {
				go func() {
					defer close(writeDone)
					_, _ = io.CopyN(client, zeroReader{}, 16<<20)
				}()
			} else {
				close(writeDone)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop proxy")
			}
			select {
			case <-writeDone:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not release blocked writer")
			}
		})
	}
}

func TestHTTPProxyUpgradeStripsProxyCredentialTrailers(t *testing.T) {
	client, _, _ := upgradeTestProxy(t, func(_ context.Context, _ string, conn net.Conn) {
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			t.Errorf("origin request: %v", err)
			return
		}
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil || string(body) != "body" || req.Trailer.Get("X-End") != "retained" {
			t.Errorf("body/trailer not preserved: body=%q trailers=%v error=%v", body, req.Trailer, err)
		}
		for _, key := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Hop-Secret"} {
			if req.Header.Get(key) != "" || req.Trailer.Get(key) != "" {
				t.Errorf("forwarded local field %s", key)
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: example\r\n\r\n")
		var frame [5]byte
		if _, err := io.ReadFull(r, frame[:]); err != nil || string(frame[:]) != "frame" {
			t.Errorf("bytes buffered after chunked body=%q error=%v", frame, err)
		}
		_, _ = conn.Write(frame[:])
	})
	request := "POST http://example.test/socket HTTP/1.1\r\nHost: example.test\r\nConnection: Upgrade, X-Hop-Secret\r\nUpgrade: example\r\nTransfer-Encoding: chunked\r\nTrailer: Proxy-Authorization, Proxy-Connection, X-Hop-Secret, X-End\r\n\r\n4\r\nbody\r\n0\r\nProxy-Authorization: Basic synthetic-trailer\r\nProxy-Connection: keep-alive\r\nX-Hop-Secret: local-only\r\nX-End: retained\r\n\r\nframe"
	_, _ = io.WriteString(client, request)
	r := bufio.NewReader(client)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade response=%v error=%v", resp, err)
	}
	var frame [5]byte
	if _, err := io.ReadFull(r, frame[:]); err != nil || string(frame[:]) != "frame" {
		t.Fatalf("upgraded echo=%q error=%v", frame, err)
	}
}

func TestHTTPProxyDisconnectDoesNotDrainStalledResponse(t *testing.T) {
	for _, framing := range []string{"fixed", "chunked"} {
		t.Run(framing, func(t *testing.T) {
			releaseBody := make(chan struct{})
			client, _, done := upgradeTestProxy(t, func(ctx context.Context, _ string, conn net.Conn) {
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					t.Errorf("origin request: %v", err)
					return
				}
				header := "Content-Length: 1048576\r\n"
				if framing == "chunked" {
					header = "Transfer-Encoding: chunked\r\n"
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n"+header+"\r\n")
				<-releaseBody
				if framing == "chunked" {
					_, _ = io.WriteString(conn, "8000\r\n")
				}
				_, _ = conn.Write(make([]byte, 32768))
				// Deliberately no final bytes/chunk/FIN despite request Connection: close.
				<-ctx.Done()
			})
			_, _ = io.WriteString(client, "GET http://example.test/ HTTP/1.1\r\nHost: example.test\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("response=%v error=%v", resp, err)
			}
			_ = client.SetLinger(0)
			_ = client.Close()
			close(releaseBody)
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				t.Error("client disconnect drained stalled upstream body before canceling it")
			}
		})
	}
}

func TestHTTPProxyFailedRequestWriteDoesNotDrainClientBody(t *testing.T) {
	client, _, done := upgradeTestProxy(t, func(_ context.Context, _ string, conn net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			t.Errorf("origin request: %v", err)
		}
		// Refuse the origin stream while the client has more body left to send.
		_ = conn.Close()
	})
	request := "POST http://example.test/ HTTP/1.1\r\nHost: example.test\r\nContent-Length: 1048576\r\n\r\n"
	_, _ = client.Write(append([]byte(request), make([]byte, 32768)...))
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("failed upstream request write drained incomplete client body")
	}
}

func TestHTTPProxyPreservesEmptyPOSTFraming(t *testing.T) {
	for _, lengthHeader := range []string{"", "Content-Length: 0\r\n"} {
		t.Run(fmt.Sprint("explicit-length=", lengthHeader != ""), func(t *testing.T) {
			client, _, _ := upgradeTestProxy(t, func(_ context.Context, _ string, conn net.Conn) {
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					t.Errorf("origin request: %v", err)
					return
				}
				if req.ContentLength != 0 || len(req.TransferEncoding) != 0 || req.Header.Get("Content-Length") != "0" {
					t.Errorf("empty POST framing changed: length=%d transfer=%v headers=%v", req.ContentLength, req.TransferEncoding, req.Header)
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
			})
			_, _ = io.WriteString(client, "POST http://example.test/ HTTP/1.1\r\nHost: example.test\r\n"+lengthHeader+"\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil || resp.ContentLength != 0 || len(resp.TransferEncoding) != 0 {
				t.Fatalf("empty response=%v error=%v", resp, err)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestWebSocketTLSOverHTTPConnectAndSOCKS(t *testing.T) {
	for _, mode := range []string{"connect", "socks"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Proxy-Authorization") != "" {
					t.Error("CONNECT credentials entered TLS origin")
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("origin hijack: %v", err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				digest := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
				_ = rw.Flush()
				for _, want := range []byte{9, 8} {
					op, body, err := readWSTestFrame(rw, true)
					if err != nil || op != want {
						t.Errorf("TLS origin frame=%d want=%d error=%v", op, want, err)
						return
					}
					if op == 9 {
						op = 10
					}
					if err := writeAll(conn, wsTestFrame(op, body, false)); err != nil {
						t.Errorf("TLS origin reply: %v", err)
						return
					}
				}
			}))
			defer origin.Close()
			target := strings.TrimPrefix(origin.URL, "https://")
			var raw net.Conn
			if mode == "connect" {
				client, _, _ := upgradeTestProxy(t, upgradeTestDialHandler(t))
				_, _ = fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic synthetic-only\r\n\r\n", target, target)
				r := bufio.NewReader(client)
				resp, err := http.ReadResponse(r, nil)
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("CONNECT response=%v error=%v", resp, err)
				}
				raw = bufferedConn{Conn: client, reader: r}
			} else {
				server, client := net.Pipe()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					(&SOCKSServer{Handler: upgradeTestDialHandler(t)}).handle(ctx, server)
				}()
				t.Cleanup(func() { cancel(); _ = client.Close(); <-done })
				_ = client.SetDeadline(time.Now().Add(3 * time.Second))
				_ = writeAll(client, []byte{5, 2, 2, 0})
				var method [2]byte
				if _, err := io.ReadFull(client, method[:]); err != nil || method != [2]byte{5, 0} {
					t.Fatalf("SOCKS method=%x error=%v", method, err)
				}
				addr, _ := net.ResolveTCPAddr("tcp", target)
				request := append([]byte{5, 1, 0, 1}, addr.IP.To4()...)
				request = append(request, byte(addr.Port>>8), byte(addr.Port))
				_ = writeAll(client, request)
				var reply [10]byte
				if _, err := io.ReadFull(client, reply[:]); err != nil || reply[1] != 0 {
					t.Fatalf("SOCKS CONNECT=%x error=%v", reply, err)
				}
				raw = client
			}
			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			secure := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
			defer secure.Close()
			if err := secure.Handshake(); err != nil {
				t.Fatal(err)
			}
			request := "GET /socket HTTP/1.1\r\nHost: " + target + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
			_, _ = io.WriteString(secure, request)
			r := bufio.NewReader(secure)
			resp, err := http.ReadResponse(r, nil)
			if err != nil || resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
				t.Fatalf("TLS WebSocket handshake=%v error=%v", resp, err)
			}
			for _, frame := range []struct {
				op, reply byte
				body      []byte
			}{{9, 10, []byte("secure-ping")}, {8, 8, []byte{3, 232}}} {
				if err := writeAll(secure, wsTestFrame(frame.op, frame.body, true)); err != nil {
					t.Fatal(err)
				}
				op, body, err := readWSTestFrame(r, false)
				if err != nil || op != frame.reply || !bytes.Equal(body, frame.body) {
					t.Fatalf("TLS WebSocket reply=%d body=%x error=%v", op, body, err)
				}
			}
		})
	}
}
