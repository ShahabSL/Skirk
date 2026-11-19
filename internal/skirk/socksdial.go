package skirk

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func DialViaProxy(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, errors.New("invalid outbound proxy URL")
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		return dialViaSOCKS5(ctx, proxyURL, target)
	case "http", "https":
		return dialViaHTTPConnect(ctx, u, target)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func dialViaSOCKS5(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("invalid outbound SOCKS5 proxy URL")
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if err := validateSOCKS5User(u.User); err != nil {
		return nil, err
	}
	proxyPort := u.Port()
	if proxyPort == "" {
		proxyPort = "1080"
	}
	proxyAddr := net.JoinHostPort(u.Hostname(), proxyPort)
	dialer := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	}
	if err := authenticateUpstreamSOCKS5(conn, u.User); err != nil {
		conn.Close()
		return nil, err
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		conn.Close()
		return nil, fmt.Errorf("invalid target port %q", portText)
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 0x01)
			request = append(request, v4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			conn.Close()
			return nil, fmt.Errorf("target host too long for SOCKS5")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, []byte(host)...)
	}
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(port))
	request = append(request, portBuf[:]...)
	if err := writeAll(conn, request); err != nil {
		conn.Close()
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		conn.Close()
		return nil, err
	}
	if head[0] != 0x05 || head[2] != 0 || head[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed rep=0x%02x", head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			conn.Close()
			return nil, err
		}
		skip = int(lenBuf[0])
	case 0x04:
		skip = 16
	default:
		conn.Close()
		return nil, fmt.Errorf("socks5 invalid bind address type 0x%02x", head[3])
	}
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if _, err := io.CopyN(io.Discard, conn, 2); err != nil {
		conn.Close()
		return nil, err
	}
	if !stop() {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// OutboundProxyMode is safe to include in logs even for an authenticated URL.
func OutboundProxyMode(proxyURL string) string {
	if strings.TrimSpace(proxyURL) == "" {
		return "direct"
	}
	if u, err := url.Parse(proxyURL); err == nil {
		switch u.Scheme {
		case "socks5", "socks5h", "http", "https":
			return u.Scheme
		}
	}
	return "invalid"
}

func DialViaSOCKS5(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	return dialViaSOCKS5(ctx, proxyURL, target)
}

func dialViaHTTPConnect(ctx context.Context, u *url.URL, target string) (_ net.Conn, err error) {
	if u.Hostname() == "" {
		return nil, errors.New("invalid outbound HTTP proxy URL")
	}
	proxyPort := u.Port()
	if proxyPort == "" {
		if u.Scheme == "https" {
			proxyPort = "443"
		} else {
			proxyPort = "80"
		}
	}
	proxyAddr := net.JoinHostPort(u.Hostname(), proxyPort)
	dialer := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	raw := conn
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = raw.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
	}()
	if u.Scheme == "https" {
		host := u.Hostname()
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if deadline, ok := ctx.Deadline(); ok {
			_ = tlsConn.SetDeadline(deadline)
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	}
	var request strings.Builder
	request.WriteString("CONNECT ")
	request.WriteString(target)
	request.WriteString(" HTTP/1.1\r\nHost: ")
	request.WriteString(target)
	request.WriteString("\r\nProxy-Connection: Keep-Alive\r\n")
	if u.User != nil {
		password, _ := u.User.Password()
		credential := u.User.Username() + ":" + password
		request.WriteString("Proxy-Authorization: Basic ")
		request.WriteString(base64.StdEncoding.EncodeToString([]byte(credential)))
		request.WriteString("\r\n")
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		conn.Close()
		return nil, fmt.Errorf("http proxy CONNECT failed status=%d", resp.StatusCode)
	}
	// A successful CONNECT switches to opaque tunnel bytes. Closing an HTTP
	// body or discarding read-ahead would consume a server's immediate banner.
	if !stop() {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if reader.Buffered() != 0 {
		return &bufferedProxyConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type bufferedProxyConn struct {
	net.Conn
	reader *bufio.Reader
	readMu sync.Mutex
}

func (c *bufferedProxyConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.reader.Read(p)
}

func (c *bufferedProxyConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return errors.ErrUnsupported
}
