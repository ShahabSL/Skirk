package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const googleDesktopAuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"

func normalizeOAuthFlow(raw string) (string, error) {
	flow := strings.ToLower(strings.TrimSpace(raw))
	switch flow {
	case "", "auto":
		return "auto", nil
	case "device", "tv", "limited", "limited-input", "limited_input":
		return "device", nil
	case "desktop", "installed", "native", "browser", "auth-code", "authorization-code":
		return "desktop", nil
	default:
		return "", fmt.Errorf("--oauth-flow must be auto, device, or desktop; got %q", raw)
	}
}

func resolveSetupOAuthFlow(mode, requested string, client oauthClientCredentials) (string, error) {
	if requested != "" && requested != "auto" {
		if mode == "easy" && requested == "desktop" {
			return "", errors.New("--oauth-flow desktop is only supported with --oauth-mode personal")
		}
		return requested, nil
	}
	if client.Flow != "" {
		flow, err := normalizeOAuthFlow(client.Flow)
		if err != nil {
			return "", err
		}
		if flow != "auto" {
			return flow, nil
		}
	}
	if mode == "personal" {
		return "desktop", nil
	}
	return "device", nil
}

func runGoogleOAuth(ctx context.Context, client oauthClientCredentials, oauthScopes, flow string, reader *bufio.Reader) (adcCredentials, error) {
	switch flow {
	case "device":
		return runGoogleDeviceOAuth(ctx, client, oauthScopes)
	case "desktop":
		return runGoogleDesktopOAuth(ctx, client, oauthScopes, reader)
	default:
		return adcCredentials{}, fmt.Errorf("unsupported Google OAuth flow %q", flow)
	}
}

func runGoogleDesktopOAuth(ctx context.Context, client oauthClientCredentials, oauthScopes string, reader *bufio.Reader) (adcCredentials, error) {
	if reader == nil {
		reader = bufio.NewReader(os.Stdin)
	}
	scopes := normalizeOAuthScopes(oauthScopes)
	state, err := randomOAuthToken(24)
	if err != nil {
		return adcCredentials{}, err
	}
	verifier, err := randomOAuthToken(64)
	if err != nil {
		return adcCredentials{}, err
	}
	redirectURI, callbacks, shutdown, err := startDesktopOAuthCallback(ctx, state)
	if err != nil {
		return adcCredentials{}, err
	}
	defer shutdown()

	authURL := desktopOAuthURL(client.ClientID, scopes, redirectURI, state, verifier)
	fmt.Printf("Open this Google approval URL in a browser:\n\n%s\n\n", authURL)
	fmt.Println("If this setup is running on a VPS over SSH, Google may redirect your browser to a localhost URL that cannot load. That is expected.")
	fmt.Println("Copy the full localhost address-bar URL after approval and paste it here.")
	fmt.Println()
	fmt.Print("Paste redirected localhost URL or authorization code, or press Enter to wait for local browser redirect: ")

	inputs := make(chan string, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			inputs <- ""
			return
		}
		inputs <- strings.TrimSpace(line)
	}()

	for {
		select {
		case <-ctx.Done():
			return adcCredentials{}, ctx.Err()
		case cb := <-callbacks:
			if cb.err != nil {
				return adcCredentials{}, cb.err
			}
			return exchangeDesktopOAuthCode(ctx, client, cb.code, verifier, redirectURI)
		case input := <-inputs:
			if input == "" {
				fmt.Println("Waiting for local browser redirect...")
				continue
			}
			code, err := parseOAuthRedirectInput(input, state)
			if err != nil {
				return adcCredentials{}, err
			}
			return exchangeDesktopOAuthCode(ctx, client, code, verifier, redirectURI)
		}
	}
}

type desktopOAuthCallback struct {
	code string
	err  error
}

