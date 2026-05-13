//go:build !unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type processLock struct {
	file *os.File
	path string
}

func acquireExitLock(sessionID [16]byte) (*processLock, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("skirk-exit-%x.lock", sessionID))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another skirk exit may already be running for session %x", sessionID)
		}
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &processLock{file: file, path: path}, nil
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	_ = os.Remove(l.path)
	return err
}
