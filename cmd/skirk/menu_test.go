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
	for _, input := range []string{"127.0.0.1:40000"} {
		if err := validateProxyListenAddr(input); err != nil {
			t.Fatalf("validateProxyListenAddr(%q): %v", input, err)
		}
	}
	for _, input := range []string{"", "127.0.0.1", ":40000", "127.0.0.1:bad", "127.0.0.1:40000\n[HTTP]", "localhost:40000", "[::1]:40000", "0.0.0.0:40000", "192.168.1.10:40000"} {
		if err := validateProxyListenAddr(input); err == nil {
			t.Fatalf("validateProxyListenAddr(%q) = nil, want error", input)
		}
	}
}

func TestUpdateInstallerEnvPinsCurrentBinaryDirAndStripsSetupEnv(t *testing.T) {
	got := updateInstallerEnv([]string{
		"PATH=/usr/bin",
		"SKIRK_SERVER_SETUP=1",
		"SKIRK_UNINSTALL=1",
		"SKIRK_REPO=attacker/repo",
		"SKIRK_ASSET_BASE=file:///tmp/bad",
		"SKIRK_VERSION=v9.9.9",
		"SKIRK_DEV_INSTALL=1",
		"SKIRK_INSTALL_WIREPROXY=1",
		"SKIRK_WIREPROXY_BIND=0.0.0.0:40000",
		"SKIRK_ACCEPT_WARP_TOS=1",
		"SKIRK_EXIT_PROXY=socks5h://127.0.0.1:40000",
		"SKIRK_INSTALL_DIR=/old/path",
	})
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, forbidden := range []string{
		"SKIRK_SERVER_SETUP=",
		"SKIRK_UNINSTALL=",
		"SKIRK_REPO=",
		"SKIRK_ASSET_BASE=",
		"SKIRK_VERSION=",
		"SKIRK_DEV_INSTALL=",
		"SKIRK_INSTALL_WIREPROXY=",
		"SKIRK_WIREPROXY_BIND=",
		"SKIRK_ACCEPT_WARP_TOS=",
		"SKIRK_EXIT_PROXY=",
		"SKIRK_INSTALL_DIR=/old/path",
	} {
		if strings.Contains(joined, "\n"+forbidden) {
			t.Fatalf("update env kept forbidden entry %q in:\n%s", forbidden, joined)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "SKIRK_INSTALL_DIR=", "SKIRK_REQUIRE_RELEASE_ASSET=1"} {
		if !strings.Contains(joined, "\n"+want) {
			t.Fatalf("update env missing %q in:\n%s", want, joined)
		}
	}
}

func TestValidMenuUpdateVersion(t *testing.T) {
	for _, input := range []string{"latest", "v0.1.49", "v10.20.30"} {
		if !validMenuUpdateVersion(input) {
			t.Fatalf("validMenuUpdateVersion(%q) = false", input)
		}
	}
	for _, input := range []string{"main", "dev", "v1", "v1.2", "v1.2.3-rc1", "v1.2.3/bad", "v1.2.x"} {
		if validMenuUpdateVersion(input) {
			t.Fatalf("validMenuUpdateVersion(%q) = true", input)
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
