//go:build !darwin && !linux && !windows

package skirk

func automaticExitStreamLimit() int { return defaultExitStreamLimit }

func socketResourcesExhausted(error) bool { return false }
