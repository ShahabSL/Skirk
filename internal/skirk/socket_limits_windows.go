package skirk

import (
	"errors"

	"golang.org/x/sys/windows"
)

func socketResourcesExhausted(err error) bool {
	return errors.Is(err, windows.WSAENOBUFS) || errors.Is(err, windows.WSAEMFILE)
}

func automaticExitStreamLimit() int { return defaultExitStreamLimit }
