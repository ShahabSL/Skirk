package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const testSlotName = "slotv1/session/client/run/generation/up"

func TestMetadataSlotNameAllowsConfiguredDotsButNotDotTraversal(t *testing.T) {
	if !validMetadataSlotName("slotv1/session/client.one/run.two/up") {
		t.Fatal("valid dotted client and run labels rejected")
	}
	for _, name := range []string{
		"slotv1/session/../run/up",
		"slotv1/session/client..one/run/up",
		"slotv1/session/client//up",
	} {
		if validMetadataSlotName(name) {
			t.Fatalf("accepted ambiguous slot name %q", name)
		}
	}
}

func slotResponse(id, name, description, version, parent, space string) string {
	value, _ := json.Marshal(map[string]any{
		"id": id, "name": name, "description": description, "version": version,
		"parents": []string{parent}, "spaces": []string{space},
		"mimeType": "application/octet-stream", "size": "0",
		"ownedByMe": true, "isAppAuthorized": true,
	})
	return string(value)
}

func slotTestStore(t *testing.T, cfg DriveConfig, transport roundTripFunc) *DriveStore {
	t.Helper()
	route := RouteConfig{Mode: "google_front_pinned"}
	client := &GoogleHTTPClient{client: &http.Client{Transport: transport}, route: route}
	return NewDriveStoreWithTokenSource(client, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, route), cfg)
}

func TestDriveMetadataSlotVisibleLifecycleAndQuota(t *testing.T) {
	var calls int
	description := ""
	version := "1"
	store := slotTestStore(t, DriveConfig{FolderID: "folder-1", MetadataSlotV1: true}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			t.Errorf("route = %s with Host %s", req.URL.Host, req.Host)
		}
		if req.URL.Query().Get("fields") != driveMetadataSlotFields {
			t.Errorf("fields = %q", req.URL.Query().Get("fields"))
		}
		switch req.Method {
		case http.MethodPost:
			var body struct {
				ID      string   `json:"id"`
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.ID != "generated-id" || body.Name != testSlotName || len(body.Parents) != 1 || body.Parents[0] != "folder-1" {
				t.Errorf("create body = %+v", body)
			}
		case http.MethodPatch:
			var body struct {
				Description string `json:"description"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			description = body.Description
			version = "2"
		case http.MethodGet:
			if !strings.Contains(req.URL.Path, "/generated-id") {
				t.Errorf("GET path = %s", req.URL.Path)
			}
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		return stringResponse(http.StatusOK, slotResponse("generated-id", testSlotName, description, version, "folder-1", "drive")), nil
	}))
	if _, err := store.PatchMetadataSlot(context.Background(), "generated-id", testSlotName, "c2VhbGVk"); err == nil || calls != 0 {
		t.Fatalf("PATCH before validation err=%v calls=%d", err, calls)
	}
	created, err := store.CreateMetadataSlot(context.Background(), "generated-id", testSlotName)
	if err != nil || created.ID != "generated-id" || created.Version != 1 {
		t.Fatalf("create = %+v, %v", created, err)
	}
	patched, err := store.PatchMetadataSlot(context.Background(), "generated-id", testSlotName, "c2VhbGVk")
	if err != nil || patched.Description != "c2VhbGVk" || patched.Version != 2 {
		t.Fatalf("patch = %+v, %v", patched, err)
	}
	read, err := store.GetMetadataSlot(context.Background(), "generated-id", testSlotName)
	if err != nil || read.Description != "c2VhbGVk" || read.Version != 2 {
		t.Fatalf("get = %+v, %v", read, err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	quota := store.QuotaSnapshot()
	if quota.Units != 105 || quota.Calls != 3 || quota.Ops["patch"].Units != 50 {
		t.Fatalf("quota = %+v", quota)
	}
}

func TestDriveMetadataSlotAppDataAndIdentityChecks(t *testing.T) {
	var createdParent string
	store := slotTestStore(t, DriveConfig{Space: "appDataFolder", MetadataSlotV1: true}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			var body struct {
				Parents []string `json:"parents"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if len(body.Parents) != 1 {
				t.Errorf("parents = %#v", body.Parents)
			} else {
				createdParent = body.Parents[0]
			}
		}
		return stringResponse(http.StatusOK, slotResponse("id-a", testSlotName, "", "1", "opaque-appdata-parent", "appDataFolder")), nil
	}))
	if _, err := store.CreateMetadataSlot(context.Background(), "id-a", testSlotName); err != nil {
		t.Fatal(err)
	}
	if createdParent != "appDataFolder" {
		t.Fatalf("appData parent = %q", createdParent)
	}
	if _, err := store.GetMetadataSlot(context.Background(), "id-a", testSlotName); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		name, payload string
	}{
		{"different id", slotResponse("id-b", testSlotName, "", "1", "folder-1", "drive")},
		{"different name", slotResponse("id-a", testSlotName+"x", "", "1", "folder-1", "drive")},
		{"different parent", slotResponse("id-a", testSlotName, "", "1", "folder-2", "drive")},
		{"different space", slotResponse("id-a", testSlotName, "", "1", "folder-1", "appDataFolder")},
		{"bad version", slotResponse("id-a", testSlotName, "", "x", "folder-1", "drive")},
		{"oversized description", slotResponse("id-a", testSlotName, strings.Repeat("a", driveMetadataSlotMaxDescription+1), "1", "folder-1", "drive")},
		{"malformed JSON", `{"id":`},
		{"malformed description", `{"id":"id-a","name":"` + testSlotName + `","description":12}`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			visible := NewDriveStore(nil, "token", DriveConfig{FolderID: "folder-1", MetadataSlotV1: true})
			if _, err := visible.parseMetadataSlot([]byte(tc.payload), "id-a", testSlotName); err == nil {
				t.Fatal("accepted invalid slot metadata")
			}
		})
	}
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"unowned", map[string]any{"ownedByMe": false}},
		{"other app", map[string]any{"isAppAuthorized": false}},
		{"media content", map[string]any{"size": "5"}},
		{"wrong mime", map[string]any{"mimeType": "text/plain"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(slotResponse("id-a", testSlotName, "", "1", "folder-1", "drive")), &payload); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.body {
				payload[k] = v
			}
			encoded, _ := json.Marshal(payload)
			visible := NewDriveStore(nil, "token", DriveConfig{FolderID: "folder-1", MetadataSlotV1: true})
			if _, err := visible.parseMetadataSlot(encoded, "id-a", testSlotName); err == nil {
				t.Fatal("accepted invalid slot metadata")
			}
		})
	}
}

