package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type HTTPResult struct {
	Status   int
	Body     []byte
	Header   http.Header
	Proto    string
	Attempts int
	// Bounds of the HTTP exchange, excluding token refresh and quota backoff.
	RequestStarted   time.Time
	ResponseReceived time.Time
}

type googleResponseLimitKey struct{}

var errGoogleResponseTooLarge = errors.New("Google response exceeded configured byte limit")

func withGoogleResponseLimit(ctx context.Context, limit int64) context.Context {
	return context.WithValue(ctx, googleResponseLimitKey{}, limit)
}

func withGoogleResponseLimitAtMost(ctx context.Context, limit int64) context.Context {
	if current, ok := ctx.Value(googleResponseLimitKey{}).(int64); ok && current > 0 && current <= limit {
		return ctx
	}
	return withGoogleResponseLimit(ctx, limit)
}

// Failed network attempts may have reached Google even without a response.
// Preserve their count so quota estimates include the complete retry path.
type httpAttemptError struct {
	err      error
	attempts int
}

func (e *httpAttemptError) Error() string { return e.err.Error() }
func (e *httpAttemptError) Unwrap() error { return e.err }

func httpAttemptCount(result *HTTPResult, err error) int {
	var attemptErr *httpAttemptError
	if errors.As(err, &attemptErr) {
		return attemptErr.attempts
	}
	if result != nil && result.Attempts > 0 {
		return result.Attempts
	}
	return 1
}

type GoogleAPIError struct {
	Op      string
	Status  int
	Reason  string
	Message string
	Body    string
}

func (e *GoogleAPIError) Error() string {
	if e == nil {
		return ""
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "unknown"
	}
	return fmt.Sprintf("%s failed status=%d reason=%s body=%q", e.Op, e.Status, reason, e.Body)
}

func (e *GoogleAPIError) IsStorageQuotaExceeded() bool {
	if e == nil {
		return false
	}
	return normalizeGoogleErrorReason(e.Reason) == "storagequotaexceeded"
}

type GoogleHTTPClient struct {
	client *http.Client
	route  RouteConfig
}

func keepGoogleHTTPRedirectResponse(*http.Request, []*http.Request) error {
	// Redirects can change the dial target and SNI without reapplying the
	// selected route. Let the caller handle the original 3xx response.
	return http.ErrUseLastResponse
}

const googleH3DialTimeout = 2500 * time.Millisecond
const googleH3RetryCooldown = time.Minute

// This only marks a failure while establishing the QUIC connection, before an
// HTTP request can be sent. It is safe to replay the request over HTTP/2 then.
type googleH3DialError struct{ cause error }

func (e *googleH3DialError) Error() string { return e.cause.Error() }
func (e *googleH3DialError) Unwrap() error { return e.cause }

type googleH3FallbackTransport struct {
	h3               http.RoundTripper
	h2               http.RoundTripper
	unavailableUntil atomic.Int64
}

func (t *googleH3FallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if time.Now().UnixNano() < t.unavailableUntil.Load() {
		return t.h2.RoundTrip(req)
	}
	response, err := t.h3.RoundTrip(req)
	if err == nil {
		return response, nil
	}
	var dialError *googleH3DialError
	if !errors.As(err, &dialError) || req.Context().Err() != nil || isGoogleH3CertificateError(err) {
		return response, err
	}
	// The QUIC handshake failed before HTTP headers or body were sent. Recreate
	// the body because RoundTrip implementations may close the original reader.
	fallbackRequest := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return response, err
		}
		fallbackRequest.Body, err = req.GetBody()
		if err != nil {
			return response, err
		}
	}
	now := time.Now().UnixNano()
	previous := t.unavailableUntil.Swap(now + int64(googleH3RetryCooldown))
	if previous <= now {
		log.Print("google front HTTP/3 handshake unavailable; using HTTP/2 temporarily")
	}
	return t.h2.RoundTrip(fallbackRequest)
}

func isGoogleH3CertificateError(err error) bool {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	var verification *stdtls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &roots) || errors.As(err, &verification)
}

func googleH3DialTarget(route RouteConfig, addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err == nil && port == "443" && route.GoogleIP != "" && shouldPinGoogleIP(route.Mode) {
		return net.JoinHostPort(route.GoogleIP, port)
	}
	return addr
}

