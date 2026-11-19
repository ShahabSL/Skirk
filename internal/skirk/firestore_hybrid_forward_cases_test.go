package skirk

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestFSHybridTransportPolicyDoesNotTreatURLWrapperAsRetryProof(t *testing.T) {
	for _, failure := range []error{
		&url.Error{Op: "POST", URL: "https://public.invalid", Err: errors.New("decoder fixture")},
		&url.Error{Op: "POST", URL: "https://public.invalid", Err: x509.UnknownAuthorityError{}},
		errors.Join(io.EOF, &GoogleAPIError{Op: "public_fixture", Status: 403}),
	} {
		if fsHybridTransient(failure) {
			t.Fatal("an unknown/decode/TLS/auth cause was treated as a transport retry")
		}
	}
	for _, failure := range []error{context.DeadlineExceeded, io.EOF, &url.Error{Op: "POST", URL: "https://public.invalid", Err: syscall.ECONNRESET}} {
		if !fsHybridTransient(failure) {
			t.Fatal("declared bounded network retry was rejected")
		}
	}
}

type fsHybridCapabilityBase struct {
	*MemoryStore
	normalReads, quotaWaits, fallbacks atomic.Int64
	fallbackErr                        error
	fallbackGate                       <-chan struct{}
}

func (b *fsHybridCapabilityBase) GetNormalByID(ctx context.Context, id string) ([]byte, error) {
	b.normalReads.Add(1)
	return b.MemoryStore.GetByID(ctx, id)
}
func (b *fsHybridCapabilityBase) QuotaSnapshot() DriveQuotaSnapshot {
	return DriveQuotaSnapshot{Calls: 17, Units: 31, Errors: 2, Ops: map[string]DriveQuotaOpSnapshot{}}
}
func (b *fsHybridCapabilityBase) QuotaBackoffSnapshot() DriveQuotaBackoffSnapshot {
	return DriveQuotaBackoffSnapshot{Active: true, Reason: "public_fixture_backoff", Failures: 3}
}
func (b *fsHybridCapabilityBase) WaitForDriveQuota(ctx context.Context, _ string) error {
	b.quotaWaits.Add(1)
	return ctx.Err()
}
func (b *fsHybridCapabilityBase) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	b.fallbacks.Add(1)
	if b.fallbackGate != nil {
		select {
		case <-b.fallbackGate:
		case <-ctx.Done():
			return ObjectInfo{}, ctx.Err()
		}
	}
	if b.fallbackErr != nil {
		return ObjectInfo{}, b.fallbackErr
	}
	return b.MemoryStore.PutObjectWithID(ctx, id, name, data)
}