func startDesktopOAuthCallback(ctx context.Context, expectedState string) (string, <-chan desktopOAuthCallback, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	callbacks := make(chan desktopOAuthCallback, 1)
	mux := http.NewServeMux()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code, err := parseOAuthRedirectValues(r.URL.Query(), expectedState)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!doctype html><title>Skirk OAuth</title><p>Skirk received Google approval. Return to the terminal.</p>")
		}
		select {
		case callbacks <- desktopOAuthCallback{code: code, err: err}:
		default:
		}
	})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case callbacks <- desktopOAuthCallback{err: err}:
			default:
			}
		}
	}()
	var stopOnce sync.Once
	done := make(chan struct{})
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return "http://" + listener.Addr().String() + "/", callbacks, stop, nil
}

func desktopOAuthURL(clientID, scopes, redirectURI, state, verifier string) string {
	values := url.Values{}
	values.Set("client_id", strings.TrimSpace(clientID))
	values.Set("redirect_uri", redirectURI)
	values.Set("response_type", "code")
	values.Set("scope", scopes)
	values.Set("access_type", "offline")
	values.Set("prompt", "consent")
	values.Set("code_challenge", pkceChallenge(verifier))
	values.Set("code_challenge_method", "S256")
	values.Set("state", state)
	return googleDesktopAuthEndpoint + "?" + values.Encode()
}

func parseOAuthRedirectInput(raw, expectedState string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty OAuth redirect input")
	}
	candidate := raw
	if strings.HasPrefix(candidate, "localhost:") || strings.HasPrefix(candidate, "127.") {
		candidate = "http://" + candidate
	}
	if strings.Contains(candidate, "://") || strings.Contains(candidate, "?") {
		u, err := url.Parse(candidate)
		if err == nil {
			if code, parseErr := parseOAuthRedirectValues(u.Query(), expectedState); parseErr == nil {
				return code, nil
			} else if strings.Contains(candidate, "code=") || strings.Contains(candidate, "error=") {
				return "", parseErr
			}
		}
	}
	if strings.ContainsAny(raw, " \t\r\n?&=") {
		return "", errors.New("paste the full localhost redirect URL or just the authorization code")
	}
	return raw, nil
}

func parseOAuthRedirectValues(values url.Values, expectedState string) (string, error) {
	if oauthErr := strings.TrimSpace(values.Get("error")); oauthErr != "" {
		desc := strings.TrimSpace(values.Get("error_description"))
		return "", fmt.Errorf("Google OAuth approval failed: %s %s", oauthErr, desc)
	}
	state := strings.TrimSpace(values.Get("state"))
	if expectedState != "" && state != "" && state != expectedState {
		return "", errors.New("Google OAuth state did not match; rerun setup to avoid using a stale approval URL")
	}
	code := strings.TrimSpace(values.Get("code"))
	if code == "" {
		return "", errors.New("Google OAuth redirect did not include code")
	}
	return code, nil
}

func exchangeDesktopOAuthCode(ctx context.Context, client oauthClientCredentials, code, verifier, redirectURI string) (adcCredentials, error) {
	values := url.Values{}
	values.Set("client_id", strings.TrimSpace(client.ClientID))
	if secret := strings.TrimSpace(client.ClientSecret); secret != "" {
		values.Set("client_secret", secret)
	}
	values.Set("code", strings.TrimSpace(code))
	values.Set("code_verifier", verifier)
	values.Set("grant_type", "authorization_code")
	values.Set("redirect_uri", redirectURI)
	var out deviceTokenResponse
	if err := postOAuthForm(ctx, "https://oauth2.googleapis.com/token", values, &out); err != nil {
		return adcCredentials{}, err
	}
	if out.Error != "" {
		return adcCredentials{}, fmt.Errorf("desktop OAuth token request failed: %s %s", out.Error, out.ErrorDesc)
	}
	if out.RefreshToken == "" {
		return adcCredentials{}, errors.New("desktop OAuth token response did not include a refresh token; rerun setup and approve the consent prompt")
	}
	return adcCredentials{
		Account:      "unknown",
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		RefreshToken: out.RefreshToken,
		Type:         "authorized_user",
	}, nil
}

func randomOAuthToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
