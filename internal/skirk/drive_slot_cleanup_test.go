package skirk

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

const slotMissingBody = `{"error":{"code":404,"errors":[{"reason":"notFound"}]}}`

func cleanupProbeStore(t *testing.T, transport roundTripFunc) *DriveStore {
	t.Helper()
	return slotTestStore(t, DriveConfig{FolderID: "folder-1", MetadataSlotV1: true}, transport)
}

func TestSlotCleanupProbeFourReadbacksKeepCallsAndUnits(t *testing.T) {
	store := cleanupProbeStore(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("unexpected method", req.Method)
		}
		return stringResponse(http.StatusNotFound, slotMissingBody), nil
	})
	for _, id := range []string{"slot-up", "slot-down"} {
		ctx := withMetadataSlotCleanupProbe(context.Background(), id, testSlotName)
		for i := 0; i < 2; i++ {
			if _, err := store.GetMetadataSlot(ctx, id, testSlotName); !isDriveNotFound(err) {
				t.Fatal(err)
			}
		}
	}
	q := store.QuotaSnapshot()
	op := q.Ops["slot_cleanup_probe"]
	if q.Calls != 4 || q.Units != 20 || q.Errors != 0 || op.Calls != 4 || op.Units != 20 || op.Errors != 0 {
		t.Fatalf("expected404 accounting %+v, op=%+v", q, op)
	}
}

func TestSlotCleanupProbeActualRetirementMarksOnlyPostDeleteReadbacks(t *testing.T) {
	var sequence []string
	store := cleanupProbeStore(t, func(req *http.Request) (*http.Response, error) {
		sequence = append(sequence, req.Method)
		if req.Method == http.MethodDelete {
			return stringResponse(http.StatusNoContent, ""), nil
		}
		return stringResponse(http.StatusNotFound, slotMissingBody), nil
	})
	mux := &driveMux{t: &Tunnel{Data: store}}
	if !mux.deleteMuxSlotTargets(store, store, []muxSlotCleanupTarget{{"slot-up", testSlotName}, {"slot-down", testSlotName}}) {
		t.Fatal("exact owned cleanup did not confirm absence")
	}
	want := []string{"DELETE", "GET", "GET", "DELETE", "GET", "GET"}
	if len(sequence) != len(want) {
		t.Fatal(sequence)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatal(sequence)
		}
	}
	q := store.QuotaSnapshot()
	if q.Calls != 6 || q.Units != 120 || q.Errors != 0 || q.Ops["slot_cleanup_probe"].Calls != 4 || q.Ops["slot_cleanup_probe"].Units != 20 {
		t.Fatalf("retirement accounting %+v", q)
	}
}

func TestSlotCleanupProbeOrdinaryAndMismatchedLookupsRemainErrors(t *testing.T) {
	for _, mode := range []string{"ordinary", "wrong-id", "wrong-name"} {
		t.Run(mode, func(t *testing.T) {
			store := cleanupProbeStore(t, func(*http.Request) (*http.Response, error) { return stringResponse(404, slotMissingBody), nil })
			ctx := context.Background()
			switch mode {
			case "wrong-id":
				ctx = withMetadataSlotCleanupProbe(ctx, "another-slot", testSlotName)
			case "wrong-name":
				ctx = withMetadataSlotCleanupProbe(ctx, "slot-up", testSlotName+"-other")
			}
			if _, err := store.GetMetadataSlot(ctx, "slot-up", testSlotName); !isDriveNotFound(err) {
				t.Fatal(err)
			}
			q := store.QuotaSnapshot()
			if q.Calls != 1 || q.Units != 5 || q.Errors != 1 || q.Ops["metadata"].Errors != 1 || q.Ops["slot_cleanup_probe"].Calls != 0 {
				t.Fatalf("ordinary404 suppressed: %+v", q)
			}
		})
	}
}

func TestSlotCleanupProbeWrongStatusesAndTransportErrorsRemainErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			store := cleanupProbeStore(t, func(*http.Request) (*http.Response, error) { return stringResponse(status, `{"error":{}}`), nil })
			ctx := withMetadataSlotCleanupProbe(context.Background(), "slot-up", testSlotName)
			if _, err := store.GetMetadataSlot(ctx, "slot-up", testSlotName); err == nil {
				t.Fatal("status accepted", status)
			}
			q := store.QuotaSnapshot()
			if q.Calls < 1 || q.Errors != q.Calls || q.Units != 5*q.Calls {
				t.Fatalf("status suppressed %+v", q)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		store := cleanupProbeStore(t, func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		ctx = withMetadataSlotCleanupProbe(ctx, "slot-up", testSlotName)
		if _, err := store.GetMetadataSlot(ctx, "slot-up", testSlotName); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		q := store.QuotaSnapshot()
		if q.Calls != 1 || q.Units != 5 || q.Errors != 1 {
			t.Fatalf("timeout suppressed %+v", q)
		}
	})
}

func TestSlotCleanupProbeRecoveredServerFailureStaysCounted(t *testing.T) {
	var calls atomic.Int32
	store := cleanupProbeStore(t, func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return stringResponse(500, `{"error":{}}`), nil
		}
		return stringResponse(404, slotMissingBody), nil
	})
	ctx := withMetadataSlotCleanupProbe(context.Background(), "slot-up", testSlotName)
	if _, err := store.GetMetadataSlot(ctx, "slot-up", testSlotName); !isDriveNotFound(err) {
		t.Fatal(err)
	}
	q := store.QuotaSnapshot()
	if q.Calls != 2 || q.Units != 10 || q.Errors != 1 || q.Ops["slot_cleanup_probe"].Errors != 1 {
		t.Fatalf("recovered failure erased %+v", q)
	}
}

func TestSlotCleanupProbeDoesNotQuietOrdinaryDeleteNotFound(t *testing.T) {
	store := cleanupProbeStore(t, func(*http.Request) (*http.Response, error) { return stringResponse(404, slotMissingBody), nil })
	if err := store.DeleteID(context.Background(), "slot-up"); err != nil {
		t.Fatal(err)
	}
	q := store.QuotaSnapshot()
	if q.Calls != 1 || q.Units != 50 || q.Errors != 1 || q.Ops["delete"].Errors != 1 {
		t.Fatalf("delete accounting broadened %+v", q)
	}
}

func TestSlotCleanupProbeMarkerDoesNotQuietMediaGet(t *testing.T) {
	store := cleanupProbeStore(t, func(*http.Request) (*http.Response, error) {
		return stringResponse(404, slotMissingBody), nil
	})
	ctx := withMetadataSlotCleanupProbe(context.Background(), "slot-up", testSlotName)
	if _, err := store.GetMetadataSlot(ctx, "slot-up", testSlotName); !isDriveNotFound(err) {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, "slot-up"); !isDriveNotFound(err) {
		t.Fatal(err)
	}
	q := store.QuotaSnapshot()
	media := q.Ops["download"]
	if media.Calls < 1 || media.Errors != media.Calls || q.Calls != 1+media.Calls || q.Units != 5+200*media.Calls || q.Errors != media.Calls {
		t.Fatalf("slot marker leaked into media read %+v", q)
	}
}
