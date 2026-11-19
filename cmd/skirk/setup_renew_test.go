package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func writeTestOAuthRenewKit(t *testing.T, kitDir string, clientSecret string) (skirk.Config, skirk.Config, string) {
	t.Helper()
	auth := skirk.AuthConfig{
		ClientID:     "personal-client.apps.googleusercontent.com",
		ClientSecret: clientSecret,
		RefreshToken: "old-refresh-token",
		TokenURL:     "https://oauth2.googleapis.com/token",
	}
	exit := skirk.Config{
		Secret:    "original-secret",
		SessionID: "original-session",
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: "direct"},
		Drive:     skirk.DriveConfig{FolderID: "original-folder"},
		Tunnel:    skirk.TunnelConfig{Profile: "auto", ChunkSize: 4096, PollIntervalMS: 1000, Concurrency: 8},
	}
	client := exit
	client.Route = skirk.RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120"}
	client.Tunnel.Listen = "127.0.0.1:18080"
	if err := writeJSONFile(filepath.Join(kitDir, "exit.json"), exit); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(kitDir, "client.json"), client); err != nil {
		t.Fatal(err)
	}
	oldText, err := skirk.EncodeConfigText(&client)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(kitDir, "client.skirk"), oldText+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(kitDir, "client-command.txt"), "skirk serve-client --config '"+oldText+"' --listen 127.0.0.1:18080\n"); err != nil {
		t.Fatal(err)
	}
	return exit, client, oldText
}

