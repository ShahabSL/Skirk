package skirk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

const socksUDPProxyHandshakeTimeout = 10 * time.Second
const socksUDPProxyMaxWire = 65507

var errSOCKSUDPUnsupported = errors.New("upstream proxy does not support SOCKS5 UDP ASSOCIATE")
var errSOCKS5Authentication = errors.New("upstream SOCKS5 authentication failed")

// A SOCKS5 UDP association lives as long as its TCP control connection.
// The UDP socket is connected to the negotiated relay so the kernel filters
// packets from any other source before the SOCKS header is parsed.
type socksUDPProxy struct {
	control net.Conn
	udp     *net.UDPConn
	close   sync.Once
	readMu  sync.Mutex
	readBuf [64 << 10]byte
}

func parseSOCKSUDPProxyURL(proxyURL string) (*url.URL, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid upstream SOCKS5 UDP proxy URL")
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, errSOCKSUDPUnsupported
	}
	if err := validateSOCKS5User(u.User); err != nil {
		return nil, err
	}
	return u, nil
}

func dialSOCKSUDPProxy(parent context.Context, proxyURL string) (_ *socksUDPProxy, err error) {
	u, err := parseSOCKSUDPProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "1080"
	}
	setup, cancel := context.WithTimeout(parent, socksUDPProxyHandshakeTimeout)
	defer cancel()
	defer func() {
		if err != nil && setup.Err() != nil {
			err = setup.Err()
		}
	}()
	dialer := net.Dialer{KeepAlive: 30 * time.Second}
	control, err := dialer.DialContext(setup, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("dial upstream SOCKS5 control: %w", err)
	}
	defer func() {
		if err != nil {
			_ = control.Close()
		}
	}()
	stopSetup := context.AfterFunc(setup, func() { _ = control.Close() })
	defer stopSetup()
	deadline, _ := setup.Deadline()
	if err := control.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := authenticateUpstreamSOCKS5(control, u.User); err != nil {
		return nil, err
	}
	peer := control.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	// An exit behind NAT cannot know the UDP endpoint that the proxy will see.
	unspecified := netip.IPv4Unspecified()
	if peer.Is6() {
		unspecified = netip.IPv6Unspecified()
	}
	request := append([]byte{5, 3, 0}, buildSOCKSAddress(unspecified.String(), 0)...)
	if err := writeAll(control, request); err != nil {
		return nil, fmt.Errorf("send upstream SOCKS5 UDP ASSOCIATE: %w", err)
	}
	relay, err := readSOCKSUDPProxyRelay(setup, control, peer)
	if err != nil {
		return nil, err
	}
	data, err := dialer.DialContext(setup, "udp", relay.String())
	if err != nil {
		return nil, fmt.Errorf("dial negotiated SOCKS5 UDP relay: %w", err)
	}
	if !stopSetup() {
		_ = data.Close()
		return nil, setup.Err()
	}
	if err := control.SetDeadline(time.Time{}); err != nil {
		_ = data.Close()
		return nil, err
	}
	if err := parent.Err(); err != nil {
		_ = data.Close()
		return nil, err
	}
	proxy := &socksUDPProxy{control: control, udp: data.(*net.UDPConn)}
	stop := context.AfterFunc(parent, func() { _ = proxy.Close() })
	go func() {
		defer stop()
		// The protocol carries only UDP over the relay after the handshake.
		// Either EOF or unexpected control data ends this association.
		var extra [1]byte
		_, _ = control.Read(extra[:])
		_ = proxy.Close()
	}()
	return proxy, nil
}

func validateSOCKS5User(user *url.Userinfo) error {
	if user != nil {
		password, present := user.Password()
		if !present || len(user.Username()) < 1 || len(user.Username()) > 255 || len(password) < 1 || len(password) > 255 {
			return errors.New("SOCKS5 username and password must both be 1 to 255 bytes")
		}
	}
	return nil
}

func authenticateUpstreamSOCKS5(control net.Conn, user *url.Userinfo) error {
	method := byte(0)
	if user != nil {
		method = 2
	}
	if err := writeAll(control, []byte{5, 1, method}); err != nil {
		return fmt.Errorf("send upstream SOCKS5 greeting: %w", err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		return fmt.Errorf("read upstream SOCKS5 method: %w", err)
	}
	if reply != [2]byte{5, method} {
		return fmt.Errorf("%w: configured method rejected", errSOCKS5Authentication)
	}
	if user == nil {
		return nil
	}
	username := user.Username()
	password, _ := user.Password()
	request := append([]byte{1, byte(len(username))}, username...)
	request = append(append(request, byte(len(password))), password...)
	if err := writeAll(control, request); err != nil {
		return fmt.Errorf("send upstream SOCKS5 authentication: %w", err)
	}
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		return fmt.Errorf("read upstream SOCKS5 authentication: %w", err)
	}
	if reply != [2]byte{1, 0} {
		return fmt.Errorf("%w: username/password rejected", errSOCKS5Authentication)
	}
	return nil
}

