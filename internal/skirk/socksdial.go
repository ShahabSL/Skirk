package skirk

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func dialViaSOCKS5(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	proxyAddr := u.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr += ":1080"
	}
	dialer := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 proxy rejected no-auth method")
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
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		conn.Close()
		return nil, err
	}
	if head[0] != 0x05 || head[1] != 0x00 {
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
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func DialViaSOCKS5(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	return dialViaSOCKS5(ctx, proxyURL, target)
}
