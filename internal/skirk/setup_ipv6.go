package skirk

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// ProbeExitDNS checks the same authenticated upstream used by VPN clients.
func ProbeExitDNS(ctx context.Context, proxy string) error {
	ctx, cancel := context.WithTimeout(ctx, previewDNSUpstreamTO)
	defer cancel()
	query, request, err := makeSetupDNSQuery()
	if err != nil {
		return err
	}
	_, err = newExitPreviewDNS(ctx, strings.TrimSpace(proxy)).exchange(ctx, query, request)
	return err
}

// ProbeExitIPv6 verifies IPv6 TCP and UDP using the exit's effective egress.
// Missing or filtered DNS answers leave IPv6 off until the operator enables it.
func ProbeExitIPv6(ctx context.Context, proxy string) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return probeExitIPv6DNS(ctx, strings.TrimSpace(proxy), netip.MustParseAddrPort("[2001:4860:4860::8888]:53"))
}

// ProbeExitProxyUDP checks that a SOCKS5 association can relay an IPv4 DNS
// query. A command-not-supported reply is distinct from a failed probe.
func ProbeExitProxyUDP(ctx context.Context, proxy string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return probeExitProxyUDP(ctx, strings.TrimSpace(proxy), netip.MustParseAddrPort("8.8.8.8:53"))
}

func probeExitProxyUDP(ctx context.Context, proxy string, endpoint netip.AddrPort) (bool, error) {
	query, request, err := makeSetupDNSQuery()
	if err != nil {
		return false, err
	}
	err = exchangeSetupProxyDNS(ctx, proxy, endpoint, query, request)
	if errors.Is(err, errSOCKSUDPUnsupported) {
		return false, nil
	}
	return err == nil, err
}

func probeExitIPv6DNS(ctx context.Context, proxy string, endpoint netip.AddrPort) bool {
	if !endpoint.Addr().Is6() {
		return false
	}
	query, request, err := makeSetupDNSQuery()
	if err != nil {
		return false
	}
	var route net.Dialer
	var tcp net.Conn
	if proxy == "" {
		tcp, err = route.DialContext(ctx, "tcp6", endpoint.String())
	} else {
		tcp, err = DialViaProxy(ctx, proxy, endpoint.String())
	}
	if err != nil {
		return false
	}
	if _, err = exchangePreviewDNS(ctx, tcp, query, request); err != nil {
		tcp.Close()
		return false
	}
	tcp.Close()

	if proxy != "" {
		return exchangeSetupProxyDNS(ctx, proxy, endpoint, query, request) == nil
	}
	udp, err := route.DialContext(ctx, "udp6", endpoint.String())
	if err != nil {
		return false
	}
	defer udp.Close()
	stop := context.AfterFunc(ctx, func() { _ = udp.Close() })
	defer stop()
	if _, err := udp.Write(query); err != nil {
		return false
	}
	var wire [4096]byte
	n, err := udp.Read(wire[:])
	return err == nil && validSetupDNSReply(wire[:n], request)
}

func exchangeSetupProxyDNS(ctx context.Context, proxy string, endpoint netip.AddrPort, query []byte, request dnsmessage.Message) error {
	udp, err := dialSOCKSUDPProxy(ctx, proxy)
	if err != nil {
		return err
	}
	defer udp.Close()
	if _, err := udp.WriteToUDPAddrPort(query, endpoint); err != nil {
		return err
	}
	var wire [4096]byte
	n, source, err := udp.ReadFromUDPAddrPort(wire[:])
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if source != endpoint || !validSetupDNSReply(wire[:n], request) {
		return errors.New("exit proxy returned an unexpected UDP DNS reply")
	}
	return nil
}

func validSetupDNSReply(wire []byte, request dnsmessage.Message) bool {
	var reply dnsmessage.Message
	return reply.Unpack(wire) == nil && reply.Response && !reply.Truncated && reply.OpCode == request.OpCode &&
		reply.ID == request.ID && len(reply.Questions) == 1 && reply.Questions[0] == request.Questions[0]
}

func makeSetupDNSQuery() ([]byte, dnsmessage.Message, error) {
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, dnsmessage.Message{}, err
	}
	request := dnsmessage.Message{
		Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET,
		}},
	}
	query, err := request.Pack()
	return query, request, err
}
