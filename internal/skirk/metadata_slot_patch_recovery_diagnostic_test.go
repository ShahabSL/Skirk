package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type slotRecoveryLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *slotRecoveryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *slotRecoveryLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

type slotRecoveryDiagnosticStore struct {
	*shadowSlotMemoryStore
	commit          bool
	patchErr        error
	readErr         error
	readDescription *string
	wrongIdentity   bool
	captured        string
	afterRead       func()
}

func (s *slotRecoveryDiagnosticStore) PatchMetadataSlot(ctx context.Context, id, name, description string) (MetadataSlotInfo, error) {
	s.captured = description
	var info MetadataSlotInfo
	if s.commit {
		var err error
		info, err = s.shadowSlotMemoryStore.PatchMetadataSlot(ctx, id, name, description)
		if err != nil {
			return info, err
		}
	}
	return info, s.patchErr
}
func (s *slotRecoveryDiagnosticStore) GetMetadataSlot(ctx context.Context, id, name string) (MetadataSlotInfo, error) {
	if s.readErr != nil {
		return MetadataSlotInfo{}, s.readErr
	}
	info, err := s.shadowSlotMemoryStore.GetMetadataSlot(ctx, id, name)
	if err != nil {
		return info, err
	}
	if s.readDescription != nil {
		info.Description = *s.readDescription
	}
	if s.wrongIdentity {
		info.Name = "foreign-test-name"
	}
	if s.afterRead != nil {
		s.afterRead()
	}
	return info, nil
}

func TestSlotPatchRecoveryTimeoutClassificationRejectsMixedCauses(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("single: %w", context.DeadlineExceeded),
		&url.Error{Op: "Patch", URL: "https://www.google.com/public", Err: context.DeadlineExceeded},
		&net.DNSError{Err: "public timeout fixture", IsTimeout: true}} {
		if !metadataSlotDiagnosticTimeout(err) {
			t.Fatalf("typed timeout not classified: %T", err)
		}
	}
	for _, err := range []error{context.Canceled, errors.New("context deadline exceeded"),
		errors.Join(context.DeadlineExceeded, errors.New("metadata slot authentication failed")),
		fmt.Errorf("wrapped: %w", errors.Join(context.DeadlineExceeded, errors.New("state mismatch"))),
		errors.Join(context.DeadlineExceeded, &GoogleAPIError{Status: 403, Reason: "forbidden"})} {
		if metadataSlotDiagnosticTimeout(err) {
			t.Fatalf("mixed/untyped cause classified as recoverable: %v", err)
		}
	}
}

func TestSlotPatchRecoveryParentDeadlineNeverGainsRequestTimeoutLabel(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	client.t.Data = &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, patchErr: context.DeadlineExceeded}
	client.t.Observe = true
	var output slotRecoveryLog
	client.t.Logger = log.New(&output, "", 0)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := client.patchSlotShadow(ctx, up)
	if err == nil || err == errMuxSlotPatchTimeoutFallback || !strings.Contains(output.String(), "result=non_timeout_failure") || strings.Contains(output.String(), "result=timeout_started") {
		t.Fatalf("parent cancellation relabeled: %v; %s", err, output.String())
	}
}

func TestSlotPatchRecoveryCommittedReadbackHasExactBoundedIdentityMarkers(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	store := &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, commit: true, patchErr: context.DeadlineExceeded}
	client.t.Data = store
	var output slotRecoveryLog
	client.t.Logger = log.New(&output, "", 0)
	client.t.Observe = true
	batch := deliveryBatch(t, client, []byte("public committed payload"))
	if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	if !up.canSend() {
		t.Fatal("diagnostics changed proven-committed writer continuation")
	}
	text := output.String()
	target := sha256.Sum256([]byte(up.upID + "\x00" + up.upName))
	digest := sha256.Sum256([]byte(store.captured))
	for _, want := range []string{fmt.Sprintf("target=%x patch=%x revision=1", target, digest), "result=timeout_started readback_ms=6000", "result=exact_readback readback_ms=6000"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing source-bound diagnostic %q: %s", want, text)
		}
	}
	if strings.Contains(text, store.captured) || strings.Contains(text, up.upID) || strings.Contains(text, "result=immutable_fallback") {
		t.Fatal("diagnostics leaked content/target or falsely claimed retirement")
	}
}

