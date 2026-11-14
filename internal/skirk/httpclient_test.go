package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
)

func TestHTTPDiagnosticTimingOmitsCredentialsAndRequestDetails(t *testing.T) {
	t.Setenv("SKIRK_HTTP_TRACE", "1")
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil {
			t.Fatal("diagnostic trace not installed")
		}
		trace.GotConn(httptrace.GotConnInfo{Reused: true})
		trace.WroteRequest(httptrace.WroteRequestInfo{})
		return nil, errors.New("transport-error-private-marker")
	})}}
	_, _ = client.requestOnce(context.Background(), http.MethodPost, "private-host-marker", "/upload/drive/v3/files?private-query-marker",
		map[string]string{"Authorization": "Bearer private-token-marker"}, []byte("private-payload-marker"))
	text := output.String()
	if !strings.Contains(text, "http timing op=upload") || !strings.Contains(text, "reused=true") {
		t.Fatalf("missing bounded diagnostic fields: %s", text)
	}
	for _, marker := range []string{"private-host-marker", "private-query-marker", "private-token-marker", "private-payload-marker", "transport-error-private-marker"} {
		if strings.Contains(text, marker) {
			t.Fatalf("diagnostic exposed %s", marker)
		}
	}
}

func TestHTTPRetryReturnsLatestResponseAfterNetworkFailure(t *testing.T) {
	requests := 0
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, errors.New("temporary network failure")
		}
		return stringResponse(http.StatusBadRequest, `{"error":"bad request"}`), nil
	})}}
	result, err := client.Request(context.Background(), http.MethodGet, "www.googleapis.com", "/drive/v3/files", nil, nil)
	if err != nil || result == nil || result.Status != http.StatusBadRequest || result.Attempts != 2 {
		t.Fatalf("latest response = %+v, error=%v, requests=%d; want HTTP 400 after two attempts", result, err, requests)
	}
}

func TestHTTPFailedAttemptsSurviveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 2 {
			cancel()
		}
		return nil, errors.New("temporary network failure")
	})}}
	result, err := client.Request(ctx, http.MethodGet, "www.googleapis.com", "/drive/v3/files", nil, nil)
	if !errors.Is(err, context.Canceled) || httpAttemptCount(result, err) != 2 || requests != 2 {
		t.Fatalf("request error=%v attempts=%d requests=%d, want cancellation after two attempts", err, httpAttemptCount(result, err), requests)
	}
}

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

func TestGoogleHTTPClientCanReturnRateLimitWithoutRetry(t *testing.T) {
	requests := 0
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return stringResponse(http.StatusForbidden, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`), nil
	})}}
	result, err := client.RequestNoRateLimitRetry(context.Background(), http.MethodGet, "www.googleapis.com", "/drive/v3/files", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", result.Status)
	}
	if requests != 1 || result.Attempts != 1 {
		t.Fatalf("requests=%d result attempts=%d, want one immediate Drive rate-limit response", requests, result.Attempts)
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
