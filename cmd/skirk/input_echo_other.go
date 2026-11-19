//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows && !zos

package main

import (
	"errors"
	"os"
)

func disableInputEcho(input *os.File) (func() error, error) {
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil, errors.New("this terminal cannot hide input; use --oauth-client-file PATH")
	}
	return nil, nil
}
