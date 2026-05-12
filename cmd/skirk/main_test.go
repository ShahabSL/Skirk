package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"skirk/internal/skirk"
)

func TestSummarizeHTTPSamples(t *testing.T) {
	got := summarizeHTTPSamples([]benchHTTPResult{
		{Status: 200, Bytes: 1000, TTFBMS: 30, TotalMS: 100, Mbps: 0.08},
		{Status: 200, Bytes: 2000, TTFBMS: 10, TotalMS: 200, Mbps: 0.08},
		{Status: 500, Bytes: 3000, TTFBMS: 50, TotalMS: 300, Mbps: 0.08},
	})
	if got.Samples != 3 || got.Successes != 2 || got.Bytes != 6000 {
		t.Fatalf("summary = %+v, want sample/success/byte counts", got)
	}
	if got.P50TTFBMS != 30 || got.P95TTFBMS != 50 || got.P50TotalMS != 200 || got.P95TotalMS != 300 {
		t.Fatalf("summary = %+v, want percentile latency values", got)
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
