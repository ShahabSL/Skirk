// Package probeledger contains portability helpers for test-only Drive probe
// recovery ledgers. The file is fsynced before its atomic rename.
package probeledger

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// SyncParent makes a renamed recovery ledger durable where directory syncing
// is supported. Windows rejects Sync on an opened directory with access denied;
// its probe ledger retains the flushed file and rename without that extra step.
func SyncParent(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
