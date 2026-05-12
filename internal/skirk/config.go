package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const ConfigTextPrefix = "skirk:"

const proactiveTokenRefreshMargin = 10 * time.Minute
const hostileTokenRefreshMargin = 20 * time.Minute
const staleTokenMinimumLifetime = 2 * time.Minute
const tokenRefreshRetryDelay = 1 * time.Minute

type Config struct {
	Secret    string       `json:"secret"`
	SessionID string       `json:"session_id,omitempty"`
	Auth      AuthConfig   `json:"auth"`
	Route     RouteConfig  `json:"route"`
	Drive     DriveConfig  `json:"drive"`
	Tunnel    TunnelConfig `json:"tunnel"`
}

type AuthConfig struct {
	AccessToken  string `json:"access_token,omitempty"`
	TokenCommand string `json:"token_command,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
}

type OAuthAccessToken struct {
	Token     string
	ExpiresAt time.Time
	Source    string
}

type AccessTokenSource struct {
	auth  AuthConfig
	route RouteConfig

	mu                 sync.Mutex
	token              string
	expiresAt          time.Time
	source             string
	refreshing         bool
	nextRefreshAttempt time.Time
	Logger             *log.Logger
}

type RouteConfig struct {
	Mode           string `json:"mode,omitempty"`
	Proxy          string `json:"proxy,omitempty"`
	GoogleIP       string `json:"google_ip,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type DriveConfig struct {
	FolderID string `json:"folder_id,omitempty"`
	Space    string `json:"space,omitempty"`
}

