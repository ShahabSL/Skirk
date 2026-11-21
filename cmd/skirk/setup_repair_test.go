package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestRepairMailboxPreservesPeerAndExportSettings(t *testing.T) {
	dir := t.TempDir()
	exit, client, _ := writeTestOAuthRenewKit(t, dir, "test-secret")
	exit.Drive = skirk.DriveConfig{
		FolderID: "old-exit", QuotaProject: "exit-quota", AdaptiveMuxDiscovery: true,
		NormalListPageSize: 100, SeparateUploadPool: true,
	}
	client.Drive = skirk.DriveConfig{
		Space: "appDataFolder", QuotaProject: "client-quota", CombinedMuxDiscovery: true,
		NormalListPageSize: 500, UploadHedgeDelayMS: 2000,
	}
	exported := client
	exported.Client.ID = "custom-export"
	exported.Drive = skirk.DriveConfig{FolderID: "old-export", QuotaProject: "export-quota", MetadataOnlyPriority: true}
	exitPath := filepath.Join(dir, "custom-exit.json")
	if err := writeJSONFile(exitPath, exit); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(dir, "client.json"), client); err != nil {
		t.Fatal(err)
	}
	text, err := skirk.EncodeConfigText(&exported)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(dir, "client.skirk"), text+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(dir, "client-command.txt"), generatedClientCommand); err != nil {
		t.Fatal(err)
	}
	kit, err := loadConfigKit(dir, exitPath, checkRepairClientIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.writeRepairedMailbox("new-visible-folder"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]skirk.Config{"custom-exit.json": exit, "client.json": client, "client.skirk": exported} {
		want.ApplyDefaults()
		want.Drive.FolderID, want.Drive.Space = "new-visible-folder", ""
		got, err := skirk.LoadConfig(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, &want) {
			t.Errorf("%s changed outside the mailbox location or still uses the old mailbox", name)
		}
	}
}

func TestRepairMailboxStandaloneExitKeepsUnknownSettings(t *testing.T) {
	dir := t.TempDir()
	exitPath := filepath.Join(dir, "standalone.json")
	data := []byte(`{"secret":"fixture","custom":true,"Drive":{"Space":"appDataFolder","FOLDER_ID":"old","future_setting":9007199254740993}}`)
	if err := os.WriteFile(exitPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	kit, err := loadConfigKit(filepath.Join(dir, "absent-kit"), exitPath, checkRepairClientIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.writeRepairedMailbox("new-visible-folder"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(exitPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated struct {
		Custom bool
		Drive  map[string]json.RawMessage
	}
	if err := json.Unmarshal(content, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Custom || len(updated.Drive) != 2 || string(updated.Drive["future_setting"]) != "9007199254740993" || string(updated.Drive["folder_id"]) != `"new-visible-folder"` {
		t.Fatal("standalone repair lost unknown settings or retained an old location")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected files after standalone repair: %v, %v", entries, err)
	}
}

func TestRepairMailboxAcceptsProfileInConfigFile(t *testing.T) {
	dir := t.TempDir()
	_, _, profile := writeTestOAuthRenewKit(t, dir, "test-secret")
	clientPath := filepath.Join(dir, "client.json")
	if err := writeTextFile(clientPath, profile); err != nil {
		t.Fatal(err)
	}
	kit, err := loadConfigKit(dir, filepath.Join(dir, "exit.json"), checkRepairClientIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.writeRepairedMailbox("replacement"); err != nil {
		t.Fatal(err)
	}
	got, err := skirk.LoadConfig(clientPath)
	if err != nil || got.Drive.FolderID != "replacement" || got.Route.Mode != "google_front_pinned" {
		t.Fatalf("repairing a config containing a pasted profile: %v", err)
	}
}
