//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// A pipe has no echo to disable. On a terminal, keep line editing and signals.
func disableInputEcho(input *os.File) (func() error, error) {
	fd := int(input.Fd())
	state, err := unix.IoctlGetTermios(fd, getInputMode)
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENODEV) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hidden := *state
	hidden.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, setInputMode, &hidden); err != nil {
		return nil, err
	}
	return func() error { return unix.IoctlSetTermios(fd, setInputMode, state) }, nil
}
