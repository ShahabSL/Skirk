package skirk

import "testing"

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
