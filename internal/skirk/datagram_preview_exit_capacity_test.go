package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewExitCarrierConfiguration(t *testing.T) {
	for _, test := range []struct {
		limit  int
		shared bool
		valid  bool
	}{
		{0, false, true}, {0, true, true}, {1, true, true}, {4, true, true}, {8, true, true},
		{-1, true, false}, {9, true, false}, {1, false, false},
	} {
		t.Run(fmt.Sprintf("limit_%d_shared_%t", test.limit, test.shared), func(t *testing.T) {
			base := &Config{
				Secret: "exit-capacity-validation-test",
				Drive:  DriveConfig{FolderID: "fixture-folder"},
				Tunnel: TunnelConfig{
					UDPPreviewPublicIPv4:   test.shared,
					UDPPreviewStableUDP:    test.shared,
					UDPPreviewSharedUDP:    test.shared,
					UDPPreviewExitCarriers: test.limit,
				},
			}
			encoded, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseConfig(encoded)
			if !test.valid {
				if err == nil || !strings.Contains(err.Error(), "udp_preview_exit_carriers") {
					t.Fatalf("invalid configuration error: %v", err)
				}
				if _, err := NewTunnel(NewMemoryStore(), base); err == nil || !strings.Contains(err.Error(), "udp_preview_exit_carriers") {
					t.Fatalf("direct constructor accepted invalid configuration: %v", err)
				}
				return
			}
			if err != nil || cfg.Tunnel.UDPPreviewExitCarriers != test.limit {
				t.Fatalf("configuration rejected or lost its limit: %v", err)
			}
			if !test.shared {
				return
			}
			httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}}
			peer, err := NewTunnel(NewDriveStore(httpClient, "owned-test-token", cfg.Drive), cfg)
			if err != nil {
				t.Fatal(err)
			}
			want := test.limit
			if want == 0 {
				want = 4
			}
			metrics := peer.UDPPreviewSnapshot()
			if metrics.ExitCarrierLimit != want || metrics.ActiveCarriers != 0 || cap(peer.udpPreview.slots) != 4 {
				t.Fatalf("wrong execution limits: exit=%+v client=%d", metrics, cap(peer.udpPreview.slots))
			}
		})
	}
}

func TestUDPPreviewExitCarrierLimitKeepsExistingClientCompatible(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
	newPeer := func(clientID string, exitCarriers int) *Tunnel {
		t.Helper()
		cfg := &Config{
			Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
			Client: ClientConfig{ID: clientID, RunID: "fixture-run"},
			Drive:  DriveConfig{FolderID: "fixture-folder"},
			Tunnel: TunnelConfig{
				Transport: "muxv5", PollIntervalMS: 10, CleanupProcessed: true,
				UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true,
				UDPPreviewExitCarriers: exitCarriers, UDPPreviewGroupedDiscovery: clientID == "",
			},
		}
		cfg.ApplyDefaults()
		peer, err := NewTunnel(NewDriveStore(httpClient, "owned-test-token", cfg.Drive), cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer.Data, peer.Logger, peer.udpPreview.http = store, nil, httpClient
		return peer
	}
	exit, first, second := newPeer("", 1), newPeer("first", 0), newPeer("second", 0)
	exitDone := make(chan struct{})
	go func() {
		defer close(exitDone)
		_ = exit.ServeExit(ctx)
	}()
	t.Cleanup(func() { cancel(); <-exitDone })
	clientMux := func(client *Tunnel) *driveMux {
		t.Helper()
		mux, err := client.getClientMux(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.releaseClientMux(mux) })
		return mux
	}
	firstMux, secondMux := clientMux(first), clientMux(second)
	lease, err := first.udpPreview.openClientLease(ctx, firstMux)
	if err != nil {
		t.Fatalf("unchanged client failed to verify configured exit: %v", err)
	}
	defer func() {
		if lease != nil {
			lease.finish("fixture_done")
		}
	}()
	if rejected, err := second.udpPreview.openClientLease(ctx, secondMux); err == nil {
		rejected.finish("fixture_done")
		t.Fatal("second independent client exceeded configured exit capacity")
	} else if !errors.Is(err, errUDPExitBusy) {
		t.Fatalf("exit capacity feedback was lost: %v", err)
	}
	metrics := exit.UDPPreviewSnapshot()
	if metrics.ExitCarrierLimit != 1 || metrics.ActiveCarriers != 1 || metrics.ExitCarrierLimitHits != 1 || metrics.LocalFrontendLimitHits != 0 || metrics.AdmissionLimitHits != 1 {
		t.Fatalf("wrong exit capacity metrics: %+v", metrics)
	}
	if metrics := second.UDPPreviewSnapshot(); metrics.ActiveCarriers != 0 || metrics.AdmissionLimitHits != 0 || metrics.RemoteExitLimitHits != 1 {
		t.Fatalf("remote refusal counted as a client-side limit: %+v", metrics)
	}
	type leaseResult struct {
		lease *pilotClientLease
		err   error
	}
	joining := make(chan leaseResult, 1)
	go func() {
		fresh, err := second.udpPreview.openClientLease(ctx, secondMux)
		joining <- leaseResult{fresh, err}
	}()
	for len(exit.udpPreview.exitWaiters) != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("new client failed to wait for the occupied slot")
		case <-time.After(10 * time.Millisecond):
		}
	}
	lease.finish("fixture_done")
	lease = nil
	select {
	case result := <-joining:
		if result.err != nil {
			t.Fatalf("second client failed to use released carrier slot: %v", result.err)
		}
		lease = result.lease
	case <-ctx.Done():
		t.Fatal("waiting client did not receive the released slot")
	}
}

func TestUDPPreviewExitCapacityWaitIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		preview := &datagramPreview{exitSlots: make(chan struct{}, 1), exitWaiters: make(chan struct{}, 1)}
		ctx := t.Context()
		slots := preview.exitCarrierSlots()
		if !preview.acquireExitCarrier(ctx, slots) {
			t.Fatal("available carrier rejected")
		}
		waiting := make(chan bool, 1)
		started := time.Now()
		go func() { waiting <- preview.acquireExitCarrier(ctx, slots) }()
		synctest.Wait()
		if len(preview.exitWaiters) != 1 || preview.acquireExitCarrier(ctx, slots) || !time.Now().Equal(started) {
			t.Fatal("occupancy did not bound the waiting queue")
		}
		if <-waiting || time.Since(started) != udpPreviewCapacityGrace || len(preview.exitWaiters) != 0 {
			t.Fatal("sustained congestion did not time out and release the waiter")
		}
		<-slots
	})
}
