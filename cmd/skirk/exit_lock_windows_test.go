package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsExitLockAdmitsOneWriter(t *testing.T) {
	t.Setenv("TMP", t.TempDir())
	session := [16]byte{1}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("skirk-exit-%x.lock", session))
	// An older exit creates and retains an ordinary file write handle.
	old, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if lock, err := acquireExitLock(session); err == nil {
		lock.Close()
		t.Fatal("second exit acquired the session while an older exit was running")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireExitLock(session)
	if err != nil {
		t.Fatal("could not reuse the existing marker:", err)
	}
	defer lock.Close()
	if second, err := acquireExitLock(session); err == nil {
		second.Close()
		t.Fatal("two exits acquired the same session")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("unexpected lock conflict error: %v", err)
	}
	independent, err := acquireExitLock([16]byte{2})
	if err != nil {
		t.Fatal("an independent session was blocked:", err)
	}
	independent.Close()
}

func TestWindowsExitLockSurvivesProcessExit(t *testing.T) {
	const child = "SKIRK_TEST_WINDOWS_EXIT_LOCK_CHILD"
	session := [16]byte{3}
	if os.Getenv(child) == "1" {
		lock, err := acquireExitLock(session)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		os.Exit(37)
	}
	t.Setenv("TMP", t.TempDir())
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsExitLockSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), child+"=1")
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 37 {
		t.Fatalf("test child did not exit with an open lock: %v: %s", err, output)
	}
	lock, err := acquireExitLock(session)
	if err != nil {
		t.Fatal("the exited process still blocks the session:", err)
	}
	defer lock.Close()
}
