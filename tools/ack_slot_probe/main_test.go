package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T) *probe {
	t.Helper()
	p := &probe{}
	p.key[0] = 42
	for i := 0; i < frameCount; i++ {
		p.state.payloads[i] = bytes.Repeat([]byte{byte(i + 1)}, frameBytes)
		p.state.tags[i] = tag(p.key, 'D', i+1, p.state.payloads[i])
		p.state.scheduled[i] = time.Now().Add(-time.Second)
	}
	return p
}

func TestCumulativeSnapshotRecoversSkippedVersions(t *testing.T) {
	p := fixture(t)
	first, err := p.dataDescription(1, 10)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := p.verifyData(first)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := p.acceptFrames(frames, time.Now())
	if err != nil || ack != 10 {
		t.Fatalf("first snapshot: ack=%d err=%v", ack, err)
	}
	if err := p.observeDataVersion(0, 4); err != nil {
		t.Fatal(err)
	}
	// The sender trims only after observing this cumulative ACK.
	p.state.acked = ack
	trimmed, err := p.dataDescription(p.state.acked+1, frameCount)
	if err != nil {
		t.Fatal(err)
	}
	trimmedFrames, err := p.verifyData(trimmed)
	if err != nil || trimmedFrames[0].Sequence != 11 || len(trimmedFrames) != 10 {
		t.Fatalf("trimmed snapshot lost unacknowledged frames: %v", err)
	}
	// The receiver misses all intermediate Drive revisions during its pause.
	latest, err := p.dataDescription(11, frameCount)
	if err != nil {
		t.Fatal(err)
	}
	frames, err = p.verifyData(latest)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.observeDataVersion(4, 9); err != nil || p.state.skippedVersions != 4 {
		t.Fatalf("did not record skipped Drive versions: %v", err)
	}
	ack, err = p.acceptFrames(frames, time.Now())
	if err != nil || ack != frameCount || len(p.state.latencies) != frameCount {
		t.Fatalf("latest cumulative snapshot: ack=%d err=%v", ack, err)
	}
	ack, err = p.acceptFrames(frames, time.Now())
	if err != nil || ack != frameCount || len(p.state.latencies) != frameCount {
		t.Fatalf("duplicate cumulative snapshot was not idempotent: ack=%d err=%v", ack, err)
	}
}

func TestRejectsGapAndTamperedFrame(t *testing.T) {
	p := fixture(t)
	p.state.received = 10
	missing, _ := p.dataDescription(12, 20)
	frames, err := p.verifyData(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.acceptFrames(frames, time.Now()); err == nil {
		t.Fatal("accepted missing sequence 11")
	}
	frames[0].Payload = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("x"), frameBytes))
	body, _ := json.Marshal(dataSnapshot{Frames: frames})
	if _, err := p.verifyData("skirk-ack-data-v1:" + base64.RawURLEncoding.EncodeToString(body)); err == nil {
		t.Fatal("accepted tampered frame")
	}
	if _, err := p.verifyData("skirk-ack-data-v1:invalid"); err == nil {
		t.Fatal("accepted invalid snapshot")
	}
	if tag(p.key, 'D', 12, p.state.payloads[11]) == tag(p.key, 'A', 12, p.state.payloads[11]) {
		t.Fatal("data and ACK domains are not distinct")
	}
}

func TestQuotaAndCallCaps(t *testing.T) {
	p := &probe{}
	p.units = hotQuotaCeiling - 24
	if err := p.reserve("get", false); err == nil {
		t.Fatal("allowed GET beyond hot quota reserve")
	}
	p.units = 0
	p.gets = maxGets - 12
	if err := p.reserve("get", false); err == nil {
		t.Fatal("used cleanup GET reserve")
	}
	p.gets = 0
	p.dataWrites = maxDataPatches
	if err := p.reserve("data", false); err == nil {
		t.Fatal("exceeded data PATCH cap")
	}
	p.dataWrites = 0
	p.ackWrites = maxAckPatches
	if err := p.reserve("ack", false); err == nil {
		t.Fatal("exceeded ACK PATCH cap")
	}
}

func TestRequiresLiveAndPrivateCreateOnlyFiles(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Mkdir(".skirk-runs", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOptions([]string{"--config", "test.json", "--out", ".skirk-runs/result.json"}); err == nil {
		t.Fatal("accepted missing --live")
	}
	if _, err := parseOptions([]string{"--live", "--config", "test.json", "--out", "outside.json"}); err == nil {
		t.Fatal("accepted non-private output")
	}
	path := ".skirk-runs/result.json"
	defaultOptions, err := parseOptions([]string{"--live", "--config", "test.json", "--out", path})
	if err != nil || defaultOptions.readerPauseMS != 1000 || defaultOptions.pollMS != 200 || defaultOptions.hotUnits != 12000 {
		t.Fatal(err)
	}
	zeroPause, err := parseOptions([]string{"--live", "--config", "test.json", "--out", path, "--reader-pause-ms", "0"})
	if err != nil || zeroPause.readerPauseMS != 0 {
		t.Fatalf("zero pause rejected: %v", err)
	}
	if _, err := parseOptions([]string{"--live", "--config", "test.json", "--out", path, "--reader-pause-ms", "1001"}); err == nil {
		t.Fatal("accepted pause outside bound")
	}
	for _, poll := range []string{"99", "501"} {
		if _, err := parseOptions([]string{"--live", "--config", "test.json", "--out", path, "--poll-ms", poll}); err == nil {
			t.Fatalf("accepted poll interval %s outside bound", poll)
		}
	}
	for _, cap := range []string{"3999", "12001"} {
		if _, err := parseOptions([]string{"--live", "--config", "test.json", "--out", path, "--hot-units", cap}); err == nil {
			t.Fatalf("accepted hot-unit cap %s outside bound", cap)
		}
	}
	if err := writeRecoveryIDs(path+".recovery-ids", []string{"id-a", "id-b"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".recovery-ids")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery ledger not private: %v", err)
	}
	if err := validateOutputPath(path); err == nil {
		t.Fatal("allowed reuse of recovery ledger")
	}
	if err := writeReport(path, report{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report not private: %v", err)
	}
	if err := writeReport(path, report{}); err == nil {
		t.Fatal("overwrote existing report")
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".skirk-runs", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputPath(".skirk-runs/escape/other.json"); err == nil {
		t.Fatal("accepted symlink escape")
	}
}
