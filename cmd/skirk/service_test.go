package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	for _, input := range []string{"", ".service", "--help", "../bad", "bad/name", "bad name"} {
		if got, err := normalizeSystemdServiceName(input); err == nil {
			t.Fatalf("normalizeSystemdServiceName(%q) = %q, want error", input, got)
		}
	}
}

func TestIsSkirkExitSystemdUnitText(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		want bool
	}{
		{"generated", systemdUnitText("/opt/Skirk build/skirk-rc", "/etc/skirk/exit.json", "root"), true},
		{"legacy", "[Service]\nExecStart = /usr/local/bin/skirk serve-exit --config /etc/skirk/exit.json\n", true},
		{"wireproxy", "[Unit]\n# Managed by Skirk\n[Service]\nExecStart=/usr/local/bin/wireproxy -c /etc/wireproxy/wireproxy.conf\n", false},
		{"foreign program", "[Service]\nExecStart=/usr/bin/worker serve-exit --config /etc/worker.json\n", false},
		{"commented command", "[Unit]\n# Managed by Skirk\n[Service]\n#ExecStart=/usr/bin/skirk serve-exit --config /etc/exit.json\nExecStart=/usr/bin/worker\n", false},
		{"conflicting commands", "[Service]\nExecStart=/usr/bin/skirk serve-exit --config /etc/exit.json\nExecStart=/usr/bin/worker\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSkirkExitSystemdUnitText(tc.unit); got != tc.want {
				t.Fatalf("isSkirkExitSystemdUnitText(%q) = %v, want %v", tc.unit, got, tc.want)
			}
		})
	}
}

func TestCheckSystemdExitUnit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake systemctl requires a shell")
	}
	commands := t.TempDir()
	ctl := `#!/bin/sh
test "$*" = "show --all --property=LoadState,ActiveState,FragmentPath -- skirk-test.service" || exit 93
printf '%s' "$TEST_SYSTEMCTL_UNIT_STATE"
`
	if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte(ctl), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name      string
		file      string
		load      string
		active    string
		fragment  string
		wantOwned bool
		wantError bool
	}{
		{"free name", "", "not-found", "inactive", "", false, false},
		{"owned active unit", "managed", "loaded", "active", "local", true, false},
		{"foreign local unit", "foreign", "loaded", "active", "local", false, true},
		{"alias to owned file", "symlink", "loaded", "active", "foreign", false, true},
		{"vendor unit with no local override", "", "loaded", "inactive", "foreign", false, true},
		{"managed file shadowed by a different fragment", "managed", "loaded", "active", "foreign", false, true},
		{"masked unit", "", "masked", "inactive", "/dev/null", false, true},
		{"active unit lost its definition", "", "not-found", "active", "", false, true},
		{"manager output missing", "", "", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unitPath := filepath.Join(t.TempDir(), "skirk-test.service")
			switch tc.file {
			case "managed", "foreign":
				contents := "[Service]\nExecStart=/usr/sbin/unrelated\n"
				if tc.file == "managed" {
					contents = systemdUnitText("/usr/local/bin/skirk", "/etc/skirk/exit.json", "root")
				}
				if err := os.WriteFile(unitPath, []byte(contents), 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "skirk-other.service")
				contents := systemdUnitText("/usr/local/bin/skirk", "/etc/skirk/exit.json", "root")
				if err := os.WriteFile(target, []byte(contents), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, unitPath); err != nil {
					t.Fatal(err)
				}
			}
			fragment := tc.fragment
			switch fragment {
			case "local":
				fragment = unitPath
			case "foreign":
				fragment = "/usr/lib/systemd/system/skirk-test.service"
			}
			t.Setenv("TEST_SYSTEMCTL_UNIT_STATE", "LoadState="+tc.load+"\nFragmentPath="+fragment+"\nActiveState="+tc.active+"\n")
			owned, err := checkSystemdExitUnit(context.Background(), unitPath)
			if owned != tc.wantOwned || (err != nil) != tc.wantError {
				t.Fatalf("checkSystemdExitUnit() = %v, %v; want owned=%v, error=%v", owned, err, tc.wantOwned, tc.wantError)
			}
		})
	}
}

func TestSystemdUnitText(t *testing.T) {
	unit := systemdUnitText("/usr/local/bin/skirk", "/opt/skirk-kit/exit.json", "root")
	for _, want := range []string{
		"Description=Skirk exit",
		"# Managed by Skirk",
		"User=root",
		"WorkingDirectory=/",
		"ExecStart=\"/usr/local/bin/skirk\" serve-exit --config \"/opt/skirk-kit/exit.json\"",
		"Restart=always",
		"RestartPreventExitStatus=78",
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
		"WorkingDirectory=/",
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

func TestValidateServicePathsForUserRejectsRootPrivatePathsForNonRootUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd /root path policy is Unix-specific")
	}
	if err := validateServicePathsForUser("/root/.local/bin/skirk", "/home/ubuntu/skirk-kit/exit.json", "ubuntu"); err == nil {
		t.Fatal("validateServicePathsForUser accepted /root binary for non-root service user")
	}
	if err := validateServicePathsForUser("/usr/local/bin/skirk", "/root/skirk-kit/exit.json", "ubuntu"); err == nil {
		t.Fatal("validateServicePathsForUser accepted /root config for non-root service user")
	}
	if err := validateServicePathsForUser("/root/.local/bin/skirk", "/root/skirk-kit/exit.json", "root"); err != nil {
		t.Fatalf("validateServicePathsForUser rejected root service user: %v", err)
	}
}

func TestVerifyServiceFileAccessUsesServiceIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd uses POSIX file permissions")
	}
	ctx := t.Context()
	user, err := currentUsername(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe, config := filepath.Join(dir, "test program"), filepath.Join(dir, "secret kit.json")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyServiceFileAccess(ctx, exe, config, user); err != nil {
		t.Fatal(err)
	}
	if err := verifyServiceFileAccess(ctx, exe, filepath.Join(dir, "missing.json"), user); err == nil || !strings.Contains(err.Error(), "read the exit config") {
		t.Fatalf("missing config access result = %v", err)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		if _, err := exec.LookPath("runuser"); err != nil {
			t.Skip("cross-account preflight needs runuser")
		}
		if err := verifyServiceFileAccess(ctx, "/bin/sh", config, "nobody"); err == nil || !strings.Contains(err.Error(), "read the exit config") {
			t.Fatalf("another user's private kit access result = %v", err)
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
