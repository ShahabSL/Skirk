package skirk

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// The request uses the existing exit-probe frame size. Older exits can
	// reject it without resolving a new hostname or dialing an origin.
	udpPreviewRequest    = "skirk-udp-lease-rq3:"
	udpPreviewReply      = "skirk-udp-lease-v3:"
	udpPreviewBusyReply  = "skirk-udp-limit-v3:"
	udpPreviewHandshake  = 12 * time.Second
	udpPreviewLifetime   = 2 * time.Minute
	udpPreviewPublicIdle = 45 * time.Second
	// Reserve most of the handshake budget for Drive delivery and proof.
	udpPreviewCapacityGrace   = 2 * time.Second
	udpPreviewLeases          = 4
	udpPreviewMaxExitCarriers = 8
)

var errUDPPreviewIdle = errors.New("UDP preview idle")
var errUDPExitBusy = errors.New("exit UDP capacity reached")

// datagramPreview is available only in an explicit, disposable Drive profile.
// Both peers agree to the same destination policy. A regular TCP profile
// does not start this carrier.
type datagramPreview struct {
	tunnel                 *Tunnel
	drive                  *DriveStore
	http                   *GoogleHTTPClient
	allowed                map[netip.AddrPort]bool
	publicIPv4             bool
	publicIPv6             bool
	remoteDNSEnabled       bool
	namedUDP               bool
	batchBytes             int
	media                  bool
	mediaBytes             int
	stableUDP              bool
	sharedUDP              bool
	sharedLimit            int
	groupedDiscovery       bool
	exitDiscovery          pilotExitDiscovery
	adaptiveUploads        bool
	policy                 [sha256.Size]byte
	slots                  chan struct{}
	exitSlots              chan struct{}
	exitWaiters            chan struct{}
	exitCarrierLimitHits   atomic.Uint64
	remoteExitLimitHits    atomic.Uint64
	frontendLimitHits      atomic.Uint64
	associationLimitHits   atomic.Uint64
	clientCarrierLimitHits atomic.Uint64
	metricsMu              sync.RWMutex
	metricsMailboxes       map[*pilotMailbox]struct{}
	retiredMetrics         UDPPreviewPacketCounters
	frontendMu             sync.Mutex
	frontendCount          int
	frontendIdle           time.Duration // Zero uses the production idle interval.
}

func previewUDPBatchBytes(cfg TunnelConfig) (int, error) {
	if cfg.UDPPreviewGroupedDiscovery && !cfg.UDPPreviewSharedUDP {
		return 0, errors.New("config.tunnel.udp_preview_grouped_discovery requires shared UDP")
	}
	if limit := cfg.UDPPreviewExitCarriers; limit != 0 {
		if limit < 1 || limit > udpPreviewMaxExitCarriers {
			return 0, errors.New("config.tunnel.udp_preview_exit_carriers must be 0 or between 1 and 8")
		}
		if !cfg.UDPPreviewSharedUDP {
			return 0, errors.New("config.tunnel.udp_preview_exit_carriers requires shared UDP")
		}
	}
	if cfg.UDPPreviewSharedAssociations != 0 {
		if !cfg.UDPPreviewSharedUDP {
			return 0, errors.New("UDP preview association cap requires shared UDP")
		}
		switch cfg.UDPPreviewSharedAssociations {
		case pilotSharedMaxAssociations, 32, 64:
		default:
			return 0, errors.New("UDP preview association cap must be 16, 32, or 64")
		}
	}
	if cfg.UDPPreviewSharedUDP && (!cfg.UDPPreviewStableUDP || !cfg.UDPPreviewPublicIPv4) {
		return 0, errors.New("UDP preview shared leases require stable UDP mapping and public IPv4")
	}
	if cfg.UDPPreviewNamedUDP && (!cfg.UDPPreviewRemoteDNS || !cfg.UDPPreviewPublicIPv4) {
		return 0, errors.New("UDP preview named UDP requires remote DNS and public IPv4")
	}
	if cfg.UDPPreviewRemoteDNS && !cfg.UDPPreviewPublicIPv4 {
		return 0, errors.New("UDP preview remote DNS requires public IPv4")
	}
	if cfg.UDPPreviewIPv6 && (!cfg.UDPPreviewStableUDP || !cfg.UDPPreviewPublicIPv4) {
		return 0, errors.New("UDP preview IPv6 requires stable UDP mapping and public IPv4")
	}
	if cfg.UDPPreviewIPv6 && !cfg.UDPPreviewRemoteDNS {
		return 0, errors.New("UDP preview IPv6 requires remote DNS")
	}
	if cfg.UDPPreviewStableUDP && !cfg.UDPPreviewPublicIPv4 {
		return 0, errors.New("UDP preview stable mapping requires public IPv4")
	}
	if cfg.UDPPreviewMedia && (!cfg.UDPPreviewPublicIPv4 || cfg.UDPPreviewBatchBytes != pilotWidePlain) {
		return 0, errors.New("UDP preview media requires public IPv4 and 16384 byte metadata batches")
	}
	if cfg.UDPPreviewAdaptiveUploads && !cfg.UDPPreviewMedia {
		return 0, errors.New("UDP preview adaptive uploads require media batches")
	}
	switch cfg.UDPPreviewBatchBytes {
	case 0, pilotMaxPlain:
		return pilotMaxPlain, nil
	case pilotWidePlain:
		if cfg.UDPPreviewPublicIPv4 {
			return pilotWidePlain, nil
		}
		return 0, errors.New("UDP preview batches of 16384 bytes require public IPv4")
	default:
		return 0, errors.New("UDP preview batch bytes must be 0, 8192, or 16384")
	}
}

