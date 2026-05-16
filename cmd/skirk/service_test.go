package main

import (
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
		"WorkingDirectory=\"/opt/skirk-kit\"",
		"ExecStart=\"/usr/local/bin/skirk\" serve-exit --config \"/opt/skirk-kit/exit.json\"",
		"Restart=always",
		"NoNewPrivileges=true",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}
