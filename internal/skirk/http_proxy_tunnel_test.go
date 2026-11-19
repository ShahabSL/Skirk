package skirk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPProxyPreservesImmediateBytesAndHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			served <- err
			return
		}
		if req.Method != "CONNECT" || req.Host != "origin.invalid:1234" || req.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			served <- errors.New("invalid authenticated CONNECT request")
			return
		}
		_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\nbanner")
		if err != nil {
			served <- err
			return
		}
		body, err := io.ReadAll(reader)
		if err != nil || string(body) != "request" {
			served <- errors.New("tunnel did not half-close after request")
			return
		}
		_, err = io.WriteString(conn, "response")
		served <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := DialViaProxy(ctx, "http://user:pass@"+listener.Addr().String(), "origin.invalid:1234")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	banner := make([]byte, len("banner"))
	if _, err := io.ReadFull(conn, banner); err != nil || string(banner) != "banner" {
		t.Fatalf("immediate tunnel bytes lost: %q (%v)", banner, err)
	}
	if _, err := io.WriteString(conn, "request"); err != nil {
		t.Fatal(err)
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("CONNECT tunnel lost TCP half-close capability")
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(conn)
	if err != nil || string(body) != "response" {
		t.Fatalf("reply after half-close: %q (%v)", body, err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPProxyHandshakeHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	arrived, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		close(arrived)
		<-release
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := DialViaProxy(ctx, "http://"+listener.Addr().String(), "origin.invalid:1234")
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not see handshake")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wrong cancellation result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled handshake retained its socket")
	}
}
