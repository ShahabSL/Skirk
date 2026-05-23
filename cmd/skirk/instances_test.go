package main

import (
	"os"
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

func TestDiscoverExitInstancesFindsInstalledDefaultKitOutsideCWD(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Chdir(filepath.Join(tmp))
	installedKit := filepath.Join(tmp, "opt", "skirk-kit")
	if err := os.MkdirAll(installedKit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedKit, "exit.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	withDefaultKitCandidates(t, []string{"skirk-kit", installedKit})

	instances, err := discoverExitInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances len = %d, want 1", len(instances))
	}
	if instances[0].ID != "default" || instances[0].ConfigPath != filepath.Join(installedKit, "exit.json") {
		t.Fatalf("unexpected default instance: %+v", instances[0])
	}
}

func TestDefaultKitFilePrefersCWDDefaultKit(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	cwdKit := filepath.Join(tmp, "skirk-kit")
	installedKit := filepath.Join(tmp, "opt", "skirk-kit")
	for _, dir := range []string{cwdKit, installedKit} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "exit.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withDefaultKitCandidates(t, []string{"skirk-kit", installedKit})

	if got, want := defaultKitFile("client.skirk"), filepath.Join(cwdKit, "client.skirk"); got != want {
		t.Fatalf("defaultKitFile = %q, want %q", got, want)
	}
}

func TestDefaultKitFileFallsBackWhenNoKitExists(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	withDefaultKitCandidates(t, []string{"skirk-kit", filepath.Join(tmp, "opt", "skirk-kit")})

	if got, want := defaultKitFile("exit.json"), filepath.Join("skirk-kit", "exit.json"); got != want {
		t.Fatalf("defaultKitFile = %q, want %q", got, want)
	}
}

func withDefaultKitCandidates(t *testing.T, candidates []string) {
	t.Helper()
	original := defaultKitDirCandidates
	defaultKitDirCandidates = func() []string {
		return candidates
	}
	t.Cleanup(func() {
		defaultKitDirCandidates = original
	})
}