func TestFSHybridStoreNormalCapabilitiesPreserveBulkAndQuotaAccounting(t *testing.T) {
	client, _, _, base := fsHybridMemoryStores(t)
	spy := &fsHybridCapabilityBase{MemoryStore: base}
	client.mu.Lock()
	client.base = spy
	client.mu.Unlock()
	if snapshot := client.QuotaSnapshot(); snapshot.Calls != 17 || snapshot.Units != 31 || snapshot.Errors != 2 {
		t.Fatal("Drive accounting disappeared behind the wrapper")
	}
	if snapshot := client.QuotaBackoffSnapshot(); !snapshot.Active || snapshot.Failures != 3 {
		t.Fatal("Drive backoff disappeared")
	}
	if err := client.WaitForDriveQuota(context.Background(), "get"); err != nil || spy.quotaWaits.Load() != 1 {
		t.Fatal("quota gate was bypassed", err)
	}
	name := muxDirPrefix(client.cfg.Session, DirectionDown, "client", "run") + "p1/public-normal"
	ids, err := client.GenerateObjectIDs(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.PutObjectWithID(context.Background(), ids[0], name, []byte("normal bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.GetNormalByID(context.Background(), info.ID); err != nil || string(got) != "normal bytes" || spy.normalReads.Load() != 1 {
		t.Fatal("bulk pool capability was bypassed", err)
	}
	if got, _, err := client.GetObjectRangeByID(context.Background(), info.ID, 0, 5); err != nil || string(got) != "normal" {
		t.Fatal("range capability changed", err)
	}
	if client.MetadataOnlyPriorityEnabled() || client.MetadataSlotEnabled() {
		t.Fatal("legacy protocol accidentally enabled")
	}
}

func TestFSHybridStorePreconditionCannotMaskJoinedIntegrity(t *testing.T) {
	client, _, cloud, _ := fsHybridMemoryStores(t)
	next := cloud.nextWrite(0)
	_, readsBefore, _ := cloud.counts()
	failure := errors.Join(&GoogleAPIError{Op: "public_fixture", Status: 412, Reason: "FAILED_PRECONDITION"}, errFSHybridIntegrity)
	cloud.setWriteFault(0, next, fsHybridMemoryFault{Err: failure})
	r := fsHybridStoreRecord(t, client, 1, 7)
	if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err == nil {
		t.Fatal("joined integrity error was retried as an ordinary CAS conflict")
	}
	_, readsAfter, _ := cloud.counts()
	if readsAfter[0] != readsBefore[0] {
		t.Fatal("integrity failure initiated ambiguity readback")
	}
	if !errors.Is(client.Error(), errFSHybridIntegrity) {
		t.Fatal("integrity cause lost", client.Error())
	}
}

func TestFSHybridStoreFallbackAuthorizationStopsAfterOneAttempt(t *testing.T) {
	client, _, cloud, base := fsHybridMemoryStores(t)
	spy := &fsHybridCapabilityBase{MemoryStore: base, fallbackErr: &GoogleAPIError{Op: "public_fixture_fallback", Status: 403, Reason: "PERMISSION_DENIED"}}
	client.mu.Lock()
	client.base = spy
	client.mu.Unlock()
	cloud.setDrop(0, true)
	r := fsHybridStoreRecord(t, client, 1, 7)
	if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err != nil {
		t.Fatal(err)
	}
	client.forceFallback(r.Name)
	fsHybridEventually(t, func() bool { return client.Error() != nil })
	if spy.fallbacks.Load() != 1 || client.Snapshot()["fallback_attempts"] != 1 {
		t.Fatal("authorization failure was retried")
	}
}

func TestFSHybridStoreLateProcessingReceiptWinsOverFallbackNetworkFailure(t *testing.T) {
	client, exit, _, base := fsHybridMemoryStores(t)
	gate := make(chan struct{})
	spy := &fsHybridCapabilityBase{MemoryStore: base, fallbackGate: gate, fallbackErr: context.DeadlineExceeded}
	client.mu.Lock()
	client.base = spy
	client.mu.Unlock()
	seen := fsHybridReceiveChannel(exit)
	r := fsHybridStoreRecord(t, client, 1, 7)
	if _, err := client.PutObject(context.Background(), r.Name, r.Sealed); err != nil {
		t.Fatal(err)
	}
	client.forceFallback(r.Name)
	fsHybridEventually(t, func() bool { return spy.fallbacks.Load() == 1 })
	var info ObjectInfo
	select {
	case info = <-seen:
	case <-time.After(time.Second):
		t.Fatal("missing primary")
	}
	if err := exit.NoteMuxProcessed(info.Name, info.ID); err != nil {
		t.Fatal(err)
	}
	fsHybridEventually(t, func() bool { return client.Snapshot()["peer_processed_prefix"] == 1 })
	close(gate)
	fsHybridEventually(t, func() bool { return client.Snapshot()["fallback_errors"] == 1 })
	if client.Error() != nil || spy.fallbacks.Load() != 1 {
		t.Fatal("late receipt was replaced by failed immutable retry", client.Error())
	}
	client.mu.Lock()
	processed := client.state.outNames[r.Name].processed
	client.mu.Unlock()
	if !processed {
		t.Fatal("processing ownership lost")
	}
}
