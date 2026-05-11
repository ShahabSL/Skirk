package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	stdtls "crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type HTTPResult struct {
	Status int
	Body   []byte
	Header http.Header
}

type GoogleHTTPClient struct {
	client *http.Client
	route  RouteConfig
}

func NewGoogleHTTPClient(route RouteConfig) *GoogleHTTPClient {
	if route.TimeoutSeconds == 0 {
		route.TimeoutSeconds = 240
	}
	baseDialer := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		target := addr
		if route.GoogleIP != "" && port == "443" && route.Mode != "direct" && route.Mode != "google_front" {
			target = net.JoinHostPort(route.GoogleIP, port)
		} else if host == "" {
			target = addr
		}
		if route.Proxy != "" {
			return dialViaSOCKS5(ctx, route.Proxy, target)
		}
		return baseDialer.DialContext(ctx, network, target)
	}
	if isGoogleFrontRoute(route.Mode) {
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
			client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second},
			route:  route,
		}
	}
	transport := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Duration(route.TimeoutSeconds) * time.Second,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       &stdtls.Config{MinVersion: stdtls.VersionTLS12},
	}
	return &GoogleHTTPClient{
		client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second},
		route:  route,
	}
}

func (c *GoogleHTTPClient) Request(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	attempts := 4
	if isGoogleFrontRoute(c.route.Mode) {
		attempts = 5
	}
	var lastErr error
	var lastResult *HTTPResult
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := c.requestOnce(ctx, method, host, path, headers, body)
		if err == nil && !shouldRetryResult(result) {
			return result, nil
		}
		if err == nil {
			lastResult = result
		} else {
			lastErr = err
		}
		if attempt == attempts-1 {
			break
		}
		if err := sleepBeforeRetry(ctx, attempt, retryAfter(result)); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return lastResult, err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return lastResult, nil
}

func (c *GoogleHTTPClient) requestOnce(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
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
	responseBody, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, err
	}
	return &HTTPResult{Status: resp.StatusCode, Body: responseBody, Header: resp.Header}, nil
}

func isGoogleFrontRoute(mode string) bool {
	return mode == "google_front" || mode == "google_front_pinned"
}

func shouldRetryResult(result *HTTPResult) bool {
	if result == nil {
		return false
	}
	if result.Status == http.StatusRequestTimeout || result.Status == http.StatusTooManyRequests || result.Status >= 500 {
		return true
	}
	if result.Status != http.StatusForbidden {
		return false
	}
	body := string(result.Body)
	return strings.Contains(body, "rateLimitExceeded") || strings.Contains(body, "userRateLimitExceeded")
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
	return fmt.Errorf("%s failed status=%d body=%q", op, result.Status, body)
}
