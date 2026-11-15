package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedLiveArgumentsAndPrivateOutput(t *testing.T) {
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
		t.Fatal("accepted output outside private root")
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".skirk-runs", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputPath(".skirk-runs/escape/result.json"); err == nil {
		t.Fatal("accepted symlink escape")
	}
	value, err := parseOptions([]string{"--live", "--config", "test.json", "--out", ".skirk-runs/result.json"})
	if err != nil || !value.live || !strings.HasSuffix(value.out, "result.json") {
		t.Fatalf("valid bounded arguments failed: %v", err)
	}
}

func TestReportIsPrivateAndCreateOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	if err := writeReport(path, report{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode = %v", info.Mode().Perm())
	}
	if err := writeReport(path, report{SchemaVersion: 2}); err == nil {
		t.Fatal("overwrote existing result")
	}
	recovery := path + ".recovery-id"
	if err := writeRecoveryID(recovery, "known-id"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(recovery)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery record not private: %v", err)
	}
	if err := writeRecoveryID(recovery, "other-id"); err == nil {
		t.Fatal("overwrote exact-ID recovery record")
	}
}
