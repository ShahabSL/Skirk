//go:build !linux && !android

package main

import "context"

func enableParentDeathSignal() {}

func watchParentProcess(_ context.Context, _ int, _ context.CancelFunc) {}
