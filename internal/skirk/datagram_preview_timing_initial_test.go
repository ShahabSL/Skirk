package skirk

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestPilotInitialTimingAuthenticatesExistingRoundTrip(t *testing.T) {
	for _, mode := range []string{"current", "forged nonce", "legacy proof", "strict v2 proof", "busy", "altered busy header", "altered lease header", "replayed busy"} {
		t.Run(mode, func(t *testing.T) {
			preview := publicPreviewForLifetimeTest(t)
			preview.tunnel.ClientID, preview.tunnel.RunID = "timing-client", "timing-run"
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				var request [len(udpPreviewRequest) + exitProbeNonceBytes]byte
				if _, err := io.ReadFull(server, request[:]); err != nil {
					done <- err
					return
				}
				clientNonce := [exitProbeNonceBytes]byte(request[len(udpPreviewRequest):])
				exitNonce := [exitProbeNonceBytes]byte{8: 42}
				header := udpPreviewReply
				if mode == "busy" || mode == "altered busy header" || mode == "replayed busy" {
					header = udpPreviewBusyReply
				}
				if mode == "replayed busy" {
					clientNonce[0] ^= 1
				}
				proof, err := preview.replyProof(preview.tunnel.ClientID, preview.tunnel.RunID, clientNonce, exitNonce, header)
				if err != nil {
					done <- err
					return
				}
				if mode == "legacy proof" || mode == "strict v2 proof" {
					root, _ := DeriveKey(preview.tunnel.Secret)
					domain := "skirk-udp-preview-lease-v1"
					if mode == "strict v2 proof" {
						domain = "skirk-udp-preview-lease-causal-reset-v2"
					}
					key := hkdfSHA256(root, preview.tunnel.SessionID[:], []byte(domain), sha256.Size)
					mac := hmac.New(sha256.New, key)
					_, _ = io.WriteString(mac, preview.tunnel.ClientID+"\x00"+preview.tunnel.RunID+"\x00"+preview.drive.folderID+"\x00")
					_, _ = mac.Write(preview.policy[:])
					_, _ = mac.Write(clientNonce[:])
					_, _ = mac.Write(exitNonce[:])
					copy(proof[:], mac.Sum(nil))
				}
				if mode == "forged nonce" {
					exitNonce[0] ^= 1
				}
				switch mode {
				case "altered busy header":
					header = udpPreviewReply
				case "altered lease header":
					header = udpPreviewBusyReply
				}
				done <- writeAll(server, append(append([]byte(header), exitNonce[:]...), proof[:]...))
			}()
			timing := newPilotTimingState()
			_, err := runPilotPipeHandshake(timing, preview, client)
			if writeErr := <-done; writeErr != nil {
				t.Fatal(writeErr)
			}
			if mode == "current" {
				if err != nil || timing.active != 1 {
					t.Fatalf("one-round-trip handshake failed: %v", err)
				}
			} else if mode == "busy" {
				if !errors.Is(err, errUDPExitBusy) || timing.active != 0 || preview.remoteExitLimitHits.Load() != 1 {
					t.Fatalf("authenticated busy reply mishandled: %v", err)
				}
			} else if !errors.Is(err, ErrExitNotVerified) || preview.remoteExitLimitHits.Load() != 0 {
				t.Fatalf("unauthenticated timing accepted: %v", err)
			}
		})
	}
}

func runPilotPipeHandshake(timing *pilotTimingState, preview *datagramPreview, control net.Conn) ([16]byte, error) {
	nonce := [exitProbeNonceBytes]byte{0x6f, 0x3d}
	started := timing.begin()
	if err := control.SetDeadline(started.Add(udpPreviewHandshake)); err != nil {
		return [16]byte{}, err
	}
	if err := writeAll(control, append([]byte(udpPreviewRequest), nonce[:]...)); err != nil {
		return [16]byte{}, err
	}
	return preview.readClientLeaseProof(control, timing, nonce, started)
}

func TestPilotInitialTimingRealBatchIgnoresPeerWallOffset(t *testing.T) {
	for _, offset := range []time.Duration{-24 * time.Hour, -2 * time.Second, 0, 2 * time.Second, 24 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			secret, err := RandomSecret()
			if err != nil {
				t.Fatal(err)
			}
			tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{1}}
			target := netip.MustParseAddrPort("127.0.0.1:3478")
			allow := map[netip.AddrPort]bool{target: true}
			client, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{2}, true, allow)
			if err != nil {
				t.Fatal(err)
			}
			server, err := newPilotMailbox(tunnel, &DriveStore{}, [16]byte{2}, false, allow)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			clientBase, serverBase := base.Add(offset), base.Add(200*time.Millisecond)
			clientNow, serverNow := clientBase.Add(400*time.Millisecond), serverBase.Add(200*time.Millisecond)
			client.timing, server.timing = newPilotTimingState(), newPilotTimingState()
			client.timing.now = func() time.Time { return clientNow }
			server.timing.now = func() time.Time { return serverNow }
			if err := client.timing.install(1, clientBase.Add(400*time.Millisecond), clientBase); err != nil {
				t.Fatal(err)
			}
			if err := server.timing.install(1, serverBase, serverBase); err != nil {
				t.Fatal(err)
			}
			if err := client.timing.activate(1); err != nil {
				t.Fatal(err)
			}
			if err := server.timing.activate(1); err != nil {
				t.Fatal(err)
			}
			var up, down int
			server.deliver = func(context.Context, pilotPacket) { up++ }
			client.deliver = func(context.Context, pilotPacket) { down++ }
			clientNow = clientBase.Add(800 * time.Millisecond)
			client.offer(1, target, []byte("up"))
			packet := <-client.queue
			serverNow = serverBase.Add(800 * time.Millisecond)
			wire, _ := Seal(client.sendKey, tunnel.SessionID, client.sendDir, 1, pilotMarshal([]pilotPacket{packet}), false)
			server.acceptBatch(t.Context(), "up-batch", 1, wire, pilotMaxPlain, pilotMaxBatch)
			if up != 1 {
				t.Fatal("authenticated first packet did not use its causal timing map")
			}
			serverNow = serverBase.Add(900 * time.Millisecond)
			server.offer(1, target, []byte("down"))
			packet = <-server.queue
			clientNow = clientBase.Add(1300 * time.Millisecond)
			wire, _ = Seal(server.sendKey, tunnel.SessionID, server.sendDir, 1, pilotMarshal([]pilotPacket{packet}), false)
			client.acceptBatch(t.Context(), "down-batch", 1, wire, pilotMaxPlain, pilotMaxBatch)
			if down != 1 {
				t.Fatal("reverse packet was lost because of wall offset")
			}
			// The same encrypted batch is consumed exactly once and cleaned up.
			client.acceptBatch(t.Context(), "down-batch", 1, wire, pilotMaxPlain, pilotMaxBatch)
			if down != 1 || len(client.cleanup) != 1 {
				t.Fatal("timing mode changed replay or authenticated cleanup behavior")
			}
		})
	}
}
