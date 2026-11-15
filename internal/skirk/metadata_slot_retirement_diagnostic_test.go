package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSlotPatchBudgetStopDiagnosticRequiresActualLocalRetirement(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, store)
	defer client.stopSlotLinks()
	var output bytes.Buffer
	client.t.Logger = log.New(&output, "", 0)
	up.patches = muxSlotShadowMaxPatches
	client.logSlotShadowError(errMetadataSlotClosed)
	if strings.Contains(output.String(), "reason=patch_budget_exhausted") {
		t.Fatal("count64 alone was mistaken for a known retirement cause")
	}
	output.Reset()
	err := client.patchSlotShadow(context.Background(), up)
	if !errors.Is(err, errMuxSlotPatchBudgetExhausted) {
		t.Fatalf("budget refusal lost its typed identity: %v", err)
	}
	if err.Error() != "metadata slot shadow PATCH budget exhausted" {
		t.Fatalf("diagnostic change altered the original error text: %v", err)
	}
	if errors.Is(err, errMetadataSlotClosed) {
		t.Fatal("diagnostic change altered the existing budget error's closed semantics")
	}
	store.mu.Lock()
	patches := len(store.patchedIDs)
	store.mu.Unlock()
	if patches != 0 || up.patches != muxSlotShadowMaxPatches {
		t.Fatal("diagnostic change issued a PATCH or changed the bound")
	}
	client.logSlotShadowError(err)
	if strings.Count(output.String(), "reason=patch_budget_exhausted authoritative=immutable") != 1 {
		t.Fatalf("local budget refusal not identified: %s", &output)
	}

	batch := deliveryBatch(t, client, []byte("public authentication fixture"))
	bad := bytes.Clone(batch.sealed)
	bad[len(bad)-1] ^= 1
	integrityErr := exit.validateSlotShadow(metadataSlotRecord{MuxName: batch.name, MuxSealed: bad}, down)
	if integrityErr == nil {
		t.Fatal("tampered AEAD fixture unexpectedly authenticated")
	}
	for _, unrelated := range []error{integrityErr, errMetadataSlotReplay, errMetadataSlotClosed,
		errors.New(errMuxSlotPatchBudgetExhausted.Error()), errors.New("unknown failure"),
		fmt.Errorf("wrapped: %w", errMuxSlotPatchBudgetExhausted), errors.Join(errMuxSlotPatchBudgetExhausted, integrityErr)} {
		output.Reset()
		// Generic closed and every other error remain generic, including
		// after a local budget refusal. No cause is guessed from link state.
		client.logSlotShadowError(unrelated)
		if !strings.Contains(output.String(), "reason=transport_error authoritative=immutable") ||
			strings.Contains(output.String(), "reason=patch_budget_exhausted") {
			t.Fatalf("unrelated/integrity error relabeled as retirement: %v: %s", unrelated, &output)
		}
	}
}

func TestSlotPatchBudgetRetirementKeepsExactImmutableFallbackDelivery(t *testing.T) {
	store := newShadowSlotMemoryStore()
	client, exit, up, _ := deliverySlotPair(t, store)
	client.t.MetadataSlotAsyncV1 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); client.stopSlotLinks(); exit.stopSlotLinks() }()
	var output bytes.Buffer
	client.t.Logger = log.New(&output, "", 0)
	up.patches = muxSlotShadowMaxPatches
	batch := deliveryBatch(t, client, []byte("public exact fallback"))
	owner := slotShadowRecord{name: batch.name, sealed: batch.sealed,
		deliveryDone: make(chan error, 1), publicationDone: make(chan error, 1)}
	up.shadowQ <- owner
	done := make(chan struct{})
	go func() { defer close(done); client.runSlotShadowWriter(ctx, up) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("budget retirement did not join writer")
	}
	if up.canSend() || !up.isActive() {
		t.Fatal("diagnostic change altered stopped-writer/live-reader ownership")
	}
	for _, waiter := range []chan error{owner.deliveryDone, owner.publicationDone} {
		select {
		case err := <-waiter:
			if err == nil {
				t.Fatal("retirement acknowledged unpublished/unprocessed payload")
			}
		default:
			t.Fatal("retirement did not release retained owner")
		}
	}
	if !strings.Contains(output.String(), "reason=patch_budget_exhausted") {
		t.Fatalf("actual writer path omitted diagnostic: %s", &output)
	}
	if err := client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(ctx, batch); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, batch.name)
	if err != nil || !bytes.Equal(stored, batch.sealed) {
		t.Fatalf("retirement changed immutable fallback identity: %v", err)
	}
	objects, err := store.List(ctx, batch.name)
	if err != nil || len(objects) != 1 {
		t.Fatalf("expected one immutable fallback: %d %v", len(objects), err)
	}
	meta, ok := parseMuxObjectInfo(objects[0])
	if !ok {
		t.Fatal("fallback could not be parsed by unchanged mux")
	}
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	if err := exit.processMuxObject(ctx, meta); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-stream.inbound:
		if !bytes.Equal(payload, batch.frames[0].Payload) {
			t.Fatalf("fallback delivered wrong bytes: %q", payload)
		}
	default:
		t.Fatal("fallback did not deliver through actual mux frame processing")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.patchedIDs) != 0 {
		t.Fatal("retired writer issued a PATCH during immutable delivery")
	}
}
