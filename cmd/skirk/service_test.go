package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSystemdServiceName(t *testing.T) {
	tests := map[string]string{
		"skirk-exit":         "skirk-exit.service",
		"skirk-exit.service": "skirk-exit.service",
		"skirk_exit@1":       "skirk_exit@1.service",
	}
	for input, want := range tests {
		got, err := normalizeSystemdServiceName(input)
		if err != nil {
			t.Fatalf("normalizeSystemdServiceName(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeSystemdServiceName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeSystemdServiceNameRejectsUnsafeNames(t *testing.T) {
	for _, input := range []string{"", "../bad", "bad/name", "bad name"} {
		if got, err := normalizeSystemdServiceName(input); err == nil {
			t.Fatalf("normalizeSystemdServiceName(%q) = %q, want error", input, got)
		}
	}
}

func TestSystemdUnitText(t *testing.T) {
	unit := systemdUnitText("/usr/local/bin/skirk", "/opt/skirk-kit/exit.json", "root")
	for _, want := range []string{
		"Description=Skirk exit",
		"# Managed by Skirk",
		"User=root",
		"WorkingDirectory=/opt/skirk-kit",
		"ExecStart=\"/usr/local/bin/skirk\" serve-exit --config \"/opt/skirk-kit/exit.json\"",
		"Restart=always",
		"NoNewPrivileges=true",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}

func TestIsSkirkSystemdUnitText(t *testing.T) {
	if !isSkirkSystemdUnitText(systemdUnitText("/usr/local/bin/skirk", "/opt/skirk-kit/exit.json", "root")) {
		t.Fatal("generated Skirk unit should be recognized")
	}
	legacy := `[Service]
ExecStart="/usr/local/bin/skirk" serve-exit --config "/opt/skirk-kit/exit.json"
`
	if !isSkirkSystemdUnitText(legacy) {
		t.Fatal("legacy Skirk unit should be recognized by ExecStart")
	}
	if !isSkirkSystemdUnitText("[Unit]\nDescription=Wireproxy WARP SOCKS proxy for Skirk exit\n") {
		t.Fatal("legacy Skirk wireproxy unit should be recognized")
	}
	if isSkirkSystemdUnitText("[Service]\nExecStart=/usr/sbin/sshd -D\n") {
		t.Fatal("non-Skirk unit should not be recognized")
	}
}

func TestSystemdUnitTextEscapesWorkingDirectorySpaces(t *testing.T) {
	unit := systemdUnitText("/opt/skirk bin/skirk", "/opt/skirk kit/exit.json", "root")
	for _, want := range []string{
		"WorkingDirectory=/opt/skirk\\skit",
		"ExecStart=\"/opt/skirk bin/skirk\" serve-exit --config \"/opt/skirk kit/exit.json\"",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}

func TestValidateSystemdUserRejectsUnsafeValues(t *testing.T) {
	for _, input := range []string{"", "bad user", "bad\"user", "bad\\user"} {
		if err := validateSystemdUser(input); err == nil {
			t.Fatalf("validateSystemdUser(%q) = nil, want error", input)
		}
	}
}

func TestAssertSkirkDropInDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "10-wireproxy.conf")
	if err := os.WriteFile(path, []byte("[Unit]\nAfter=wireproxy.service\nWants=wireproxy.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertSkirkDropInDir(dir); err != nil {
		t.Fatalf("assertSkirkDropInDir accepted Skirk drop-in: %v", err)
	}
	if err := assertSkirkDropInFile(path); err != nil {
		t.Fatalf("assertSkirkDropInFile accepted Skirk drop-in: %v", err)
	}

	foreign := filepath.Join(t.TempDir(), "foreign.service.d")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(foreign, "override.conf")
	if err := os.WriteFile(foreignPath, []byte("[Service]\nEnvironment=SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertSkirkDropInDir(foreign); err == nil {
		t.Fatal("assertSkirkDropInDir accepted non-Skirk drop-in")
	}
	if err := assertSkirkDropInFile(foreignPath); err == nil {
		t.Fatal("assertSkirkDropInFile accepted non-Skirk drop-in")
	}

	wireproxyForeign := filepath.Join(t.TempDir(), "wireproxy-foreign.conf")
	if err := os.WriteFile(wireproxyForeign, []byte("[Unit]\nAfter=wireproxy.service\nEnvironment=FOREIGN=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertSkirkDropInFile(wireproxyForeign); err == nil {
		t.Fatal("assertSkirkDropInFile accepted non-legacy wireproxy drop-in without Skirk marker")
	}
}

func TestRemoveSystemdDependencyFromUnitText(t *testing.T) {
	input := "[Unit]\nAfter=network-online.target wireproxy.service\nWants=network-online.target wireproxy.service\n[Service]\nExecStart=/usr/local/bin/skirk serve-exit --config /opt/skirk-kit/exit.json\n"
	got, changed := removeSystemdDependencyFromUnitText(input, "wireproxy.service")
	if !changed {
		t.Fatal("removeSystemdDependencyFromUnitText did not report change")
	}
	if strings.Contains(got, "wireproxy.service") {
		t.Fatalf("wireproxy dependency remained:\n%s", got)
	}
	for _, want := range []string{"After=network-online.target", "Wants=network-online.target", "ExecStart=/usr/local/bin/skirk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("updated unit missing %q:\n%s", want, got)
		}
	}
}

func TestSystemdUnitTextPassesSystemdAnalyzeVerify(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "skirk bin")
	kitDir := filepath.Join(dir, "skirk kit")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "skirk")
	config := filepath.Join(kitDir, "exit.json")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dir, "skirk-exit.service")
	if err := os.WriteFile(unitPath, []byte(systemdUnitText(exe, config, "root")), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("systemd-analyze", "verify", unitPath).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify failed: %v\n%s", err, output)
	}
}
