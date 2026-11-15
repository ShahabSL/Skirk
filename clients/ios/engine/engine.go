// Package engine exposes a small gomobile-compatible bridge for the iOS app and
// packet tunnel extension. The Go transport runs inside the extension process.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const startupTimeout = 30 * time.Second

type session struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	mu           sync.Mutex
	err          error
	secret       string
	socksAddress string
}

var bridge struct {
	sync.Mutex
	active  *session
	pending *session
	lastErr string
}

// Start starts a loopback SOCKS5 listener. profile is the complete skirk:
// profile or JSON config, and clientID should be stable for this device.
func Start(profile, clientID string, socksPort int) error {
	return start(profile, clientID, socksPort, 0, func(ctx context.Context, cfg *skirk.Config) (skirk.BlobStore, error) {
		return skirk.StoresFromConfig(ctx, cfg)
	})
}

// StartWithRoute starts the client with the route explicitly selected on this
// device. The profile and its OAuth credentials remain unchanged.
func StartWithRoute(profile, clientID string, socksPort int, routeMode string) error {
	return startWithRoute(profile, clientID, socksPort, 0, routeMode, func(ctx context.Context, cfg *skirk.Config) (skirk.BlobStore, error) {
		return skirk.StoresFromConfig(ctx, cfg)
	})
}

// StartWithHTTP also starts a loopback HTTP proxy. Pass httpPort=0 to disable it.
func StartWithHTTP(profile, clientID string, socksPort, httpPort int) error {
	return start(profile, clientID, socksPort, httpPort, func(ctx context.Context, cfg *skirk.Config) (skirk.BlobStore, error) {
		return skirk.StoresFromConfig(ctx, cfg)
	})
}

// PinnedGoogleIP returns the route's Google IP for a pinned mode without an
// upstream proxy. The packet tunnel can exclude this address from its VPN route.
func PinnedGoogleIP(profile string) (string, error) {
	return pinnedGoogleIP(profile, "")
}

// PinnedGoogleIPWithRoute returns the address that must be excluded from the
// device VPN route for the selected transport route.
func PinnedGoogleIPWithRoute(profile, routeMode string) (string, error) {
	if err := validateSelectedRoute(routeMode); err != nil {
		return "", err
	}
	return pinnedGoogleIP(profile, routeMode)
}

func pinnedGoogleIP(profile, routeMode string) (string, error) {
	cfg, err := skirk.ParseConfig([]byte(profile))
	if err != nil {
		return "", fmt.Errorf("invalid Skirk profile: %w", err)
	}
	if routeMode != "" && routeMode != "profile" {
		cfg.Route.Mode = routeMode
	}
	if strings.TrimSpace(cfg.Route.Proxy) != "" {
		return "", nil
	}
	switch cfg.Route.Mode {
	case "real_pinned", "google_front_pinned", "google_front_h1_pinned":
		return strings.TrimSpace(cfg.Route.GoogleIP), nil
	default:
		return "", nil
	}
}

type storeFactory func(context.Context, *skirk.Config) (skirk.BlobStore, error)

func start(profile, clientID string, socksPort, httpPort int, makeStore storeFactory) error {
	return startWithRoute(profile, clientID, socksPort, httpPort, "", makeStore)
}

func validateSelectedRoute(routeMode string) error {
	switch routeMode {
	case "profile", "direct", "google_front_pinned":
		return nil
	default:
		return fmt.Errorf("unsupported iOS Google route %q", routeMode)
	}
}

func startWithRoute(profile, clientID string, socksPort, httpPort int, routeMode string, makeStore storeFactory) error {
	if routeMode != "" {
		if err := validateSelectedRoute(routeMode); err != nil {
			return err
		}
	}
	if err := validatePorts(socksPort, httpPort); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	bridge.Lock()
	if bridge.active != nil || bridge.pending != nil {
		bridge.Unlock()
		cancel()
		return errors.New("Skirk is already starting or running")
	}
	bridge.pending = s
	bridge.lastErr = ""
	bridge.Unlock()

	err := prepareAndRun(s, profile, clientID, socksPort, httpPort, routeMode, makeStore)
	bridge.Lock()
	if bridge.pending == s {
		bridge.pending = nil
	}
	if err == nil && ctx.Err() == nil {
		select {
		case <-s.done:
			err = s.failure()
			if err == nil {
				err = errors.New("Skirk listener stopped during startup")
			}
		default:
			bridge.active = s
		}
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		bridge.lastErr = err.Error()
	}
	bridge.Unlock()
	if err != nil {
		cancel()
	}
	return err
}

func validatePorts(socksPort, httpPort int) error {
	if socksPort < 1 || socksPort > 65535 {
		return errors.New("SOCKS port must be between 1 and 65535")
	}
	if httpPort < 0 || httpPort > 65535 || httpPort == socksPort {
		return errors.New("HTTP port must be 0 or a distinct port between 1 and 65535")
	}
	return nil
}

