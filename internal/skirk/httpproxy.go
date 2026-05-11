package skirk

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

type HTTPProxyServer struct {
	Listen  string
	Handler SOCKSHandler
	Logger  *log.Logger
}

func (s *HTTPProxyServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, listener)
}

func (s *HTTPProxyServer) ServeListener(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *HTTPProxyServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		if s.Logger != nil && err != io.EOF {
			s.Logger.Printf("http proxy request failed: %v", err)
		}
		return
	}
	defer req.Body.Close()
	if req.Method == http.MethodConnect {
		s.handleConnect(ctx, conn, reader, req)
		return
	}
	s.handleHTTPRequest(ctx, conn, req)
}

func (s *HTTPProxyServer) handleConnect(ctx context.Context, conn net.Conn, reader *bufio.Reader, req *http.Request) {
	target, err := hostWithDefaultPort(req.Host, "443")
	if err != nil {
		writeProxyError(conn, http.StatusBadRequest, err.Error())
		return
	}
	if s.Handler == nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	s.Handler(ctx, target, bufferedConn{Conn: conn, reader: reader})
}

func (s *HTTPProxyServer) handleHTTPRequest(ctx context.Context, client net.Conn, req *http.Request) {
	if req.URL == nil || req.URL.Host == "" {
		writeProxyError(client, http.StatusBadRequest, "absolute proxy URL is required")
		return
	}
	defaultPort := "80"
	if req.URL.Scheme == "https" {
		defaultPort = "443"
	}
	target, err := hostWithDefaultPort(req.URL.Host, defaultPort)
	if err != nil {
		writeProxyError(client, http.StatusBadRequest, err.Error())
		return
	}
	if s.Handler == nil {
		writeProxyError(client, http.StatusBadGateway, "proxy handler unavailable")
		return
	}

	tunnelSide, proxySide := net.Pipe()
	defer proxySide.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer tunnelSide.Close()
		s.Handler(ctx, target, tunnelSide)
	}()

	req.RequestURI = ""
	req.Close = true
	req.Header.Del("Proxy-Connection")
	req.Header.Set("Connection", "close")
	if err := req.Write(proxySide); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(proxySide), req)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("http proxy target=%s response failed: %s", targetFingerprint(target), errorSummary(err))
		}
		return
	}
	defer resp.Body.Close()
	resp.Close = true
	_ = resp.Write(client)
	_ = proxySide.Close()
	<-done
}

func (t *Tunnel) ServeHTTPProxyClient(ctx context.Context, listen string) error {
	t.role = "client"
	server := HTTPProxyServer{
		Listen: listen,
		Logger: t.Logger,
		Handler: func(connCtx context.Context, target string, conn net.Conn) {
			if err := t.handleClientConn(connCtx, target, conn); err != nil && t.Logger != nil {
				t.Logger.Printf("http proxy client target=%s failed: %s", targetFingerprint(target), errorSummary(err))
			}
		},
	}
	return server.Serve(ctx)
}

func hostWithDefaultPort(host, defaultPort string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("missing proxy target host")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host, nil
	}
	if strings.HasPrefix(host, "[") && strings.Contains(host, "]") {
		return host + ":" + defaultPort, nil
	}
	return net.JoinHostPort(host, defaultPort), nil
}

func writeProxyError(conn net.Conn, status int, message string) {
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "Error"
	}
	body := message + "\n"
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", status, statusText, len(body), body)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
