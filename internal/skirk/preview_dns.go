package skirk

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	previewDNSAddress    = "198.18.0.2:53"
	previewDNSMuxHost    = "skirk-dns-v1.invalid"
	previewDNSMuxTarget  = previewDNSMuxHost + ":53"
	previewDNSUpstream   = "8.8.8.8:853"
	previewDNSName       = "dns.google"
	previewDNSMaxQuery   = 4096
	previewDNSUDPMax     = 1232
	previewDNSTimeout    = 16 * time.Second
	previewDNSUpstreamTO = 5 * time.Second
	previewDNSExitLimit  = 16
)

var (
	errPreviewDNSDisabled = errors.New("dns_streams_disabled")
	errPreviewDNSTarget   = errors.New("dns_target_invalid")
	errPreviewDNSCapacity = errors.New("dns_stream_capacity")
)

type previewDNS struct {
	ctx    context.Context
	dial   func(context.Context) (net.Conn, error)
	idle   chan net.Conn
	active chan struct{}
	idleMu sync.Mutex
	exits  *previewDNSExitPool
}

type previewDNSExitPool struct {
	mu    sync.Mutex
	limit int
	// Entries with a zero timestamp are waiting for their first query or
	// processing one. Only pipes that finished a reply can be reclaimed.
	pipes map[*previewDNSExitPipe]time.Time
}

type previewDNSExitPipe struct {
	net.Conn
	pool *previewDNSExitPool
}

// The reserved DNS stream is a serialized query/reply pipe. Its owner closes
// it only when it stops using both directions, so mux FIN must close both ends
// of this local net.Pipe adapter.
type previewDNSMuxConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *previewDNSMuxConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	if errors.Is(err, io.ErrClosedPipe) {
		// Mux sends its own orderly FIN rather than a reset on local EOF.
		err = io.EOF
	}
	return n, err
}

func (c *previewDNSMuxConn) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	return c.Conn.Close()
}

func (c *previewDNSMuxConn) CloseWrite() error { return c.Close() }

func newPreviewDNSExitPool(limit int) *previewDNSExitPool {
	return &previewDNSExitPool{limit: limit, pipes: make(map[*previewDNSExitPipe]time.Time, limit)}
}

func (p *previewDNSExitPool) open() (net.Conn, *previewDNSExitPipe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pipes) >= p.limit {
		var oldest *previewDNSExitPipe
		var oldestSince time.Time
		for candidate, idleSince := range p.pipes {
			if !idleSince.IsZero() && (oldest == nil || idleSince.Before(oldestSince)) {
				oldest, oldestSince = candidate, idleSince
			}
		}
		if oldest == nil {
			return nil, nil
		}
		delete(p.pipes, oldest)
		_ = oldest.Close()
	}
	local, handler := net.Pipe()
	pipe := &previewDNSExitPipe{Conn: handler, pool: p}
	p.pipes[pipe] = time.Time{}
	return local, pipe
}

func (p *previewDNSExitPool) setIdleSince(pipe *previewDNSExitPipe, since time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pipes[pipe]; !exists {
		return false
	}
	p.pipes[pipe] = since
	return true
}

func (p *previewDNSExitPool) release(pipe *previewDNSExitPipe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pipes, pipe)
}

func (p *previewDNSExitPipe) Read(buffer []byte) (int, error) {
	n, err := p.Conn.Read(buffer)
	// Claim the pipe as soon as the next frame's length bytes arrive.
	// If eviction raced that read, let the client's normal retry reopen it.
	if n > 0 && !p.pool.setIdleSince(p, time.Time{}) {
		return 0, net.ErrClosed
	}
	return n, err
}

type previewDNSResolver struct {
	address string
	tlsName string
}

func configuredExitPreviewDNS(config TunnelConfig) (previewDNSResolver, error) {
	address := strings.TrimSpace(config.ExitDNSAddress)
	tlsName := strings.TrimSuffix(strings.TrimSpace(config.ExitDNSTLSName), ".")
	if address == "" && strings.TrimSpace(config.ExitDNSTLSName) == "" {
		return previewDNSResolver{address: previewDNSUpstream, tlsName: previewDNSName}, nil
	}
	if !config.UDPPreviewRemoteDNS {
		return previewDNSResolver{}, errors.New("config.tunnel exit DNS settings require udp_preview_remote_dns")
	}
	destination, err := netip.ParseAddrPort(address)
	ip := destination.Addr().Unmap()
	if err != nil || destination.Port() == 0 || destination.Addr().Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return previewDNSResolver{}, errors.New("config.tunnel.exit_dns_address must be a numeric unicast IP and port")
	}
	if !validPreviewDNSTLSName(tlsName) {
		return previewDNSResolver{}, errors.New("config.tunnel.exit_dns_tls_name must be a certificate DNS hostname")
	}
	return previewDNSResolver{address: netip.AddrPortFrom(ip, destination.Port()).String(), tlsName: tlsName}, nil
}

