package main

import (
	"context"
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
