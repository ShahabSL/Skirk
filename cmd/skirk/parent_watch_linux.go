//go:build linux && !android

package main

import (
	"context"
	"log"
	"syscall"
	"time"
)

func enableParentDeathSignal() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, 1, uintptr(syscall.SIGTERM), 0)
}

func watchParentProcess(ctx context.Context, pid int, cancel context.CancelFunc) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
					log.Printf("parent process disappeared pid=%d", pid)
					cancel()
					return
				}
			}
		}
	}()
}
