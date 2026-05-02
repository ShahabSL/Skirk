package main

import (
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
	got := gcloudLoginArgs("", "")
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

func TestGcloudLoginArgsUseCustomOAuthClient(t *testing.T) {
	got := gcloudLoginArgs("client.json", "")
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"auth application-default login",
		"--no-browser",
		"--client-id-file client.json",
		"https://www.googleapis.com/auth/drive.file",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("gcloudLoginArgs missing %q in %#v", want, got)
		}
	}
}
