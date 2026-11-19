package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewWideMediaPolicyAuthentication(t *testing.T) {
	base := publicPreviewForLifetimeTest(t)
	base.tunnel.Data = base.drive
	base.tunnel.ClientID, base.tunnel.RunID = "media-client", "media-run"
	cfg := &Config{Drive: DriveConfig{FolderID: base.drive.folderID}, Tunnel: TunnelConfig{
		UDPPreviewPublicIPv4: true, UDPPreviewBatchBytes: pilotWidePlain, UDPPreviewMedia: true, UDPPreviewAdaptiveUploads: true,
	}}
	makePreview := func(mediaBytes int) *datagramPreview {
		t.Helper()
		cfg.Tunnel.UDPPreviewMediaBytes = mediaBytes
		p, err := newDatagramPreview(base.tunnel, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	legacy := makePreview(0)
	explicit := makePreview(pilotMediaMaxPlain)
	wide := makePreview(pilotMediaWidePlain)
	legacyPolicy := sha256.Sum256([]byte("media-batches-v1\nbatch-bytes=16384\npublic-ipv4-v1"))
	if legacy.policy != legacyPolicy || explicit.policy != legacy.policy || explicit.mediaBytes != pilotMediaMaxPlain || wide.policy == legacy.policy {
		t.Fatal("explicit default changed the old proof or wide media did not bind its cap")
	}
	for _, tc := range []struct {
		name         string
		client, exit *datagramPreview
		matches      bool
	}{
		{"explicit 64 KiB and old media", explicit, legacy, true},
		{"128 KiB peers", wide, wide, true},
		{"128 KiB client and old exit", wide, legacy, false},
		{"old client and 128 KiB exit", legacy, wide, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, exit := net.Pipe()
			defer client.Close()
			provider := make(chan error, 1)
			go func() {
				defer exit.Close()
				var request [len(udpPreviewRequest) + exitProbeNonceBytes]byte
				if _, err := io.ReadFull(exit, request[:]); err != nil {
					provider <- err
					return
				}
				clientNonce := [exitProbeNonceBytes]byte(request[len(udpPreviewRequest):])
				exitNonce := [exitProbeNonceBytes]byte{8: 7}
				proof, err := tc.exit.proof(base.tunnel.ClientID, base.tunnel.RunID, clientNonce, exitNonce)
				if err == nil {
					err = writeAll(exit, append(append([]byte(udpPreviewReply), exitNonce[:]...), proof[:]...))
				}
				provider <- err
			}()
			lease, err := runPilotPipeHandshake(newPilotTimingState(), tc.client, client)
			if exitErr := <-provider; exitErr != nil {
				t.Fatal(exitErr)
			}
			if tc.matches && (err != nil || lease == [16]byte{}) || !tc.matches && !errors.Is(err, ErrExitNotVerified) {
				t.Fatalf("handshake matches=%t got %v", tc.matches, err)
			}
		})
	}
	for _, tc := range []struct {
		media, adaptive bool
		bytes           int
	}{{false, false, pilotMediaWidePlain}, {true, false, pilotMediaWidePlain}, {true, true, pilotMediaWidePlain + 1}} {
		cfg.Tunnel.UDPPreviewMedia, cfg.Tunnel.UDPPreviewAdaptiveUploads, cfg.Tunnel.UDPPreviewMediaBytes = tc.media, tc.adaptive, tc.bytes
		if _, err := newDatagramPreview(base.tunnel, cfg); err == nil {
			t.Fatalf("accepted invalid media policy %+v", tc)
		}
	}
}

func TestUDPPreviewWideMediaPublishesOnlyQueuedWork(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, tc := range []struct {
		name                      string
		mediaBytes, payloadBytes  int
		wantPackets, pool, queued int
	}{
		{"existing media", 0, 987, 63, 1, 64},
		{"burst media", pilotMediaWidePlain, 987, 126, 1, 1},
		{"small records stay in metadata", pilotMediaWidePlain, 160, pilotMaxBatch, 0, pilotMediaWideBatch - pilotMaxBatch - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotUploadTestProvider{}
				mailbox := newAdaptiveTestMailbox(t, provider)
				mailbox.mediaBytes = tc.mediaBytes
				if cap(mailbox.queue) != 128 {
					t.Fatalf("ingress queue capacity changed to %d", cap(mailbox.queue))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go mailbox.publishLoop(ctx)
				synctest.Wait()
				offerPilotUploadTestPackets(mailbox, pilotMediaWideBatch, tc.payloadBytes)
				time.Sleep(pilotPublishTick + time.Millisecond)
				synctest.Wait()
				attempts, _, _, _ := provider.snapshot()
				wantBytes := len(pilotDatagramMagic) + tc.wantPackets*(pilotPacketHeader+tc.payloadBytes)
				if tc.pool == 1 {
					if wantBytes > mailbox.mediaPlainLimit() || wantBytes+pilotPacketHeader+tc.payloadBytes <= mailbox.mediaPlainLimit() {
						t.Fatalf("media batch did not pack the exact byte limit: bytes=%d cap=%d", wantBytes, mailbox.mediaPlainLimit())
					}
					// One record remains buffered for the next tick. The rest
					// retain the existing bounded ingress queue accounting.
					if tc.wantPackets+tc.queued+1 != pilotMediaWideBatch {
						t.Fatal("publication lost queued or buffered records")
					}
				}
				if len(attempts) != 1 || attempts[0].pool != tc.pool || attempts[0].plain != wantBytes || mailbox.published.Load() != uint64(tc.wantPackets) || len(mailbox.queue) != tc.queued {
					t.Fatalf("unexpected batch, published=%d queued=%d: %+v", mailbox.published.Load(), len(mailbox.queue), attempts)
				}
			})
		})
	}
}

