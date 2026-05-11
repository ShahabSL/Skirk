package skirk

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPProxyConnectTunnelsBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	targetCh := make(chan string, 1)
	server := HTTPProxyServer{
		Listen: "127.0.0.1:0",
		Handler: func(ctx context.Context, target string, conn net.Conn) {
			targetCh <- target
			buf := make([]byte, 4)
			_, _ = io.ReadFull(conn, buf)
			_, _ = conn.Write([]byte("pong"))
		},
	}
	listener, err := net.Listen("tcp", server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	go func() { _ = server.ServeListener(ctx, listener) }()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(reader, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "pong" {
		t.Fatalf("reply = %q, want pong", string(reply))
	}
	if got := <-targetCh; got != "example.com:443" {
		t.Fatalf("target = %q, want example.com:443", got)
	}
}

func TestHTTPProxyAbsoluteRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	targetCh := make(chan string, 1)
	server := HTTPProxyServer{
		Listen: "127.0.0.1:0",
		Handler: func(ctx context.Context, target string, conn net.Conn) {
			targetCh <- target
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				t.Errorf("target request read failed: %v", err)
				return
			}
			if req.URL.Path != "/hello" {
				t.Errorf("path = %q, want /hello", req.URL.Path)
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		},
	}
	listener, err := net.Listen("tcp", server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	go func() { _ = server.ServeListener(ctx, listener) }()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET http://example.com/hello HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", string(body))
	}
	if got := <-targetCh; got != "example.com:80" {
		t.Fatalf("target = %q, want example.com:80", got)
	}
}
