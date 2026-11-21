package skirk

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestIdleUDPPollResetsOnUncertainOrActivePage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		page    pilotDiscoveryPage
		err     error
		offered uint64
	}{
		{name: "incomplete search", page: pilotDiscoveryPage{IncompleteSearch: true}},
		{name: "continuation", page: pilotDiscoveryPage{NextPageToken: "next"}},
		{name: "list error", err: errors.New("list failed")},
		{name: "failed page", page: pilotDiscoveryPage{failed: true}},
		{name: "inbound traffic", page: pilotDiscoveryPage{Files: []pilotObject{{ID: "object"}}}},
		{name: "outbound traffic during list", offered: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var q idleUDPPoll
			for range 4 {
				if !q.due(0) {
					t.Fatal("skipped before four complete empty pages")
				}
				q.observe(pilotDiscoveryPage{}, nil, 0)
			}
			if q.due(0) || !q.due(0) {
				t.Fatal("expected one skipped tick followed by a poll")
			}
			q.observe(tc.page, tc.err, tc.offered)
			for range 4 {
				if !q.due(tc.offered) {
					t.Fatal("skipped before four new quiet pages")
				}
				q.observe(pilotDiscoveryPage{}, nil, tc.offered)
			}
			if q.due(tc.offered) {
				t.Fatal("did not resume conservation")
			}
		})
	}
}

func TestIdleUDPPollClientAndExitCadence(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, tc := range []struct {
		name    string
		up      bool
		enabled bool
		before  int32
	}{
		{"default client", true, false, 8},
		{"quiet client", true, true, 6},
		{"exit given client setting", false, true, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := publicPreviewForLifetimeTest(t)
				cfg := Config{Secret: base.tunnel.Secret, SessionID: SessionString(base.tunnel.SessionID),
					Drive: DriveConfig{FolderID: base.drive.folderID}, Tunnel: TunnelConfig{UDPPreviewPublicIPv4: true}}
				defaultPeer, err := NewTunnel(base.drive, &cfg)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Tunnel.ConserveIdleUDP = tc.enabled
				tunnel, err := NewTunnel(base.drive, &cfg)
				if err != nil {
					t.Fatal(err)
				}
				if tunnel.udpPreview.policy != defaultPeer.udpPreview.policy {
					t.Fatal("client-local option changed the peer's protocol policy")
				}
				box, err := newPilotMailbox(tunnel, base.drive, [16]byte{1}, tc.up, nil)
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				box.http.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					return stringResponse(http.StatusOK, `{"files":[]}`), nil
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go box.poll(ctx)
				synctest.Wait()
				for range 8 {
					time.Sleep(pilotPollTick)
					synctest.Wait()
				}
				if got := calls.Load(); got != tc.before {
					t.Fatalf("first eight ticks made %d lists, want %d", got, tc.before)
				}
				// The quiet client would otherwise skip the ninth tick.
				box.offered.Add(1)
				time.Sleep(pilotPollTick)
				synctest.Wait()
				if got := calls.Load(); got != tc.before+1 {
					t.Fatalf("expected a list on the next tick after an outbound packet, got %d", got)
				}
			})
		})
	}
}
