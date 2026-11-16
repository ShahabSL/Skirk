package skirk

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	muxDriveV2IntentVersion    = 1
	muxDriveV2IntentMaxBytes   = 1024
	muxDriveV2IntentMaxEntries = 1024
)

var errMuxDriveV2Intent = errors.New("invalid Drive v2 owned-target intent")
var errMuxDriveV2JournalUnsupported = errors.New("Drive v2 intent journal durability unavailable on this platform")

// An immutable intent is synced before either peer creates the target. It is
// evidence for recovery, never sufficient authorization for remote deletion.
type muxDriveV2OwnedIntent struct {
	Version           int                  `json:"version"`
	Offer             muxDriveV2OfferValue `json:"offer"`
	Direction         byte                 `json:"direction"`
	Name              string               `json:"name"`
	ParentID          string               `json:"parent_id"`
	Space             string               `json:"space"`
	OwnerPermissionID string               `json:"owner_permission_id"`
}

func newMuxDriveV2OwnedIntent(offer muxDriveV2OfferValue, direction byte, parentID, space, ownerPermissionID string) (muxDriveV2OwnedIntent, error) {
	up, down := offer.names()
	intent := muxDriveV2OwnedIntent{Version: muxDriveV2IntentVersion, Offer: offer, Direction: direction,
		ParentID: parentID, Space: space, OwnerPermissionID: ownerPermissionID}
	if direction == DirectionUp {
		intent.Name = up
	} else {
		intent.Name = down
	}
	if !intent.valid() {
		return muxDriveV2OwnedIntent{}, errMuxDriveV2Intent
	}
	return intent, nil
}

func validMuxDriveV2OwnerID(id string) bool {
	if len(id) < 3 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
			b >= '0' && b <= '9' || b == '_' || b == '-' {
			continue
		}
		return false
	}
	return true
}

func (i muxDriveV2OwnedIntent) valid() bool {
	if i.Version != muxDriveV2IntentVersion || !i.Offer.valid() ||
		!validMuxDriveV2OwnerID(i.OwnerPermissionID) {
		return false
	}
	up, down := i.Offer.names()
	switch i.Direction {
	case DirectionUp:
		if i.Name != up {
			return false
		}
	case DirectionDown:
		if i.Name != down {
			return false
		}
	default:
		return false
	}
	if i.Space == "appDataFolder" {
		return i.ParentID == "appDataFolder"
	}
	return i.Space == "drive" && validMuxDriveV2ID(i.ParentID)
}

func (i muxDriveV2OwnedIntent) fileID() string {
	if i.Direction == DirectionUp {
		return i.Offer.UpID
	}
	return i.Offer.DownID
}

func (i muxDriveV2OwnedIntent) filename() string {
	return "v2-owned-" + hex.EncodeToString(i.Offer.SessionID[:]) + "-" +
		hex.EncodeToString(i.Offer.Generation[:]) + "-" + directionName(i.Direction) + ".json"
}

func validateMuxDriveV2JournalDir(dir string) error {
	// Directory-entry durability is not proven for the Windows filesystem
	// backend. Keep the feature closed there until it has its own durable path.
	if runtime.GOOS == "windows" {
		return errMuxDriveV2JournalUnsupported
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errMuxDriveV2Intent
	}
	for part := dir; ; part = filepath.Dir(part) {
		info, err := os.Lstat(part)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errMuxDriveV2Intent
		}
		if part == dir && (info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700) {
			return errMuxDriveV2Intent
		}
		if parent := filepath.Dir(part); parent == part {
			break
		}
	}
	return nil
}

func syncMuxDriveV2JournalDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// writeMuxDriveV2OwnedIntent must succeed before the corresponding Drive
// create begins. A failed sync leaves the intent in place for read-only
// recovery; callers must not continue with the remote create.
func writeMuxDriveV2OwnedIntent(dir string, intent muxDriveV2OwnedIntent) error {
	return writeMuxDriveV2OwnedIntentWithSync(dir, intent, syncMuxDriveV2JournalDir)
}

func writeMuxDriveV2OwnedIntentWithSync(dir string, intent muxDriveV2OwnedIntent, syncDir func(string) error) error {
	if err := validateMuxDriveV2JournalDir(dir); err != nil {
		return err
	}
	if !intent.valid() || syncDir == nil {
		return errMuxDriveV2Intent
	}
	data, err := json.Marshal(intent)
	if err != nil || len(data)+1 > muxDriveV2IntentMaxBytes {
		return errMuxDriveV2Intent
	}
	path := filepath.Join(dir, intent.filename())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(append(data, '\n'))
	if writeErr == nil && n != len(data)+1 {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return syncDir(dir)
}

// recoverMuxDriveV2OwnedIntents is deliberately read-only. A caller must
// separately authenticate remote metadata, coordinate process ownership, and
// prove exact deletion before retiring any intent.
func recoverMuxDriveV2OwnedIntents(dir string) ([]muxDriveV2OwnedIntent, error) {
	if err := validateMuxDriveV2JournalDir(dir); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, readErr := f.ReadDir(muxDriveV2IntentMaxEntries + 1)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) > muxDriveV2IntentMaxEntries {
		return nil, errMuxDriveV2Intent
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	out := make([]muxDriveV2OwnedIntent, 0, len(entries))
	seenIDs := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "v2-owned-") || !strings.HasSuffix(name, ".json") {
			return nil, errMuxDriveV2Intent
		}
		path := filepath.Join(dir, name)
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
			info.Mode().Perm()&0o600 != 0o600 || info.Size() < 2 || info.Size() > muxDriveV2IntentMaxBytes {
			return nil, errMuxDriveV2Intent
		}
		opened, openErr := os.Open(path)
		if openErr != nil {
			return nil, errMuxDriveV2Intent
		}
		openedInfo, statErr := opened.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) {
			_ = opened.Close()
			return nil, errMuxDriveV2Intent
		}
		raw, readErr := io.ReadAll(io.LimitReader(opened, muxDriveV2IntentMaxBytes+1))
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil || len(raw) != int(info.Size()) || len(raw) < 2 || raw[len(raw)-1] != '\n' {
			return nil, errMuxDriveV2Intent
		}
		var intent muxDriveV2OwnedIntent
		if json.Unmarshal(raw[:len(raw)-1], &intent) != nil || !intent.valid() || intent.filename() != name {
			return nil, errMuxDriveV2Intent
		}
		canonical, err := json.Marshal(intent)
		if err != nil || !bytes.Equal(canonical, raw[:len(raw)-1]) {
			return nil, errMuxDriveV2Intent
		}
		if _, exists := seenIDs[intent.fileID()]; exists {
			return nil, errMuxDriveV2Intent
		}
		seenIDs[intent.fileID()] = struct{}{}
		out = append(out, intent)
	}
	return out, nil
}
