package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestExitStatusForTerminalOAuthGrantError(t *testing.T) {
	err := &skirk.OAuthTokenError{Status: 400, Code: "invalid_grant", Description: "expired"}
	if got := exitStatusForError([]string{"skirk", "serve-exit"}, err); got != 78 {
		t.Fatalf("exit status = %d, want 78", got)
	}
	if got := exitStatusForError([]string{"skirk", "serve-client"}, err); got != 78 {
		t.Fatalf("client exit status = %d, want 78", got)
	}
	if got := exitStatusForError([]string{"skirk", "--config", "client.json"}, err); got != 78 {
		t.Fatalf("implicit client exit status = %d, want 78", got)
	}
	if got := exitStatusForError([]string{"skirk", "serve-exit"}, errors.New("temporary network failure")); got != 1 {
		t.Fatalf("temporary error exit status = %d, want 1", got)
	}
}

func TestServeExitStopsRestartingForInaccessibleKit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing exit.json")
	err := serveExit(t.Context(), []string{"--config", path})
	if !errors.Is(err, os.ErrNotExist) || exitStatusForError([]string{"skirk", "serve-exit"}, err) != 78 {
		t.Fatalf("unexpected config failure %v", err)
	}
}

func TestServeExitStopsRestartingForExpiredGrant(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has expired"}`))
	}))
	defer tokenServer.Close()
	path := filepath.Join(t.TempDir(), "exit.json")
	if err := writeJSONFile(path, skirk.Config{
		Secret: "test-secret",
		Auth: skirk.AuthConfig{
			ClientID: "test-client", RefreshToken: "expired-refresh-token", TokenURL: tokenServer.URL,
		},
		Route: skirk.RouteConfig{Mode: "direct"},
		Drive: skirk.DriveConfig{Space: "appDataFolder"},
	}); err != nil {
		t.Fatal(err)
	}
	err := serveExit(context.Background(), []string{"--config", path})
	if got := exitStatusForError([]string{"skirk", "serve-exit"}, err); got != 78 {
		t.Fatalf("expired-grant exit status = %d, error = %v; want 78", got, err)
	}
}

func TestSummarizeHTTPSamples(t *testing.T) {
	got := summarizeHTTPSamples([]benchHTTPResult{
		{OK: true, Status: 200, Bytes: 1000, TTFBMS: 30, TotalMS: 100, Mbps: 0.08},
		{OK: true, Status: 200, Bytes: 2000, TTFBMS: 10, TotalMS: 200, Mbps: 0.08},
		{Status: 500, Bytes: 3000, TTFBMS: 50, TotalMS: 300, Mbps: 0.08, Error: "http_status_500"},
	})
	if got.Samples != 3 || got.Successes != 2 || got.Failures != 1 || got.Bytes != 6000 {
		t.Fatalf("summary = %+v, want sample/success/byte counts", got)
	}
	if got.P50TTFBMS != 30 || got.P95TTFBMS != 30 || got.P99TTFBMS != 30 || got.P50TotalMS != 200 || got.P95TotalMS != 200 || got.P99TotalMS != 200 {
		t.Fatalf("summary = %+v, want success-only percentile latency values", got)
	}
	if got.Errors["http_status_500"] != 1 {
		t.Fatalf("summary errors = %+v, want one HTTP 500", got.Errors)
	}
}

func TestBenchLiveResultFailsOnStall(t *testing.T) {
	result := benchLiveResult{
		Small: benchHTTPSummary{Samples: 1, Failures: 1, Stalls: 1},
	}
	err := result.benchmarkFailure()
	if err == nil || !strings.Contains(err.Error(), "stalls=1") {
		t.Fatalf("benchmarkFailure error = %v, want stall failure", err)
	}
}

func TestBenchLiveResultPassesWithoutHTTPFailures(t *testing.T) {
	result := benchLiveResult{
		Small: benchHTTPSummary{Samples: 1, Successes: 1},
		Bulk:  &benchHTTPSummary{Samples: 2, Successes: 2},
	}
	if err := result.benchmarkFailure(); err != nil {
		t.Fatalf("benchmarkFailure = %v, want nil", err)
	}
}

func TestCopyResponseBodyWithGuardsDetectsStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &blockingAfterFirstByteReader{ctx: ctx}

	n, stalled, slow, err := copyResponseBodyWithGuards(ctx, cancel, reader, 50*time.Millisecond, 0, 0)
	if n != 1 || !stalled || slow || !errors.Is(err, errDownloadStalled) {
		t.Fatalf("copy result n=%d stalled=%t slow=%t err=%v, want stalled after first byte", n, stalled, slow, err)
	}
}

