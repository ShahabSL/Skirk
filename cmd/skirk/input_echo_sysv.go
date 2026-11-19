//go:build aix || linux || solaris || zos

package main

import "golang.org/x/sys/unix"

const getInputMode = unix.TCGETS
const setInputMode = unix.TCSETS