func previewUDPMediaBytes(cfg TunnelConfig) (int, error) {
	switch cfg.UDPPreviewMediaBytes {
	case 0, pilotMediaMaxPlain:
		return pilotMediaMaxPlain, nil
	case pilotMediaWidePlain:
		if !cfg.UDPPreviewMedia || !cfg.UDPPreviewAdaptiveUploads {
			return 0, errors.New("UDP preview media batches of 131072 bytes require media and adaptive uploads")
		}
		return pilotMediaWidePlain, nil
	default:
		return 0, errors.New("UDP preview media bytes must be 0, 65536, or 131072")
	}
}

func previewUDPAllowlist(cfg *Config) (map[netip.AddrPort]bool, error) {
	if transport := strings.TrimSpace(cfg.Tunnel.Transport); transport != "" && transport != "muxv5" {
		return nil, errors.New("UDP preview requires muxv5")
	}
	if proxy := strings.TrimSpace(cfg.Tunnel.ExitProxy); proxy != "" {
		if !cfg.Tunnel.UDPPreviewSharedUDP || !cfg.Tunnel.UDPPreviewStableUDP {
			return nil, errors.New("UDP preview exit proxy requires shared UDP and stable mapping")
		}
		if _, err := parseSOCKSUDPProxyURL(proxy); err != nil {
			return nil, err
		}
	}
	if folder := strings.TrimSpace(cfg.Drive.FolderID); folder == "" || folder == "root" || folder == "appDataFolder" || cfg.Drive.Space == "appDataFolder" {
		return nil, errors.New("UDP preview requires an explicit disposable Drive folder")
	}
	if len(cfg.Tunnel.UDPPreviewTargets) > 3 {
		return nil, errors.New("UDP preview accepts at most three exact loopback endpoints")
	}
	allowed := make(map[netip.AddrPort]bool, len(cfg.Tunnel.UDPPreviewTargets))
	for _, target := range cfg.Tunnel.UDPPreviewTargets {
		endpoint, err := netip.ParseAddrPort(target)
		if err != nil || !previewUDPOwnedLoopback(endpoint.Addr(), cfg.Tunnel.UDPPreviewIPv6) ||
			!previewUDPAllowedPort(endpoint.Port()) || allowed[endpoint] {
			return nil, errors.New("UDP preview endpoints must be unique 127.0.0.1 or enabled [::1] ports other than 0, 53, and 853")
		}
		allowed[endpoint] = true
	}
	return allowed, nil
}

