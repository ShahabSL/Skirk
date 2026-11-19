package skirk

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testMuxDriveV2JournalDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the durable intent journal fails closed on Windows")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testMuxDriveV2Intent(t *testing.T, direction byte) muxDriveV2OwnedIntent {
	t.Helper()
	intent, err := newMuxDriveV2OwnedIntent(testMuxDriveV2Offer(), direction,
		"owned-folder-id-0123456789", "drive", "owner-permission-12345")
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func TestMuxDriveV2OwnedIntentBeforeCreateAndReadOnlyRecovery(t *testing.T) {
	dir := testMuxDriveV2JournalDir(t)
	up := testMuxDriveV2Intent(t, DirectionUp)
	down := testMuxDriveV2Intent(t, DirectionDown)
	for _, intent := range []muxDriveV2OwnedIntent{up, down} {
		if err := writeMuxDriveV2OwnedIntent(dir, intent); err != nil {
			t.Fatal("durable intent required before remote create", err)
		}
	}
	before, err := os.ReadDir(dir)
	if err != nil || len(before) != 2 {
		t.Fatal("intent files missing", err)
	}
	intents, err := recoverMuxDriveV2OwnedIntents(dir)
	if err != nil || len(intents) != 2 || intents[0].fileID() == intents[1].fileID() {
		t.Fatal("recovery lost exact owned targets", intents, err)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(after) != len(before) {
		t.Fatal("read-only recovery mutated the journal", err)
	}
	path := filepath.Join(dir, up.filename())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMuxDriveV2OwnedIntent(dir, up); !errors.Is(err, os.ErrExist) {
		t.Fatal("O_EXCL collision was not rejected", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("collision changed first durable intent", err)
	}
}

func TestMuxDriveV2OwnedIntentSyncFailureLeavesRecoverableIntent(t *testing.T) {
	dir := testMuxDriveV2JournalDir(t)
	intent := testMuxDriveV2Intent(t, DirectionUp)
	syncFailed := errors.New("simulated directory sync failure")
	called := false
	err := writeMuxDriveV2OwnedIntentWithSync(dir, intent, func(path string) error {
		called = true
		if path != dir {
			t.Fatal("wrong sync directory", path)
		}
		if _, e := os.ReadFile(filepath.Join(dir, intent.filename())); e != nil {
			t.Fatal("directory sync ran before intent file existed", e)
		}
		return syncFailed
	})
	if !called || !errors.Is(err, syncFailed) {
		t.Fatal("unsynced directory did not fail closed", called, err)
	}
	items, err := recoverMuxDriveV2OwnedIntents(dir)
	if err != nil || len(items) != 1 || items[0] != intent {
		t.Fatal("uncertain intent not retained for later inspection", items, err)
	}
}

func TestMuxDriveV2OwnedIntentRejectsCorruptionAndForeignEntries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, muxDriveV2OwnedIntent)
	}{
		{"torn", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			if err := os.Truncate(filepath.Join(dir, i.filename()), 5); err != nil {
				t.Fatal(err)
			}
		}},
		{"trailing", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			f, err := os.OpenFile(filepath.Join(dir, i.filename()), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("extra")); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"permissive", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			if err := os.Chmod(filepath.Join(dir, i.filename()), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			path := filepath.Join(dir, i.filename())
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "foreign"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign", func(t *testing.T, dir string, _ muxDriveV2OwnedIntent) {
			if err := os.WriteFile(filepath.Join(dir, "foreign.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"renamed", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			if err := os.Rename(filepath.Join(dir, i.filename()), filepath.Join(dir, "v2-owned-wrong.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate id", func(t *testing.T, dir string, i muxDriveV2OwnedIntent) {
			other := i
			other.Offer.Generation[0]++
			other.Name, _ = other.Offer.names()
			if err := writeMuxDriveV2OwnedIntent(dir, other); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testMuxDriveV2JournalDir(t)
			intent := testMuxDriveV2Intent(t, DirectionUp)
			if err := writeMuxDriveV2OwnedIntent(dir, intent); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, dir, intent)
			if got, err := recoverMuxDriveV2OwnedIntents(dir); err == nil || got != nil {
				t.Fatal("corrupt journal returned cleanup candidates", got, err)
			}
		})
	}
}

func TestMuxDriveV2OwnedIntentRejectsUnsafePathsAndIdentity(t *testing.T) {
	dir := testMuxDriveV2JournalDir(t)
	intent := testMuxDriveV2Intent(t, DirectionUp)
	for _, mutate := range []func(*muxDriveV2OwnedIntent){
		func(i *muxDriveV2OwnedIntent) { i.OwnerPermissionID = "" },
		func(i *muxDriveV2OwnedIntent) { i.ParentID = "../foreign" },
		func(i *muxDriveV2OwnedIntent) { i.Space = "appDataFolder" },
		func(i *muxDriveV2OwnedIntent) { i.Name += "-foreign" },
		func(i *muxDriveV2OwnedIntent) { i.Direction = 99 },
	} {
		bad := intent
		mutate(&bad)
		if err := writeMuxDriveV2OwnedIntent(dir, bad); !errors.Is(err, errMuxDriveV2Intent) {
			t.Fatal("unsafe target intent accepted", bad, err)
		}
	}
	if err := writeMuxDriveV2OwnedIntent(dir+string(filepath.Separator)+".."+string(filepath.Separator)+filepath.Base(dir), intent); !errors.Is(err, errMuxDriveV2Intent) {
		t.Fatal("unclean path accepted", err)
	}
	link := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-symlink")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if err := writeMuxDriveV2OwnedIntent(link, intent); !errors.Is(err, errMuxDriveV2Intent) {
		t.Fatal("symlinked directory accepted", err)
	}
	ancestorLink := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-ancestor-symlink")
	if err := os.Symlink(filepath.Dir(dir), ancestorLink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(ancestorLink) })
	if err := writeMuxDriveV2OwnedIntent(filepath.Join(ancestorLink, filepath.Base(dir)), intent); !errors.Is(err, errMuxDriveV2Intent) {
		t.Fatal("symlinked ancestor accepted", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMuxDriveV2OwnedIntent(dir, intent); !errors.Is(err, errMuxDriveV2Intent) {
		t.Fatal("public directory accepted", err)
	}
}

func TestMuxDriveV2OwnedIntentWindowsFailsClosed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows durability gate")
	}
	if err := validateMuxDriveV2JournalDir(t.TempDir()); !errors.Is(err, errMuxDriveV2JournalUnsupported) {
		t.Fatal("Windows journal admission must fail closed", err)
	}
}
