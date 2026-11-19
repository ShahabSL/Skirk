package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func disableInputEcho(input *os.File) (func() error, error) {
	handle := windows.Handle(input.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return nil, nil // Redirected input has no console echo.
		}
		return nil, err
	}
	if err := windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, err
	}
	return func() error { return windows.SetConsoleMode(handle, mode) }, nil
}
