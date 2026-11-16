package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
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
	for _, mode := range []string{"google_front", "google_front_pinned", "google_front_h3_pinned"} {
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

func TestGoogleHTTPClientReturnsRedirectWithoutChangingSelectedRoute(t *testing.T) {
	for _, route := range []RouteConfig{
		{Mode: "direct"},
		{Mode: "direct", Proxy: "socks5h://127.0.0.1:11093"},
		{Mode: "real_pinned", GoogleIP: "216.239.38.120"},
		{Mode: "google_front", Proxy: "socks5h://127.0.0.1:11093"},
		{Mode: "google_front_pinned", GoogleIP: "216.239.38.120"},
		{Mode: "google_front_h1", Proxy: "socks5h://127.0.0.1:11093"},
		{Mode: "google_front_h3_pinned", GoogleIP: "216.239.38.120"},
		{Mode: "google_front_h3_pinned", GoogleIP: "216.239.38.120", Proxy: "socks5h://127.0.0.1:11093"},
	} {
		t.Run(route.Mode+"/proxy="+strconv.FormatBool(route.Proxy != ""), func(t *testing.T) {
			client := NewGoogleHTTPClient(route)
			requests := 0
			client.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests != 1 {
					t.Fatalf("followed redirect to %q", req.URL.String())
				}
				wantURLHost := "www.googleapis.com"
				if isGoogleFrontRoute(route.Mode) {
					wantURLHost = "www.google.com"
				}
				if req.URL.Host != wantURLHost || req.Host != "www.googleapis.com" {
					t.Fatalf("first request URL host=%q authority=%q", req.URL.Host, req.Host)
				}
				response := stringResponse(http.StatusTemporaryRedirect, "redirect response")
				response.Header.Set("Location", "https://redirect.example:8443/other")
				return response, nil
			})
			result, err := client.Request(context.Background(), http.MethodPost, "www.googleapis.com", "/token",
				map[string]string{"Authorization": "Bearer test-token"}, []byte("refresh-token"))
			if err != nil || result == nil || result.Status != http.StatusTemporaryRedirect ||
				result.Header.Get("Location") != "https://redirect.example:8443/other" ||
				string(result.Body) != "redirect response" || result.Attempts != 1 || requests != 1 {
				t.Fatalf("redirect result=%+v err=%v requests=%d", result, err, requests)
			}
		})
	}
}

func TestGoogleH3PinnedDialKeepsTLSVerificationAndSNI(t *testing.T) {
	route := RouteConfig{Mode: "google_front_h3_pinned", GoogleIP: "216.239.38.120"}
	wantError := errors.New("dial test complete")
	transport := newGoogleH3Transport(route, func(ctx context.Context, address string, tlsConfig *stdtls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
		if address != net.JoinHostPort(route.GoogleIP, "443") {
			t.Errorf("UDP address=%q, want pinned Google IP", address)
		}
		if tlsConfig.ServerName != "www.google.com" || tlsConfig.InsecureSkipVerify {
			t.Errorf("TLS config SNI=%q skip_verify=%t", tlsConfig.ServerName, tlsConfig.InsecureSkipVerify)
		}
		if len(tlsConfig.NextProtos) != 1 || tlsConfig.NextProtos[0] != http3.NextProtoH3 {
			t.Errorf("QUIC ALPN=%v, want h3", tlsConfig.NextProtos)
		}
		if quicConfig.Allow0RTT {
			t.Error("0-RTT must be disabled")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > googleH3DialTimeout || time.Until(deadline) <= 0 {
			t.Errorf("QUIC dial deadline missing or unbounded: %v", deadline)
		}
		return nil, wantError
	})
	_, err := transport.Dial(context.Background(), "www.google.com:443", transport.TLSClientConfig, transport.QUICConfig)
	if !errors.Is(err, wantError) {
		t.Fatalf("dial error=%v, want test error", err)
	}
}

func TestGoogleH3FrontRequestPreservesDriveAuthority(t *testing.T) {
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_h3_pinned"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			t.Fatalf("front URL host=%q API authority=%q", req.URL.Host, req.Host)
		}
		if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("method=%q or authorization missing", req.Method)
		}
		if req.Method == http3.MethodGet0RTT || req.Method == http3.MethodHead0RTT {
			t.Fatal("early-data method on POST")
		}
		response := stringResponse(http.StatusOK, "{}")
		response.Proto = "HTTP/3.0"
		return response, nil
	})}}
	result, err := client.requestOnce(context.Background(), http.MethodPost, "www.googleapis.com", "/upload/drive/v3/files",
		map[string]string{"Authorization": "Bearer test-token"}, []byte("body"))
	if err != nil || result.Status != http.StatusOK || result.Proto != "HTTP/3.0" {
		t.Fatalf("request result=%+v error=%v", result, err)
	}
}

func TestGoogleH3OnlyFallsBackAfterPreRequestDialFailure(t *testing.T) {
	h3Calls, h2Calls := 0, 0
	transport := &googleH3FallbackTransport{
		h3: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			h3Calls++
			return nil, &googleH3DialError{cause: errors.New("UDP blocked")}
		}),
		h2: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			h2Calls++
			if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
				t.Fatalf("fallback host=%q authority=%q", req.URL.Host, req.Host)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != "same-upload-body" {
				t.Fatalf("fallback body=%q error=%v", body, err)
			}
			response := stringResponse(http.StatusOK, "{}")
			response.Proto = "HTTP/2.0"
			return response, nil
		}),
	}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, "https://www.google.com/upload", strings.NewReader("same-upload-body"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "www.googleapis.com"
		response, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.Proto != "HTTP/2.0" {
			t.Fatalf("fallback proto=%q, want HTTP/2.0", response.Proto)
		}
		response.Body.Close()
	}
	if h3Calls != 1 || h2Calls != 2 {
		t.Fatalf("H3 calls=%d H2 calls=%d; want one bounded H3 attempt and two H2 requests", h3Calls, h2Calls)
	}
}