func TestRenewOAuthUpdatesKitCredentialsAndPreservesIdentity(t *testing.T) {
	kitDir := t.TempDir()
	oldExit, oldClient, oldText := writeTestOAuthRenewKit(t, kitDir, "original-client-secret")
	kit, err := loadOAuthRenewKit(kitDir, filepath.Join(kitDir, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	newAuth := oldExit.Auth
	newAuth.RefreshToken = "renewed-refresh-token"
	files, err := kit.updatedFiles(newAuth)
	if err != nil {
		t.Fatal(err)
	}
	backupDir, err := replaceOAuthRenewFiles(kitDir, files)
	if err != nil {
		t.Fatal(err)
	}
	backupInfo, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && backupInfo.Mode().Perm() != 0700 {
		t.Fatalf("backup directory mode = %o; want 0700", backupInfo.Mode().Perm())
	}
	for _, tc := range []struct {
		name string
		old  skirk.Config
	}{
		{name: "exit.json", old: oldExit},
		{name: "client.json", old: oldClient},
	} {
		path := filepath.Join(kitDir, tc.name)
		updated, err := skirk.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Auth.RefreshToken != newAuth.RefreshToken || updated.Auth.ClientID != tc.old.Auth.ClientID {
			t.Fatalf("%s OAuth credentials were not renewed", tc.name)
		}
		if updated.Secret != tc.old.Secret || updated.SessionID != tc.old.SessionID || updated.Drive != tc.old.Drive || updated.Route.Mode != tc.old.Route.Mode {
			t.Fatalf("%s changed a non-OAuth identity or route", tc.name)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %o; want 0600", tc.name, info.Mode().Perm())
		}
	}
	textBytes, err := os.ReadFile(filepath.Join(kitDir, "client.skirk"))
	if err != nil {
		t.Fatal(err)
	}
	newText := strings.TrimSpace(string(textBytes))
	if newText == oldText {
		t.Fatal("client.skirk still contains the old OAuth credentials")
	}
	textConfig, err := skirk.LoadConfig(filepath.Join(kitDir, "client.skirk"))
	if err != nil {
		t.Fatal(err)
	}
	if textConfig.Auth.RefreshToken != newAuth.RefreshToken || textConfig.Secret != oldClient.Secret || textConfig.Drive != oldClient.Drive {
		t.Fatal("renewed client.skirk lost the original identity or new OAuth credentials")
	}
	command, err := os.ReadFile(filepath.Join(kitDir, "client-command.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(command) != generatedClientCommand {
		t.Fatal("renewal did not migrate the embedded profile to a file argument")
	}
	for i, file := range files {
		backupPath := filepath.Join(backupDir, fmt.Sprintf("%02d-%s", i, filepath.Base(file.path)))
		backup, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(backup) != string(file.original) {
			t.Fatalf("backup of %s differs from original", file.path)
		}
		backupInfo, err := os.Stat(backupPath)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && backupInfo.Mode().Perm() != 0600 {
			t.Fatalf("backup %s mode = %o; want 0600", backupPath, backupInfo.Mode().Perm())
		}
	}
}

func TestRenewOAuthPreservesExportedProfileSettings(t *testing.T) {
	kitDir := t.TempDir()
	oldExit, client, oldText := writeTestOAuthRenewKit(t, kitDir, "original-client-secret")
	client.Route.GoogleIP = "216.239.38.121"
	client.Tunnel.PollIntervalMS = 1500
	client.Client.ID = "exported-device"
	customText, err := skirk.EncodeConfigText(&client)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "client.skirk"), []byte(customText+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := "skirk serve-client --config '" + customText + "' --listen 127.0.0.1:18080\n"
	if err := os.WriteFile(filepath.Join(kitDir, "client-command.txt"), []byte(command), 0600); err != nil {
		t.Fatal(err)
	}
	kit, err := loadOAuthRenewKit(kitDir, filepath.Join(kitDir, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	newAuth := oldExit.Auth
	newAuth.RefreshToken = "renewed-refresh-token"
	files, err := kit.updatedFiles(newAuth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replaceOAuthRenewFiles(kitDir, files); err != nil {
		t.Fatal(err)
	}
	got, err := skirk.LoadConfig(filepath.Join(kitDir, "client.skirk"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Auth.RefreshToken != newAuth.RefreshToken || got.Route.GoogleIP != client.Route.GoogleIP || got.Tunnel.PollIntervalMS != client.Tunnel.PollIntervalMS || got.Client.ID != client.Client.ID {
		t.Fatalf("renewal lost exported profile settings: got route=%q poll=%d client=%q", got.Route.GoogleIP, got.Tunnel.PollIntervalMS, got.Client.ID)
	}
	if gotText, err := os.ReadFile(filepath.Join(kitDir, "client.skirk")); err != nil || strings.TrimSpace(string(gotText)) == oldText {
		t.Fatalf("renewal did not update the exported profile: %v", err)
	}
}

func TestRenewOAuthRejectsDifferentOAuthClient(t *testing.T) {
	old := skirk.AuthConfig{ClientID: "old-id", ClientSecret: "old-secret", RefreshToken: "old-token"}
	path := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(path, []byte(`{"installed":{"client_id":"different-id","client_secret":"new-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveRenewOAuthClient(old, path)
	if err == nil || !strings.Contains(err.Error(), "different Google OAuth client") {
		t.Fatalf("expected clear OAuth client mismatch, got %v", err)
	}
}

func TestRenewOAuthRejectsMismatchedClientKitBeforeLogin(t *testing.T) {
	kitDir := t.TempDir()
	_, client, _ := writeTestOAuthRenewKit(t, kitDir, "secret")
	client.Drive.FolderID = "some-other-folder"
	if err := writeJSONFile(filepath.Join(kitDir, "client.json"), client); err != nil {
		t.Fatal(err)
	}
	_, err := loadOAuthRenewKit(kitDir, filepath.Join(kitDir, "exit.json"))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected mismatched kit rejection, got %v", err)
	}
}

func TestRenewOAuthAllowsClientTransportSettings(t *testing.T) {
	kitDir := t.TempDir()
	exit, client, _ := writeTestOAuthRenewKit(t, kitDir, "secret")
	client.Drive.NormalListPageSize = 500
	client.Drive.SeparateBulkMediaPool = true
	client.Drive.QuotaProject = "client-quota-project"
	if err := writeJSONFile(filepath.Join(kitDir, "client.json"), client); err != nil {
		t.Fatal(err)
	}
	// The exported profile may have its own transport settings as well.
	client.Drive.MetadataSlotV1 = true
	text, err := skirk.EncodeConfigText(&client)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(kitDir, "client.skirk"), text+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(kitDir, "client-command.txt"), "skirk serve-client --config '"+text+"' --listen 127.0.0.1:18080\n"); err != nil {
		t.Fatal(err)
	}
	kit, err := loadOAuthRenewKit(kitDir, filepath.Join(kitDir, "exit.json"))
	if err != nil {
		t.Fatalf("same mailbox with client transport settings should renew: %v", err)
	}
	newAuth := exit.Auth
	newAuth.RefreshToken = "renewed-refresh-token"
	files, err := kit.updatedFiles(newAuth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replaceOAuthRenewFiles(kitDir, files); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"client.json", "client.skirk"} {
		got, err := skirk.LoadConfig(filepath.Join(kitDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got.Auth.RefreshToken != newAuth.RefreshToken || got.Drive.NormalListPageSize != 500 ||
			!got.Drive.SeparateBulkMediaPool || got.Drive.QuotaProject != "client-quota-project" {
			t.Fatalf("%s lost renewed OAuth or client Drive settings", name)
		}
		if name == "client.skirk" && !got.Drive.MetadataSlotV1 {
			t.Fatal("exported profile lost its slot setting")
		}
	}
}

func TestRenewOAuthRejectsDifferentDriveSpace(t *testing.T) {
	kitDir := t.TempDir()
	_, client, _ := writeTestOAuthRenewKit(t, kitDir, "secret")
	client.Drive.Space = "appDataFolder"
	if err := writeJSONFile(filepath.Join(kitDir, "client.json"), client); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOAuthRenewKit(kitDir, filepath.Join(kitDir, "exit.json")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected Drive space mismatch rejection, got %v", err)
	}
}

func TestReplaceAuthJSONPreservesUnknownFields(t *testing.T) {
	original := []byte(`{"secret":"secret","session_id":"session","auth":{"client_id":"old","refresh_token":"old"},"future_setting":{"enabled":true}}`)
	updated, err := replaceAuthJSON(original, skirk.AuthConfig{ClientID: "old", RefreshToken: "new"})
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updated, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "auth")
	delete(after, "auth")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("renewal changed a non-auth JSON field")
	}
}

func TestRenewOAuthDoesNotTouchKitWhenStagingFails(t *testing.T) {
	kitDir := t.TempDir()
	exitPath := filepath.Join(kitDir, "exit.json")
	if err := os.WriteFile(exitPath, []byte("old-exit"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := replaceOAuthRenewFiles(kitDir, []kitFileUpdate{
		{path: exitPath, original: []byte("old-exit"), replacement: []byte("new-exit")},
		{path: filepath.Join(kitDir, "missing-directory", "client.json"), replacement: []byte("new-client")},
	})
	if err == nil {
		t.Fatal("expected staging failure")
	}
	got, err := os.ReadFile(exitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-exit" {
		t.Fatalf("exit config changed after staging failure: %q", got)
	}
}
