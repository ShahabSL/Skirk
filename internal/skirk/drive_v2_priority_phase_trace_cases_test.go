package skirk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDriveV2PriorityPhaseTraceBoundedAndSanitized(t *testing.T) {
	start := time.Unix(1_000, 0)
	trace := &fsHybridPhaseTrace{start: start, limit: 2, events: make([]fsHybridPhaseEvent, 0, 2)}
	trace.key[0] = 1
	secretName := "skirk-private-mailbox/session/user-value"
	trace.recordAt(start.Add(100*time.Millisecond), DirectionUp, secretName, 7, "queued_inline", 11, 12, 13, 14, 15)
	trace.recordAt(start.Add(250*time.Millisecond), DirectionUp, secretName, 7, "patch_completed")
	trace.recordAt(start.Add(time.Second), DirectionUp, secretName, 7, "sender_processing_ack")
	snapshot := trace.snapshot()
	if snapshot.Limit != 2 || snapshot.Dropped != 1 || len(snapshot.Events) != 2 ||
		snapshot.Events[0].Claim != snapshot.Events[1].Claim || len(snapshot.Events[0].Claim) != 16 ||
		snapshot.Events[0].ElapsedMS != 100 || snapshot.Events[1].ElapsedMS != 250 ||
		snapshot.Events[0].StreamCount != 5 || len(snapshot.Events[0].StreamIDs) != 4 {
		t.Fatal("finite phase trace changed identity, timing or drop count", snapshot)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(raw), secretName) || strings.Contains(string(raw), "private-mailbox") {
		t.Fatal("phase summary exposed an owned name", err)
	}
	snapshot.Events[0].Phase = "mutated snapshot"
	snapshot.Events[0].StreamIDs[0] = 999
	if trace.snapshot().Events[0].Phase != "queued_inline" {
		t.Fatal("snapshot aliases mutable trace storage")
	}
	if trace.snapshot().Events[0].StreamIDs[0] != 11 {
		t.Fatal("snapshot aliases stream identifiers")
	}
	other := &fsHybridPhaseTrace{start: start, limit: 2}
	other.key[0] = 2
	other.recordAt(start.Add(100*time.Millisecond), DirectionUp, secretName, 7, "queued_inline")
	if trace.snapshot().Events[0].Claim == other.snapshot().Events[0].Claim {
		t.Fatal("separate run keys linked the same private name")
	}
}

func TestDriveV2PriorityPhaseTraceRejectsUnboundedCapacity(t *testing.T) {
	for _, limit := range []int{0, -1, 1025} {
		if _, err := newFSHybridPhaseTrace(limit); err == nil {
			t.Fatal("accepted unbounded phase trace", limit)
		}
	}
}
