package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsGoogleFrontRoute(t *testing.T) {
	for _, mode := range []string{"google_front", "google_front_pinned"} {
		if !isGoogleFrontRoute(mode) {
			t.Fatalf("expected %s to be a Google-fronted route", mode)
		}
	}
	for _, mode := range []string{"", "direct", "real_pinned", "unknown_route"} {
		if isGoogleFrontRoute(mode) {
			t.Fatalf("expected %s not to be a Google-fronted route", mode)
		}
	}
}

func TestGoogleFrontRouteProtocolSelection(t *testing.T) {
	for _, mode := range []string{"google_front", "google_front_pinned"} {
		if !isGoogleFrontHTTP2Route(mode) {
			t.Fatalf("expected %s to use HTTP/2 fronting", mode)
		}
	}
}

func TestGoogleHTTPRouteAttemptsAddGoogleSNIFallback(t *testing.T) {
	directProxy := RouteConfig{Mode: "direct", Proxy: "socks5h://127.0.0.1:11093", GoogleIP: "216.239.38.120", TimeoutSeconds: 240}
	attempts := googleHTTPRouteAttempts(directProxy)
	if len(attempts) != 2 {
		t.Fatalf("direct proxy attempts = %d, want 2", len(attempts))
	}
	if attempts[0].Mode != "direct" || attempts[1].Mode != "google_front" || attempts[1].Proxy != directProxy.Proxy || attempts[1].GoogleIP != "" {
		t.Fatalf("direct proxy attempts = %+v", attempts)
	}

	front := RouteConfig{Mode: "google_front", Proxy: "socks5h://127.0.0.1:11093"}
	if got := googleHTTPRouteAttempts(front); len(got) != 1 {
		t.Fatalf("clean fronted route attempts = %d, want 1", len(got))
	}

	pinned := RouteConfig{Mode: "google_front_pinned", Proxy: "socks5h://127.0.0.1:11093", GoogleIP: "216.239.38.120"}
	attempts = googleHTTPRouteAttempts(pinned)
	if len(attempts) != 1 || attempts[0].Mode != "google_front_pinned" || attempts[0].GoogleIP != pinned.GoogleIP {
		t.Fatalf("pinned route attempts = %+v", attempts)
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

func TestGoogleHTTPClientRequestsAndDecodesGzip(t *testing.T) {
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Accept-Encoding") != "gzip" {
			t.Fatalf("Accept-Encoding = %q, want gzip", req.Header.Get("Accept-Encoding"))
		}
		if !strings.Contains(strings.ToLower(req.Header.Get("User-Agent")), "gzip") {
			t.Fatalf("User-Agent = %q, want gzip marker", req.Header.Get("User-Agent"))
		}
		var body bytes.Buffer
		writer := gzip.NewWriter(&body)
		if _, err := writer.Write([]byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Encoding": []string{"gzip"}},
			Body:       io.NopCloser(bytes.NewReader(body.Bytes())),
		}, nil
	})}}
	result, err := client.Request(context.Background(), http.MethodGet, "www.googleapis.com", "/drive/v3/files", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != `{"ok":true}` {
		t.Fatalf("body = %q", result.Body)
	}
	if result.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be cleared after decompression, got %q", result.Header.Get("Content-Encoding"))
	}
}
