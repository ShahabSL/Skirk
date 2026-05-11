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
	"os/exec"
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

const defaultCustomOAuthScopes = "openid,email,https://www.googleapis.com/auth/drive.appdata"

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
	title := fs.String("title", defaultTitle, "Google workspace title prefix")
	sheet := fs.String("sheet", "skirk", "Google Sheet tab name")
	adcPath := fs.String("adc", "", "Application Default Credentials JSON path")
	noLogin := fs.Bool("no-gcloud-login", false, "fail instead of launching gcloud login if ADC is missing")
	googleLogin := fs.Bool("google-login", false, "run Google login even if existing credentials are present")
	resetGoogleLogin := fs.Bool("reset-google-login", false, "revoke local gcloud and ADC credentials before Google login")
	oauthClientFile := fs.String("oauth-client-file", "", "Google OAuth TV/Limited Input client JSON; uses device login and your own OAuth project/quota")
	oauthScopes := fs.String("oauth-scopes", defaultCustomOAuthScopes, "comma- or space-separated scopes used with --oauth-client-file")
	clientRoute := fs.String("client-route", "google_front", "client Google API route: direct, real_pinned, google_front, google_front_pinned")
	exitRoute := fs.String("exit-route", "direct", "exit Google API route: direct, real_pinned, google_front, google_front_pinned")
	clientProxy := fs.String("client-proxy", "", "optional upstream SOCKS5 URL for the client")
	exitProxy := fs.String("exit-proxy", "", "optional upstream SOCKS5 URL for the exit")
	googleIP := fs.String("google-ip", "216.239.38.120", "Google edge IP for pinned routes")
	listen := fs.String("listen", "127.0.0.1:18080", "client SOCKS5 listen address")
	chunkSize := fs.Int("chunk-size", 1024*1024, "maximum tunnel chunk size")
	pollMS := fs.Int("poll-ms", 100, "mailbox poll interval in milliseconds")
	clientConcurrency := fs.Int("client-concurrency", 0, "legacy client Drive upload/download concurrency; sets both split knobs")
	exitConcurrency := fs.Int("exit-concurrency", 0, "legacy exit Drive upload/download concurrency; sets both split knobs")
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
	if *clientConcurrency > 0 {
		*clientUploadConcurrency = *clientConcurrency
		*clientDownloadConcurrency = *clientConcurrency
	}
	if *exitConcurrency > 0 {
		*exitUploadConcurrency = *exitConcurrency
		*exitDownloadConcurrency = *exitConcurrency
	}

	credsPath := firstNonEmpty(*adcPath, defaultADCPath())
	creds, err := readADCCredentials(credsPath)
	loginRequested := *googleLogin || strings.TrimSpace(*oauthClientFile) != ""
	if *resetGoogleLogin {
		fmt.Printf("Resetting local Google credentials before login.\n\n")
		if strings.TrimSpace(*oauthClientFile) != "" {
			_ = os.Remove(credsPath)
		} else {
			if err := runGcloudCredentialReset(ctx); err != nil {
				return err
			}
		}
		creds = adcCredentials{}
		err = errors.New("Google login was reset")
	}
	if loginRequested || err != nil {
		if *noLogin {
			return fmt.Errorf("google ADC unavailable at %s: %w", credsPath, err)
		}
		if oauthPath := strings.TrimSpace(*oauthClientFile); oauthPath != "" {
			fmt.Printf("Google login will use the device flow with your OAuth client file, so Drive API quota is charged to your own Google project.\n\n")
			creds, err = runGoogleDeviceOAuth(ctx, oauthPath, *oauthScopes)
			if err != nil {
				return err
			}
		} else if *googleLogin && err == nil {
			fmt.Printf("Google login was requested. Skirk will run gcloud and ask you to paste the browser code.\n\n")
			if err := runGcloudLogin(ctx); err != nil {
				return err
			}
			creds, err = readADCCredentials(credsPath)
			if err != nil {
				return fmt.Errorf("google ADC still unavailable at %s after login: %w", credsPath, err)
			}
		} else {
			fmt.Printf("Google login is required. Skirk will run gcloud and ask you to paste the browser code.\n\n")
			if err := runGcloudLogin(ctx); err != nil {
				return err
			}
			creds, err = readADCCredentials(credsPath)
			if err != nil {
				return fmt.Errorf("google ADC still unavailable at %s after login: %w", credsPath, err)
			}
		}
	}
	if strings.TrimSpace(creds.Account) == "" {
		creds.Account = "unknown"
	}
	auth := creds.AuthConfig()
	spreadsheetID := ""
	folderID := "appDataFolder"

	secret, err := skirk.RandomSecret()
	if err != nil {
		return err
	}
	session, err := skirk.NewSessionID()
	if err != nil {
		return err
	}
	sessionID := skirk.SessionString(session)
	baseDrive := skirk.DriveConfig{Space: "appDataFolder"}
	baseSheets := skirk.SheetsConfig{SpreadsheetID: spreadsheetID, Range: *sheet + "!A:D"}
	if err := validateDriveMailbox(ctx, auth, baseDrive, *googleIP, sessionID); err != nil {
		return err
	}
	clientCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: *clientRoute, Proxy: *clientProxy, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Drive:     baseDrive,
		Sheets:    baseSheets,
		Tunnel:    setupTunnelConfig(*listen, *chunkSize, *pollMS, *clientUploadConcurrency, *clientDownloadConcurrency),
	}
	exitCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: *exitRoute, Proxy: *exitProxy, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Drive:     baseDrive,
		Sheets:    baseSheets,
		Tunnel:    setupTunnelConfig(*listen, *chunkSize, *pollMS, *exitUploadConcurrency, *exitDownloadConcurrency),
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
		SpreadsheetID:     spreadsheetID,
		DriveFolderID:     folderID,
		Transport:         driveTransportName(baseDrive, baseSheets),
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
		SpreadsheetID:     spreadsheetID,
		DriveFolderID:     folderID,
		Transport:         driveTransportName(baseDrive, baseSheets),
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
			"spreadsheet_id":      result.SpreadsheetID,
			"drive_folder_id":     result.DriveFolderID,
			"client_route":        result.ClientRoute,
			"exit_route":          result.ExitRoute,
			"note":                "generated configs contain Google refresh credentials; treat them like passwords",
			"transport":           driveTransportName(baseDrive, baseSheets),
		})
	}
	printSetupResult(result)
	return nil
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
	drive, _, _, err := skirk.StoresFromConfig(ctx, &cfg)
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

