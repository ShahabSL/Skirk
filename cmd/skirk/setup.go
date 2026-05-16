package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"skirk/internal/skirk"
)

type adcCredentials struct {
	Account      string `json:"account"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Type         string `json:"type"`
}

type oauthClientCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

const defaultCustomOAuthScopes = "https://www.googleapis.com/auth/drive.file"

var (
	defaultOAuthClientID     string
	defaultOAuthClientSecret string
)

func setup(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("setup needs init")
	}
	switch args[0] {
	case "init":
		return setupInit(ctx, args[1:])
	default:
		return fmt.Errorf("unknown setup command %q", args[0])
	}
}

func setupInit(ctx context.Context, args []string) error {
	defaultTitle := "skirk-" + time.Now().UTC().Format("20060102-150405")
	fs := flag.NewFlagSet("setup init", flag.ExitOnError)
	outDir := fs.String("out", "skirk-kit", "directory for generated configs")
	title := fs.String("title", defaultTitle, "kit title used in generated docs")
	adcPath := fs.String("adc", "", "Application Default Credentials JSON path")
	noLogin := fs.Bool("no-gcloud-login", false, "fail instead of starting Google login if ADC is missing")
	googleLogin := fs.Bool("google-login", false, "run Google login even if existing credentials are present")
	resetGoogleLogin := fs.Bool("reset-google-login", false, "remove local ADC credentials before Google login")
	oauthClientFile := fs.String("oauth-client-file", "", "override Google OAuth client JSON for TVs and Limited Input devices")
	oauthScopes := fs.String("oauth-scopes", defaultCustomOAuthScopes, "comma- or space-separated scopes used for Google OAuth login")
	clientRoute := fs.String("client-route", "google_front", "client Google API route: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
	exitRoute := fs.String("exit-route", "direct", "exit Google API route: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
	clientProxy := fs.String("client-proxy", "", "optional upstream SOCKS5 URL for client Google API traffic")
	exitProxy := fs.String("exit-proxy", "", "optional outbound proxy URL for exit target traffic, for example socks5h://127.0.0.1:40000")
	googleIP := fs.String("google-ip", "216.239.38.120", "Google edge IP for pinned routes")
	listen := fs.String("listen", "127.0.0.1:18080", "client SOCKS5 listen address")
	chunkSize := fs.Int("chunk-size", 8*1024*1024, "maximum tunnel chunk size")
	pollMS := fs.Int("poll-ms", 100, "Drive mailbox poll interval in milliseconds")
	clientUploadConcurrency := fs.Int("client-upload-concurrency", 0, "client Drive upload concurrency; 0 uses auto profile")
	clientDownloadConcurrency := fs.Int("client-download-concurrency", 0, "client Drive download concurrency; 0 uses auto profile")
	exitUploadConcurrency := fs.Int("exit-upload-concurrency", 0, "exit Drive upload concurrency; 0 uses auto profile")
	exitDownloadConcurrency := fs.Int("exit-download-concurrency", 0, "exit Drive download concurrency; 0 uses auto profile")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON instead of the copy-paste setup summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noLogin && (*googleLogin || *resetGoogleLogin || *oauthClientFile != "") {
		return fmt.Errorf("--no-gcloud-login cannot be combined with --google-login, --reset-google-login, or --oauth-client-file")
	}
	if *adcPath != "" && (*googleLogin || *resetGoogleLogin || *oauthClientFile != "") {
		return fmt.Errorf("--adc supplies explicit credentials and cannot be combined with --google-login, --reset-google-login, or --oauth-client-file")
	}

	oauthClient, oauthSource, oauthErr := resolveOAuthClientCredentials(strings.TrimSpace(*oauthClientFile))
	if oauthErr != nil {
		return oauthErr
	}
	credsPath := firstNonEmpty(*adcPath, defaultADCPath())
	creds, err := readADCCredentials(credsPath)
	loginRequested := *googleLogin || oauthSource != ""
	if *resetGoogleLogin {
		fmt.Printf("Resetting local Google credentials before login.\n\n")
		_ = os.Remove(credsPath)
		creds = adcCredentials{}
		err = errors.New("Google login was reset")
	}
	if loginRequested || err != nil {
		if *noLogin {
			return fmt.Errorf("google ADC unavailable at %s: %w", credsPath, err)
		}
		if oauthSource != "" {
			fmt.Printf("Google login will use %s.\n\n", oauthSource)
			creds, err = runGoogleDeviceOAuth(ctx, oauthClient, *oauthScopes)
			if err != nil {
				return err
			}
		} else {
			return driveOAuthClientRequiredError(credsPath, err)
		}
	}
	if strings.TrimSpace(creds.Account) == "" {
		creds.Account = "unknown"
	}
	auth := creds.AuthConfig()

	secret, err := skirk.RandomSecret()
	if err != nil {
		return err
	}
	session, err := skirk.NewSessionID()
	if err != nil {
		return err
	}
	sessionID := skirk.SessionString(session)
	baseDrive, folderID, err := setupDriveMailbox(ctx, auth, *googleIP, sessionID)
	if err != nil {
		return err
	}
	clientCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: *clientRoute, Proxy: *clientProxy, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Drive:     baseDrive,
		Tunnel:    setupTunnelConfig(*listen, *chunkSize, *pollMS, *clientUploadConcurrency, *clientDownloadConcurrency),
	}
	exitTunnel := setupTunnelConfig(*listen, *chunkSize, *pollMS, *exitUploadConcurrency, *exitDownloadConcurrency)
	exitTunnel.ExitProxy = strings.TrimSpace(*exitProxy)
	exitCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: *exitRoute, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Drive:     baseDrive,
		Tunnel:    exitTunnel,
	}
	if err := os.MkdirAll(*outDir, 0700); err != nil {
		return err
	}
	clientPath := filepath.Join(*outDir, "client.json")
	clientTextPath := filepath.Join(*outDir, "client.skirk")
	clientCommandPath := filepath.Join(*outDir, "client-command.txt")
	exitPath := filepath.Join(*outDir, "exit.json")
	readmePath := filepath.Join(*outDir, "README.md")
	if err := writeJSONFile(clientPath, clientCfg); err != nil {
		return err
	}
	if err := writeJSONFile(exitPath, exitCfg); err != nil {
		return err
	}
	clientText, err := skirk.EncodeConfigText(&clientCfg)
	if err != nil {
		return err
	}
	if err := writeTextFile(clientTextPath, clientText+"\n"); err != nil {
		return err
	}
	clientCommand := fmt.Sprintf("skirk serve-client --config '%s' --listen %s\n", clientText, *listen)
	if err := writeTextFile(clientCommandPath, clientCommand); err != nil {
		return err
	}
	if err := writeSetupReadme(readmePath, setupSummary{
		Title:             *title,
		ADCPath:           credsPath,
		Account:           creds.Account,
		ClientPath:        clientPath,
		ClientTextPath:    clientTextPath,
		ClientCommandPath: clientCommandPath,
		ExitPath:          exitPath,
		DriveFolderID:     folderID,
		Transport:         driveTransportName(baseDrive),
		Listen:            *listen,
		ClientRoute:       *clientRoute,
		ExitRoute:         *exitRoute,
	}); err != nil {
		return err
	}

	result := setupResult{
		Account:           creds.Account,
		ClientPath:        clientPath,
		ClientTextPath:    clientTextPath,
		ClientCommandPath: clientCommandPath,
		ClientText:        clientText,
		ClientCommand:     strings.TrimSpace(clientCommand),
		ExitPath:          exitPath,
		ReadmePath:        readmePath,
		DriveFolderID:     folderID,
		Transport:         driveTransportName(baseDrive),
		ClientRoute:       *clientRoute,
		ExitRoute:         *exitRoute,
		Listen:            *listen,
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"result":              "ok",
			"account":             result.Account,
			"client_config":       result.ClientPath,
			"client_text":         result.ClientTextPath,
			"client_command_file": result.ClientCommandPath,
			"client_config_text":  result.ClientText,
			"client_command":      result.ClientCommand,
			"exit_config":         result.ExitPath,
			"readme":              result.ReadmePath,
			"drive_folder_id":     result.DriveFolderID,
			"client_route":        result.ClientRoute,
			"exit_route":          result.ExitRoute,
			"note":                "generated configs contain Google refresh credentials; treat them like passwords",
			"transport":           result.Transport,
		})
	}
	printSetupResult(result)
	return nil
}

func setupDriveMailbox(ctx context.Context, auth skirk.AuthConfig, googleIP, sessionID string) (skirk.DriveConfig, string, error) {
	appDataDrive := skirk.DriveConfig{Space: "appDataFolder"}
	if err := validateDriveMailbox(ctx, auth, appDataDrive, googleIP, sessionID); err != nil {
		if !isAppDataScopeError(err) {
			return skirk.DriveConfig{}, "", err
		}
		fmt.Printf("Google OAuth cannot access Drive appDataFolder with the current scope; creating a Skirk-owned Drive mailbox folder instead.\n\n")
		folderName := "skirk-mailbox-" + sessionID
		folderDrive, folderID, fallbackErr := createVisibleDriveMailbox(ctx, auth, googleIP, folderName, sessionID)
		if fallbackErr != nil {
			return skirk.DriveConfig{}, "", fmt.Errorf("drive appDataFolder unavailable (%v); Drive folder mailbox setup failed: %w", err, fallbackErr)
		}
		return folderDrive, folderID, nil
	}
	return appDataDrive, "appDataFolder", nil
}

func driveAppDataValidationError(err error) error {
	if isAppDataScopeError(err) {
		return fmt.Errorf("drive appDataFolder validation failed: the OAuth token did not grant https://www.googleapis.com/auth/drive.appdata; rerun setup with --reset-google-login after enabling Drive API and authorizing that scope: %w", err)
	}
	return fmt.Errorf("drive appDataFolder validation failed: %w", err)
}

func isAppDataScopeError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "insufficientscopes") &&
		(strings.Contains(text, "application data folder") || strings.Contains(text, "appdatafolder"))
}

func createVisibleDriveMailbox(ctx context.Context, auth skirk.AuthConfig, googleIP, folderName, sessionID string) (skirk.DriveConfig, string, error) {
	cfg := skirk.Config{
		Secret: "setup-only",
		Auth:   auth,
		Route:  skirk.RouteConfig{Mode: "direct", GoogleIP: googleIP, TimeoutSeconds: 240},
		Tunnel: skirk.TunnelConfig{Profile: "fixed", ChunkSize: 4096, PollIntervalMS: 1200, Concurrency: 1, CleanupProcessed: true},
	}
	cfg.ApplyDefaults()
	drive, err := skirk.StoresFromConfig(ctx, &cfg)
	if err != nil {
		return skirk.DriveConfig{}, "", err
	}
	info, err := drive.EnsureFolder(ctx, folderName)
	if err != nil {
		return skirk.DriveConfig{}, "", fmt.Errorf("drive mailbox folder create failed: %w", err)
	}
	folderID := strings.TrimSpace(info.ID)
	if folderID == "" {
		return skirk.DriveConfig{}, "", errors.New("drive mailbox folder create returned empty id")
	}
	driveCfg := skirk.DriveConfig{FolderID: folderID}
	if err := validateDriveMailbox(ctx, auth, driveCfg, googleIP, sessionID); err != nil {
		return skirk.DriveConfig{}, "", err
	}
	return driveCfg, folderID, nil
}

func validateDriveMailbox(ctx context.Context, auth skirk.AuthConfig, driveCfg skirk.DriveConfig, googleIP, sessionID string) error {
	cfg := skirk.Config{
		Secret: "setup-only",
		Auth:   auth,
		Route:  skirk.RouteConfig{Mode: "direct", GoogleIP: googleIP, TimeoutSeconds: 240},
		Drive:  driveCfg,
		Tunnel: skirk.TunnelConfig{Profile: "fixed", ChunkSize: 4096, PollIntervalMS: 1200, Concurrency: 1, CleanupProcessed: true},
	}
	cfg.ApplyDefaults()
	drive, err := skirk.StoresFromConfig(ctx, &cfg)
	if err != nil {
		return err
	}
	name := "setup/" + sessionID + "/marker.json"
	if err := drive.Put(ctx, name, []byte(`{"ok":true}`)); err != nil {
		return fmt.Errorf("drive mailbox validation upload failed: %w", err)
	}
	if err := drive.Delete(ctx, name); err != nil {
		return fmt.Errorf("drive mailbox validation cleanup failed: %w", err)
	}
	return nil
}

func setupTunnelConfig(listen string, chunkSize, pollMS int, uploadConcurrency, downloadConcurrency int) skirk.TunnelConfig {
	cfg := skirk.TunnelConfig{
		Listen:           listen,
		Profile:          "auto",
		ChunkSize:        chunkSize,
		PollIntervalMS:   pollMS,
		Concurrency:      32,
		CleanupProcessed: true,
	}
	if uploadConcurrency > 0 {
		cfg.UploadConcurrency = uploadConcurrency
	}
	if downloadConcurrency > 0 {
		cfg.DownloadConcurrency = downloadConcurrency
	}
	return cfg
}

func (c adcCredentials) AuthConfig() skirk.AuthConfig {
	return skirk.AuthConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RefreshToken: c.RefreshToken,
		TokenURL:     "https://oauth2.googleapis.com/token",
	}
}

func readADCCredentials(path string) (adcCredentials, error) {
	if strings.TrimSpace(path) == "" {
		return adcCredentials{}, errors.New("empty ADC path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return adcCredentials{}, err
	}
	var creds adcCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return adcCredentials{}, err
	}
	if creds.Type != "" && creds.Type != "authorized_user" {
		return adcCredentials{}, fmt.Errorf("ADC type %q is not supported for one-file client configs; run user OAuth login", creds.Type)
	}
	if creds.ClientID == "" || creds.RefreshToken == "" {
		return adcCredentials{}, errors.New("ADC does not contain client_id and refresh_token")
	}
	return creds, nil
}

func defaultADCPath() string {
	if path := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		return path
	}
	if config := strings.TrimSpace(os.Getenv("CLOUDSDK_CONFIG")); config != "" {
		return filepath.Join(config, "application_default_credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "gcloud", "application_default_credentials.json")
		}
	}
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

func driveOAuthClientRequiredError(credsPath string, cause error) error {
	if cause == nil {
		cause = errors.New("Google login was requested without an OAuth client")
	}
	return fmt.Errorf("Google Drive setup needs a Google OAuth client.\n"+
		"Google blocks the default Google Cloud SDK OAuth client when Skirk requests Drive scopes, which produces the browser page \"This app is blocked\".\n"+
		"Official release builds should include Skirk's OAuth client automatically. Source/dev builds can set SKIRK_OAUTH_CLIENT_ID and SKIRK_OAUTH_CLIENT_SECRET, or pass --oauth-client-file for testing.\n"+
		"Current ADC path was %s; original credential error: %w", credsPath, cause)
}

func resolveOAuthClientCredentials(path string) (oauthClientCredentials, string, error) {
	if path != "" {
		creds, err := readOAuthClientCredentials(path)
		if err != nil {
			return oauthClientCredentials{}, "", err
		}
		return creds, "the OAuth client file " + path, nil
	}
	if creds, ok, err := oauthClientFromEnv(); err != nil || ok {
		if err != nil {
			return oauthClientCredentials{}, "", err
		}
		return creds, "the OAuth client configured in SKIRK_OAUTH_CLIENT_ID/SKIRK_OAUTH_CLIENT_SECRET", nil
	}
	if creds, ok, err := builtInOAuthClient(); err != nil || ok {
		if err != nil {
			return oauthClientCredentials{}, "", err
		}
		return creds, "Skirk's built-in OAuth client", nil
	}
	if _, statErr := os.Stat("oauth-client.json"); statErr == nil {
		creds, err := readOAuthClientCredentials("oauth-client.json")
		if err != nil {
			return oauthClientCredentials{}, "", err
		}
		return creds, "the OAuth client file oauth-client.json", nil
	}
	return oauthClientCredentials{}, "", nil
}

func oauthClientFromEnv() (oauthClientCredentials, bool, error) {
	return oauthClientFromPair(os.Getenv("SKIRK_OAUTH_CLIENT_ID"), os.Getenv("SKIRK_OAUTH_CLIENT_SECRET"), "SKIRK_OAUTH_CLIENT_ID/SKIRK_OAUTH_CLIENT_SECRET")
}

func builtInOAuthClient() (oauthClientCredentials, bool, error) {
	return oauthClientFromPair(defaultOAuthClientID, defaultOAuthClientSecret, "built-in OAuth client")
}

func oauthClientFromPair(clientID, clientSecret, source string) (oauthClientCredentials, bool, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" && clientSecret == "" {
		return oauthClientCredentials{}, false, nil
	}
	if clientID == "" || clientSecret == "" {
		return oauthClientCredentials{}, true, fmt.Errorf("%s must provide both client_id and client_secret", source)
	}
	return oauthClientCredentials{ClientID: clientID, ClientSecret: clientSecret}, true, nil
}

func readOAuthClientCredentials(path string) (oauthClientCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return oauthClientCredentials{}, err
	}
	var raw struct {
		Installed *oauthClientCredentials `json:"installed"`
		Web       *oauthClientCredentials `json:"web"`
		ClientID  string                  `json:"client_id"`
		Secret    string                  `json:"client_secret"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return oauthClientCredentials{}, err
	}
	creds := oauthClientCredentials{ClientID: raw.ClientID, ClientSecret: raw.Secret}
	if raw.Installed != nil {
		creds = *raw.Installed
	}
	if raw.Web != nil && creds.ClientID == "" {
		creds = *raw.Web
	}
	if strings.TrimSpace(creds.ClientID) == "" || strings.TrimSpace(creds.ClientSecret) == "" {
		return oauthClientCredentials{}, errors.New("OAuth client JSON must contain client_id and client_secret")
	}
	return creds, nil
}