func readSOCKSUDPProxyRelay(ctx context.Context, control io.Reader, peer netip.Addr) (netip.AddrPort, error) {
	var reply [4]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		return netip.AddrPort{}, fmt.Errorf("read upstream SOCKS5 UDP ASSOCIATE: %w", err)
	}
	if reply[0] != 5 || reply[2] != 0 {
		return netip.AddrPort{}, errors.New("invalid upstream SOCKS5 UDP ASSOCIATE header")
	}
	if reply[1] == 7 {
		return netip.AddrPort{}, errSOCKSUDPUnsupported
	}
	if reply[1] != 0 {
		return netip.AddrPort{}, fmt.Errorf("upstream SOCKS5 UDP ASSOCIATE rejected (reply 0x%02x)", reply[1])
	}
	var wire [1 + 1 + 255 + 2]byte
	wire[0] = reply[3]
	start, length := 1, 0
	switch reply[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		if _, err := io.ReadFull(control, wire[1:2]); err != nil {
			return netip.AddrPort{}, fmt.Errorf("read upstream SOCKS5 UDP relay name length: %w", err)
		}
		start, length = 2, int(wire[1])
	default:
		return netip.AddrPort{}, errors.New("invalid upstream SOCKS5 UDP relay address type")
	}
	end := start + length + 2
	if _, err := io.ReadFull(control, wire[start:end]); err != nil {
		return netip.AddrPort{}, fmt.Errorf("read upstream SOCKS5 UDP relay address: %w", err)
	}
	host, port, err := parseSOCKSAddress(wire[:end])
	if err != nil || port == 0 {
		return netip.AddrPort{}, errors.New("invalid upstream SOCKS5 UDP relay endpoint")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		addresses, lookupErr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if lookupErr != nil {
			return netip.AddrPort{}, errors.New("cannot resolve negotiated SOCKS5 UDP relay")
		}
		for _, candidate := range addresses {
			candidate = candidate.Unmap()
			if !usableSOCKSUDPRelay(candidate) {
				continue
			}
			if !addr.IsValid() || candidate.Is4() == peer.Is4() {
				addr = candidate
			}
			if candidate.Is4() == peer.Is4() {
				break
			}
		}
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() {
		addr = peer
	}
	if !usableSOCKSUDPRelay(addr) {
		return netip.AddrPort{}, errors.New("invalid upstream SOCKS5 UDP relay endpoint")
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

func usableSOCKSUDPRelay(addr netip.Addr) bool {
	return addr.IsValid() && !addr.IsUnspecified() && !addr.IsMulticast()
}

func (p *socksUDPProxy) ReadFromUDPAddrPort(dst []byte) (int, netip.AddrPort, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	for {
		n, err := p.udp.Read(p.readBuf[:])
		if err != nil {
			return 0, netip.AddrPort{}, err
		}
		if n <= socksUDPAddressOffset || (p.readBuf[socksUDPAddressOffset] != 1 && p.readBuf[socksUDPAddressOffset] != 4) {
			continue
		}
		packet, err := parseSOCKSUDPDatagram(p.readBuf[:n])
		if err != nil || packet.Port == 0 {
			continue
		}
		addr, err := netip.ParseAddr(packet.Host)
		if err != nil || addr.IsUnspecified() {
			continue
		}
		if len(dst) < len(packet.Payload) {
			return 0, netip.AddrPort{}, io.ErrShortBuffer
		}
		return copy(dst, packet.Payload), netip.AddrPortFrom(addr.Unmap(), uint16(packet.Port)), nil
	}
}

func (p *socksUDPProxy) WriteToUDPAddrPort(payload []byte, target netip.AddrPort) (int, error) {
	addr := target.Addr().Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.Zone() != "" || target.Port() == 0 {
		return 0, errors.New("invalid numeric SOCKS5 UDP destination")
	}
	if len(payload) > socksUDPProxyMaxWire-(socksUDPAddressOffset+1+addr.BitLen()/8+2) {
		return 0, errors.New("SOCKS5 UDP payload exceeds the relay datagram limit")
	}
	wire := buildSOCKSUDPDatagram(addr.String(), int(target.Port()), payload)
	n, err := p.udp.Write(wire)
	if err != nil {
		return 0, err
	}
	if n != len(wire) {
		return 0, io.ErrShortWrite
	}
	return len(payload), nil
}

func (p *socksUDPProxy) SetWriteDeadline(deadline time.Time) error {
	return p.udp.SetWriteDeadline(deadline)
}

func (p *socksUDPProxy) Close() error {
	p.close.Do(func() {
		_ = p.udp.Close()
		_ = p.control.Close()
	})
	return nil
}
