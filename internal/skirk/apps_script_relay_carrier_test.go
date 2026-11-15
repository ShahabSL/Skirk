package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type relayResearchRetryAfter struct{ delay time.Duration }

func (e *relayResearchRetryAfter) Error() string { return "relay Google HTTP temporarily unavailable" }

type relayResearchStageError struct{ stage string }

func (e *relayResearchStageError) Error() string { return "relay rejected stage=" + e.stage }
func (e *relayResearchStageError) Unwrap() error { return errRelayResearchConflict }

func relayResearchSafeStage(stage string) bool {
	switch stage {
	case "expired", "auth", "request_bound", "request_shape", "fetch_failed", "response_bound":
		return true
	}
	if len(stage) == len("relay_http_")+3 && strings.HasPrefix(stage, "relay_http_") {
		n, e := strconv.Atoi(strings.TrimPrefix(stage, "relay_http_"))
		return e == nil && n >= 100 && n <= 599
	}
	return false
}

// Limits count visible HTTP dispatches and reserve the x/net/http2 v0.61
// internal replay upper bound before dispatch. Body limits exclude TLS/headers;
// these are not a claim to observe Google's accepted or billed request count.
type relayResearchCarrierBudget struct {
	Statuses                                                                                                  map[int]int64
	Stages                                                                                                    map[string]int64
	HTTP2Responses, TransportErrors                                                                           int64
	mu                                                                                                        sync.Mutex
	MaxAttempts, MaxBodyBytes, MaxPosts                                                                       int64
	Posts                                                                                                     int64
	Dispatches, AttemptUpper, RequestBodyUpper, ResponseBodyUpper, ExposedResponseBytes, PendingResponseBytes int64
}

func (b *relayResearchCarrierBudget) reserve(request, response int, scriptPost bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if scriptPost && (b.MaxPosts <= 0 || b.Posts >= b.MaxPosts) || b.AttemptUpper+8 > b.MaxAttempts || b.RequestBodyUpper+b.ResponseBodyUpper+8*int64(request)+int64(response) > b.MaxBodyBytes {
		return errRelayResearchBounds
	}
	b.Dispatches++
	if scriptPost {
		b.Posts++
	}
	b.AttemptUpper += 8
	b.RequestBodyUpper += 8 * int64(request)
	b.ResponseBodyUpper += int64(response)
	b.PendingResponseBytes += int64(response)
	return nil
}
func (b *relayResearchCarrierBudget) finish(reserved, received int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.PendingResponseBytes -= int64(reserved)
	b.ExposedResponseBytes += int64(received)
}
func (b *relayResearchCarrierBudget) snapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := map[string]int64{"dispatches": b.Dispatches, "script_posts": b.Posts, "urlfetch_attempts_upper": b.Posts * 8, "attempts_upper": b.AttemptUpper, "request_body_bytes_upper": b.RequestBodyUpper, "response_body_bytes_upper": b.ResponseBodyUpper, "returned_response_body_bytes": b.ExposedResponseBytes, "pending_response_bytes": b.PendingResponseBytes, "http2_responses": b.HTTP2Responses, "transport_errors": b.TransportErrors}
	for code, n := range b.Statuses {
		snapshot["http_status_"+strconv.Itoa(code)] = n
	}
	for stage, n := range b.Stages {
		snapshot["stage_"+stage] = n
	}
	return snapshot
}

func (b *relayResearchCarrierBudget) observe(result *HTTPResult, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.TransportErrors++
	}
	if result != nil {
		if b.Statuses == nil {
			b.Statuses = map[int]int64{}
		}
		b.Statuses[result.Status]++
		if result.Proto == "HTTP/2.0" {
			b.HTTP2Responses++
		}
	}
}
func (b *relayResearchCarrierBudget) stage(stage string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Stages == nil {
		b.Stages = map[string]int64{}
	}
	b.Stages[stage]++
}

