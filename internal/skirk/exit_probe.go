package skirk

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The target is reserved for an on-demand challenge to an updated exit. It is
// intercepted before DNS or TCP dialing; old exits fail closed on .invalid.
const (
	exitProbeTarget        = "skirk-probe.invalid:1"
	exitProbeRequestMagic  = "skirk-exit-probe-v1:"
	exitProbeResponseMagic = "skirk-exit-proof-v1:"
	exitProbeNonceBytes    = 32
	exitProbeTimeout       = 45 * time.Second
)

var ErrExitNotVerified = errors.New("exit did not return a valid proof")

// ProbeExit verifies one authenticated round trip through the running local
// SOCKS client and the remote exit. It does not check arbitrary Internet sites
// or make a continuous background request. The returned duration is one sample.
func ProbeExit(ctx context.Context, socksAddress, secret string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, exitProbeTimeout)
	defer cancel()
	started := time.Now()
	var nonce [exitProbeNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	conn, err := DialViaSOCKS5(ctx, "socks5h://"+socksAddress, exitProbeTarget)
	if err != nil {
		return 0, fmt.Errorf("local SOCKS probe: %w", err)
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	request := make([]byte, 0, len(exitProbeRequestMagic)+len(nonce))
	request = append(request, exitProbeRequestMagic...)
	request = append(request, nonce[:]...)
	if err := writeAll(conn, request); err != nil {
		return 0, fmt.Errorf("send exit challenge: %w", err)
	}
	proof := make([]byte, len(exitProbeResponseMagic)+sha256.Size)
	if _, err := io.ReadFull(conn, proof); err != nil {
		return 0, fmt.Errorf("read exit proof: %w", err)
	}
	if string(proof[:len(exitProbeResponseMagic)]) != exitProbeResponseMagic {
		return 0, ErrExitNotVerified
	}
	expected, err := exitProbeMAC(secret, nonce[:])
	if err != nil {
		return 0, err
	}
	if !hmac.Equal(proof[len(exitProbeResponseMagic):], expected) {
		return 0, ErrExitNotVerified
	}
	return time.Since(started), nil
}

func exitProbeMAC(secret string, nonce []byte) ([]byte, error) {
	key, err := DeriveKey(secret)
	if err != nil {
		return nil, err
	}
	proofKey := hkdfSHA256(key, []byte("skirk-exit-probe-salt-v1"), []byte("exit-proof"), sha256.Size)
	mac := hmac.New(sha256.New, proofKey)
	_, _ = mac.Write([]byte("skirk-exit-proof-v1\x00"))
	_, _ = mac.Write(nonce)
	return mac.Sum(nil), nil
}

func newExitProbeConnection(secret string) (net.Conn, error) {
	// Reject malformed key material without opening a synthetic stream.
	if _, err := DeriveKey(secret); err != nil {
		return nil, err
	}
	remote, responder := net.Pipe()
	go func() {
		defer responder.Close()
		request := make([]byte, len(exitProbeRequestMagic)+exitProbeNonceBytes)
		if _, err := io.ReadFull(responder, request); err != nil {
			return
		}
		if string(request[:len(exitProbeRequestMagic)]) != exitProbeRequestMagic {
			return
		}
		mac, err := exitProbeMAC(secret, request[len(exitProbeRequestMagic):])
		if err != nil {
			return
		}
		response := make([]byte, 0, len(exitProbeResponseMagic)+len(mac))
		response = append(response, exitProbeResponseMagic...)
		response = append(response, mac...)
		_ = writeAll(responder, response)
	}()
	return remote, nil
}
