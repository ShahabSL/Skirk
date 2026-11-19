package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type processLock struct {
	file *os.File
}

func acquireExitLock(sessionID [16]byte) (*processLock, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("skirk-exit-%x.lock", sessionID))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// The open handle excludes writers, including an earlier Skirk build
	// that relies on creating this file exclusively. Windows releases
	// the handle when a process exits, even if the marker is still there.
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, fmt.Errorf("another skirk exit is already running for session %x", sessionID)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		file.Close()
		return nil, err
	}
	return &processLock{file: file}, nil
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}
