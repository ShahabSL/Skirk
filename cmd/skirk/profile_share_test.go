package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestShowClientProfile(t *testing.T) {
	cfg := &skirk.Config{Secret: "private-test-value", Drive: skirk.DriveConfig{FolderID: "fixture"}}
	profile, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "client.skirk")
	if err := os.WriteFile(path, []byte(profile+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readShareableProfile(path); err != nil || got != profile {
		t.Fatal("saved client profile did not round-trip")
	}
	for _, tc := range []struct {
		name    string
		columns int
		rows    int
		want    string
	}{
		{name: "room for QR", columns: 180, rows: 100, want: "\x1b[30;107m"},
		{name: "narrow terminal", columns: 20, rows: 100, want: "Zoom out or resize"},
		{name: "short terminal", columns: 180, rows: 10, want: "Zoom out or resize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := printClientProfile(&output, profile, tc.columns, tc.rows); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "\n"+profile+"\n") || !strings.Contains(output.String(), tc.want) {
				t.Fatal("display did not keep the full profile alongside the QR or its fallback")
			}
		})
	}
	imagePath := filepath.Join(dir, "profile.png")
	if err := showProfileCommand([]string{"--config", path, "--qr-out", imagePath}); err != nil {
		t.Fatal(err)
	}
	imageFile, err := os.Open(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer imageFile.Close()
	info, err := imageFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("QR PNG mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := png.Decode(imageFile); err != nil {
		t.Fatal("QR export was not a PNG")
	}
}

func TestShowClientProfileRejectsNonProfiles(t *testing.T) {
	for _, contents := range []string{`{"secret":"private-test-value"}`, skirk.ConfigTextPrefix + "broken"} {
		path := filepath.Join(t.TempDir(), "client.skirk")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readShareableProfile(path); err == nil || strings.Contains(err.Error(), contents) {
			t.Fatal("invalid client profile was accepted or included in the error")
		}
	}
}