func newGoogleH3Transport(route RouteConfig, dial func(context.Context, string, *stdtls.Config, *quic.Config) (*quic.Conn, error)) *http3.Transport {
	return &http3.Transport{
		TLSClientConfig: &stdtls.Config{
			ServerName: "www.google.com", MinVersion: stdtls.VersionTLS13,
			NextProtos: []string{http3.NextProtoH3},
		},
		QUICConfig: &quic.Config{HandshakeIdleTimeout: googleH3DialTimeout, Allow0RTT: false},
		Dial: func(ctx context.Context, addr string, tlsConfig *stdtls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, googleH3DialTimeout)
			defer cancel()
			verifiedTLS := tlsConfig.Clone()
			verifiedTLS.ServerName = "www.google.com"
			verifiedTLS.InsecureSkipVerify = false
			connection, err := dial(dialCtx, googleH3DialTarget(route, addr), verifiedTLS, quicConfig)
			if err != nil {
				return nil, &googleH3DialError{cause: err}
			}
			return connection, nil
		},
	}
}

func NewGoogleHTTPClient(route RouteConfig) *GoogleHTTPClient {
	if route.TimeoutSeconds == 0 {
		route.TimeoutSeconds = 240
	}
	if route.Mode == "google_front_h3_pinned" {
		h2Route := route
		h2Route.Mode = "google_front_pinned"
		h2Client := NewGoogleHTTPClient(h2Route)
		if route.Proxy != "" {
			// SOCKS-only routes must not leak traffic via direct UDP.
			return &GoogleHTTPClient{client: h2Client.client, route: route}
		}
		h3Transport := newGoogleH3Transport(route, quic.DialAddr)
		transport := &googleH3FallbackTransport{h3: h3Transport, h2: h2Client.client.Transport}
		return &GoogleHTTPClient{
			client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second, CheckRedirect: keepGoogleHTTPRedirectResponse},
			route:  route,
		}
	}
	baseDialer := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		target := addr
		if route.GoogleIP != "" && port == "443" && shouldPinGoogleIP(route.Mode) {
			target = net.JoinHostPort(route.GoogleIP, port)
		} else if host == "" {
			target = addr
		}
		if route.Proxy != "" {
			return dialViaSOCKS5(ctx, route.Proxy, target)
		}
		return baseDialer.DialContext(ctx, network, target)
	}
	if isGoogleFrontHTTP2Route(route.Mode) {
		transport := &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, _ *stdtls.Config) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				raw, err := dialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				handshakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				uconn := utls.UClient(raw, &utls.Config{
					ServerName: host,
					MinVersion: utls.VersionTLS12,
				}, utls.HelloChrome_Auto)
				if err := uconn.HandshakeContext(handshakeCtx); err != nil {
					_ = raw.Close()
					return nil, err
				}
				return uconn, nil
			},
			ReadIdleTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		}
		return &GoogleHTTPClient{
			client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second, CheckRedirect: keepGoogleHTTPRedirectResponse},
			route:  route,
		}
	}
	tlsDialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := dialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		handshakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		clientHelloID := utls.HelloChrome_Auto
		if isGoogleFrontHTTP1Route(route.Mode) {
			clientHelloID = utls.HelloRandomizedNoALPN
		}
		uconn := utls.UClient(raw, &utls.Config{
			ServerName: host,
			MinVersion: utls.VersionTLS12,
		}, clientHelloID)
		if err := uconn.HandshakeContext(handshakeCtx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return uconn, nil
	}
	transport := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     !isGoogleFrontHTTP1Route(route.Mode),
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Duration(route.TimeoutSeconds) * time.Second,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       &stdtls.Config{MinVersion: stdtls.VersionTLS12},
	}
	if isGoogleFrontHTTP1Route(route.Mode) {
		transport.DialTLSContext = tlsDialContext
	}
	return &GoogleHTTPClient{
		client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second, CheckRedirect: keepGoogleHTTPRedirectResponse},
		route:  route,
	}
}

func (c *GoogleHTTPClient) Request(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	return c.request(ctx, method, host, path, headers, body, true)
}

func (c *GoogleHTTPClient) RequestNoRateLimitRetry(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	return c.request(ctx, method, host, path, headers, body, false)
}

// RequestSingleAttempt is used by mutable protocol records. An uncertain
// PATCH must not be transparently replayed after a transport timeout.
func (c *GoogleHTTPClient) RequestSingleAttempt(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	result, err := c.requestOnce(ctx, method, host, path, headers, body)
	if result != nil && result.Attempts < 1 {
		result.Attempts = 1
	}
	return result, err
}

