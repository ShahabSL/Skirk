package main

import (
	"path/filepath"
	"strings"
	"testing"

	"skirk/internal/skirk"
)

func TestUpdateExitProxyConfigSetsAndUnsetsProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	cfg := skirk.Config{Secret: "test-secret"}
	cfg.ApplyDefaults()
	if err := writeJSONFile(path, cfg); err != nil {
		t.Fatal(err)
	}

	if err := updateExitProxyConfig(path, " socks5h://127.0.0.1:40000 "); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	loaded, err := skirk.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loaded.Tunnel.ExitProxy, "socks5h://127.0.0.1:40000"; got != want {
		t.Fatalf("exit proxy = %q, want %q", got, want)
	}

	if err := updateExitProxyConfig(path, ""); err != nil {
		t.Fatalf("unset proxy: %v", err)
	}
	loaded, err = skirk.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tunnel.ExitProxy != "" {
		t.Fatalf("exit proxy = %q, want direct", loaded.Tunnel.ExitProxy)
	}
}

func TestUpdateExitProxyConfigRejectsInvalidProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	cfg := skirk.Config{Secret: "test-secret", Tunnel: skirk.TunnelConfig{ExitProxy: "socks5h://127.0.0.1:40000"}}
	cfg.ApplyDefaults()
	if err := writeJSONFile(path, cfg); err != nil {
		t.Fatal(err)
	}

	if err := updateExitProxyConfig(path, "ftp://127.0.0.1:21"); err == nil {
		t.Fatal("expected invalid proxy error")
	}
	loaded, err := skirk.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loaded.Tunnel.ExitProxy, "socks5h://127.0.0.1:40000"; got != want {
		t.Fatalf("exit proxy after failed update = %q, want %q", got, want)
	}
}

func TestValidateProxyListenAddr(t *testing.T) {
	for _, input := range []string{"127.0.0.1:40000", "localhost:40000", "[::1]:40000"} {
		if err := validateProxyListenAddr(input); err != nil {
			t.Fatalf("validateProxyListenAddr(%q): %v", input, err)
		}
	}
	for _, input := range []string{"", "127.0.0.1", ":40000", "127.0.0.1:bad"} {
		if err := validateProxyListenAddr(input); err == nil {
			t.Fatalf("validateProxyListenAddr(%q) = nil, want error", input)
		}
	}
}

func TestInstallerScriptURLPinsReleaseTagsOnly(t *testing.T) {
	oldVersion := version
	defer func() { version = oldVersion }()

	version = "v1.2.3"
	if got := installerScriptURL(); !strings.Contains(got, "/v1.2.3/install.sh") {
		t.Fatalf("installerScriptURL for release = %q", got)
	}

	version = "dev"
	if got := installerScriptURL(); !strings.Contains(got, "/main/install.sh") {
		t.Fatalf("installerScriptURL for dev = %q", got)
	}

	version = "v1.2.3/bad"
	if got := installerScriptURL(); !strings.Contains(got, "/main/install.sh") {
		t.Fatalf("installerScriptURL for unsafe version = %q", got)
	}

	version = "v1.2.3;bad"
	if got := installerScriptURL(); !strings.Contains(got, "/main/install.sh") {
		t.Fatalf("installerScriptURL for unsafe shell-like version = %q", got)
	}
}
