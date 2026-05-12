package main

import (
	"os"
	"path/filepath"
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