func prepareAndRun(s *session, profile, clientID string, socksPort, httpPort int, routeMode string, makeStore storeFactory) error {
	if strings.TrimSpace(profile) == "" {
		return errors.New("Skirk profile is empty")
	}
	cfg, err := skirk.ParseConfig([]byte(profile))
	if err != nil {
		return fmt.Errorf("invalid Skirk profile: %w", err)
	}
	if routeMode != "" && routeMode != "profile" {
		cfg.Route.Mode = routeMode
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID, err = skirk.RandomClientID()
		if err != nil {
			return err
		}
	}
	cfg.Client.ID = clientID
	cfg.Client.RunID, err = skirk.RandomRunID()
	if err != nil {
		return err
	}
	cfg.Tunnel.Listen = loopbackAddr(socksPort)
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.secret = cfg.Secret
	s.socksAddress = cfg.Tunnel.Listen
	for _, port := range []int{socksPort, httpPort} {
		if port == 0 {
			continue
		}
		listener, err := net.Listen("tcp", loopbackAddr(port))
		if err != nil {
			return fmt.Errorf("local port %d is unavailable: %w", port, err)
		}
		_ = listener.Close()
	}
	store, err := makeStore(s.ctx, cfg)
	if err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	tunnel, err := skirk.NewTunnel(store, cfg)
	if err != nil {
		return err
	}
	listenerCount := 1
	if httpPort != 0 {
		listenerCount++
	}
	errCh := make(chan error, listenerCount)
	go func() { errCh <- tunnel.ServeClient(s.ctx, loopbackAddr(socksPort)) }()
	var httpStart chan struct{}
	if httpPort != 0 {
		httpStart = make(chan struct{})
		go func() {
			select {
			case <-s.ctx.Done():
				errCh <- nil
			case <-httpStart:
				errCh <- tunnel.ServeHTTPProxyClient(s.ctx, loopbackAddr(httpPort))
			}
		}()
	}
	go watchListeners(s, errCh, listenerCount)
	if err := waitForListener(s, loopbackAddr(socksPort)); err != nil {
		return fmt.Errorf("SOCKS listener: %w", err)
	}
	if httpPort != 0 {
		close(httpStart)
		if err := waitForListener(s, loopbackAddr(httpPort)); err != nil {
			return fmt.Errorf("HTTP listener: %w", err)
		}
	}
	return nil
}

// ProbeExit verifies one on-demand authenticated round trip to the running
// exit. It returns the sample duration in milliseconds, not a persistent state.
func ProbeExit() (int64, error) {
	bridge.Lock()
	s := bridge.active
	bridge.Unlock()
	if s == nil {
		return 0, errors.New("Skirk client is not running")
	}
	duration, err := skirk.ProbeExit(s.ctx, s.socksAddress, s.secret)
	if err != nil {
		return 0, err
	}
	return duration.Milliseconds(), nil
}

func watchListeners(s *session, errCh <-chan error, count int) {
	err := <-errCh
	if s.ctx.Err() == nil {
		if err == nil {
			err = errors.New("Skirk listener stopped unexpectedly")
		}
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		s.cancel()
	}
	for i := 1; i < count; i++ {
		<-errCh
	}
	close(s.done)
	bridge.Lock()
	if bridge.active == s {
		bridge.active = nil
	}
	if failure := s.failure(); failure != nil {
		bridge.lastErr = failure.Error()
	}
	bridge.Unlock()
}

func (s *session) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func waitForListener(s *session, address string) error {
	timeout := time.NewTimer(startupTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-s.done:
			if failure := s.failure(); failure != nil {
				return failure
			}
			return errors.New("listener stopped")
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("not ready after %s: %w", startupTimeout, err)
		case <-tick.C:
		}
	}
}

func loopbackAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// Stop cancels both listeners and waits briefly for their sockets to close.
func Stop() error {
	bridge.Lock()
	s := bridge.active
	pending := false
	if s == nil {
		s = bridge.pending
		pending = true
	}
	bridge.Unlock()
	if s == nil {
		return nil
	}
	s.cancel()
	if pending {
		return nil
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		// A pending token refresh can still be unwinding; its context is canceled.
		return errors.New("Skirk stop timed out")
	}
	bridge.Lock()
	if bridge.active == s {
		bridge.active = nil
	}
	bridge.Unlock()
	return nil
}

// IsRunning reports whether the in-process listeners are active.
func IsRunning() bool {
	bridge.Lock()
	defer bridge.Unlock()
	return bridge.active != nil && bridge.active.ctx.Err() == nil
}

// LastError returns the most recent asynchronous listener error.
func LastError() string {
	bridge.Lock()
	defer bridge.Unlock()
	return bridge.lastErr
}