func validPreviewDNSTLSName(name string) bool {
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func newPreviewDNS(ctx context.Context, mux *driveMux) *previewDNS {
	return newPreviewDNSPool(ctx, func(queryCtx context.Context) (net.Conn, error) {
		if err := queryCtx.Err(); err != nil {
			return nil, err
		}
		local, stream := net.Pipe()
		go func() {
			defer stream.Close()
			_ = mux.openClientStream(ctx, previewDNSMuxTarget, &previewDNSMuxConn{Conn: stream})
		}()
		return local, nil
	})
}

func newExitPreviewDNS(ctx context.Context, proxyURL string) *previewDNS {
	return (previewDNSResolver{address: previewDNSUpstream, tlsName: previewDNSName}).open(ctx, proxyURL)
}

func (upstream previewDNSResolver) open(ctx context.Context, proxyURL string) *previewDNS {
	config := upstream.tlsConfig()
	resolver := newPreviewDNSPool(ctx, func(ctx context.Context) (net.Conn, error) {
		return upstream.dial(ctx, proxyURL, config)
	})
	resolver.exits = newPreviewDNSExitPool(previewDNSExitLimit)
	return resolver
}

func (upstream previewDNSResolver) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: upstream.tlsName, MinVersion: tls.VersionTLS12}
}

func (upstream previewDNSResolver) dial(ctx context.Context, proxyURL string, config *tls.Config) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, previewDNSUpstreamTO)
	defer cancel()
	if proxyURL == "" {
		dialer := &tls.Dialer{Config: config}
		return dialer.DialContext(ctx, "tcp", upstream.address)
	}
	stream, err := DialViaProxy(ctx, proxyURL, upstream.address)
	if err != nil {
		return nil, err
	}
	secure := tls.Client(stream, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return secure, nil
}

func newPreviewDNSPool(ctx context.Context, dial func(context.Context) (net.Conn, error)) *previewDNS {
	resolver := &previewDNS{
		ctx:    ctx,
		idle:   make(chan net.Conn, 2),
		active: make(chan struct{}, 4),
		dial:   dial,
	}
	context.AfterFunc(ctx, resolver.closeIdle)
	return resolver
}

func isPreviewDNSReservedTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	return err == nil && strings.EqualFold(strings.TrimSuffix(host, "."), previewDNSMuxHost)
}

func (d *previewDNS) openExit(ctx context.Context, target string) (net.Conn, error) {
	if target != previewDNSMuxTarget {
		return nil, errPreviewDNSTarget
	}
	if d == nil || d.exits == nil {
		return nil, errPreviewDNSDisabled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	local, pipe := d.exits.open()
	if pipe == nil {
		return nil, errPreviewDNSCapacity
	}
	pipeCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer d.exits.release(pipe)
		defer pipe.Close()
		defer cancel()
		d.serveTCP(pipeCtx, pipe)
	}()
	return &previewDNSMuxConn{Conn: local, cancel: cancel}, nil
}

func unpackPreviewDNSQuery(wire []byte) (dnsmessage.Message, error) {
	var request dnsmessage.Message
	if len(wire) > previewDNSMaxQuery {
		return request, errors.New("DNS request exceeds preview size limit")
	}
	if err := request.Unpack(wire); err != nil {
		return request, err
	}
	if request.Response || request.OpCode != 0 || len(request.Questions) != 1 ||
		len(request.Answers) != 0 || len(request.Authorities) != 0 {
		return request, errors.New("expected one standard DNS query")
	}
	return request, nil
}