func TestDriveMetadataSlotBoundsAnd404(t *testing.T) {
	var calls int
	store := slotTestStore(t, DriveConfig{FolderID: "folder-1", MetadataSlotV1: true}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method == http.MethodPost {
			return stringResponse(http.StatusOK, slotResponse("id-a", testSlotName, "", "1", "folder-1", "drive")), nil
		}
		if req.Method == http.MethodGet {
			return stringResponse(http.StatusNotFound, `{"error":{"message":"missing"}}`), nil
		}
		return stringResponse(http.StatusOK, slotResponse("id-a", testSlotName, "value", "2", "folder-1", "drive")), nil
	}))
	if _, err := store.CreateMetadataSlot(context.Background(), "id-a", testSlotName); err != nil {
		t.Fatal(err)
	}
	for _, description := range []string{strings.Repeat("a", driveMetadataSlotMaxDescription+1), "bad\x00value", string([]byte{0xff})} {
		if _, err := store.PatchMetadataSlot(context.Background(), "id-a", testSlotName, description); err == nil {
			t.Fatalf("accepted description %q", description[:min(len(description), 12)])
		}
	}
	if calls != 1 {
		t.Fatalf("invalid PATCH made network calls: %d", calls)
	}
	if _, err := store.GetMetadataSlot(context.Background(), "id-a", testSlotName); err == nil {
		t.Fatal("404 accepted")
	}
	if _, err := store.PatchMetadataSlot(context.Background(), "id-a", testSlotName, "valid"); err == nil || calls != 2 {
		t.Fatalf("PATCH after 404 err=%v calls=%d", err, calls)
	}
	for _, cfg := range []DriveConfig{{FolderID: "folder-1"}, {MetadataSlotV1: true}} {
		disabled := NewDriveStore(nil, "token", cfg)
		if disabled.MetadataSlotEnabled() {
			t.Fatalf("enabled with %+v", cfg)
		}
		if _, err := disabled.CreateMetadataSlot(context.Background(), "id", testSlotName); err == nil {
			t.Fatal("disabled slot created")
		}
	}
}

func TestDriveMetadataSlotPatchNeverRetriesAmbiguousTransport(t *testing.T) {
	var patches atomic.Int32
	store := slotTestStore(t, DriveConfig{FolderID: "folder-1", MetadataSlotV1: true}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return stringResponse(http.StatusOK, slotResponse("id-a", testSlotName, "", "1", "folder-1", "drive")), nil
		}
		if req.Method == http.MethodPatch {
			patches.Add(1)
			_, _ = io.Copy(io.Discard, req.Body)
			return nil, errors.New("ambiguous transport failure")
		}
		return stringResponse(http.StatusOK, slotResponse("id-a", testSlotName, "", "1", "folder-1", "drive")), nil
	}))
	if _, err := store.CreateMetadataSlot(context.Background(), "id-a", testSlotName); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PatchMetadataSlot(context.Background(), "id-a", testSlotName, "c2VhbGVk"); err == nil {
		t.Fatal("ambiguous transport error was accepted")
	}
	if patches.Load() != 1 {
		t.Fatalf("PATCH attempts = %d, want 1", patches.Load())
	}
}