type TunnelConfig struct {
	Listen              string `json:"listen,omitempty"`
	Profile             string `json:"profile,omitempty"`
	ExitProxy           string `json:"exit_proxy,omitempty"`
	BurstPoll           bool   `json:"burst_poll,omitempty"`
	BurstPollMS         int    `json:"burst_poll_ms,omitempty"`
	BurstPollWindowMS   int    `json:"burst_poll_window_ms,omitempty"`
	ChunkSize           int    `json:"chunk_size,omitempty"`
	PollIntervalMS      int    `json:"poll_interval_ms,omitempty"`
	Concurrency         int    `json:"concurrency,omitempty"`
	UploadConcurrency   int    `json:"upload_concurrency,omitempty"`
	DownloadConcurrency int    `json:"download_concurrency,omitempty"`
	CleanupProcessed    bool   `json:"cleanup_processed,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	if cfg, ok, err := ParseInlineConfig(path); ok || err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*Config, error) {
	text := strings.TrimSpace(string(data))
	if cfg, ok, err := ParseInlineConfig(text); ok || err != nil {
		return cfg, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ParseInlineConfig(text string) (*Config, bool, error) {
	normalized, ok := NormalizeConfigText(text)
	if !ok {
		return nil, false, nil
	}
	cfg, err := DecodeConfigText(normalized)
	return cfg, true, err
}

func EncodeConfigText(cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("nil config")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return ConfigTextPrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

func DecodeConfigText(text string) (*Config, error) {
	text, ok := NormalizeConfigText(text)
	if !ok {
		return nil, fmt.Errorf("config text must start with %q", ConfigTextPrefix)
	}
	encoded := strings.TrimPrefix(text, ConfigTextPrefix)
	compressed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("config text base64 decode failed: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("config text gzip decode failed: %w", err)
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("config text read failed: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config text JSON decode failed: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func NormalizeConfigText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "SKIRK_CONFIG=")
	text = strings.TrimSpace(text)
	text = strings.Trim(text, `"'`)
	if !strings.HasPrefix(text, ConfigTextPrefix) {
		idx := strings.Index(text, ConfigTextPrefix)
		if idx < 0 {
			return "", false
		}
		text = text[idx:]
	}

	payload := strings.TrimPrefix(text, ConfigTextPrefix)
	var encoded strings.Builder
	seenPayload := false
	for i := 0; i < len(payload); {
		r, size := utf8.DecodeRuneInString(payload[i:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		if isRawURLBase64Rune(r) {
			encoded.WriteRune(r)
			seenPayload = true
			i += size
			continue
		}
		if unicode.IsSpace(r) {
			if !seenPayload {
				i += size
				continue
			}
			next := i + size
			for next < len(payload) {
				nextRune, nextSize := utf8.DecodeRuneInString(payload[next:])
				if !unicode.IsSpace(nextRune) {
					break
				}
				next += nextSize
			}
			if next >= len(payload) || strings.HasPrefix(payload[next:], "--") {
				break
			}
			nextRune, _ := utf8.DecodeRuneInString(payload[next:])
			if nextRune == '\'' || nextRune == '"' || nextRune == '`' {
				break
			}
			if isRawURLBase64Rune(nextRune) {
				i += size
				continue
			}
			break
		}
		if r == '\'' || r == '"' || r == '`' {
			if seenPayload {
				break
			}
			i += size
			continue
		}
		if seenPayload {
			break
		}
		return "", false
	}
	if encoded.Len() == 0 {
		return "", false
	}
	return ConfigTextPrefix + encoded.String(), true
}

func isRawURLBase64Rune(r rune) bool {
	return (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '-' ||
		r == '_'
}

func (c *Config) ApplyDefaults() {
	if c.Route.Mode == "" {
		c.Route.Mode = "real_pinned"
	}
	if c.Route.GoogleIP == "" {
		c.Route.GoogleIP = "216.239.38.120"
	}
	if c.Route.TimeoutSeconds == 0 {
		c.Route.TimeoutSeconds = 240
	}
	if c.Auth.TokenCommand == "" {
		c.Auth.TokenCommand = "gcloud auth print-access-token"
	}
	if c.Tunnel.Listen == "" {
		c.Tunnel.Listen = "127.0.0.1:18080"
	}
	if c.Tunnel.Profile == "" {
		c.Tunnel.Profile = "auto"
	}
	if c.Tunnel.ChunkSize == 0 {
		c.Tunnel.ChunkSize = 1024 * 1024
	}
	if c.Tunnel.PollIntervalMS == 0 {
		c.Tunnel.PollIntervalMS = 100
	}
	if c.Tunnel.BurstPollMS == 0 {
		c.Tunnel.BurstPollMS = 75
	}
	if c.Tunnel.BurstPollWindowMS == 0 {
		c.Tunnel.BurstPollWindowMS = 5000
	}
	if c.Tunnel.Concurrency == 0 {
		c.Tunnel.Concurrency = 32
	}
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.Secret) == "" {
		return errors.New("config.secret is required")
	}
	if c.Tunnel.ChunkSize < 512 || c.Tunnel.ChunkSize > 16*1024*1024 {
		return fmt.Errorf("config.tunnel.chunk_size must be between 512 and 16777216 bytes")
	}
	if c.Tunnel.Concurrency < 1 || c.Tunnel.Concurrency > 64 {
		return fmt.Errorf("config.tunnel.concurrency must be between 1 and 64")
	}
	switch strings.TrimSpace(c.Tunnel.Profile) {
	case "", "auto", "fixed":
	default:
		return fmt.Errorf("config.tunnel.profile must be auto or fixed")
	}
	if c.Tunnel.UploadConcurrency < 0 || c.Tunnel.UploadConcurrency > 64 {
		return fmt.Errorf("config.tunnel.upload_concurrency must be between 0 and 64")
	}
	if c.Tunnel.DownloadConcurrency < 0 || c.Tunnel.DownloadConcurrency > 64 {
		return fmt.Errorf("config.tunnel.download_concurrency must be between 0 and 64")
	}
	if c.Tunnel.BurstPollMS < 25 || c.Tunnel.BurstPollMS > 1000 {
		return fmt.Errorf("config.tunnel.burst_poll_ms must be between 25 and 1000")
	}
	if c.Tunnel.BurstPollWindowMS < 1000 || c.Tunnel.BurstPollWindowMS > 30000 {
		return fmt.Errorf("config.tunnel.burst_poll_window_ms must be between 1000 and 30000")
	}
	if strings.TrimSpace(c.Tunnel.ExitProxy) != "" {
		u, err := url.Parse(c.Tunnel.ExitProxy)
		if err != nil || u.Host == "" {
			return fmt.Errorf("config.tunnel.exit_proxy must be a valid proxy URL")
		}
		switch u.Scheme {
		case "socks5", "socks5h", "http", "https":
		default:
			return fmt.Errorf("config.tunnel.exit_proxy scheme must be socks5, socks5h, http, or https")
		}
	}
	return nil
}

func NewAccessTokenSource(auth AuthConfig, route RouteConfig) *AccessTokenSource {
	return &AccessTokenSource{auth: auth, route: route}
}

func (s *AccessTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.token != "" {
		if !tokenNeedsRefreshForRoute(now, s.expiresAt, s.route) {
			return s.token, nil
		}
		if staleSafeTokenRefresh(s.route) && tokenUsableDuringRefresh(now, s.expiresAt) {
			if !s.refreshing && !now.Before(s.nextRefreshAttempt) {
				s.refreshing = true
				go s.refreshInBackground()
			}
			return s.token, nil
		}
	}
	token, err := s.auth.accessTokenForRoute(ctx, s.route)
	if err != nil {
		if s.token != "" && tokenUsableDuringRefresh(now, s.expiresAt) {
			s.nextRefreshAttempt = now.Add(tokenRefreshRetryDelay)
			if s.Logger != nil {
				s.Logger.Printf("oauth token refresh failed source=%s action=use-cached-token retry_after=%s error=%s", firstNonEmptyString(s.source, "cached"), tokenRefreshRetryDelay, errorSummary(err))
			}
			return s.token, nil
		}
		return "", err
	}
	s.token = token.Token
	s.expiresAt = token.ExpiresAt
	s.source = token.Source
	s.nextRefreshAttempt = time.Time{}
	if s.Logger != nil {
		if s.expiresAt.IsZero() {
			s.Logger.Printf("oauth token loaded source=%s expiry=unknown", firstNonEmptyString(s.source, "static"))
		} else {
			s.Logger.Printf("oauth token loaded source=%s expires_in=%s refresh_margin=%s", firstNonEmptyString(s.source, "unknown"), time.Until(s.expiresAt).Round(time.Second), tokenRefreshMargin(s.route))
		}
	}
	return s.token, nil
}

func (s *AccessTokenSource) refreshInBackground() {
	token, err := s.auth.accessTokenForRoute(context.Background(), s.route)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = false
	if err != nil {
		s.nextRefreshAttempt = time.Now().Add(tokenRefreshRetryDelay)
		if s.Logger != nil {
			s.Logger.Printf("oauth background refresh failed source=%s action=keep-cached-token retry_after=%s error=%s", firstNonEmptyString(s.source, "cached"), tokenRefreshRetryDelay, errorSummary(err))
		}
		return
	}
	s.token = token.Token
	s.expiresAt = token.ExpiresAt
	s.source = token.Source
	s.nextRefreshAttempt = time.Time{}
	if s.Logger != nil {
		if s.expiresAt.IsZero() {
			s.Logger.Printf("oauth background refresh loaded source=%s expiry=unknown", firstNonEmptyString(s.source, "static"))
		} else {
			s.Logger.Printf("oauth background refresh loaded source=%s expires_in=%s refresh_margin=%s", firstNonEmptyString(s.source, "unknown"), time.Until(s.expiresAt).Round(time.Second), tokenRefreshMargin(s.route))
		}
	}
}

func (s *AccessTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.expiresAt = time.Time{}
	s.source = ""
	s.refreshing = false
	s.nextRefreshAttempt = time.Time{}
}

func (a AuthConfig) Token(ctx context.Context) (string, error) {
	return a.TokenForRoute(ctx, RouteConfig{Mode: "direct"})
}

func (a AuthConfig) Revoke(ctx context.Context, route RouteConfig) error {
	token := strings.TrimSpace(a.RefreshToken)
	if token == "" {
		token = strings.TrimSpace(a.AccessToken)
	}
	if token == "" {
		return errors.New("auth.refresh_token or auth.access_token is required for OAuth revocation")
	}
	values := url.Values{}
	values.Set("token", token)
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	result, err := NewGoogleHTTPClient(route).Request(ctx, http.MethodPost, "oauth2.googleapis.com", "/revoke", headers, []byte(values.Encode()))
	if err != nil {
		return err
	}
	if result.Status == http.StatusOK {
		return nil
	}
	return require2xx(result, "oauth revoke")
}

func (a AuthConfig) TokenForRoute(ctx context.Context, route RouteConfig) (string, error) {
	return NewAccessTokenSource(a, route).Token(ctx)
}

func (a AuthConfig) accessTokenForRoute(ctx context.Context, route RouteConfig) (OAuthAccessToken, error) {
	if token := strings.TrimSpace(os.Getenv("SKIRK_ACCESS_TOKEN")); token != "" {
		return OAuthAccessToken{Token: token, Source: "env"}, nil
	}
	if token := strings.TrimSpace(a.AccessToken); token != "" {
		return OAuthAccessToken{Token: token, Source: "config_access_token"}, nil
	}
	if strings.TrimSpace(a.RefreshToken) != "" {
		return a.refreshAccessToken(ctx, route)
	}
	token, err := a.tokenFromCommand(ctx)
	if err != nil {
		return OAuthAccessToken{}, err
	}
	return OAuthAccessToken{Token: token, ExpiresAt: time.Now().Add(55 * time.Minute), Source: "token_command"}, nil
}

func (a AuthConfig) tokenFromCommand(ctx context.Context) (string, error) {
	command := strings.TrimSpace(a.TokenCommand)
	if command == "" {
		return "", errors.New("no access token, refresh token, or token_command configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	path := os.Getenv("PATH")
	home := os.Getenv("HOME")
	if home != "" {
		path = home + "/google-cloud-sdk/bin:" + path
	}
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token command failed: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("token command returned an empty token")
	}
	return token, nil
}

func (a AuthConfig) refreshAccessToken(ctx context.Context, route RouteConfig) (OAuthAccessToken, error) {
	clientID := strings.TrimSpace(a.ClientID)
	if clientID == "" {
		return OAuthAccessToken{}, errors.New("auth.client_id is required when auth.refresh_token is set")
	}
	tokenURL := strings.TrimSpace(a.TokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	values := url.Values{}
	values.Set("client_id", clientID)
	values.Set("refresh_token", strings.TrimSpace(a.RefreshToken))
	values.Set("grant_type", "refresh_token")
	if secret := strings.TrimSpace(a.ClientSecret); secret != "" {
		values.Set("client_secret", secret)
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if tokenURL == "https://oauth2.googleapis.com/token" {
		return refreshAccessTokenViaGoogleEndpoints(ctx, route, values)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return OAuthAccessToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return OAuthAccessToken{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return OAuthAccessToken{}, err
	}
	return parseOAuthTokenResponse(resp.StatusCode, body)
}

type oauthTokenAttempt struct {
	source  string
	route   RouteConfig
	host    string
	path    string
	timeout time.Duration
}

func refreshAccessTokenViaGoogleEndpoints(ctx context.Context, route RouteConfig, values url.Values) (OAuthAccessToken, error) {
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	body := []byte(values.Encode())
	var errs []string
	for _, attempt := range oauthTokenAttempts(route) {
		attemptCtx, cancel := context.WithTimeout(ctx, attempt.timeout)
		result, err := NewGoogleHTTPClient(attempt.route).Request(attemptCtx, http.MethodPost, attempt.host, attempt.path, headers, body)
		cancel()
		if err != nil {
			errs = append(errs, attempt.source+": "+errorSummary(err))
			continue
		}
		token, err := parseOAuthTokenResponse(result.Status, result.Body)
		if err == nil {
			token.Source = attempt.source
			return token, nil
		}
		if terminalOAuthTokenError(result.Status, result.Body) {
			return OAuthAccessToken{}, err
		}
		errs = append(errs, attempt.source+": "+errorSummary(err))
	}
	return OAuthAccessToken{}, fmt.Errorf("oauth token refresh failed through all routes: %s", strings.Join(errs, "; "))
}

func oauthTokenAttempts(route RouteConfig) []oauthTokenAttempt {
	baseTimeout := time.Duration(route.TimeoutSeconds) * time.Second
	if baseTimeout <= 0 || baseTimeout > 30*time.Second {
		baseTimeout = 30 * time.Second
	}
	if route.Proxy != "" && baseTimeout > 12*time.Second {
		baseTimeout = 12 * time.Second
	}
	attempts := []oauthTokenAttempt{
		{
			source:  "refresh_token:oauth2",
			route:   route,
			host:    "oauth2.googleapis.com",
			path:    "/token",
			timeout: baseTimeout,
		},
	}
	if route.Proxy != "" || isGoogleFrontRoute(route.Mode) {
		frontRoute := route
		if !isGoogleFrontRoute(frontRoute.Mode) {
			frontRoute.Mode = "google_front"
		}
		directRoute := route
		directRoute.Mode = "direct"
		directRoute.GoogleIP = ""
		attempts = append(attempts,
			oauthTokenAttempt{
				source:  "refresh_token:accounts_fronted",
				route:   frontRoute,
				host:    "accounts.google.com",
				path:    "/o/oauth2/token",
				timeout: 12 * time.Second,
			},
			oauthTokenAttempt{
				source:  "refresh_token:accounts_legacy",
				route:   directRoute,
				host:    "accounts.google.com",
				path:    "/o/oauth2/token",
				timeout: 20 * time.Second,
			},
			oauthTokenAttempt{
				source:  "refresh_token:oauth2_direct",
				route:   directRoute,
				host:    "oauth2.googleapis.com",
				path:    "/token",
				timeout: 12 * time.Second,
			},
		)
	}
	return attempts
}

func terminalOAuthTokenError(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error == "" {
		return false
	}
	switch payload.Error {
	case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "deleted_client":
		return true
	default:
		return false
	}
}

func parseOAuthTokenResponse(status int, body []byte) (OAuthAccessToken, error) {
	var payload struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return OAuthAccessToken{}, fmt.Errorf("oauth token response decode failed: %w", err)
	}
	if status < 200 || status >= 300 {
		if payload.Error != "" {
			return OAuthAccessToken{}, fmt.Errorf("oauth token refresh failed status=%d error=%s description=%s", status, payload.Error, payload.ErrorDescription)
		}
		return OAuthAccessToken{}, fmt.Errorf("oauth token refresh failed status=%d body=%q", status, string(body))
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return OAuthAccessToken{}, errors.New("oauth token refresh returned an empty access_token")
	}
	token := OAuthAccessToken{Token: strings.TrimSpace(payload.AccessToken)}
	if payload.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	token.Source = "refresh_token"
	return token, nil
}

func tokenNeedsRefresh(now time.Time, expiresAt time.Time) bool {
	return tokenNeedsRefreshForRoute(now, expiresAt, RouteConfig{})
}

func tokenNeedsRefreshForRoute(now time.Time, expiresAt time.Time, route RouteConfig) bool {
	if expiresAt.IsZero() {
		return false
	}
	margin := tokenRefreshMargin(route)
	lifetime := expiresAt.Sub(now)
	if lifetime < margin {
		return true
	}
	return !now.Before(expiresAt.Add(-margin))
}

func tokenRefreshMargin(route RouteConfig) time.Duration {
	if staleSafeTokenRefresh(route) {
		return hostileTokenRefreshMargin
	}
	return proactiveTokenRefreshMargin
}

func staleSafeTokenRefresh(route RouteConfig) bool {
	return route.Proxy != "" || isGoogleFrontRoute(route.Mode)
}

func tokenUsableDuringRefresh(now time.Time, expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return now.Add(staleTokenMinimumLifetime).Before(expiresAt)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (c Config) PollInterval() time.Duration {
	interval := c.Tunnel.PollIntervalMS
	if strings.TrimSpace(c.Tunnel.Profile) == "" || strings.TrimSpace(c.Tunnel.Profile) == "auto" {
		if interval <= 0 || interval > 100 {
			interval = 100
		}
	}
	return time.Duration(interval) * time.Millisecond
}
