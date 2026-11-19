//go:build darwin || linux

package skirk

import (
	"errors"

	"golang.org/x/sys/unix"
)

func socketResourcesExhausted(err error) bool {
	return errors.Is(err, unix.ENOBUFS) || errors.Is(err, unix.ENOMEM)
}

// Keep descriptors available for Drive connections, UDP sockets and local
// frontends when the exit runs under a low process limit.
func automaticExitStreamLimit() int {
	var limit unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &limit) != nil {
		return defaultExitStreamLimit
	}
	return int(max(1, min(uint64(defaultExitStreamLimit), uint64(limit.Cur)/2)))
}
