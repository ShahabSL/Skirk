package skirk

import (
	"strings"
	"testing"
)

func TestDriveStoreAppDataQuery(t *testing.T) {
	store := NewDriveStore(nil, "token", DriveConfig{Space: "appDataFolder"})
	if !store.isAppData() {
		t.Fatal("expected appDataFolder mode")
	}
	query := store.query("control/session/", false)
	if strings.Contains(query, "in parents") {
		t.Fatalf("appDataFolder query should not include a visible folder parent: %s", query)
	}
	if !strings.Contains(query, "name contains 'control/session/'") {
		t.Fatalf("query did not include name prefix: %s", query)
	}
}

func TestDriveStoreLegacyAppDataFolderID(t *testing.T) {
	store := NewDriveStore(nil, "token", DriveConfig{FolderID: "appDataFolder"})
	if !store.isAppData() {
		t.Fatal("expected legacy appDataFolder folder_id to enable appDataFolder mode")
	}
}