func TestUDPPreviewWideMediaReceivesThroughKnownID(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, tc := range []struct {
		name                                             string
		mediaBytes, plainBytes, packets, gets, delivered int
		metadata                                         bool
	}{
		{"old receiver rejects wide size", 0, pilotMediaWidePlain, pilotMediaWideBatch, 0, 0, false},
		{"wide receiver accepts exact size", pilotMediaWidePlain, pilotMediaWidePlain, pilotMediaWideBatch, 1, pilotMediaWideBatch, false},
		{"wide receiver caps oversized object", pilotMediaWidePlain, pilotMediaWidePlain + 1, pilotMediaWideBatch, 0, 0, false},
		{"wide receiver caps datagram count in binary", pilotMediaWidePlain, pilotMediaWidePlain, pilotMediaWideBatch + 1, 1, 0, false},
		{"old receiver caps datagram count in binary", 0, pilotMediaMaxPlain, pilotMaxBatch + 1, 1, 0, false},
		{"wide receiver preserves metadata count", pilotMediaWidePlain, pilotWidePlain, pilotMaxBatch + 1, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := publicPreviewForLifetimeTest(t)
			mailbox, err := newPilotMailbox(base.tunnel, base.drive, [16]byte{8}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			mailbox.publicIPv4, mailbox.batchBytes, mailbox.media, mailbox.mediaBytes = true, pilotWidePlain, true, tc.mediaBytes
			mailbox.deliver = func(context.Context, pilotPacket) {}
			packets := pilotMediaTestPackets(time.Now(), netip.MustParseAddrPort("8.8.8.8:3478"), tc.plainBytes, tc.packets)
			const sequence = 42
			wire, err := Seal(mailbox.recvKey, mailbox.tunnel.SessionID, mailbox.recvDir, sequence, pilotMarshal(packets), false)
			if err != nil {
				t.Fatal(err)
			}
			var gets int
			mailbox.http = &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gets++
				if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files/"+pilotMediaTestID || req.URL.RawQuery != "alt=media" || req.Header.Get("Range") != "" {
					return nil, fmt.Errorf("unexpected media GET")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(wire))}, nil
			})}}
			object := pilotObject{ID: pilotMediaTestID, Name: fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), sequence), Size: strconv.Itoa(len(wire))}
			if tc.metadata {
				object.Size, object.Description = "0", base64.RawStdEncoding.EncodeToString(wire)
			}
			mailbox.read(t.Context(), object)
			mailbox.liveCalls.Wait()
			if gets != tc.gets || mailbox.delivered.Load() != uint64(tc.delivered) || len(mailbox.cleanup) != tc.delivered/tc.packets || (mailbox.rejected.Load() != 0) != (tc.delivered == 0) {
				t.Fatalf("gets=%d delivered=%d cleanup=%d rejected=%d", gets, mailbox.delivered.Load(), len(mailbox.cleanup), mailbox.rejected.Load())
			}
		})
	}
}
