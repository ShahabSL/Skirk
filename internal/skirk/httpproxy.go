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
	"time"

	"golang.org/x/net/http/httpguts"
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
	return serveProxyListener(ctx, listener, s.handle)
}

func (s *HTTPProxyServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		if s.Logger != nil && err != io.EOF {
			s.Logger.Printf("http proxy request failed: %v", err)
		}
		return
	}
	requestBody := req.Body
	defer func() {
		_ = conn.Close()
		_ = requestBody.Close()
	}()
	if req.Method == http.MethodConnect {
		s.handleConnect(ctx, conn, reader, req)
		return
	}
	s.handleHTTPRequest(ctx, bufferedConn{Conn: conn, reader: reader}, req)
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
	protocols, err := proxyUpgradeProtocols(req.Header)
	if err != nil || len(protocols) > 0 && (req.ProtoMajor != 1 || req.ProtoMinor != 1) {
		writeProxyError(client, http.StatusBadRequest, "invalid HTTP/1.1 upgrade request")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	var tunnelSide, proxySide net.Conn
	if len(protocols) > 0 {
		tunnelSide, proxySide = newHTTPUpgradePipe()
	} else {
		tunnelSide, proxySide = net.Pipe()
	}
	stop := context.AfterFunc(ctx, func() { _ = proxySide.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer tunnelSide.Close()
		s.Handler(ctx, target, tunnelSide)
	}()
	defer func() {
		cancel()
		_ = proxySide.Close()
		_ = tunnelSide.Close()
		stop()
		<-done
	}()

	req.RequestURI = ""
	req.Close = len(protocols) == 0
	// Proxy credentials and connection-local headers must never reach the origin.
	localHeaders := []string{"Connection", "Proxy-Authorization", "Proxy-Connection"}
	for _, value := range req.Header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			localHeaders = append(localHeaders, strings.TrimSpace(name))
		}
	}
	if req.Trailer != nil {
		body := &httpProxyTrailerBody{ReadCloser: req.Body, trailer: req.Trailer, localHeaders: localHeaders}
		body.strip()
		req.Body = body
	}
	stripProxyConnectionHeaders(req.Header)
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	if len(protocols) > 0 {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", strings.Join(protocols, ", "))
	} else {
		req.Header.Set("Connection", "close")
	}
	// Request.Write closes its body even on write failure. Own that close only
	// after the client socket closes, so failure cannot drain a stalled client.
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = io.NopCloser(req.Body)
	}
	if err := req.Write(proxySide); err != nil {
		return
	}
	reader := bufio.NewReader(proxySide)
	resp, err := http.ReadResponse(reader, req)
	// An informational response does not complete the request or switch the
	// stream. Bound the count so an origin cannot keep this phase alive forever.
	for interim := 0; err == nil && resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols; interim++ {
		_ = resp.Body.Close()
		if interim >= 16 {
			writeProxyError(client, http.StatusBadGateway, "too many informational responses")
			return
		}
		if err := resp.Write(client); err != nil {
			return
		}
		resp, err = http.ReadResponse(reader, req)
	}
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("http proxy target=%s response failed: %s", targetFingerprint(target), errorSummary(err))
		}
		return
	}
	responseBody := resp.Body
	defer func() {
		cancel()
		_ = proxySide.Close()
		_ = responseBody.Close()
	}()
	// Response.Write also closes on write failure; a draining Close must not
	// precede upstream cancellation when the downstream client disconnects.
	if responseBody != nil && responseBody != http.NoBody {
		resp.Body = io.NopCloser(responseBody)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		selected, err := proxyUpgradeProtocols(resp.Header)
		if err != nil || resp.ProtoMajor != 1 || resp.ProtoMinor != 1 || !proxyUpgradeOffered(protocols, selected) {
			writeProxyError(client, http.StatusBadGateway, "invalid upstream protocol switch")
			return
		}
		stripProxyConnectionHeaders(resp.Header)
		resp.Header.Set("Connection", "Upgrade")
		resp.Header.Set("Upgrade", strings.Join(selected, ", "))
		resp.Body = nil
		resp.Close = false
		if err := resp.Write(client); err != nil {
			return
		}
		bridgeHTTPUpgrade(client, bufferedConn{Conn: proxySide, reader: reader})
		return
	}
	resp.Close = true
	_ = resp.Write(client)
	// A complete HTTP response ends this stream even if the origin delays FIN.
}

func stripProxyConnectionHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	h.Del("Connection")
	h.Del("Proxy-Connection")
}

