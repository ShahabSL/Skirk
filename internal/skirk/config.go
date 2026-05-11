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
)

const ConfigTextPrefix = "skirk:"

const proactiveTokenRefreshMargin = 10 * time.Minute

type Config struct {
	Secret    string       `json:"secret"`
	SessionID string       `json:"session_id,omitempty"`
	Auth      AuthConfig   `json:"auth"`
	Route     RouteConfig  `json:"route"`
	Drive     DriveConfig  `json:"drive"`
	Sheets    SheetsConfig `json:"sheets"`
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

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	source    string
	Logger    *log.Logger
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

type SheetsConfig struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	Range         string `json:"range,omitempty"`
}

type TunnelConfig struct {
	Listen              string `json:"listen,omitempty"`
	Profile             string `json:"profile,omitempty"`
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
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "SKIRK_CONFIG=")
	text = strings.Trim(text, `"'`)
	if !strings.HasPrefix(text, ConfigTextPrefix) {
		return nil, false, nil
	}
	cfg, err := DecodeConfigText(text)
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
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "SKIRK_CONFIG=")
	text = strings.Trim(text, `"'`)
	if !strings.HasPrefix(text, ConfigTextPrefix) {
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
	if c.Sheets.Range == "" {
		c.Sheets.Range = "skirk!A:D"
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
	if c.Tunnel.Concurrency < 1 || c.Tunnel.Concurrency > 32 {
		return fmt.Errorf("config.tunnel.concurrency must be between 1 and 32")
	}
	switch strings.TrimSpace(c.Tunnel.Profile) {
	case "", "auto", "fixed":
	default:
		return fmt.Errorf("config.tunnel.profile must be auto or fixed")
	}
	if c.Tunnel.UploadConcurrency < 0 || c.Tunnel.UploadConcurrency > 32 {
		return fmt.Errorf("config.tunnel.upload_concurrency must be between 0 and 32")
	}
	if c.Tunnel.DownloadConcurrency < 0 || c.Tunnel.DownloadConcurrency > 32 {
		return fmt.Errorf("config.tunnel.download_concurrency must be between 0 and 32")
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
	if s.token != "" && !tokenNeedsRefresh(now, s.expiresAt) {
		return s.token, nil
	}
	token, err := s.auth.accessTokenForRoute(ctx, s.route)
	if err != nil {
		return "", err
	}
	s.token = token.Token
	s.expiresAt = token.ExpiresAt
	s.source = token.Source
	if s.Logger != nil {
		if s.expiresAt.IsZero() {
			s.Logger.Printf("oauth token loaded source=%s expiry=unknown", firstNonEmptyString(s.source, "static"))
		} else {
			s.Logger.Printf("oauth token loaded source=%s expires_in=%s refresh_margin=%s", firstNonEmptyString(s.source, "unknown"), time.Until(s.expiresAt).Round(time.Second), proactiveTokenRefreshMargin)
		}
	}
	return s.token, nil
}

func (s *AccessTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.expiresAt = time.Time{}
	s.source = ""
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

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if tokenURL == "https://oauth2.googleapis.com/token" {
		headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		result, err := NewGoogleHTTPClient(route).Request(ctx, http.MethodPost, "oauth2.googleapis.com", "/token", headers, []byte(values.Encode()))
		if err != nil {
			return OAuthAccessToken{}, err
		}
		return parseOAuthTokenResponse(result.Status, result.Body)
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
	if expiresAt.IsZero() {
		return false
	}
	margin := proactiveTokenRefreshMargin
	lifetime := expiresAt.Sub(now)
	if lifetime < margin {
		return true
	}
	return !now.Before(expiresAt.Add(-margin))
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