func normalizeOAuthScopes(scopes string) string {
	scopes = strings.TrimSpace(scopes)
	if scopes == "" {
		scopes = defaultCustomOAuthScopes
	}
	parts := strings.FieldsFunc(scopes, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	clean := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		clean = append(clean, part)
	}
	return strings.Join(clean, " ")
}

func runGoogleDeviceOAuth(ctx context.Context, client oauthClientCredentials, oauthScopes string) (adcCredentials, error) {
	scopes := normalizeOAuthScopes(oauthScopes)
	code, err := requestDeviceCode(ctx, client.ClientID, scopes)
	if err != nil {
		return adcCredentials{}, err
	}
	fmt.Printf("Open this URL in a browser and enter the code:\n\n%s\n\nCode: %s\n\nWaiting for Google approval...\n\n", code.VerificationURL, code.UserCode)
	token, err := pollDeviceToken(ctx, client, code)
	if err != nil {
		return adcCredentials{}, err
	}
	return adcCredentials{
		Account:      "unknown",
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		RefreshToken: token.RefreshToken,
		Type:         "authorized_user",
	}, nil
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	ErrorCode       string `json:"error_code"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

type deviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func requestDeviceCode(ctx context.Context, clientID, scopes string) (deviceCodeResponse, error) {
	values := url.Values{}
	values.Set("client_id", strings.TrimSpace(clientID))
	values.Set("scope", scopes)
	var out deviceCodeResponse
	if err := postOAuthForm(ctx, "https://oauth2.googleapis.com/device/code", values, &out); err != nil {
		return out, err
	}
	if out.ErrorCode != "" || out.Error != "" {
		return out, fmt.Errorf("device code request failed: %s %s", firstNonEmpty(out.ErrorCode, out.Error), out.ErrorDesc)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURL == "" {
		return out, errors.New("device code response was missing required fields")
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 1800
	}
	return out, nil
}

func pollDeviceToken(ctx context.Context, client oauthClientCredentials, code deviceCodeResponse) (deviceTokenResponse, error) {
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	interval := time.Duration(code.Interval) * time.Second
	for {
		if time.Now().After(deadline) {
			return deviceTokenResponse{}, errors.New("device authorization expired before approval")
		}
		select {
		case <-ctx.Done():
			return deviceTokenResponse{}, ctx.Err()
		case <-time.After(interval):
		}
		values := url.Values{}
		values.Set("client_id", strings.TrimSpace(client.ClientID))
		values.Set("client_secret", strings.TrimSpace(client.ClientSecret))
		values.Set("device_code", code.DeviceCode)
		values.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		var out deviceTokenResponse
		err := postOAuthForm(ctx, "https://oauth2.googleapis.com/token", values, &out)
		if err == nil && out.RefreshToken != "" {
			return out, nil
		}
		if out.Error == "authorization_pending" {
			continue
		}
		if out.Error == "slow_down" {
			interval += 5 * time.Second
			continue
		}
		if out.Error != "" {
			return out, fmt.Errorf("device token request failed: %s %s", out.Error, out.ErrorDesc)
		}
		if err != nil {
			return out, err
		}
		return out, errors.New("device token response did not include a refresh token")
	}
}

func postOAuthForm(ctx context.Context, endpoint string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("OAuth response JSON decode failed status=%d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusPreconditionRequired || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("OAuth request failed status=%d body=%q", resp.StatusCode, string(body))
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

func writeTextFile(path string, text string) error {
	return os.WriteFile(path, []byte(text), 0600)
}

type setupSummary struct {
	Title             string
	ADCPath           string
	Account           string
	ClientPath        string
	ClientTextPath    string
	ClientCommandPath string
	ExitPath          string
	DriveFolderID     string
	Transport         string
	Listen            string
	ClientRoute       string
	ExitRoute         string
}

type setupResult struct {
	Account           string
	ClientPath        string
	ClientTextPath    string
	ClientCommandPath string
	ClientText        string
	ClientCommand     string
	ExitPath          string
	ReadmePath        string
	DriveFolderID     string
	Transport         string
	ClientRoute       string
	ExitRoute         string
	Listen            string
}

func printSetupResult(result setupResult) {
	fmt.Println()
	fmt.Println("Skirk kit created.")
	fmt.Printf("Google account: %s\n", result.Account)
	fmt.Printf("Exit config: %s\n", result.ExitPath)
	fmt.Printf("Client JSON config: %s\n", result.ClientPath)
	fmt.Printf("Client text config: %s\n", result.ClientTextPath)
	fmt.Printf("Ready client command: %s\n", result.ClientCommandPath)
	fmt.Printf("Transport: %s\n", result.Transport)
	if result.DriveFolderID != "" {
		fmt.Printf("Data folder: %s\n", result.DriveFolderID)
	}
	fmt.Println()
	fmt.Println("Run this on the exit machine:")
	fmt.Println()
	fmt.Printf("skirk serve-exit --config %s\n", result.ExitPath)
	fmt.Println()
	fmt.Println("Copy and send this one-line client config:")
	fmt.Println()
	fmt.Println(result.ClientText)
	fmt.Println()
	fmt.Println("Or copy and send this full client command:")
	fmt.Println()
	fmt.Println(result.ClientCommand)
	fmt.Println()
	fmt.Println("Anyone using the client config does not need Google login or gcloud.")
	fmt.Println("Treat the client config like a password. Revoke/delete the kit if it leaks.")
}

func writeSetupReadme(path string, summary setupSummary) error {
	content := fmt.Sprintf(`# Skirk Generated Kit

Created kit: %s

Google account: %s
ADC path: %s
Transport: %s
Data store: %s
Client route: %s
Exit route: %s

## What To Run

On the machine with normal internet egress, run the exit:

`+"```bash"+`
skirk serve-exit --config %s
`+"```"+`

On the client machine, run the SOCKS proxy:

`+"```bash"+`
skirk serve-client --config %s --listen %s
curl --socks5-hostname %s http://example.com/
`+"```"+`

Or send the one-line text config instead of a JSON file:

`+"```bash"+`
read -r SKIRK_CLIENT_CONFIG
# paste the contents of %s, press Enter, then run:
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen %s
`+"```"+`

A ready-to-copy command is also written to:

`+"```text"+`
%s
`+"```"+`

## Config Handling

Send only `+"`client.skirk`"+` or `+"`client.json`"+` to client devices. Keep `+"`exit.json`"+` on the exit machine.

All generated client and exit configs contain Google refresh credentials and the Skirk tunnel secret. Treat them like passwords:

- do not commit them;
- do not paste them into logs or chats;
- regenerate the kit if one leaks.

## Cleanup / Disconnect

Processed mailbox objects are deleted during normal runtime, and `+"`serve-exit`"+` starts a janitor for stale leftovers. To inspect old mailbox objects manually:

`+"```bash"+`
skirk cleanup --config %s --older-than 2h
`+"```"+`

To delete those matched stale objects:

`+"```bash"+`
skirk cleanup --config %s --older-than 2h --delete
`+"```"+`

To revoke the embedded OAuth token:

`+"```bash"+`
skirk revoke --config %s --revoke-oauth
`+"```"+`

Then delete the local kit directory when you no longer need it.

To immediately invalidate every config generated from this OAuth login, revoke the app token from the Google account security page or run Google's OAuth revocation endpoint against the refresh token.

## Notes

The exit can be a VPS, a home server, or a laptop. It does not need an inbound port because both sides exchange encrypted chunks through Google Drive. A VPS is still best for reliability because laptops sleep, move networks, and disappear when closed.
`, summary.Title, summary.Account, summary.ADCPath, summary.Transport, summary.DriveFolderID, summary.ClientRoute, summary.ExitRoute, summary.ExitPath, summary.ClientPath, summary.Listen, summary.Listen, summary.ClientTextPath, summary.Listen, summary.ClientCommandPath, summary.ExitPath, summary.ExitPath, summary.ExitPath)
	return os.WriteFile(path, []byte(content), 0600)
}

func driveTransportName(drive skirk.DriveConfig) string {
	if drive.Space == "appDataFolder" {
		return "drive_appdata"
	}
	return "drive_folder"
}