func newDatagramPreview(tunnel *Tunnel, cfg *Config) (*datagramPreview, error) {
	batchBytes, err := previewUDPBatchBytes(cfg.Tunnel)
	if err != nil {
		return nil, err
	}
	mediaBytes, err := previewUDPMediaBytes(cfg.Tunnel)
	if err != nil {
		return nil, err
	}
	allowed, err := previewUDPAllowlist(cfg)
	if err != nil {
		return nil, err
	}
	drive, ok := tunnel.Data.(*DriveStore)
	if !ok || drive == nil || drive.http == nil || drive.folderID != strings.TrimSpace(cfg.Drive.FolderID) || drive.isAppData() {
		return nil, errors.New("UDP preview requires a scoped Drive store")
	}
	endpoints := make([]string, 0, len(allowed))
	for endpoint := range allowed {
		endpoints = append(endpoints, endpoint.String())
	}
	slices.Sort(endpoints)
	if cfg.Tunnel.UDPPreviewPublicIPv4 {
		endpoints = append([]string{"public-ipv4-v1"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewIPv6 {
		endpoints = append([]string{"public-ipv6-v1"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewRemoteDNS {
		endpoints = append([]string{"remote-dns-exit-v2"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewNamedUDP {
		endpoints = append([]string{"named-udp-v1"}, endpoints...)
	}
	if batchBytes == pilotWidePlain {
		endpoints = append([]string{"batch-bytes=16384"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewMedia {
		endpoints = append([]string{"media-batches-v1"}, endpoints...)
	}
	if mediaBytes == pilotMediaWidePlain {
		endpoints = append([]string{"media-bytes=131072"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewStableUDP {
		endpoints = append([]string{"stable-udp-mapping-v1"}, endpoints...)
	}
	if cfg.Tunnel.UDPPreviewSharedUDP {
		endpoints = append([]string{"shared-udp-associations-v1"}, endpoints...)
		if limit := cfg.Tunnel.UDPPreviewSharedAssociations; limit > pilotSharedMaxAssociations {
			endpoints = append([]string{"shared-udp-association-cap-v1=" + strconv.Itoa(limit)}, endpoints...)
		}
	}
	var exitSlots chan struct{}
	if limit := cfg.Tunnel.UDPPreviewExitCarriers; limit != 0 {
		exitSlots = make(chan struct{}, limit)
	}
	var exitWaiters chan struct{}
	if cfg.Tunnel.UDPPreviewSharedUDP {
		limit := udpPreviewLeases
		if exitSlots != nil {
			limit = cap(exitSlots)
		}
		exitWaiters = make(chan struct{}, limit)
	}
	return &datagramPreview{
		tunnel: tunnel, drive: drive, http: NewGoogleHTTPClient(drive.http.route), allowed: allowed,
		publicIPv4:       cfg.Tunnel.UDPPreviewPublicIPv4,
		publicIPv6:       cfg.Tunnel.UDPPreviewIPv6,
		remoteDNSEnabled: cfg.Tunnel.UDPPreviewRemoteDNS,
		namedUDP:         cfg.Tunnel.UDPPreviewNamedUDP,
		batchBytes:       batchBytes,
		media:            cfg.Tunnel.UDPPreviewMedia,
		mediaBytes:       mediaBytes,
		stableUDP:        cfg.Tunnel.UDPPreviewStableUDP,
		sharedUDP:        cfg.Tunnel.UDPPreviewSharedUDP,
		sharedLimit:      cfg.Tunnel.UDPPreviewSharedAssociations,
		groupedDiscovery: cfg.Tunnel.UDPPreviewGroupedDiscovery,
		adaptiveUploads:  cfg.Tunnel.UDPPreviewAdaptiveUploads,
		policy:           sha256.Sum256([]byte(strings.Join(endpoints, "\n"))),
		slots:            make(chan struct{}, udpPreviewLeases),
		exitSlots:        exitSlots,
		exitWaiters:      exitWaiters,
	}, nil
}

func (p *datagramPreview) exitCarrierSlots() chan struct{} {
	if p.exitSlots != nil {
		return p.exitSlots
	}
	return p.slots
}

func pilotSharedAssociationLimit(limit int) int {
	if limit == 0 {
		return pilotSharedMaxAssociations
	}
	return limit
}

func (p *datagramPreview) proof(clientID, runID string, clientNonce, exitNonce [exitProbeNonceBytes]byte) ([sha256.Size]byte, error) {
	return p.replyProof(clientID, runID, clientNonce, exitNonce, udpPreviewReply)
}

func (p *datagramPreview) replyProof(clientID, runID string, clientNonce, exitNonce [exitProbeNonceBytes]byte, reply string) ([sha256.Size]byte, error) {
	root, err := DeriveKey(p.tunnel.Secret)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	domain := "skirk-udp-preview-lease-interval-reset-v3"
	switch reply {
	case udpPreviewReply:
	case udpPreviewBusyReply:
		domain = "skirk-udp-preview-busy-v1"
	default:
		return [sha256.Size]byte{}, ErrExitNotVerified
	}
	key := hkdfSHA256(root, p.tunnel.SessionID[:], []byte(domain), sha256.Size)
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, clientID+"\x00"+runID+"\x00"+p.drive.folderID+"\x00")
	_, _ = mac.Write(p.policy[:])
	_, _ = mac.Write(clientNonce[:])
	_, _ = mac.Write(exitNonce[:])
	return [sha256.Size]byte(mac.Sum(nil)), nil
}

func (p *datagramPreview) readClientLeaseProof(control net.Conn, timing *pilotTimingState, clientNonce [exitProbeNonceBytes]byte, started time.Time) ([16]byte, error) {
	if err := control.SetDeadline(started.Add(udpPreviewHandshake)); err != nil {
		return [16]byte{}, err
	}
	reply := make([]byte, len(udpPreviewReply)+exitProbeNonceBytes+sha256.Size)
	if _, err := io.ReadFull(control, reply); err != nil {
		return [16]byte{}, err
	}
	received := time.Now()
	header := string(reply[:len(udpPreviewReply)])
	exitNonce := [exitProbeNonceBytes]byte(reply[len(udpPreviewReply):])
	proof, err := p.replyProof(p.tunnel.ClientID, p.tunnel.RunID, clientNonce, exitNonce, header)
	if err != nil {
		return [16]byte{}, err
	}
	if !hmac.Equal(proof[:], reply[len(udpPreviewReply)+exitProbeNonceBytes:]) {
		return [16]byte{}, ErrExitNotVerified
	}
	if header == udpPreviewBusyReply {
		p.remoteExitLimitHits.Add(1)
		return [16]byte{}, errUDPExitBusy
	}
	if err := timing.install(1, received, started); err != nil {
		return [16]byte{}, err
	}
	if err := timing.activate(1); err != nil {
		return [16]byte{}, err
	}
	if p.tunnel.Logger != nil {
		p.tunnel.Logger.Printf("udp timing role=client epoch=1 control_ms=%d rate_bound_ppm=%d", pilotElapsedUpper(started, received).Milliseconds(), pilotTimingRatePPM)
	}
	return [16]byte(proof[:16]), nil
}

func (p *datagramPreview) serveClient(ctx context.Context, mux *driveMux, req socksRequest, control net.Conn, dns *previewDNS) bool {
	source, err := netip.ParseAddrPort(control.RemoteAddr().String())
	if err != nil || req.Command == socksCommandUDPInTCP && !source.Addr().Unmap().IsLoopback() {
		_ = socksReply(control, 0x02)
		return true
	}
	if req.Command == socksCommandUDPAssociate && !pilotUDPRelayBind(source.Addr(), control.LocalAddr()).IsValid() {
		_ = socksReply(control, 0x02)
		return true
	}
	releaseFrontend, admitted := p.admitFrontend()
	if !admitted {
		_ = socksReply(control, 0x01)
		return true
	}
	defer releaseFrontend()
	relay := &pilotClientRelay{allowed: p.allowed, publicIPv4: p.publicIPv4, publicIPv6: p.publicIPv6,
		batchBytes: p.batchBytes, mediaBytes: p.mediaBytes, dns: true, namedUDP: p.namedUDP, shared: mux.sharedUDP,
		localIdle: p.frontendIdle,
		open: func() (*pilotMailbox, *pilotFlowTable, func(), error) {
			return p.openClient(ctx, mux, control)
		}}
	if dns != nil {
		relay.remoteDNS = dns.respond
	}
	if req.Command == socksCommandUDPAssociate {
		pilotAssoc(ctx, relay, req.Target, control)
	} else {
		pilotHEVAssoc(ctx, relay, req.Target, control)
	}
	return true
}

func (p *datagramPreview) leaseContext(parent context.Context) (context.Context, context.CancelFunc, func()) {
	if !p.publicIPv4 {
		ctx, cancel := context.WithTimeout(parent, udpPreviewLifetime)
		return ctx, cancel, nil
	}
	idle := udpPreviewPublicIdle
	if p.stableUDP {
		idle = udpPreviewStableIdle
	}
	ctx, cancel := context.WithCancelCause(parent)
	var mu sync.Mutex
	lastActivity := time.Now()
	timer := time.NewTimer(idle)
	go func() {
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				mu.Lock()
				remaining := idle - time.Since(lastActivity)
				if remaining <= 0 {
					cancel(errUDPPreviewIdle)
					mu.Unlock()
					return
				}
				timer.Reset(remaining)
				mu.Unlock()
			}
		}
	}()
	return ctx, func() { cancel(nil) }, func() {
		mu.Lock()
		lastActivity = time.Now()
		mu.Unlock()
	}
}

func udpPreviewCloseReason(parent, lease context.Context, fallback string) string {
	switch {
	case errors.Is(context.Cause(lease), errUDPPreviewIdle):
		return "idle"
	case parent.Err() != nil:
		return "shutdown"
	case lease.Err() != nil:
		return "mux_or_peer_closed"
	default:
		return fallback
	}
}

func (p *datagramPreview) openClient(parent context.Context, mux *driveMux, association net.Conn) (*pilotMailbox, *pilotFlowTable, func(), error) {
	lease, err := p.openClientLease(parent, mux)
	if err != nil {
		return nil, nil, nil, err
	}
	stopAssoc := context.AfterFunc(lease.ctx, func() { _ = association.Close() })
	return lease.mailbox, lease.flows, func() {
		stopAssoc()
		lease.finish("local_association_closed")
	}, nil
}

type pilotClientLease struct {
	ctx     context.Context
	control net.Conn
	mailbox *pilotMailbox
	flows   *pilotFlowTable
	finish  func(string)
}

// A shared control lease ends when either peer sends FIN. net.Pipe cannot
// half-close, so tell the mux to close the whole synthetic connection.
type pilotSharedControlConn struct{ net.Conn }

func (c pilotSharedControlConn) CloseWrite() error { return c.Conn.Close() }

func (p *datagramPreview) openClientLease(parent context.Context, mux *driveMux) (*pilotClientLease, error) {
	select {
	case p.slots <- struct{}{}:
	default:
		p.clientCarrierLimitHits.Add(1)
		return nil, errors.New("UDP preview lease limit reached")
	}
	started := time.Now()
	var clientNonce [exitProbeNonceBytes]byte
	if _, err := rand.Read(clientNonce[:]); err != nil {
		<-p.slots
		return nil, err
	}
	request := append([]byte(udpPreviewRequest), clientNonce[:]...)
	ctx, cancel, activity := p.leaseContext(parent)
	local, stream := net.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = local.Close(); _ = stream.Close() })
	done := make(chan struct{})
	timing := newPilotTimingState()
	// The causal origin must be recorded before the OPEN can leave this peer.
	proofStarted := timing.begin()
	go func() {
		defer close(done)
		defer stream.Close()
		defer cancel()
		var control net.Conn = stream
		if p.sharedUDP {
			control = pilotSharedControlConn{stream}
		}
		_ = mux.openClientStreamWithInitial(ctx, exitProbeTarget, control, request)
	}()
	cleanup := func() {
		cancel()
		_ = local.Close()
		_ = stream.Close()
		stop()
		<-done
		<-p.slots
	}
	lease, err := p.readClientLeaseProof(local, timing, clientNonce, proofStarted)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := local.SetDeadline(time.Time{}); err != nil {
		cleanup()
		return nil, err
	}
	mailbox, flows, finish, err := p.startMailbox(ctx, lease, true, activity, nil, timing)
	if err != nil {
		cleanup()
		return nil, err
	}
	timedControl, err := newPilotTimingControl(ctx, local, p.tunnel, lease, timing, true, nil, nil, cancel)
	if err != nil {
		finish()
		cleanup()
		return nil, err
	}
	return &pilotClientLease{ctx: ctx, control: timedControl, mailbox: mailbox, flows: flows, finish: func(fallback string) {
		reason := udpPreviewCloseReason(parent, ctx, fallback)
		if reason != "idle" {
			switch context.Cause(parent) {
			case errSharedUDPNoAssociations:
				reason = "local_associations_closed"
			case errSharedUDPCloseQueueFull:
				reason = "shared_close_queue_full"
			case errSharedUDPRetiring:
				reason = fallback
			}
		}
		finish()
		cleanup()
		p.logLeaseClose("client", reason, started, mailbox)
	}}, nil
}

// Older clients reject the unfamiliar reply. A current client can verify
// where a new carrier was refused using the original handshake round trip.
func (p *datagramPreview) replyExitBusy(control net.Conn, key muxStreamKey, clientNonce [exitProbeNonceBytes]byte) {
	var nonce [exitProbeNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return
	}
	proof, err := p.replyProof(key.ClientID, key.RunID, clientNonce, nonce, udpPreviewBusyReply)
	if err != nil || control.SetWriteDeadline(time.Now().Add(udpPreviewCapacityGrace)) != nil {
		return
	}
	reply := append([]byte(udpPreviewBusyReply), nonce[:]...)
	_ = writeAll(control, append(reply, proof[:]...))
}

func (p *datagramPreview) serveExit(parent context.Context, key muxStreamKey, control net.Conn, clientNonce [exitProbeNonceBytes]byte) {
	slots := p.exitCarrierSlots()
	if !p.acquireExitCarrier(parent, slots) {
		if parent.Err() == nil {
			p.exitCarrierLimitHits.Add(1)
			p.replyExitBusy(control, key, clientNonce)
		}
		return
	}
	defer func() { <-slots }()
	if parent.Err() != nil {
		return
	}
	started := time.Now()
	timing := newPilotTimingState()
	sendOrigin := timing.begin()
	ctx, cancel, activity := p.leaseContext(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer stop()
	var exitNonce [exitProbeNonceBytes]byte
	if _, err := rand.Read(exitNonce[:]); err != nil {
		return
	}
	proof, err := p.proof(key.ClientID, key.RunID, clientNonce, exitNonce)
	if err != nil {
		return
	}
	lease := [16]byte(proof[:16])
	originFailure := make(chan error, 1)
	mailbox, _, finish, err := p.startMailbox(ctx, lease, false, activity, func(err error) {
		originFailure <- err
		cancel()
	}, timing)
	if err != nil {
		return
	}
	reason := "mux_or_peer_closed"
	defer func() {
		reason = udpPreviewCloseReason(parent, ctx, reason)
		finish()
		select {
		case err := <-originFailure:
			switch {
			case errors.Is(err, errSOCKSUDPUnsupported):
				reason = "origin_udp_proxy_unsupported"
			case errors.Is(err, errSOCKS5Authentication):
				reason = "origin_udp_proxy_auth_failed"
			case errors.Is(err, errSharedUDPSetupExhausted):
				reason = "origin_udp_setup_failed"
			case errors.Is(err, errSharedUDPRetirementLimit):
				reason = "association_retirement_limit"
			case errors.Is(err, errSharedUDPResetQueueFull):
				reason = "association_reset_queue_full"
			default:
				reason = "origin_read_error"
			}
		default:
		}
		p.logLeaseClose("exit", reason, started, mailbox)
	}()
	reply := append([]byte(udpPreviewReply), exitNonce[:]...)
	receiveOrigin := timing.nowTime()
	if err := timing.install(1, sendOrigin, receiveOrigin); err != nil {
		return
	}
	if err := timing.activate(1); err != nil {
		return
	}
	if p.tunnel.Logger != nil {
		p.tunnel.Logger.Printf("udp timing role=exit epoch=1 control_ms=%d rate_bound_ppm=%d", pilotElapsedUpper(sendOrigin, receiveOrigin).Milliseconds(), pilotTimingRatePPM)
	}
	if err := writeAll(control, append(reply, proof[:]...)); err != nil {
		return
	}
	_ = control.SetDeadline(time.Time{})
	timedControl, err := newPilotTimingControl(ctx, control, p.tunnel, lease, timing, false, mailbox.closeAssociation, mailbox.resetAssociations, cancel)
	if err != nil {
		return
	}
	if err := timedControl.wait(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		reason = "timing_control_error"
	}
}

func (p *datagramPreview) acquireExitCarrier(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	// A bounded handoff window lets an ending call yield capacity without
	// allocating mailboxes or disturbing active carriers. Sustained overload
	// still has a short deadline and a bound on pending control streams.
	select {
	case p.exitWaiters <- struct{}{}:
		defer func() { <-p.exitWaiters }()
	default:
		return false
	}
	timer := time.NewTimer(udpPreviewCapacityGrace)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

func (p *datagramPreview) startMailbox(ctx context.Context, lease [16]byte, client bool, activity func(), onOriginFailure func(error), timing *pilotTimingState) (*pilotMailbox, *pilotFlowTable, func(), error) {
	mailbox, err := newPilotMailbox(p.tunnel, p.drive, lease, client, p.allowed)
	if err != nil {
		return nil, nil, nil, err
	}
	mailbox.http = p.http
	mailbox.separateUploads = true
	mailbox.publicIPv4 = p.publicIPv4
	mailbox.publicIPv6 = p.publicIPv6
	mailbox.media = p.media
	if !client && p.sharedUDP {
		mailbox.exitProxy = p.tunnel.ExitProxy
	}
	if p.media {
		mailbox.mediaBytes = p.mediaBytes
		mailbox.mediaReader.wake = make(chan struct{}, 1)
	}
	if p.batchBytes == pilotWidePlain {
		mailbox.batchBytes = pilotWidePlain
	}
	mailbox.activity = activity
	mailbox.timing = timing
	if p.sharedUDP {
		if p.media && p.adaptiveUploads {
			mailbox.fairQueue = newPilotMediaFairQueue(mailbox.mediaPlainLimit())
			if mailbox.mediaPlainLimit() == pilotMediaWidePlain {
				mailbox.publishMode = pilotPublishFullBinary
			}
		} else {
			mailbox.fairQueue = newPilotFairQueue(pilotQueuePackets, pilotFairQueueBytes)
		}
		mailbox.queue = nil
	}
	flows := &pilotFlowTable{client: map[uint64]pilotClientFlow{}, exit: map[uint64]pilotExitFlow{}}
	if p.sharedUDP {
		flows.sharedMaxFlows = pilotSharedAssociationLimit(p.sharedLimit) * pilotMaxFlows
	}
	ctx, cancel := context.WithCancel(ctx)
	var mapping *pilotUDPMapping
	var shared *pilotSharedUDPExit
	if client {
		mailbox.deliver = flows.returnToClient
	} else if p.sharedUDP {
		limit := pilotSharedAssociationLimit(p.sharedLimit)
		resets := make(chan pilotAssociationReset, limit)
		mailbox.resetAssociations = resets
		shared = newPilotSharedUDPExitWithResets(ctx, mailbox, limit, resets, func(err error) {
			cancel()
			if onOriginFailure != nil {
				onOriginFailure(err)
			}
		})
		mailbox.deliver = shared.send
		mailbox.closeAssociation = func(id uint32) error {
			err := shared.closeAssociation(id)
			mailbox.closeQueuedAssociation(id)
			return err
		}
	} else if p.stableUDP {
		mapping, err = newPilotUDPMapping(ctx, mailbox, func(err error) {
			cancel()
			if onOriginFailure != nil {
				onOriginFailure(err)
			}
		})
		if err != nil {
			cancel()
			return nil, nil, nil, err
		}
		mailbox.deliver = mapping.send
	} else {
		mailbox.deliver = func(ctx context.Context, packet pilotPacket) { flows.sendToOrigin(ctx, mailbox, packet) }
	}
	if p.adaptiveUploads {
		mailbox.adaptiveUploads = newPilotAdaptiveUploads(func(previous *GoogleHTTPClient) *GoogleHTTPClient {
			return newPilotMediaUploadHTTP(p.http, previous)
		})
		mailbox.adaptiveUploads.tryEarlyRecovery = p.sharedUDP && mailbox.mediaPlainLimit() == pilotMediaWidePlain
	}
	var workers sync.WaitGroup
	workers.Go(func() { mailbox.publishLoop(ctx) })
	if !client && p.groupedDiscovery {
		receiver, unregister := p.exitDiscovery.register(ctx, mailbox)
		workers.Go(func() {
			defer unregister()
			mailbox.pollShared(ctx, receiver.pages)
		})
	} else {
		workers.Go(func() { mailbox.poll(ctx) })
	}
	workers.Go(func() { mailbox.deleteLoop(ctx) })
	if mailbox.media {
		workers.Go(func() { mailbox.mediaReadWakeLoop(ctx) })
	}
	p.registerMetrics(mailbox)
	return mailbox, flows, func() {
		cancel()
		mailbox.stopFrontends()
		workers.Wait()
		mailbox.liveCalls.Wait()
		if a := mailbox.adaptiveUploads; a != nil {
			a.closeHTTP(a.mediaHTTP.Load())
		}
		flows.wg.Wait()
		if mapping != nil {
			<-mapping.done
		}
		if shared != nil {
			<-shared.done
		}
		mailbox.frontendWG.Wait()
		p.retireMetrics(mailbox)
	}, nil
}

func (p *datagramPreview) logLeaseClose(role, reason string, started time.Time, mailbox *pilotMailbox) {
	if !p.publicIPv4 || p.tunnel.Logger == nil {
		return
	}
	p.tunnel.Logger.Printf("udp preview %s closed accepted=%d origin_sent=%d origin_received=%d published=%d expired=%d dropped=%d rejected=%d provider_errors=%d partial_lists=%d batch_bytes=%d published_batches=%d received_batches=%d published_plain_bytes=%d accepted_payload_bytes=%d ingress_dropped=%d hev_reply_dropped=%d flow_cap=%d lease_cap_total=%d origin_dial_errors=%d origin_write_errors=%d hev_write_errors=%d reason=%s lease_age_ms=%d media_uploaded=%d media_fetched=%d local_queue_expired=%d incoming_certainly_stale=%d invalid_timing=%d",
		role,
		mailbox.delivered.Load(), mailbox.originSent.Load(), mailbox.originReceived.Load(), mailbox.published.Load(),
		mailbox.stale.Load(), mailbox.dropped.Load(), mailbox.rejected.Load(), mailbox.errors.Load(), mailbox.partial.Load(),
		mailbox.batchBytes, mailbox.publishedBatches.Load(), mailbox.receivedBatches.Load(), mailbox.publishedPlainBytes.Load(), mailbox.acceptedPayloadBytes.Load(),
		mailbox.ingressDropped.Load(), mailbox.hevReplyDropped.Load(), mailbox.flowCap.Load(),
		p.exitCarrierLimitHits.Load()+p.frontendLimitHits.Load()+p.associationLimitHits.Load()+p.clientCarrierLimitHits.Load(),
		mailbox.originDialErrors.Load(), mailbox.originWriteErrors.Load(), mailbox.hevWriteErrors.Load(), reason, time.Since(started).Milliseconds(),
		mailbox.mediaUploaded.Load(), mailbox.mediaFetched.Load(), mailbox.localQueueExpired.Load(), mailbox.incomingCertainlyStale.Load(), mailbox.invalidTiming.Load())
	if mailbox.media {
		stats := mailbox.mediaReader.stats
		p.tunnel.Logger.Printf("udp preview %s media reads dispatched=%d retries=%d quota_skipped=%d fetch_timeouts=%d retention_timeouts=%d exhausted=%d evicted=%d capacity_skipped=%d retention_expired=%d first_wait_ms_sum=%.3f first_wait_ms_max=%.3f fetch_ms_sum=%.3f fetch_ms_max=%.3f",
			role, stats.dispatched, stats.retries, stats.quotaSkipped, stats.fetchTimeouts, stats.retentionTimeouts,
			stats.exhausted, stats.evicted, stats.capacitySkipped, stats.retentionExpired,
			float64(stats.firstWaitUS)/1000, float64(stats.maxFirstWaitUS)/1000, float64(stats.fetchUS)/1000, float64(stats.maxFetchUS)/1000)
	}
	if a := mailbox.adaptiveUploads; a != nil {
		a.logClose(p.tunnel.Logger, role)
	}
}
