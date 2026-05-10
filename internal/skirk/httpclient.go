package skirk

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
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
	transport := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Duration(route.TimeoutSeconds) * time.Second,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &GoogleHTTPClient{
		client: &http.Client{Transport: transport, Timeout: time.Duration(route.TimeoutSeconds) * time.Second},
		route:  route,
	}
}

func (c *GoogleHTTPClient) Request(ctx context.Context, method, host, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
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
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &HTTPResult{Status: resp.StatusCode, Body: responseBody, Header: resp.Header}, nil
}

func isGoogleFrontRoute(mode string) bool {
	return mode == "google_front" || mode == "google_front_pinned"
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