func TestGoogleH3DoesNotFallbackOnPostRequestOrCertificateError(t *testing.T) {
	for _, upstreamErr := range []error{errors.New("request may have been sent"),
		&googleH3DialError{cause: x509.HostnameError{Host: "bad.example"}}} {
		fallbackCalls := 0
		transport := &googleH3FallbackTransport{
			h3: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, upstreamErr }),
			h2: roundTripFunc(func(*http.Request) (*http.Response, error) {
				fallbackCalls++
				return stringResponse(http.StatusOK, "{}"), nil
			}),
		}
		req, _ := http.NewRequest(http.MethodPost, "https://www.google.com/upload", strings.NewReader("body"))
		_, err := transport.RoundTrip(req)
		if !errors.Is(err, upstreamErr) || fallbackCalls != 0 {
			t.Fatalf("error=%v fallback calls=%d, want original error and no fallback", err, fallbackCalls)
		}
	}
}

func TestGoogleH3ProxyUsesExistingH2WithoutDirectUDP(t *testing.T) {
	route := RouteConfig{Mode: "google_front_h3_pinned", GoogleIP: "216.239.38.120", Proxy: "socks5h://127.0.0.1:11093"}
	client := NewGoogleHTTPClient(route)
	if _, ok := client.client.Transport.(*googleH3FallbackTransport); ok {
		t.Fatal("proxy route installed QUIC transport")
	}
	if _, ok := client.client.Transport.(*http2.Transport); !ok {
		t.Fatalf("proxy fallback transport=%T, want existing fronted HTTP/2", client.client.Transport)
	}
	if client.route.Mode != route.Mode || client.route.Proxy != route.Proxy || client.route.GoogleIP != route.GoogleIP {
		t.Fatalf("client route=%+v, want fronted pinned H3 selection through proxy", client.route)
	}
}

func TestGoogleFrontRouteProtocolSelection(t *testing.T) {
	for _, mode := range []string{"google_front", "google_front_pinned"} {
		if !isGoogleFrontHTTP2Route(mode) {
			t.Fatalf("expected %s to use HTTP/2 fronting", mode)
		}
	}
}

func TestGoogleHTTPRouteAttemptsStayWithinUserSelectedMode(t *testing.T) {
	for _, mode := range []string{"direct", "real_pinned"} {
		selected := RouteConfig{Mode: mode, Proxy: "socks5h://127.0.0.1:11093", GoogleIP: "216.239.38.120", TimeoutSeconds: 240}
		attempts := googleHTTPRouteAttempts(selected)
		if len(attempts) != 1 || !sameGoogleRoute(attempts[0], selected) {
			t.Fatalf("selected %s unexpectedly changed request route: %+v", mode, attempts)
		}
	}
	front := RouteConfig{Mode: "google_front", Proxy: "socks5h://127.0.0.1:11093"}
	if got := googleHTTPRouteAttempts(front); len(got) != 1 {
		t.Fatalf("clean fronted route attempts = %d, want 1", len(got))
	}

	pinned := RouteConfig{Mode: "google_front_pinned", Proxy: "socks5h://127.0.0.1:11093", GoogleIP: "216.239.38.120"}
	attempts := googleHTTPRouteAttempts(pinned)
	if len(attempts) != 1 || attempts[0].Mode != "google_front_pinned" || attempts[0].GoogleIP != pinned.GoogleIP {
		t.Fatalf("pinned route attempts = %+v", attempts)
	}
	frontH1 := RouteConfig{Mode: "google_front_h1", Proxy: "socks5h://127.0.0.1:11093"}
	attempts = googleHTTPRouteAttempts(frontH1)
	if len(attempts) != 2 || attempts[1].Mode != "google_front" || attempts[1].Proxy != frontH1.Proxy {
		t.Fatalf("front-to-front fallback attempts = %+v", attempts)
	}
}

func TestShouldRetryDriveRateLimitResponses(t *testing.T) {
	for _, body := range []string{
		`{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`,
		`{"error":{"code":403,"message":"Rate Limit Exceeded","status":"RATE_LIMIT_EXCEEDED"}}`,
		`{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`,
	} {
		if !shouldRetryResult(&HTTPResult{Status: http.StatusForbidden, Body: []byte(body)}) {
			t.Fatalf("expected Drive rate-limit response %s to be retried", body)
		}
	}
	ordinaryForbidden := &HTTPResult{
		Status: http.StatusForbidden,
		Body:   []byte(`{"error":{"message":"permission denied"}}`),
	}
	if shouldRetryResult(ordinaryForbidden) {
		t.Fatal("expected ordinary 403 response not to be retried")
	}
	if shouldRetryResult(&HTTPResult{Status: http.StatusForbidden, Body: []byte(`{"error":{"status":"PERMISSION_DENIED","message":"rateLimitExceeded is not the reason"}}`)}) {
		t.Fatal("expected a 403 with a non-rate-limit status not to be retried")
	}
	if shouldRetryResult(&HTTPResult{Status: http.StatusForbidden, Body: []byte(`{"error":{"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED"}]}}`)}) {
		t.Fatal("expected an ordinary ErrorInfo permission denial not to be retried")
	}
	if shouldRetryResult(&HTTPResult{Status: http.StatusForbidden, Body: []byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}],"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED"}]}}`)}) {
		t.Fatal("expected a specific ErrorInfo permission denial to take precedence over a legacy rate-limit reason")
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