// The relay's Google-facing exchange uses only the selected Google route.
// Redirects are explicit, bounded, and restricted to documented Apps Script
// ContentService hosts. No raw connection to the owned relay is made here.
func newRelayResearchGoogleExchange(client *GoogleHTTPClient, endpoint string, auth string, codec *relayResearchCodec, budget *relayResearchCarrierBudget) (relayResearchExchange, error) {
	first, err := appsScriptEchoEndpoint(endpoint, true)
	if err != nil || client == nil || codec == nil || budget == nil || len(auth) < 32 || len(auth) > 128 {
		return nil, errRelayResearchBounds
	}
	if client.route.Mode != "google_front_pinned" || net.ParseIP(client.route.GoogleIP) == nil {
		return nil, errRelayResearchConflict
	}
	return func(ctx context.Context, wire []byte) ([]byte, error) {
		if len(wire) > codec.maxWireBytes() {
			return nil, errRelayResearchBounds
		}
		request, err := json.Marshal(relayResearchHTTPEnvelope{Auth: auth, Data: base64.StdEncoding.EncodeToString(wire)})
		if err != nil {
			return nil, errRelayResearchBounds
		}
		limit := base64.StdEncoding.EncodedLen(codec.maxWireBytes()) + 1024
		current, method, body := first, http.MethodPost, request
		for hop := 0; hop < 6; hop++ {
			if err = budget.reserve(len(body), limit+1, method == http.MethodPost && current.Hostname() == "script.google.com"); err != nil {
				return nil, err
			}
			result, callErr := client.RequestSingleAttempt(withGoogleResponseLimit(ctx, int64(limit)), method, current.Hostname(), current.RequestURI(), map[string]string{"Content-Type": "application/json", "Accept-Encoding": "identity"}, body)
			n := 0
			if result != nil {
				n = len(result.Body)
			}
			budget.finish(limit+1, n)
			budget.observe(result, callErr)
			if callErr != nil {
				return nil, errors.New("relay Google HTTP exchange failed")
			}
			if result.Status == http.StatusTooManyRequests || result.Status >= 500 {
				delay := retryAfter(result)
				if delay < time.Second {
					delay = time.Second
				}
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
				return nil, &relayResearchRetryAfter{delay: delay}
			}
			switch result.Status {
			case http.StatusOK:
				var failure struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(result.Body, &failure) == nil && relayResearchSafeStage(failure.Error) {
					budget.stage(failure.Error)
					return nil, &relayResearchStageError{stage: failure.Error}
				}
				var envelope relayResearchHTTPEnvelope
				decoder := json.NewDecoder(bytes.NewReader(result.Body))
				decoder.DisallowUnknownFields()
				if decoder.Decode(&envelope) != nil || envelope.Auth != "" || len(envelope.Data) > base64.StdEncoding.EncodedLen(codec.maxWireBytes()) {
					return nil, errRelayResearchConflict
				}
				if decoder.Decode(new(any)) != io.EOF {
					return nil, errRelayResearchConflict
				}
				response, e := base64.StdEncoding.Strict().DecodeString(envelope.Data)
				if e != nil || len(response) == 0 || len(response) > codec.maxWireBytes() {
					return nil, errRelayResearchBounds
				}
				return response, nil
			case 301, 302, 303, 307, 308:
				u, e := url.Parse(result.Header.Get("Location"))
				if e != nil || result.Header.Get("Location") == "" {
					return nil, errRelayResearchConflict
				}
				current, e = appsScriptEchoEndpoint(current.ResolveReference(u).String(), false)
				if e != nil {
					return nil, errRelayResearchConflict
				}
				if result.Status == 301 || result.Status == 302 || result.Status == 303 {
					method, body = http.MethodGet, nil
				}
			default:
				return nil, errRelayResearchConflict
			}
		}
		return nil, errRelayResearchBounds
	}, nil
}

