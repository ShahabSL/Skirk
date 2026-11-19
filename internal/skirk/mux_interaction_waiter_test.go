package skirk

import (
	"testing"
	"time"
)

func TestInteractionPriorityRefillServesOneShotArrivals(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	m := &driveMux{sparsePriorityAt: start, sparsePriorityTokens: 0}
	if m.claimInteractionPriorityAt(&muxStream{}, start) {
		t.Fatal("empty token bucket admitted the first arrival")
	}
	for i := 1; i <= 5; i++ {
		arrival := &muxStream{}
		at := start.Add(time.Duration(i) * muxSparsePriorityGlobalRefill)
		if !m.claimInteractionPriorityAt(arrival, at) {
			t.Fatalf("one-shot arrival %d failed despite a newly refilled token", i)
		}
		if m.sparsePriorityTokens != 0 {
			t.Fatalf("arrival %d left %d tokens, want zero", i, m.sparsePriorityTokens)
		}
	}
}

func TestInteractionPriorityRecurrentWaiterCannotStrandTokensAfterStopping(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	m := &driveMux{sparsePriorityAt: start, sparsePriorityTokens: 0}
	stopped := &muxStream{}
	if m.claimInteractionPriorityAt(stopped, start) || m.claimInteractionPriorityAt(stopped, start.Add(time.Millisecond)) {
		t.Fatal("empty bucket granted a recurrent waiter")
	}
	if m.claimInteractionPriorityAt(&muxStream{}, start.Add(muxSparsePriorityGlobalRefill)) {
		t.Fatal("recently recurrent waiter lost its first refill reservation")
	}
	if !m.claimInteractionPriorityAt(&muxStream{}, start.Add(3*muxSparsePriorityGlobalRefill)) {
		t.Fatal("stopped waiter stranded refilled tokens after two refill periods")
	}
}

func TestInteractionPriorityWaiterIndexIsBounded(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	m := &driveMux{sparsePriorityAt: start, sparsePriorityTokens: 0}
	for i := 0; i < muxInteractionWaiterMax*3; i++ {
		if m.claimInteractionPriorityAt(&muxStream{}, start) {
			t.Fatal("empty bucket granted a waiter")
		}
	}
	if m.interactionWaiters.Len() != muxInteractionWaiterMax || len(m.interactionWaitIndex) != muxInteractionWaiterMax {
		t.Fatalf("waiter state grew beyond cap: list=%d index=%d", m.interactionWaiters.Len(), len(m.interactionWaitIndex))
	}
}