func (d *previewDNS) exchange(ctx context.Context, query []byte, request dnsmessage.Message) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, previewDNSTimeout)
	defer cancel()
	stop := context.AfterFunc(d.ctx, cancel)
	defer stop()
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case d.active <- struct{}{}:
		defer func() { <-d.active }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.ctx.Done():
		return nil, d.ctx.Err()
	}
	var conn net.Conn
	select {
	case conn = <-d.idle:
	default:
	}
	if conn != nil {
		// A reused stream may have lost its peer during an outage. Reserve
		// part of the query deadline for the existing fresh-stream retry.
		deadline, _ := ctx.Deadline()
		warmCtx, cancelWarm := context.WithTimeout(ctx, time.Until(deadline)/2)
		reply, err := exchangePreviewDNS(warmCtx, conn, query, request)
		cancelWarm()
		if err == nil {
			d.release(conn)
			return reply, nil
		}
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := exchangePreviewDNS(ctx, conn, query, request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	d.release(conn)
	return reply, nil
}

func (d *previewDNS) release(conn net.Conn) {
	d.idleMu.Lock()
	defer d.idleMu.Unlock()
	if d.ctx.Err() != nil {
		_ = conn.Close()
		return
	}
	select {
	case d.idle <- conn:
	default:
		_ = conn.Close()
	}
}

func (d *previewDNS) closeIdle() {
	d.idleMu.Lock()
	defer d.idleMu.Unlock()
	for {
		select {
		case conn := <-d.idle:
			_ = conn.Close()
		default:
			return
		}
	}
}

func exchangePreviewDNS(ctx context.Context, conn net.Conn, query []byte, request dnsmessage.Message) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("DNS exchange requires a deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// Use deadlines for cancellation so a canceled query cannot leave a
	// reusable TLS stream with a partially consumed DNS response.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	if err := writePreviewDNSFrame(conn, query); err != nil {
		return nil, err
	}
	wire, err := readPreviewDNSFrame(conn, 65535)
	if err != nil {
		return nil, err
	}
	var reply dnsmessage.Message
	if err := reply.Unpack(wire); err != nil {
		return nil, err
	}
	q := request.Questions[0]
	if !reply.Response || reply.ID != request.ID || reply.OpCode != request.OpCode ||
		len(reply.Questions) != 1 || reply.Questions[0].Type != q.Type ||
		reply.Questions[0].Class != q.Class || !strings.EqualFold(reply.Questions[0].Name.String(), q.Name.String()) {
		return nil, errors.New("DNS response does not match the query")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, nil
}

func readPreviewDNSFrame(reader io.Reader, max int) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size < 12 || size > max {
		return nil, errors.New("invalid DNS message length")
	}
	wire := make([]byte, size)
	_, err := io.ReadFull(reader, wire)
	return wire, err
}

func writePreviewDNSFrame(writer io.Writer, wire []byte) error {
	header := binary.BigEndian.AppendUint16(nil, uint16(len(wire)))
	return writeAll(writer, append(header, wire...))
}

func (d *previewDNS) respond(ctx context.Context, query []byte, udp bool) ([]byte, error) {
	request, err := unpackPreviewDNSQuery(query)
	if err != nil {
		return nil, err
	}
	wire, err := d.exchange(ctx, query, request)
	if err != nil {
		return minimalPreviewDNSReply(request, dnsmessage.RCodeServerFailure, false)
	}
	if udp && len(wire) > previewDNSReplyLimit(request) {
		return minimalPreviewDNSReply(request, dnsmessage.RCode(wire[3]&0x0f), true)
	}
	return wire, nil
}

func previewDNSReplyLimit(query dnsmessage.Message) int {
	for _, rr := range query.Additionals {
		if rr.Header.Type == dnsmessage.TypeOPT {
			return min(previewDNSUDPMax, max(512, int(rr.Header.Class)))
		}
	}
	return 512
}

func minimalPreviewDNSReply(query dnsmessage.Message, code dnsmessage.RCode, truncated bool) ([]byte, error) {
	reply := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: query.ID, Response: true, OpCode: query.OpCode, RecursionDesired: query.RecursionDesired,
			RecursionAvailable: true, CheckingDisabled: query.CheckingDisabled, RCode: code, Truncated: truncated,
		},
		Questions: query.Questions,
	}
	return reply.Pack()
}

func (d *previewDNS) serveTCP(ctx context.Context, conn net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for ctx.Err() == nil {
		_ = conn.SetDeadline(time.Now().Add(previewDNSTimeout))
		query, err := readPreviewDNSFrame(conn, previewDNSMaxQuery)
		if err != nil {
			return
		}
		reply, err := d.respond(ctx, query, false)
		if err != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(previewDNSTimeout))
		if err := writePreviewDNSFrame(conn, reply); err != nil {
			return
		}
		if pipe, ok := conn.(*previewDNSExitPipe); ok {
			pipe.pool.setIdleSince(pipe, time.Now())
		}
	}
}