func setupTunnelConfig(listen string, chunkSize, pollMS, uploadConcurrency, downloadConcurrency int) skirk.TunnelConfig {
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

func ensureGcloud(ctx context.Context) (string, error) {
	gcloud, err := findGcloud()
	if err == nil {
		return gcloud, nil
	}
	fmt.Printf("Google Cloud CLI was not found. Skirk will install it under ~/google-cloud-sdk.\n\n")
	return installGcloud(ctx)
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

func gcloudLoginArgs() []string {
	return []string{
		"auth", "login",
		"--no-launch-browser",
		"--enable-gdrive-access",
		"--update-adc",
		"--force",
	}
}

func runGcloudLogin(ctx context.Context) error {
	gcloud, err := ensureGcloud(ctx)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, gcloud, gcloudLoginArgs()...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withGcloudPath(os.Environ())
	return cmd.Run()
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

func runGoogleDeviceOAuth(ctx context.Context, oauthClientFile, oauthScopes string) (adcCredentials, error) {
	client, err := readOAuthClientCredentials(oauthClientFile)
	if err != nil {
		return adcCredentials{}, err
	}
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

func runGcloudCredentialReset(ctx context.Context) error {
	gcloud, err := ensureGcloud(ctx)
	if err != nil {
		return err
	}
	commands := [][]string{
		{"auth", "application-default", "revoke", "--quiet"},
		{"auth", "revoke", "--all", "--quiet"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, gcloud, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = withGcloudPath(os.Environ())
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: gcloud %s failed: %v\n", strings.Join(args, " "), err)
		}
	}
	return nil
}

func findGcloud() (string, error) {
	if path, err := exec.LookPath("gcloud"); err == nil {
		return path, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, "google-cloud-sdk", "bin", "gcloud")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("gcloud not found; install Google Cloud CLI or run setup with --adc /path/to/application_default_credentials.json")
}

func installGcloud(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	archive, err := gcloudArchiveName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "skirk-gcloud-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	archivePath := filepath.Join(tmp, archive)
	url := "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/" + archive
	fmt.Printf("Downloading Google Cloud CLI archive: %s\n", url)
	if err := downloadFile(ctx, url, archivePath); err != nil {
		return "", err
	}
	fmt.Printf("Extracting Google Cloud CLI to %s\n", home)
	cmd := exec.CommandContext(ctx, "tar", "-xzf", archivePath, "-C", home)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withGcloudPath(os.Environ())
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("Google Cloud CLI extract failed: %w", err)
	}
	gcloud := filepath.Join(home, "google-cloud-sdk", "bin", "gcloud")
	if _, err := os.Stat(gcloud); err != nil {
		return "", fmt.Errorf("Google Cloud CLI install finished but %s was not found: %w", gcloud, err)
	}
	return gcloud, nil
}

func gcloudArchiveName(goos, goarch string) (string, error) {
	if goos != "linux" {
		return "", fmt.Errorf("automatic Google Cloud CLI install is not supported on %s; install gcloud manually or run setup with --adc", goos)
	}
	switch goarch {
	case "amd64":
		return "google-cloud-cli-linux-x86_64.tar.gz", nil
	case "arm64":
		return "google-cloud-cli-linux-arm.tar.gz", nil
	case "386":
		return "google-cloud-cli-linux-x86.tar.gz", nil
	default:
		return "", fmt.Errorf("automatic Google Cloud CLI install does not support %s/%s; install gcloud manually or run setup with --adc", goos, goarch)
	}
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed status=%d", resp.StatusCode)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func withGcloudPath(env []string) []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return env
	}
	bin := filepath.Join(home, "google-cloud-sdk", "bin")
	path := os.Getenv("PATH")
	path = bin + string(os.PathListSeparator) + path
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			continue
		}
		out = append(out, item)
	}
	return append(out, "PATH="+path)
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
	SpreadsheetID     string
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
	SpreadsheetID     string
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
	if result.SpreadsheetID != "" {
		fmt.Printf("Control spreadsheet: %s\n", result.SpreadsheetID)
	}
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

Created workspace: %s

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

To delete the Google workspace created by this kit:

`+"```bash"+`
skirk workspace delete --config %s --delete-drive-folder
`+"```"+`

To immediately invalidate every config generated from this OAuth login, revoke the app token from the Google account security page or run Google's OAuth revocation endpoint against the refresh token.

## Notes

The exit can be a VPS, a home server, or a laptop. It does not need an inbound port because both sides exchange encrypted chunks through Google Drive. A VPS is still best for reliability because laptops sleep, move networks, and disappear when closed.
`, summary.Title, summary.Account, summary.ADCPath, summary.Transport, summary.DriveFolderID, summary.ClientRoute, summary.ExitRoute, summary.ExitPath, summary.ClientPath, summary.Listen, summary.Listen, summary.ClientTextPath, summary.Listen, summary.ClientCommandPath, summary.ExitPath)
	return os.WriteFile(path, []byte(content), 0600)
}

func driveTransportName(drive skirk.DriveConfig, sheets skirk.SheetsConfig) string {
	if drive.Space == "appDataFolder" {
		return "drive_appdata"
	}
	if sheets.SpreadsheetID != "" {
		return "drive_folder+sheets"
	}
	return "drive_folder"
}