func TestRelayClientGoogleCarrierPreservesFrontAndReservesRedirects(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	wire, _ := codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	reply, _ := codec.seal(relayResearchPacket{Kind: rrPull | rrResponse, Seq: 1})
	var calls int
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Hostname() != "www.google.com" {
			t.Error("carrier escaped selected google.com front")
		}
		if calls == 1 {
			if req.Method != http.MethodPost || req.Host != "script.google.com" {
				t.Error("invalid first authority/method")
			}
			response := stringResponse(302, "")
			response.Header = http.Header{"Location": {"https://script.googleusercontent.com/macros/echo?opaque=test"}}
			return response, nil
		}
		if req.Method != http.MethodGet || req.Host != "script.googleusercontent.com" {
			t.Error("invalid content redirect")
		}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			if len(raw) != 0 {
				t.Error("redirect forwarded auth body")
			}
		}
		raw, _ := json.Marshal(relayResearchHTTPEnvelope{Data: base64.StdEncoding.EncodeToString(reply)})
		return stringResponse(200, string(raw)), nil
	}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
	budget := &relayResearchCarrierBudget{MaxAttempts: 16, MaxBodyBytes: 16 << 20, MaxPosts: 1}
	exchange, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, budget)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(got, reply) || calls != 2 || budget.snapshot()["attempts_upper"] != 16 || budget.snapshot()["pending_response_bytes"] != 0 || budget.snapshot()["script_posts"] != 1 || budget.snapshot()["urlfetch_attempts_upper"] != 8 {
		t.Fatal("fronted redirect exchange failed", err, calls, budget.snapshot())
	}
	if _, err = exchange(context.Background(), wire); !errors.Is(err, errRelayResearchBounds) || calls != 2 {
		t.Fatal("guard permitted excess dispatch", err, calls)
	}
}

func TestRelayClientGoogleCarrierReservesRepostedScriptBeforeDispatch(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	wire, _ := codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	var calls int
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		response := stringResponse(307, "")
		response.Header = http.Header{"Location": {"https://script.google.com/macros/s/test/exec"}}
		return response, nil
	}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
	budget := &relayResearchCarrierBudget{MaxAttempts: 64, MaxBodyBytes: 16 << 20, MaxPosts: 1}
	exchange, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exchange(context.Background(), wire); !errors.Is(err, errRelayResearchBounds) || calls != 1 || budget.snapshot()["script_posts"] != 1 || budget.snapshot()["urlfetch_attempts_upper"] != 8 {
		t.Fatal("script rePOST bypassed campaign slice", err, calls, budget.snapshot())
	}
}

func TestRelayClientGoogleCarrierRetainsOnlyBoundedFailureStageAndStatus(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	wire, _ := codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	budget := &relayResearchCarrierBudget{MaxAttempts: 32, MaxBodyBytes: 16 << 20, MaxPosts: 2}
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := stringResponse(200, `{"error":"relay_http_503"}`)
		response.Proto = "HTTP/2.0"
		return response, nil
	}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
	exchange, _ := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, budget)
	_, err := exchange(context.Background(), wire)
	var stage *relayResearchStageError
	if !errors.As(err, &stage) || !errors.Is(err, errRelayResearchConflict) || stage.stage != "relay_http_503" || budget.snapshot()["stage_relay_http_503"] != 1 || budget.snapshot()["http_status_200"] != 1 || budget.snapshot()["http2_responses"] != 1 {
		t.Fatal("bounded diagnostics missing", err, budget.snapshot())
	}
	for _, unsafe := range []string{"relay_http_503 secret", "https://secret.example/token", "fetch_failed\ncredential", "relay_http_999", "relay_http_1"} {
		if relayResearchSafeStage(unsafe) {
			t.Fatal("unsafe failure detail accepted")
		}
	}
}

func TestRelayClientGoogleCarrierRejectsForeignRedirectAndSecretReflection(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	wire, _ := codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	for _, foreign := range []bool{false, true} {
		client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if foreign {
				response := stringResponse(302, "")
				response.Header = http.Header{"Location": {"https://owned-relay.example/exchange"}}
				return response, nil
			}
			return stringResponse(200, `{"auth":"synthetic-reflection","data":"YQ=="}`), nil
		}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
		exchange, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, &relayResearchCarrierBudget{MaxAttempts: 16, MaxBodyBytes: 16 << 20, MaxPosts: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = exchange(context.Background(), wire); !errors.Is(err, errRelayResearchConflict) {
			t.Fatal("foreign redirect/reflected auth accepted", foreign, err)
		}
	}
}

func TestRelayClientGoogleCarrierRejectsUnpinnedOrDirectRoute(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	for _, route := range []RouteConfig{{Mode: "direct"}, {Mode: "google_front"}, {Mode: "google_front_pinned"}, {Mode: "google_front_pinned", GoogleIP: "not-an-ip"}} {
		client := &GoogleHTTPClient{route: route}
		if _, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, &relayResearchCarrierBudget{}); !errors.Is(err, errRelayResearchConflict) {
			t.Fatal("unpinned research route accepted", route.Mode, err)
		}
	}
}
