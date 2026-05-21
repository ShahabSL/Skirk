package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

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
