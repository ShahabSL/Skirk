package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type relayResearchRetryAfter struct{ delay time.Duration }

func (e *relayResearchRetryAfter) Error() string { return "relay Google HTTP temporarily unavailable" }

// Limits count visible HTTP dispatches and reserve the x/net/http2 v0.61
// internal replay upper bound before dispatch. Body limits exclude TLS/headers;
// these are not a claim to observe Google's accepted or billed request count.
type relayResearchCarrierBudget struct {
	mu                                                                                                        sync.Mutex
	MaxAttempts, MaxBodyBytes                                                                                 int64
	Dispatches, AttemptUpper, RequestBodyUpper, ResponseBodyUpper, ExposedResponseBytes, PendingResponseBytes int64
}

func (b *relayResearchCarrierBudget) reserve(request, response int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.AttemptUpper+8 > b.MaxAttempts || b.RequestBodyUpper+b.ResponseBodyUpper+8*int64(request)+int64(response) > b.MaxBodyBytes {
		return errRelayResearchBounds
	}
	b.Dispatches++
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
	return map[string]int64{"dispatches": b.Dispatches, "attempts_upper": b.AttemptUpper, "request_body_bytes_upper": b.RequestBodyUpper, "response_body_bytes_upper": b.ResponseBodyUpper, "returned_response_body_bytes": b.ExposedResponseBytes, "pending_response_bytes": b.PendingResponseBytes}
}

// The relay's Google-facing exchange uses only the selected Google route.
// Redirects are explicit, bounded, and restricted to documented Apps Script
// ContentService hosts. No raw connection to the owned relay is made here.
func newRelayResearchGoogleExchange(client *GoogleHTTPClient, endpoint string, auth string, codec *relayResearchCodec, budget *relayResearchCarrierBudget) (relayResearchExchange, error) {
	first, err := appsScriptEchoEndpoint(endpoint, true)
	if err != nil || client == nil || codec == nil || budget == nil || len(auth) < 32 || len(auth) > 128 {
		return nil, errRelayResearchBounds
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
			if err = budget.reserve(len(body), limit+1); err != nil {
				return nil, err
			}
			result, callErr := client.RequestSingleAttempt(withGoogleResponseLimit(ctx, int64(limit)), method, current.Hostname(), current.RequestURI(), map[string]string{"Content-Type": "application/json", "Accept-Encoding": "identity"}, body)
			n := 0
			if result != nil {
				n = len(result.Body)
			}
			budget.finish(limit+1, n)
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
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	budget := &relayResearchCarrierBudget{MaxAttempts: 16, MaxBodyBytes: 16 << 20}
	exchange, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, budget)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(got, reply) || calls != 2 || budget.snapshot()["attempts_upper"] != 16 || budget.snapshot()["pending_response_bytes"] != 0 {
		t.Fatal("fronted redirect exchange failed", err, calls, budget.snapshot())
	}
	if _, err = exchange(context.Background(), wire); !errors.Is(err, errRelayResearchBounds) || calls != 2 {
		t.Fatal("guard permitted excess dispatch", err, calls)
	}
}

func TestRelayClientGoogleCarrierRejectsForeignRedirectAndSecretReflection(t *testing.T) {
	codec, _ := newRelayResearchCodec(bytes.Repeat([]byte{8}, 32), [16]byte{3}, 128<<10)
	wire, _ := codec.seal(relayResearchPacket{Kind: rrPull, Seq: 1})
	for _, foreign := range []bool{false, true} {
		client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if foreign {
				response := stringResponse(302, "")
				response.Header = http.Header{"Location": {"https://owned-relay.example/exchange"}}
				return response, nil
			}
			return stringResponse(200, `{"auth":"synthetic-reflection","data":"YQ=="}`), nil
		}), CheckRedirect: keepGoogleHTTPRedirectResponse}}
		exchange, err := newRelayResearchGoogleExchange(client, "https://script.google.com/macros/s/test/exec", strings.Repeat("a", 32), codec, &relayResearchCarrierBudget{MaxAttempts: 16, MaxBodyBytes: 16 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = exchange(context.Background(), wire); !errors.Is(err, errRelayResearchConflict) {
			t.Fatal("foreign redirect/reflected auth accepted", foreign, err)
		}
	}
}