func TestSlotPatchRecoveryUnconfirmedReadbackRetiresBeforeLateOriginalCompletion(t *testing.T) {
	for _, readback := range []string{"empty", "timeout", "authenticated-older"} {
		t.Run(readback, func(t *testing.T) {
			base := newShadowSlotMemoryStore()
			client, _, up, _ := deliverySlotPair(t, base)
			defer client.stopSlotLinks()
			store := &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, patchErr: context.DeadlineExceeded}
			if readback == "timeout" {
				store.readErr = context.DeadlineExceeded
			}
			if readback == "authenticated-older" {
				older, err := up.peer.outbound.codec.seal(metadataSlotSnapshot{generation: up.generation, direction: DirectionUp, revision: 1})
				if err != nil {
					t.Fatal(err)
				}
				up.peer.outbound.nextRevision = 1
				store.readDescription = &older
			}
			client.t.Data = store
			client.t.Observe = true
			var output slotRecoveryLog
			client.t.Logger = log.New(&output, "", 0)
			batch := deliveryBatch(t, client, []byte("public late completion"))
			if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
				t.Fatal(err)
			}
			err := client.patchSlotShadow(context.Background(), up)
			if err != errMuxSlotPatchTimeoutFallback || errors.Is(err, errMetadataSlotClosed) || err.Error() != "metadata slot shadow PATCH unresolved; immutable path continues" {
				t.Fatalf("fallback semantics changed: %v", err)
			}
			if up.canSend() {
				t.Fatal("unconfirmed generation allowed a later PATCH")
			}
			if _, err = base.PatchMetadataSlot(context.Background(), up.upID, up.upName, store.captured); err != nil {
				t.Fatal(err)
			}
			if err = client.patchSlotShadow(context.Background(), up); !errors.Is(err, errMetadataSlotClosed) {
				t.Fatalf("late completion reopened retired writer: %v", err)
			}
			if err = client.lanes[client.frameLane(batch.frames[0])].uploadPreparedBatchV4(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			stored, err := base.Get(context.Background(), batch.name)
			if err != nil || !bytes.Equal(stored, batch.sealed) {
				t.Fatal("fallback did not preserve exact immutable bytes")
			}
			if !strings.Contains(output.String(), "result=immutable_fallback") {
				t.Fatal("retirement outcome missing")
			}
		})
	}
}

func TestSlotPatchRecoveryOnlyExactPrivateSentinelChangesLogClassification(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, _, _ := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	client.t.Observe = true
	for _, err := range []error{errMuxSlotPatchTimeoutFallback, fmt.Errorf("wrapped: %w", errMuxSlotPatchTimeoutFallback),
		errors.Join(errMuxSlotPatchTimeoutFallback, errors.New("authentication failed")),
		errors.New(errMuxSlotPatchTimeoutFallback.Error()), errMetadataSlotClosed} {
		var output slotRecoveryLog
		client.t.Logger = log.New(&output, "", 0)
		client.logSlotShadowError(err)
		if classified := strings.Contains(output.String(), "reason=patch_timeout_immutable_fallback "); classified != (err == errMuxSlotPatchTimeoutFallback) {
			t.Fatalf("error identity lost: %v; %s", err, output.String())
		}
	}
}

func TestSlotPatchRecoveryInvalidReadbacksStayUnclassified(t *testing.T) {
	for _, which := range []string{"malformed", "wrong-target", "joined-timeout-integrity", "newer-authenticated", "wrong-generation", "wrong-direction"} {
		t.Run(which, func(t *testing.T) {
			base := newShadowSlotMemoryStore()
			client, _, up, _ := deliverySlotPair(t, base)
			defer client.stopSlotLinks()
			store := &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, patchErr: context.DeadlineExceeded}
			client.t.Data = store
			client.t.Observe = true
			var output slotRecoveryLog
			client.t.Logger = log.New(&output, "", 0)
			bad := "malformed public fixture"
			switch which {
			case "wrong-target":
				store.wrongIdentity = true
			case "joined-timeout-integrity":
				store.readErr = errors.Join(context.DeadlineExceeded, errors.New("authentication failed"))
			case "newer-authenticated", "wrong-generation", "wrong-direction":
				codec := up.peer.outbound.codec
				generation, direction := up.generation, byte(DirectionUp)
				if which == "wrong-generation" {
					generation[0] ^= 1
				}
				if which == "wrong-direction" {
					direction = DirectionDown
				}
				if which != "newer-authenticated" {
					var err error
					codec, err = newMetadataSlotCodec(client.t.Secret, client.t.SessionID, direction, up.key.clientID, up.key.runID, generation, metadataSlotLimits{})
					if err != nil {
						t.Fatal(err)
					}
				}
				var err error
				bad, err = codec.seal(metadataSlotSnapshot{generation: generation, direction: direction, revision: 2})
				if err != nil {
					t.Fatal(err)
				}
			}
			if which != "wrong-target" && which != "joined-timeout-integrity" {
				store.readDescription = &bad
			}
			err := client.patchSlotShadow(context.Background(), up)
			if err == nil || err == errMuxSlotPatchTimeoutFallback {
				t.Fatalf("invalid readback gained a benign fallback label: %v", err)
			}
			if !strings.Contains(output.String(), "result=invalid_readback_or_state") || strings.Contains(output.String(), "result=immutable_fallback") {
				t.Fatalf("integrity/state failure not retained: %s", output.String())
			}
		})
	}
}

