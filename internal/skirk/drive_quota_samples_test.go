package skirk

import (
	"net/http"
	"testing"
	"time"
)

func TestDriveQuotaPercentilesAfterSampleWindowWraps(t *testing.T) {
	stats := newDriveQuotaStats(time.Hour)
	const count = 2*driveQuotaMaxSamplesPerOp + 317
	for i := range count {
		stats.Record("download", http.StatusOK, 0, time.Duration(i)*time.Millisecond, nil, "")
	}
	got := stats.Snapshot().Ops["download"]
	const oldest = count - driveQuotaMaxSamplesPerOp
	if got.P50DurationMS != oldest+driveQuotaMaxSamplesPerOp/2 ||
		got.P95DurationMS != oldest+driveQuotaMaxSamplesPerOp*95/100 ||
		got.Calls != count || got.TotalDurationMS != count*(count-1)/2 {
		t.Fatalf("unexpected duration statistics after wrap: %+v", got)
	}
}

func BenchmarkDriveQuotaRecordFullWindow(b *testing.B) {
	stats := newDriveQuotaStats(time.Hour)
	for i := range driveQuotaMaxSamplesPerOp {
		stats.Record("download", http.StatusOK, 4096, time.Duration(i)*time.Millisecond, nil, "")
	}
	b.ReportAllocs()
	for b.Loop() {
		stats.Record("download", http.StatusOK, 4096, time.Second, nil, "")
	}
}
