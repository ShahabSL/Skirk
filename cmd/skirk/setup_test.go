package main

import (
	"os"
	"path/filepath"
	"skirk/internal/skirk"
	"strings"
	"testing"
)

func TestGcloudArchiveName(t *testing.T) {
	cases := []struct {
		goos string
		arch string
		want string
	}{
		{goos: "linux", arch: "amd64", want: "google-cloud-cli-linux-x86_64.tar.gz"},
		{goos: "linux", arch: "arm64", want: "google-cloud-cli-linux-arm.tar.gz"},
		{goos: "linux", arch: "386", want: "google-cloud-cli-linux-x86.tar.gz"},
	}
	for _, tc := range cases {
		got, err := gcloudArchiveName(tc.goos, tc.arch)
		if err != nil {
			t.Fatalf("gcloudArchiveName(%q, %q): %v", tc.goos, tc.arch, err)
		}
		if got != tc.want {
			t.Fatalf("gcloudArchiveName(%q, %q) = %q, want %q", tc.goos, tc.arch, got, tc.want)
		}
	}
}

func TestGcloudArchiveNameRejectsUnsupportedOS(t *testing.T) {
	if _, err := gcloudArchiveName("windows", "amd64"); err == nil {
		t.Fatal("expected unsupported OS error")
	}
}

func TestGcloudLoginArgsUseBuiltInDriveLoginByDefault(t *testing.T) {
	got := gcloudLoginArgs()
	want := []string{
		"auth", "login",
		"--no-launch-browser",
		"--enable-gdrive-access",
		"--update-adc",
		"--force",
	}
	if len(got) != len(want) {
		t.Fatalf("len(gcloudLoginArgs) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gcloudLoginArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGcloudBrokenIPv6ErrorExplainsRecovery(t *testing.T) {
	got := gcloudBrokenIPv6Error("dial tcp6 timeout", os.ErrPermission).Error()
	for _, want := range []string{
		"broken IPv6",
		"/etc/gai.conf",
		"oauth-client.json",
		"dial tcp6 timeout",
		"permission denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q in:\n%s", want, got)
		}
	}
}

func TestGaiConfDataPrefersIPv4(t *testing.T) {
	data := []byte(`#precedence ::ffff:0:0/96 100
precedence  ::1/128       50
precedence ::ffff:0:0/96 100
`)
	if !gaiConfDataPrefersIPv4(data) {
		t.Fatal("expected active IPv4 precedence line to be detected")
	}
	if gaiConfDataPrefersIPv4([]byte("# precedence ::ffff:0:0/96 100\n")) {
		t.Fatal("commented IPv4 precedence line should not count")
	}
}

func TestAppendGaiIPv4Preference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gai.conf")
	if err := os.WriteFile(path, []byte("# defaults\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := appendGaiIPv4Preference(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !gaiConfDataPrefersIPv4(data) {
		t.Fatalf("expected IPv4 preference after append:\n%s", string(data))
	}
}

func TestNormalizeOAuthScopes(t *testing.T) {
	got := normalizeOAuthScopes("openid,email https://www.googleapis.com/auth/drive.appdata openid")
	for _, want := range []string{"openid", "email", "https://www.googleapis.com/auth/drive.appdata"} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalizeOAuthScopes missing %q in %q", want, got)
		}
	}
	if strings.Count(got, "openid") != 1 {
		t.Fatalf("normalizeOAuthScopes did not deduplicate: %q", got)
	}
}

func TestApplyTunnelOverridesConcurrencyDoesNotSetAutoProfileSplitCaps(t *testing.T) {
	cfg := &skirk.Config{
		Secret: "test-secret",
		Auth:   skirk.AuthConfig{AccessToken: "token"},
		Route:  skirk.RouteConfig{Mode: "direct"},
		Drive:  skirk.DriveConfig{Space: "appDataFolder"},
		Tunnel: skirk.TunnelConfig{Profile: "auto", ChunkSize: 16 * 1024 * 1024, PollIntervalMS: 100, BurstPollMS: 75, BurstPollWindowMS: 5000, Concurrency: 8},
	}
	if err := applyTunnelOverrides(cfg, 0, 0, 64, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Tunnel.Concurrency, 64; got != want {
		t.Fatalf("concurrency = %d, want %d", got, want)
	}
	if cfg.Tunnel.UploadConcurrency != 0 || cfg.Tunnel.DownloadConcurrency != 0 {
		t.Fatalf("split caps = upload %d download %d, want zero auto caps", cfg.Tunnel.UploadConcurrency, cfg.Tunnel.DownloadConcurrency)
	}
}

func TestApplyTunnelOverridesSplitCapsRemainExplicit(t *testing.T) {
	cfg := &skirk.Config{
		Secret: "test-secret",
		Auth:   skirk.AuthConfig{AccessToken: "token"},
		Route:  skirk.RouteConfig{Mode: "direct"},
		Drive:  skirk.DriveConfig{Space: "appDataFolder"},
		Tunnel: skirk.TunnelConfig{Profile: "auto", ChunkSize: 16 * 1024 * 1024, PollIntervalMS: 100, BurstPollMS: 75, BurstPollWindowMS: 5000, Concurrency: 8},
	}
	if err := applyTunnelOverrides(cfg, 0, 0, 0, 12, 48); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Tunnel.UploadConcurrency, 12; got != want {
		t.Fatalf("upload cap = %d, want %d", got, want)
	}
	if got, want := cfg.Tunnel.DownloadConcurrency, 48; got != want {
		t.Fatalf("download cap = %d, want %d", got, want)
	}
}

func TestWriteSetupReadmeDocumentsCurrentCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	err := writeSetupReadme(path, setupSummary{
		Title:             "test-kit",
		ADCPath:           "/tmp/adc.json",
		Account:           "user@example.com",
		ClientPath:        "skirk-kit/client.json",
		ClientTextPath:    "skirk-kit/client.skirk",
		ClientCommandPath: "skirk-kit/client-command.txt",
		ExitPath:          "skirk-kit/exit.json",
		DriveFolderID:     "appDataFolder",
		Transport:         "drive_appdata",
		ClientRoute:       "google_front",
		ExitRoute:         "direct",
		Listen:            "127.0.0.1:18080",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"skirk serve-exit --config skirk-kit/exit.json",
		"skirk serve-client --config skirk-kit/client.json --listen 127.0.0.1:18080",
		"skirk cleanup --config skirk-kit/exit.json --older-than 2h",
		"skirk revoke --config skirk-kit/exit.json --revoke-oauth",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated README missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "%!") {
		t.Fatalf("generated README has fmt mismatch:\n%s", text)
	}
}