func (c *GoogleHTTPClient) request(ctx context.Context, method, host, path string, headers map[string]string, body []byte, retryRateLimit bool) (*HTTPResult, error) {
	var lastErr error
	var lastResult *HTTPResult
	totalAttempts := 0
	for i, route := range googleHTTPRouteAttempts(c.route) {
		client := c
		if i > 0 {
			client = NewGoogleHTTPClient(route)
		}
		result, err := client.requestWithRetries(ctx, method, host, path, headers, body, retryRateLimit)
		totalAttempts += httpAttemptCount(result, err)
		if result != nil {
			result.Attempts = totalAttempts
		}
		if err == nil && !shouldRetryResultWithPolicy(result, retryRateLimit) {
			return result, nil
		}
		lastResult, lastErr = result, err
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, errGoogleResponseTooLarge) {
			break
		}
		if !shouldTryNextGoogleRoute(result, err) {
			break
		}
	}
	if lastErr != nil {
		return lastResult, &httpAttemptError{err: lastErr, attempts: totalAttempts}
	}
	return lastResult, nil
}

func (c *GoogleHTTPClient) requestWithRetries(ctx context.Context, method, host, path string, headers map[string]string, body []byte, retryRateLimit bool) (*HTTPResult, error) {
	attempts := 4
	if isGoogleFrontRoute(c.route.Mode) {
		attempts = 5
	}
	var lastErr error
	var lastResult *HTTPResult
	attempted := 0
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := c.requestOnce(ctx, method, host, path, headers, body)
		attempted++
		if result != nil {
			result.Attempts = attempt + 1
		}
		if err == nil && !shouldRetryResultWithPolicy(result, retryRateLimit) {
			return result, nil
		}
		lastResult, lastErr = result, err
		if errors.Is(err, errGoogleResponseTooLarge) {
			break
		}
		if attempt == attempts-1 {
			break
		}
		if err := sleepBeforeRetry(ctx, attempt, retryAfter(result)); err != nil {
			return lastResult, &httpAttemptError{err: err, attempts: attempted}
		}
	}
	if lastErr != nil {
		return lastResult, &httpAttemptError{err: lastErr, attempts: attempted}
	}
	return lastResult, nil
}

func googleHTTPRouteAttempts(route RouteConfig) []RouteConfig {
	routes := []RouteConfig{route}
	if !needsGoogleFrontFallback(route) {
		return routes
	}
	front := route
	front.Mode = "google_front"
	front.GoogleIP = ""
	if !sameGoogleRoute(route, front) {
		routes = append(routes, front)
	}
	return routes
}

func needsGoogleFrontFallback(route RouteConfig) bool {
	// Route selection is user-owned. A Direct request, including one sent
	// through SOCKS, must never silently switch to the Google front.
	if !isGoogleFrontRoute(route.Mode) {
		return false
	}
	if route.GoogleIP != "" && shouldPinGoogleIP(route.Mode) {
		return false
	}
	if route.Mode == "google_front" && route.GoogleIP == "" {
		return false
	}
	return true
}

func sameGoogleRoute(a, b RouteConfig) bool {
	return a.Mode == b.Mode && a.Proxy == b.Proxy && a.GoogleIP == b.GoogleIP && a.TimeoutSeconds == b.TimeoutSeconds
}

func shouldTryNextGoogleRoute(result *HTTPResult, err error) bool {
	if err != nil {
		return true
	}
	if result == nil {
		return true
	}
	switch result.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusNotFound, http.StatusMisdirectedRequest:
		return true
	default:
		return false
	}
}

func (c *GoogleHTTPClient) requestOnce(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (out *HTTPResult, outErr error) {
	status := 0
	ctx, stallAttempt, stallErr := startDriveStallHTTPAttempt(ctx, method, path, len(body))
	if stallErr != nil {
		return nil, stallErr
	}
	if stallAttempt != nil {
		defer func() {
			if out != nil {
				out.Attempts = stallAttempt.count()
			}
			if outErr != nil {
				outErr = &httpAttemptError{err: outErr, attempts: stallAttempt.count()}
			}
		}()
	}
	if os.Getenv("SKIRK_HTTP_TRACE") == "1" {
		started := time.Now()
		var acquired, wrote, firstByte atomic.Int64
		var reused, writeFailed atomic.Bool
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				acquired.Store(time.Since(started).Microseconds())
				reused.Store(info.Reused)
			},
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				wrote.Store(time.Since(started).Microseconds())
				writeFailed.Store(info.Err != nil)
			},
			GotFirstResponseByte: func() { firstByte.Store(time.Since(started).Microseconds()) },
		})
		defer func() {
			// Deliberately omit URLs, headers, payloads and credentials.
			if time.Since(started) >= time.Second || status == 0 {
				log.Printf("http timing op=%s body_bytes=%d duration=%s acquired_ms=%.3f reused=%t wrote_ms=%.3f write_failed=%t first_byte_ms=%.3f status=%d canceled=%t",
					driveRequestLabel(method, path), len(body), time.Since(started).Round(time.Millisecond), float64(acquired.Load())/1000,
					reused.Load(), float64(wrote.Load())/1000, writeFailed.Load(), float64(firstByte.Load())/1000, status, ctx.Err() != nil)
			}
		}()
	}
	requestHost := host
	if isGoogleFrontRoute(c.route.Mode) {
		requestHost = "www.google.com"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	requestURL := "https://" + requestHost + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, err
	}
	if stallAttempt != nil {
		// The coordinator owns retries and duplicate-byte reservations. Do
		// not let net/http replay this body behind that accounting boundary.
		req.GetBody = nil
	}
	if isGoogleFrontRoute(c.route.Mode) {
		req.Host = host
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "skirk/1.0 (gzip)")
	} else if !strings.Contains(strings.ToLower(req.Header.Get("User-Agent")), "gzip") {
		req.Header.Set("User-Agent", req.Header.Get("User-Agent")+" (gzip)")
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if stallAttempt != nil {
		stallAttempt.gotHeaders()
	}
	status = resp.StatusCode
	bodyReader := resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		bodyReader = gzipReader
		resp.Header.Del("Content-Encoding")
	}
	responseLimit, _ := ctx.Value(googleResponseLimitKey{}).(int64)
	if responseLimit > 0 {
		bodyReader = io.NopCloser(io.LimitReader(bodyReader, responseLimit+1))
	}
	responseBody, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, err
	}
	if responseLimit > 0 && int64(len(responseBody)) > responseLimit {
		return nil, errGoogleResponseTooLarge
	}
	return &HTTPResult{Status: resp.StatusCode, Body: responseBody, Header: resp.Header, Proto: resp.Proto}, nil
}

