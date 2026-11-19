package main

import (
	"context"
	"strings"
	"testing"
)

func TestWatchClientNetworkEventsDispatchesOnlyHandoffBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events int
	watchClientNetworkEvents(ctx, strings.NewReader("?N\nNN"), func() {
		events++
		if events == 2 {
			cancel()
		}
	})
	if events != 2 {
		t.Fatalf("dispatched %d network handoffs, want 2", events)
	}
}
