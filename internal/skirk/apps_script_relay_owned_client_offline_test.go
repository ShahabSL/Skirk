package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayOwnedTerminalCarrierFailureAbortsApplicationWait(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := relayResearchPrivateSettings{MuxSecretHex: strings.Repeat("33", 32), SessionID: strings.Repeat("44", 16), PayloadSeedHex: strings.Repeat("77", 32), ClientID: "failure-client", RunID: "failure-run", FrameBytes: 128 << 10, ChunkBytes: 4 << 20, MaxBytes: 32 << 20, PriorityReserveBytes: 1 << 20, PriorityReserveObjects: 16, PackPriority: true, BulkBytes: 4096}
	var originCalls, exchangeCalls atomic.Int64
	origin, err := rrNewOwnedTLSOrigin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { originCalls.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	payload, err := rrCampaignPayload(s)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	runtime := relayResearchOwnedRuntime{OriginURL: origin.URL, OriginTarget: origin.Listener.Addr().String(), OriginCertificateBase64: base64.StdEncoding.EncodeToString(origin.Certificate().Raw), DownloadSHA256: hex.EncodeToString(hash[:]), BulkBytes: len(payload)}
	f, err := os.OpenFile(filepath.Join(dir, "failure-rows.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = rrRunOwnedClient(ctx, dir, s, bytes.Repeat([]byte{1}, 32), [16]byte{2}, runtime, func(context.Context, []byte) ([]byte, error) {
		exchangeCalls.Add(1)
		return nil, errRelayResearchConflict
	}, &relayResearchRows{file: f})
	if err == nil || exchangeCalls.Load() == 0 || originCalls.Load() != 0 || time.Since(start) > 2*time.Second {
		t.Fatal("terminal carrier did not abort before application deadline", err, exchangeCalls.Load(), originCalls.Load(), time.Since(start))
	}
}

func TestRelayOwnedClientOfflineVerifiedTLSAndExactMixedWorkload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := relayResearchPrivateSettings{OuterKeyHex: strings.Repeat("11", 32), EpochHex: strings.Repeat("22", 16), MuxSecretHex: strings.Repeat("33", 32), SessionID: strings.Repeat("44", 16), HTTPAuth: strings.Repeat("55", 32), ScriptAuth: strings.Repeat("66", 32), PayloadSeedHex: strings.Repeat("77", 32), ClientID: "offline-client", RunID: "offline-run", Route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, FrameBytes: 128 << 10, ChunkBytes: 4 << 20, MaxBytes: 32 << 20, PriorityReserveBytes: 1 << 20, PriorityReserveObjects: 16, PackPriority: true, DurationSeconds: 60, MaxCalls: 1024, MaxEncodedBytes: 128 << 20, MaxScriptPosts: 256, MaxHTTPAttemptsUpper: 4096, MaxHTTPEncodedUpper: 1 << 30, BulkBytes: 16 << 20}
	if err := rrWritePrivateJSON(dir, "settings.json", s); err != nil {
		t.Fatal(err)
	}
	_, key, epoch, err := rrReadPrivateSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exitDone := make(chan error, 1)
	go func() { exitDone <- rrRunOwnedExit(ctx, cancel, dir, s, key, epoch) }()
	exitJoined := false
	defer func() {
		cancel()
		if !exitJoined {
			<-exitDone
		}
	}()
	var runtime relayResearchOwnedRuntime
	for {
		if rrReadPrivateJSON(dir, "runtime.json", &runtime) == nil {
			break
		}
		select {
		case err := <-exitDone:
			exitJoined = true
			t.Fatal("owned TLS exit startup failed", err)
		case <-ctx.Done():
			t.Fatal("owned TLS exit startup timed out")
		case <-time.After(time.Millisecond):
		}
	}
	rowsFile, err := os.OpenFile(filepath.Join(dir, "offline-client-samples.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer rowsFile.Close()
	rows := &relayResearchRows{file: rowsFile}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	defer httpClient.CloseIdleConnections()
	exchange := func(callCtx context.Context, wire []byte) ([]byte, error) {
		if e := relayClientDelay(callCtx, 15*time.Millisecond); e != nil {
			return nil, e
		}
		raw, e := json.Marshal(relayResearchHTTPEnvelope{Auth: s.HTTPAuth, Data: base64.StdEncoding.EncodeToString(wire)})
		if e != nil {
			return nil, e
		}
		request, e := http.NewRequestWithContext(callCtx, http.MethodPost, runtime.BrokerLocalURL, bytes.NewReader(raw))
		if e != nil {
			return nil, e
		}
		request.Header.Set("Authorization", "Bearer "+s.HTTPAuth)
		request.Header.Set("Content-Type", "application/json")
		response, e := httpClient.Do(request)
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, errors.New("offline owned RPC rejected")
		}
		body, e := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if e != nil {
			return nil, e
		}
		var envelope relayResearchHTTPEnvelope
		if json.Unmarshal(body, &envelope) != nil {
			return nil, errRelayResearchConflict
		}
		result, e := base64.StdEncoding.Strict().DecodeString(envelope.Data)
		if e != nil {
			return nil, e
		}
		if e = relayClientDelay(callCtx, 15*time.Millisecond); e != nil {
			return nil, e
		}
		return result, nil
	}
	snapshot, err := rrRunOwnedClient(ctx, dir, s, key, epoch, runtime, exchange, rows)
	if err != nil {
		t.Fatal("offline actual TLS/SOCKS workload failed", err, snapshot)
	}
	if snapshot["up_bytes"] != 0 || snapshot["down_bytes"] != 0 {
		t.Fatal("client payload cleanup incomplete", snapshot)
	}
	cancel()
	if err = <-exitDone; err != nil {
		t.Fatal(err)
	}
	exitJoined = true
	var result struct {
		Gets   int64 `json:"origin_gets"`
		Posts  int64 `json:"origin_posts"`
		Echoes int64 `json:"origin_echoes"`
		Active int64 `json:"active_streams_after_close"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "exit-result.json"))
	if err != nil || json.Unmarshal(raw, &result) != nil {
		t.Fatal("missing owned exit evidence", err)
	}
	if result.Gets != 1 || result.Posts != 1 || result.Echoes != 25 || result.Active != 0 {
		t.Fatal("owned origin side effects or cleanup differ", result)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "offline-client-samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	texts, bulks, tlsProof := 0, 0, false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		switch row["kind"] {
		case "tls_handshake":
			tlsProof = row["verified"] == true
		case "text":
			texts++
			if row["success"] != true {
				t.Fatal("failed text row", row)
			}
		case "bulk":
			bulks++
			if row["success"] != true {
				t.Fatal("failed bulk row", row)
			}
		}
	}
	if !tlsProof || texts != 25 || bulks != 2 {
		t.Fatal("missing verified TLS or exact sample counts", tlsProof, texts, bulks)
	}
}

func TestRelayOwnedCampaignReservationMustMatchFreshAllocation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".skirk-runs", "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := relayResearchPrivateSettings{MaxScriptPosts: 256}
	r := relayResearchCampaignReservation{CampaignID: "campaign-one", VariantID: "packed-one", AllocationIndex: 1, ReservedScriptPosts: 256, CampaignMaxScriptPosts: 1024, ReservedAtUTC: time.Now().UTC().Format(time.RFC3339)}
	if err := rrWritePrivateJSON(dir, "campaign-reservation.json", r); err != nil {
		t.Fatal(err)
	}
	if _, err := rrValidateClientReservation(dir, s); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*relayResearchCampaignReservation){func(v *relayResearchCampaignReservation) { v.AllocationIndex = 5 }, func(v *relayResearchCampaignReservation) { v.ReservedScriptPosts = 255 }, func(v *relayResearchCampaignReservation) { v.CampaignMaxScriptPosts = 2048 }, func(v *relayResearchCampaignReservation) { v.CampaignID = "wrong/path" }, func(v *relayResearchCampaignReservation) {
		v.ReservedAtUTC = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	}} {
		bad := r
		change(&bad)
		raw, _ := json.Marshal(bad)
		if err := os.WriteFile(filepath.Join(dir, "campaign-reservation.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := rrValidateClientReservation(dir, s); !errors.Is(err, errRelayResearchBounds) {
			t.Fatal("invalid campaign reservation accepted", err)
		}
	}
}