// ReadRequest fills Trailer only when the body reaches EOF. Filter both its
// declared keys and its final values before Request.Write serializes them.
type httpProxyTrailerBody struct {
	io.ReadCloser
	trailer      http.Header
	localHeaders []string
}

func (b *httpProxyTrailerBody) strip() {
	for _, name := range b.localHeaders {
		b.trailer.Del(name)
	}
}

func (b *httpProxyTrailerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.strip()
	}
	return n, err
}

// Upgrade names are case-insensitive; their optional versions remain exact.
func proxyUpgradeProtocols(h http.Header) ([]string, error) {
	connection := h.Values("Connection")
	upgrade := httpguts.HeaderValuesContainsToken(connection, "upgrade")
	if !upgrade && len(h.Values("Upgrade")) == 0 {
		return nil, nil
	}
	if !upgrade || httpguts.HeaderValuesContainsToken(connection, "close") {
		return nil, fmt.Errorf("missing or conflicting upgrade connection option")
	}
	for _, value := range connection {
		for _, option := range strings.Split(value, ",") {
			if option = strings.Trim(option, " \t"); option != "" && !httpguts.ValidHeaderFieldName(option) {
				return nil, fmt.Errorf("invalid connection option")
			}
		}
	}
	var protocols []string
	for _, value := range h.Values("Upgrade") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.Trim(protocol, " \t")
			if protocol == "" { // HTTP list syntax permits empty members.
				continue
			}
			name, version, hasVersion := strings.Cut(protocol, "/")
			if !httpguts.ValidHeaderFieldName(name) || hasVersion && !httpguts.ValidHeaderFieldName(version) {
				return nil, fmt.Errorf("invalid upgrade protocol")
			}
			protocols = append(protocols, protocol)
		}
	}
	if len(protocols) == 0 {
		return nil, fmt.Errorf("missing upgrade protocol")
	}
	return protocols, nil
}

func proxyUpgradeOffered(offered, selected []string) bool {
	if len(selected) == 0 {
		return false
	}
	for _, protocol := range selected {
		name, version, _ := strings.Cut(protocol, "/")
		found := false
		for _, offer := range offered {
			offerName, offerVersion, _ := strings.Cut(offer, "/")
			if strings.EqualFold(name, offerName) && version == offerVersion {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func bridgeHTTPUpgrade(client, upstream net.Conn) {
	done := make(chan error, 2)
	copyHalf := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if half, ok := dst.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			} else {
				err = dst.Close()
			}
		}
		done <- err
	}
	go copyHalf(upstream, client)
	go copyHalf(client, upstream)
	if err := <-done; err != nil {
		_ = client.Close()
		_ = upstream.Close()
	}
	<-done
}

// The mux delivers remote FIN with CloseWrite. Separate pipe legs preserve that
// EOF without closing the opposite direction, unlike a single net.Pipe pair.
type httpUpgradePipe struct {
	net.Conn // read leg, including its addresses and read deadline
	writer   net.Conn
}

func newHTTPUpgradePipe() (net.Conn, net.Conn) {
	ar, bw := net.Pipe()
	br, aw := net.Pipe()
	return &httpUpgradePipe{Conn: ar, writer: aw}, &httpUpgradePipe{Conn: br, writer: bw}
}

func (c *httpUpgradePipe) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *httpUpgradePipe) CloseWrite() error           { return c.writer.Close() }
func (c *httpUpgradePipe) Close() error {
	_ = c.writer.Close()
	return c.Conn.Close()
}
func (c *httpUpgradePipe) SetWriteDeadline(t time.Time) error { return c.writer.SetWriteDeadline(t) }
func (c *httpUpgradePipe) SetDeadline(t time.Time) error {
	if err := c.Conn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.writer.SetWriteDeadline(t)
}

func (t *Tunnel) ServeHTTPProxyClient(ctx context.Context, listen string) error {
	t.setRole("client")
	mux, err := t.getClientMux(ctx)
	if err != nil {
		return err
	}
	server := HTTPProxyServer{
		Listen: listen,
		Logger: t.Logger,
		Handler: func(connCtx context.Context, target string, conn net.Conn) {
			if err := mux.openClientStream(connCtx, target, conn); err != nil && t.Logger != nil {
				t.Logger.Printf("http proxy client target=%s failed: %s", targetFingerprint(target), errorSummary(err))
			}
		},
	}
	return t.serveClientListener(ctx, mux, server.Serve)
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

func (c bufferedConn) CloseWrite() error {
	if halfCloser, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return halfCloser.CloseWrite()
	}
	return nil
}
