package skirk

import (
	"net/http"
	"testing"
)

func TestIsGoogleFrontRoute(t *testing.T) {
	for _, mode := range []string{"google_front", "google_front_pinned"} {
		if !isGoogleFrontRoute(mode) {
			t.Fatalf("expected %s to be a Google-fronted route", mode)
		}
	}
	for _, mode := range []string{"", "direct", "real_pinned"} {
		if isGoogleFrontRoute(mode) {
			t.Fatalf("expected %s not to be a Google-fronted route", mode)
		}
	}
}

func TestShouldRetryDriveRateLimitResponses(t *testing.T) {
	rateLimited := &HTTPResult{
		Status: http.StatusForbidden,
		Body:   []byte(`{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`),
	}
	if !shouldRetryResult(rateLimited) {
		t.Fatal("expected Drive userRateLimitExceeded response to be retried")
	}
	ordinaryForbidden := &HTTPResult{
		Status: http.StatusForbidden,
		Body:   []byte(`{"error":{"message":"permission denied"}}`),
	}
	if shouldRetryResult(ordinaryForbidden) {
		t.Fatal("expected ordinary 403 response not to be retried")
	}
	if !shouldRetryResult(&HTTPResult{Status: http.StatusTooManyRequests}) {
		t.Fatal("expected 429 response to be retried")
	}
}
