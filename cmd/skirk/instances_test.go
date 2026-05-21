package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultInstanceID(t *testing.T) {
	got := defaultInstanceID("Work Account / Tehran")
	if got != "work-account-tehran" {
		t.Fatalf("defaultInstanceID = %q, want %q", got, "work-account-tehran")
	}
	if err := validateInstanceID(got); err != nil {
		t.Fatalf("generated ID failed validation: %v", err)
	}
}

func TestDiscoverExitInstances(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Chdir(tmp)

	root, err := skirkInstancesRoot()
	if err != nil {
		t.Fatal(err)
	}
	instance := exitInstance{
		ID:          "work",
		Title:       "Work",
		KitDir:      filepath.Join(root, "work"),
		ConfigPath:  filepath.Join(root, "work", "exit.json"),
		ServiceName: "skirk-exit-work",
	}
	if err := saveExitInstance(instance); err != nil {
		t.Fatal(err)
	}
	instances, err := discoverExitInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances len = %d, want 1", len(instances))
	}
	if instances[0].ID != "work" || instances[0].ServiceName != "skirk-exit-work" {
		t.Fatalf("unexpected instance: %+v", instances[0])
	}
}