func isGoogleFrontRoute(mode string) bool {
	return isGoogleFrontHTTP2Route(mode) || isGoogleFrontHTTP1Route(mode) || mode == "google_front_h3_pinned"
}

func isGoogleFrontHTTP2Route(mode string) bool {
	return mode == "google_front" || mode == "google_front_pinned"
}

func isGoogleFrontHTTP1Route(mode string) bool {
	return mode == "google_front_h1" || mode == "google_front_h1_pinned"
}

func shouldPinGoogleIP(mode string) bool {
	switch mode {
	case "real_pinned", "google_front_pinned", "google_front_h1_pinned", "google_front_h3_pinned":
		return true
	default:
		return false
	}
}

func shouldRetryResult(result *HTTPResult) bool {
	return shouldRetryResultWithPolicy(result, true)
}

func shouldRetryResultWithPolicy(result *HTTPResult, retryRateLimit bool) bool {
	if result == nil {
		return false
	}
	if result.Status == http.StatusRequestTimeout || result.Status >= 500 {
		return true
	}
	if !retryRateLimit {
		return false
	}
	if result.Status == http.StatusTooManyRequests {
		return true
	}
	if result.Status != http.StatusForbidden {
		return false
	}
	reason, _ := googleAPIErrorDetails(result.Body)
	switch normalizeGoogleErrorReason(reason) {
	case "ratelimitexceeded", "userratelimitexceeded":
		return true
	default:
		return false
	}
}

func normalizeGoogleErrorReason(reason string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(reason)), "_", "")
}

func retryAfter(result *HTTPResult) time.Duration {
	if result == nil {
		return 0
	}
	value := strings.TrimSpace(result.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return time.Until(when)
	}
	return 0
}

func sleepBeforeRetry(ctx context.Context, attempt int, serverDelay time.Duration) error {
	delay := 300 * time.Millisecond * time.Duration(1<<attempt)
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	delay += time.Duration(rand.Intn(300)) * time.Millisecond
	if serverDelay > delay {
		delay = serverDelay
	}
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func require2xx(result *HTTPResult, op string) error {
	if result.Status >= 200 && result.Status < 300 {
		return nil
	}
	body := string(result.Body)
	if len(body) > 500 {
		body = body[:500]
	}
	reason, message := googleAPIErrorDetails(result.Body)
	return &GoogleAPIError{
		Op:      op,
		Status:  result.Status,
		Reason:  reason,
		Message: message,
		Body:    body,
	}
}

func googleAPIErrorDetails(body []byte) (string, string) {
	var payload struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
			} `json:"details"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ""
	}
	// ErrorInfo is the specific cause; the top-level status can be the generic
	// PERMISSION_DENIED even when the request was rate limited.
	for _, detail := range payload.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && detail.Reason != "" {
			return detail.Reason, payload.Error.Message
		}
	}
	if len(payload.Error.Errors) > 0 && payload.Error.Errors[0].Reason != "" {
		return payload.Error.Errors[0].Reason, payload.Error.Message
	}
	if payload.Error.Status != "" {
		return payload.Error.Status, payload.Error.Message
	}
	return "", payload.Error.Message
}