func TestSlotPatchRecoveryExactDescriptionWithForeignTargetRejectsOnlyDiagnostic(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	store := &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, commit: true, patchErr: context.DeadlineExceeded, wrongIdentity: true}
	client.t.Data = store
	client.t.Observe = true
	var output slotRecoveryLog
	client.t.Logger = log.New(&output, "", 0)
	if err := client.patchSlotShadow(context.Background(), up); err != nil {
		t.Fatalf("diagnostics changed preexisting exact-description resolution: %v", err)
	}
	if !up.canSend() {
		t.Fatal("diagnostics changed writer state")
	}
	if !strings.Contains(output.String(), "result=invalid_readback_identity") || strings.Contains(output.String(), "result=exact_readback") {
		t.Fatal("foreign target acquired an exact-commit proof")
	}
}

func TestSlotPatchRecoveryAuthenticatedACKDuringReadbackIsNotAnIntegrityFailure(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, exit, up, down := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	client.t.Observe = true
	var output slotRecoveryLog
	client.t.Logger = log.New(&output, "", 0)
	batch := deliveryBatch(t, client, []byte("public processing ACK race"))
	if _, err := up.peer.enqueue(batch.name, batch.sealed); err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	defer remote.Close()
	stream := exit.registerStream(7, client.t.ClientID, client.t.RunID, local)
	defer stream.close()
	store := &slotRecoveryDiagnosticStore{shadowSlotMemoryStore: base, commit: true, patchErr: context.DeadlineExceeded}
	client.t.Data = store
	store.afterRead = func() {
		records, err := down.peer.receive(store.captured)
		if err != nil || len(records) != 1 {
			t.Fatalf("authenticated record staging: %v", err)
		}
		meta, ok := parseMuxObjectInfo(ObjectInfo{Name: records[0].MuxName})
		if !ok {
			t.Fatal("mux name parse")
		}
		meta.SlotLink = down
		meta.SlotSeq = records[0].SlotSeq
		meta.SlotSealed = records[0].MuxSealed
		if err = exit.processMuxObject(context.Background(), meta); err != nil {
			t.Fatal(err)
		}
		// The production receiver loop marks the slot only after the actual
		// mux processing above succeeds, then prepares its authenticated ACK.
		if err = down.peer.markProcessed(meta.SlotSeq, meta.Name); err != nil {
			t.Fatal(err)
		}
		ack, err := down.peer.prepare()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = up.peer.receive(ack.description); err != nil {
			t.Fatalf("authenticated peer ACK: %v", err)
		}
	}
	err := client.patchSlotShadow(context.Background(), up)
	if err != errMuxSlotPatchTimeoutFallback {
		t.Fatalf("unchanged writer retirement was not specifically classified: %v; %s", err, output.String())
	}
	if !strings.Contains(output.String(), "result=peer_processed_before_retirement") || strings.Contains(output.String(), "invalid_readback_or_state") {
		t.Fatal("positive authenticated ACK race misclassified")
	}
	select {
	case got := <-stream.inbound:
		if !bytes.Equal(got, batch.frames[0].Payload) {
			t.Fatal("wrong accepted payload")
		}
	default:
		t.Fatal("ACK was not preceded by actual mux acceptance")
	}
}

func TestSlotFiniteRetirementGetBudgetMakesNoAdditionalCall(t *testing.T) {
	base := newShadowSlotMemoryStore()
	client, _, up, _ := deliverySlotPair(t, base)
	defer client.stopSlotLinks()
	client.t.Observe = true
	var output slotRecoveryLog
	client.t.Logger = log.New(&output, "", 0)
	up.gets = muxSlotShadowMaxGets
	base.mu.Lock()
	before := base.getCalls
	base.mu.Unlock()
	client.runSlotShadowReader(context.Background(), up)
	base.mu.Lock()
	after := base.getCalls
	base.mu.Unlock()
	if before != after || !strings.Contains(output.String(), "reason=get_budget_exhausted limit=320") {
		t.Fatal("finite GET retirement changed budget or omitted exact marker")
	}
}

func TestSlotFiniteRetirementLifetimeExcludesParentDeadlineAndCancellation(t *testing.T) {
	parent := context.Background()
	child, stop := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer stop()
	if !metadataSlotOwnLifetimeExpired(parent, child) {
		t.Fatal("own expired timeout not classified")
	}
	canceled, doCancel := context.WithCancel(parent)
	doCancel()
	if metadataSlotOwnLifetimeExpired(parent, canceled) {
		t.Fatal("ordinary cancellation gained lifetime label")
	}
	expiredParent, stopParent := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer stopParent()
	inherited, stopInherited := context.WithTimeout(expiredParent, time.Hour)
	defer stopInherited()
	if metadataSlotOwnLifetimeExpired(expiredParent, inherited) {
		t.Fatal("parent deadline gained own-lifetime label")
	}
	canceledChild, stopCanceledChild := context.WithTimeout(canceled, time.Hour)
	defer stopCanceledChild()
	if metadataSlotOwnLifetimeExpired(canceled, canceledChild) {
		t.Fatal("parent cancellation gained own-lifetime label")
	}
}
