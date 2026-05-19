package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallCommandRequiresExplicitConfirmation(t *testing.T) {
	err := uninstallCommand(context.Background(), []string{"--service=false", "--binary=false"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("uninstallCommand error = %v, want --yes refusal", err)
	}
}

func TestUninstallCommandDryRunDoesNotRequireConfirmation(t *testing.T) {
	err := uninstallCommand(context.Background(), []string{"--dry-run", "--service=false", "--binary=false"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUninstallCommandDeletesBinaryAndGeneratedKit(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	kitDir := filepath.Join(dir, "skirk-kit")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(binDir, "skirk")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"exit.json", "client.json", "client.skirk"} {
		if err := os.WriteFile(filepath.Join(kitDir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := uninstallCommand(context.Background(), []string{
		"--yes",
		"--service=false",
		"--bin", binPath,
		"--delete-kit",
		"--kit", kitDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatalf("installed binary still exists or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(kitDir); !os.IsNotExist(err) {
		t.Fatalf("kit directory still exists or unexpected stat error: %v", err)
	}
}

func TestRemoveInstalledBinaryRejectsUnsafeBasename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-skirk")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := removeInstalledBinary(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "basename must be skirk") {
		t.Fatalf("removeInstalledBinary error = %v, want basename refusal", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unsafe path was removed: %v", err)
	}
}

func TestRemoveInstalledBinaryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skirk")
	if err := removeInstalledBinary(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteKitDirectoryRejectsCurrentWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatal(err)
		}
	}()
	for _, name := range []string{"exit.json", "client.json", "client.skirk"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err = deleteKitDirectory(filepath.Join(dir, "exit.json"))
	if err == nil || !strings.Contains(err.Error(), "current working directory") {
		t.Fatalf("deleteKitDirectory error = %v, want current directory refusal", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("working directory was removed: %v", err)
	}
}

func TestAssertSkirkWireproxyPathAcceptsManagedConfig(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "wgcf-profile.conf")
	if err := os.WriteFile(profile, []byte("[Interface]\nPrivateKey = test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wireproxy.conf"), []byte("WGConfig = "+profile+"\n\n[Socks5]\nBindAddress = 127.0.0.1:40000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := defaultWireproxyDir
	defaultWireproxyDir = dir
	defer func() { defaultWireproxyDir = oldDir }()

	if err := assertSkirkWireproxyPath(dir); err != nil {
		t.Fatalf("assertSkirkWireproxyPath rejected managed config: %v", err)
	}
}

func TestAssertSkirkWireproxyPathRejectsForeignConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wireproxy.conf"), []byte("WGConfig = /home/user/wgcf-profile.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := defaultWireproxyDir
	defaultWireproxyDir = dir
	defer func() { defaultWireproxyDir = oldDir }()

	if err := assertSkirkWireproxyPath(dir); err == nil {
		t.Fatal("assertSkirkWireproxyPath accepted foreign config")
	}
}

func TestVerifyWireproxyManifestRequiresManagedArtifacts(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	configDir := filepath.Join(dir, "wireproxy")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wireproxyPath := filepath.Join(binDir, "wireproxy")
	wgcfPath := filepath.Join(binDir, "wgcf")
	if err := os.WriteFile(wireproxyPath, []byte("wireproxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wgcfPath, []byte("wgcf"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(configDir, "wgcf-profile.conf")
	if err := os.WriteFile(profile, []byte("[Interface]\nPrivateKey = test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "wireproxy.conf"), []byte("WGConfig = "+profile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldDir, oldWireproxy, oldWGCF := defaultWireproxyDir, defaultWireproxyBin, defaultWGCFBin
	defaultWireproxyDir = configDir
	defaultWireproxyBin = wireproxyPath
	defaultWGCFBin = wgcfPath
	defer func() {
		defaultWireproxyDir = oldDir
		defaultWireproxyBin = oldWireproxy
		defaultWGCFBin = oldWGCF
	}()

	manifest := wireproxyManifest{
		ConfigDir:         configDir,
		WireproxyBin:      wireproxyPath,
		WireproxySHA256:   testSHA256(t, wireproxyPath),
		WGCFBin:           wgcfPath,
		WGCFSHA256:        testSHA256(t, wgcfPath),
		Service:           filepath.Join("/etc/systemd/system", defaultWireproxyService+".service"),
		HasManagedBySkirk: true,
	}
	if err := verifyWireproxyManifest(manifest); err != nil {
		t.Fatalf("verifyWireproxyManifest rejected managed manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "foreign.txt"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyWireproxyManifest(manifest); err == nil {
		t.Fatal("verifyWireproxyManifest accepted unexpected file in config dir")
	}
	if err := os.Remove(filepath.Join(configDir, "foreign.txt")); err != nil {
		t.Fatal(err)
	}
	manifest.WGCFSHA256 = strings.Repeat("0", 64)
	if err := verifyWireproxyManifest(manifest); err == nil {
		t.Fatal("verifyWireproxyManifest accepted checksum mismatch")
	}
}

func testSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