func TestBenchListenAddressAllocatesPort(t *testing.T) {
	addr, err := benchListenAddress("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if host == "" || host == "0.0.0.0" || port == "0" {
		t.Fatalf("addr = %q, want concrete loopback port", addr)
	}
}

func TestQuotaPerMinute(t *testing.T) {
	got := quotaPerMinute(skirk.DriveQuotaSnapshot{Calls: 10, Units: 500, Errors: 1, ResponseBytes: 2000}, 30*time.Second)
	if got.Calls != 20 || got.Units != 1000 || got.Errors != 2 || got.ResponseBytes != 4000 {
		t.Fatalf("quotaPerMinute = %+v", got)
	}
}

func TestQuotaPerRequest(t *testing.T) {
	got := quotaPerRequest(skirk.DriveQuotaSnapshot{Calls: 10, Units: 500, Errors: 1, ResponseBytes: 2000}, 2)
	if got.Calls != 5 || got.Units != 250 || got.Errors != 0.5 || got.ResponseBytes != 1000 {
		t.Fatalf("quotaPerRequest = %+v", got)
	}
}

func TestDefaultCleanupPrefixesIncludeExperimentalSlots(t *testing.T) {
	got := defaultCleanupPrefixes(false)
	want := append(append([]string(nil), mailboxJanitorPrefixes...), "slotv1/")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default cleanup prefixes = %#v, want %#v", got, want)
	}
	got[0] = "changed/"
	if mailboxJanitorPrefixes[0] == "changed/" {
		t.Fatal("default cleanup prefixes should not expose mutable janitor prefix slice")
	}
}

func TestDefaultCleanupPrefixesIncludeUDPOnlyForPreview(t *testing.T) {
	want := append(defaultCleanupPrefixes(false), skirk.UDPPreviewCleanupPrefix)
	if got := defaultCleanupPrefixes(true); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("preview cleanup prefixes = %#v, want %#v", got, want)
	}
}

func TestMailboxJanitorDefaultAgeProtectsLongTransfers(t *testing.T) {
	if mailboxJanitorDefaultOlderThan < 24*time.Hour {
		t.Fatalf("mailbox janitor default age = %s, want at least 24h to avoid deleting live slow-transfer objects", mailboxJanitorDefaultOlderThan)
	}
}

func TestImplicitServeClientCompatibility(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "double dash config", args: []string{"--config", "client.skirk"}, want: true},
		{name: "single dash config", args: []string{"-config", "client.skirk"}, want: true},
		{name: "command remains explicit", args: []string{"serve-client", "--config", "client.skirk"}, want: false},
		{name: "unknown flag is not captured", args: []string{"--listen", "127.0.0.1:18080"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isImplicitServeClient(tt.args); got != tt.want {
				t.Fatalf("isImplicitServeClient(%v) = %t, want %t", tt.args, got, tt.want)
			}
		})
	}
}

func TestBenchListenAddressRejectsInvalidAddress(t *testing.T) {
	_, err := benchListenAddress("not-a-host-port")
	if err == nil || !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("err = %v, want missing port error", err)
	}
}

func TestEnvDuration(t *testing.T) {
	t.Setenv("SKIRK_TEST_DURATION", "15m")
	if got := envDuration("SKIRK_TEST_DURATION", time.Hour); got != 15*time.Minute {
		t.Fatalf("envDuration = %s, want 15m", got)
	}
	t.Setenv("SKIRK_TEST_DURATION", "bad")
	if got := envDuration("SKIRK_TEST_DURATION", time.Hour); got != time.Hour {
		t.Fatalf("envDuration fallback = %s, want 1h", got)
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("SKIRK_TEST_BOOL", "yes")
	if !envBool("SKIRK_TEST_BOOL") {
		t.Fatal("envBool should accept yes")
	}
	t.Setenv("SKIRK_TEST_BOOL", "no")
	if envBool("SKIRK_TEST_BOOL") {
		t.Fatal("envBool should reject no")
	}
}

type blockingAfterFirstByteReader struct {
	ctx  context.Context
	sent bool
}

func (r *blockingAfterFirstByteReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		p[0] = 'x'
		return 1, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
