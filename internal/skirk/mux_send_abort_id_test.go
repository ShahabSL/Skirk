package skirk

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestMuxAbortedIDRefillDoesNotDisableHealthyReservations(t *testing.T) {
	for _, name := range []string{"canceled transport", "successful refill just before cancellation"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SKIRK_ACCESS_TOKEN", "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			var calls atomic.Int64
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files/generateIds" {
					return nil, errors.New("unexpected fixture request")
				}
				if calls.Add(1) == 1 {
					close(started)
					<-req.Context().Done()
					if name == "canceled transport" {
						return nil, req.Context().Err()
					}
				}
				return stringResponse(http.StatusOK, `{"ids":["offline-id", "offline-spare"]}`), nil
			})
			store := NewDriveStore(&GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"},
				client: &http.Client{Transport: transport}}, "fixture-only", DriveConfig{})
			mux := newSendAbortMux(t, store)
			type result struct {
				id  string
				err error
			}
			done := make(chan result, 1)
			go func() {
				id, err := mux.reserveUploadObjectID(ctx)
				done <- result{id: id, err: err}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("fixture ID refill did not start")
			}
			cancel()
			select {
			case canceled := <-done:
				if canceled.id != "" || !errors.Is(canceled.err, context.Canceled) {
					t.Fatalf("abort consumed an ID or lost the cancellation: %+v", canceled)
				}
			case <-time.After(time.Second):
				t.Fatal("aborted ID refill did not return")
			}
			if !mux.uploadIDReserveDisabledTill.IsZero() {
				t.Fatal("aborting a stream disabled the shared ID reservation pool")
			}
			wantCalls := int64(1)
			if name == "canceled transport" {
				wantCalls++
			}
			id, err := mux.reserveUploadObjectID(t.Context())
			if err != nil || id != "offline-id" || calls.Load() != wantCalls {
				t.Fatalf("healthy caller did not get the first ID: id=%q err=%v calls=%d", id, err, calls.Load())
			}
			if !slices.Equal(mux.uploadIDPool, []string{"offline-spare"}) {
				t.Fatal("remaining healthy reserved ID was lost")
			}
		})
	}
}

func TestMuxAbortedIDReservationLeavesPrefilledPool(t *testing.T) {
	mux := newSendAbortMux(t, NewMemoryStore())
	mux.uploadIDPool = []string{"offline-id"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	id, err := mux.reserveUploadObjectID(ctx)
	if id != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller returned id=%q err=%v", id, err)
	}
	if !slices.Equal(mux.uploadIDPool, []string{"offline-id"}) || !mux.uploadIDReserveDisabledTill.IsZero() {
		t.Fatal("canceled reservation changed the healthy pool")
	}
	id, err = mux.reserveUploadObjectID(t.Context())
	if err != nil || id != "offline-id" {
		t.Fatalf("healthy caller did not recover prefilled ID: id=%q err=%v", id, err)
	}
}
