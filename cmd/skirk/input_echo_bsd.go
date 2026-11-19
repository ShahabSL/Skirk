//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

const getInputMode = unix.TIOCGETA
const setInputMode = unix.TIOCSETA
