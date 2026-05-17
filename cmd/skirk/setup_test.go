package main

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"skirk/internal/skirk"
	"strings"
	"testing"
)

func TestDriveOAuthClientRequiredErrorExplainsRecovery(t *testing.T) {
	got := driveOAuthClientRequiredError("/tmp/adc.json", os.ErrNotExist).Error()
	for _, want := range []string{
		"needs a Google OAuth client",
		"This app is blocked",
		"SKIRK_OAUTH_CLIENT_ID",
		"SKIRK_OAUTH_FLOW=desktop",
		"--oauth-client-file",
		"/tmp/adc.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("error missing %q in:\n%s", want, got)
		}
	}
}

func TestResolveOAuthClientCredentialsPrefersExplicitFileThenEnvThenBuiltIn(t *testing.T) {
	oldID, oldSecret := defaultOAuthClientID, defaultOAuthClientSecret
	t.Cleanup(func() {
		defaultOAuthClientID = oldID
		defaultOAuthClientSecret = oldSecret
	})
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "env-client")
	t.Setenv("SKIRK_OAUTH_CLIENT_SECRET", "env-secret")
	defaultOAuthClientID = "builtin-client"
	defaultOAuthClientSecret = "builtin-secret"

	path := filepath.Join(t.TempDir(), "oauth-client.json")
	if err := os.WriteFile(path, []byte(`{"installed":{"client_id":"file-client","client_secret":"file-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, source, err := resolveOAuthClientCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "file-client" || !strings.Contains(source, path) {
		t.Fatalf("explicit file was not preferred: creds=%#v source=%q", got, source)
	}

	got, source, err = resolveOAuthClientCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "env-client" || !strings.Contains(source, "SKIRK_OAUTH_CLIENT_ID") {
		t.Fatalf("env credentials were not preferred: creds=%#v source=%q", got, source)
	}
}

func TestResolveOAuthClientCredentialsUsesBuiltInWithoutFile(t *testing.T) {
	oldID, oldSecret := defaultOAuthClientID, defaultOAuthClientSecret
	t.Cleanup(func() {
		defaultOAuthClientID = oldID
		defaultOAuthClientSecret = oldSecret
	})
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "")
	t.Setenv("SKIRK_OAUTH_CLIENT_SECRET", "")
	defaultOAuthClientID = "builtin-client"
	defaultOAuthClientSecret = "builtin-secret"

	got, source, err := resolveOAuthClientCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "builtin-client" || !strings.Contains(source, "built-in") {
		t.Fatalf("built-in credentials were not used: creds=%#v source=%q", got, source)
	}
}

func TestResolveOAuthClientCredentialsAllowsEnvClientIDOnly(t *testing.T) {
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "env-client")
	t.Setenv("SKIRK_OAUTH_CLIENT_SECRET", "")
	t.Setenv("SKIRK_OAUTH_FLOW", "desktop")
	got, source, err := resolveOAuthClientCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "env-client" || got.ClientSecret != "" || got.Flow != "desktop" || !strings.Contains(source, "SKIRK_OAUTH_CLIENT_ID") {
		t.Fatalf("unexpected env client-id-only credentials: creds=%#v source=%q", got, source)
	}
}

func TestResolveOAuthClientCredentialsRejectsSecretOnlyEnv(t *testing.T) {
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "")
	t.Setenv("SKIRK_OAUTH_CLIENT_SECRET", "env-secret")
	_, _, err := resolveOAuthClientCredentials("")
	if err == nil {
		t.Fatal("expected secret-only env credential error")
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("secret-only env error should mention client_id: %s", err)
	}
}

func TestReadOAuthClientCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-client.json")
	if err := os.WriteFile(path, []byte(`{
  "installed": {
    "client_id": "client.apps.googleusercontent.com",
    "client_secret": "secret"
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readOAuthClientCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "client.apps.googleusercontent.com" || got.ClientSecret != "secret" || got.Flow != "" {
		t.Fatalf("unexpected OAuth client credentials: %#v", got)
	}
	desktopPath := filepath.Join(t.TempDir(), "desktop-oauth-client.json")
	if err := os.WriteFile(desktopPath, []byte(`{"installed":{"client_id":"desktop.apps.googleusercontent.com","client_secret":"secret","redirect_uris":["http://localhost"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = readOAuthClientCredentials(desktopPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "desktop.apps.googleusercontent.com" || got.ClientSecret != "secret" || got.Flow != "desktop" {
		t.Fatalf("unexpected desktop OAuth client credentials: %#v", got)
	}
	idOnlyPath := filepath.Join(t.TempDir(), "id-only-oauth-client.json")
	if err := os.WriteFile(idOnlyPath, []byte(`{"installed":{"client_id":"public-client.apps.googleusercontent.com"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = readOAuthClientCredentials(idOnlyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "public-client.apps.googleusercontent.com" || got.ClientSecret != "" {
		t.Fatalf("unexpected client-id-only OAuth client credentials: %#v", got)
	}
	badPath := filepath.Join(t.TempDir(), "bad-oauth-client.json")
	if err := os.WriteFile(badPath, []byte(`{"installed":{"client_secret":"secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readOAuthClientCredentials(badPath)
	if err == nil {
		t.Fatal("expected invalid OAuth client file error")
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("invalid OAuth error should mention client_id: %s", err)
	}
}

func TestPromptPersonalOAuthClientFileCanPasteCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-client.json")
	input := strings.Join([]string{
		"1", // paste credentials
		"client-id.apps.googleusercontent.com",
		"client-secret",
		"", // save to default path
		"",
	}, "\n")
	gotPath, err := promptPersonalOAuthClientFile(context.Background(), bufio.NewReader(strings.NewReader(input)), path)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("oauth client file mode = %o, want 0600", info.Mode().Perm())
	}
	creds, err := readOAuthClientCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "client-id.apps.googleusercontent.com" || creds.ClientSecret != "client-secret" {
		t.Fatalf("unexpected pasted credentials: %#v", creds)
	}
	if creds.Flow != "desktop" {
		t.Fatalf("pasted credentials flow = %q, want desktop", creds.Flow)
	}
}

func TestPromptPersonalOAuthClientFileCanPasteClientIDOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-client.json")
	input := strings.Join([]string{
		"1", // paste credentials
		"client-id.apps.googleusercontent.com",
		"", // no client secret shown by Google
		"", // save to default path
		"",
	}, "\n")
	gotPath, err := promptPersonalOAuthClientFile(context.Background(), bufio.NewReader(strings.NewReader(input)), path)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	creds, err := readOAuthClientCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "client-id.apps.googleusercontent.com" || creds.ClientSecret != "" {
		t.Fatalf("unexpected client-id-only pasted credentials: %#v", creds)
	}
	if creds.Flow != "desktop" {
		t.Fatalf("client-id-only pasted credentials flow = %q, want desktop", creds.Flow)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "client_secret") {
		t.Fatalf("client-id-only OAuth JSON should omit client_secret:\n%s", string(body))
	}
}

func TestDeviceOAuthRequiresClientSecret(t *testing.T) {
	_, err := runGoogleDeviceOAuth(context.Background(), oauthClientCredentials{ClientID: "client-id"}, defaultCustomOAuthScopes)
	if err == nil {
		t.Fatal("expected device OAuth without client secret to fail before network")
	}
	if !strings.Contains(err.Error(), "client_secret") || !strings.Contains(err.Error(), "Desktop app") {
		t.Fatalf("unexpected missing device secret error: %s", err)
	}
}

func TestDeviceTokenFormIncludesClientSecret(t *testing.T) {
	withSecret := deviceTokenForm(oauthClientCredentials{ClientID: "client-id", ClientSecret: "secret"}, "device-code")
	if got := withSecret.Get("client_id"); got != "client-id" {
		t.Fatalf("client_id = %q, want client-id", got)
	}
	if got := withSecret.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := withSecret.Get("client_secret"); got != "secret" {
		t.Fatalf("client_secret = %q, want secret", got)
	}
}

func TestDeviceTokenAccessDeniedExplainsTestUsers(t *testing.T) {
	err := deviceTokenError(deviceTokenResponse{Error: "access_denied", ErrorDesc: "Forbidden"})
	for _, want := range []string{"auth/audience", "Test users", "exact Google account", "not fixed by adding more scopes", "drive.file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("access denied error missing %q in:\n%s", want, err)
		}
	}
}

func TestResolvePersonalOAuthDoesNotFallBackToBuiltIn(t *testing.T) {
	oldID, oldSecret := defaultOAuthClientID, defaultOAuthClientSecret
	t.Cleanup(func() {
		defaultOAuthClientID = oldID
		defaultOAuthClientSecret = oldSecret
	})
	defaultOAuthClientID = "builtin-client"
	defaultOAuthClientSecret = "builtin-secret"
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "")
	t.Setenv("SKIRK_OAUTH_CLIENT_SECRET", "")
	_, _, err := resolveOAuthClientCredentialsForMode("", false)
	if err == nil {
		t.Fatal("expected personal OAuth mode to require personal credentials")
	}
	if !strings.Contains(err.Error(), "personal OAuth mode needs") {
		t.Fatalf("unexpected personal OAuth error: %s", err)
	}
}

func TestResolveSetupOAuthFlow(t *testing.T) {
	got, err := resolveSetupOAuthFlow("personal", "auto", oauthClientCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "desktop" {
		t.Fatalf("personal auto flow = %q, want desktop", got)
	}
	got, err = resolveSetupOAuthFlow("easy", "auto", oauthClientCredentials{Flow: "device"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "device" {
		t.Fatalf("easy built-in flow = %q, want device", got)
	}
	if _, err := resolveSetupOAuthFlow("easy", "desktop", oauthClientCredentials{}); err == nil {
		t.Fatal("expected easy desktop flow to be rejected")
	}
}

func TestDesktopOAuthHelpers(t *testing.T) {
	verifier := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	challenge := pkceChallenge(verifier)
	if challenge == "" || strings.ContainsAny(challenge, "+/=") {
		t.Fatalf("PKCE challenge is not raw-url-safe base64: %q", challenge)
	}
	authURL := desktopOAuthURL("client-id", "scope-a scope-b", "http://127.0.0.1:49152/", "state-1", verifier)
	for _, want := range []string{"https://accounts.google.com/o/oauth2/v2/auth?", "access_type=offline", "prompt=consent", "code_challenge_method=S256"} {
		if !strings.Contains(authURL, want) {
			t.Fatalf("desktop auth URL missing %q: %s", want, authURL)
		}
	}
	code, err := parseOAuthRedirectInput("http://127.0.0.1:49152/?state=state-1&code=abc123", "state-1")
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc123" {
		t.Fatalf("parsed code = %q, want abc123", code)
	}
	code, err = parseOAuthRedirectInput("4/0AbCdEf", "state-1")
	if err != nil {
		t.Fatal(err)
	}
	if code != "4/0AbCdEf" {
		t.Fatalf("raw code = %q", code)
	}
	if _, err := parseOAuthRedirectInput("http://127.0.0.1:49152/?state=stale&code=abc123", "state-1"); err == nil {
		t.Fatal("expected stale state to fail")
	}
}

func TestIsAppDataScopeError(t *testing.T) {
	err := &staticError{text: `drive mailbox validation upload failed: drive upload failed status=403 body="The granted scopes do not allow use of the Application Data folder. reason=insufficientScopes"`}
	if !isAppDataScopeError(err) {
		t.Fatal("expected appDataFolder insufficientScopes error to be recognized")
	}
	if isAppDataScopeError(&staticError{text: "drive upload failed status=403 body=userRateLimitExceeded"}) {
		t.Fatal("rate-limit errors must not be treated as scope errors")
	}
	got := driveAppDataValidationError(err).Error()
	for _, want := range []string{"drive.appdata", "--reset-google-login", "appDataFolder"} {
		if !strings.Contains(got, want) {
			t.Fatalf("validation error missing %q in:\n%s", want, got)
		}
	}
}

type staticError struct {
	text string
}

func (e *staticError) Error() string {
	return e.text
}

func TestNormalizeOAuthScopes(t *testing.T) {
	got := normalizeOAuthScopes("openid,email https://www.googleapis.com/auth/drive.appdata openid")
	for _, want := range []string{"openid", "email", "https://www.googleapis.com/auth/drive.appdata"} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalizeOAuthScopes missing %q in %q", want, got)
		}
	}
	if strings.Count(got, "openid") != 1 {
		t.Fatalf("normalizeOAuthScopes did not deduplicate: %q", got)
	}
}

func TestDefaultOAuthScopesIncludeDriveSetupRequirements(t *testing.T) {
	if got, want := defaultCustomOAuthScopes, "https://www.googleapis.com/auth/drive.file"; got != want {
		t.Fatalf("defaultCustomOAuthScopes = %q, want %q", got, want)
	}
	if strings.Contains(defaultCustomOAuthScopes, "https://www.googleapis.com/auth/drive,") ||
		strings.Contains(defaultCustomOAuthScopes, "https://www.googleapis.com/auth/drive.appdata") ||
		strings.Contains(defaultCustomOAuthScopes, "https://www.googleapis.com/auth/cloud-platform") {
		t.Fatalf("defaultCustomOAuthScopes should not request extra scopes: %q", defaultCustomOAuthScopes)
	}
}

func TestApplyTunnelOverridesConcurrencyDoesNotSetAutoProfileSplitCaps(t *testing.T) {
	cfg := &skirk.Config{
		Secret: "test-secret",
		Auth:   skirk.AuthConfig{AccessToken: "token"},
		Route:  skirk.RouteConfig{Mode: "direct"},
		Drive:  skirk.DriveConfig{Space: "appDataFolder"},
		Tunnel: skirk.TunnelConfig{Profile: "auto", ChunkSize: 16 * 1024 * 1024, PollIntervalMS: 100, BurstPollMS: 75, BurstPollWindowMS: 5000, Concurrency: 8},
	}
	if err := applyTunnelOverrides(cfg, 0, 0, 64, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Tunnel.Concurrency, 64; got != want {
		t.Fatalf("concurrency = %d, want %d", got, want)
	}
	if cfg.Tunnel.UploadConcurrency != 0 || cfg.Tunnel.DownloadConcurrency != 0 {
		t.Fatalf("split caps = upload %d download %d, want zero auto caps", cfg.Tunnel.UploadConcurrency, cfg.Tunnel.DownloadConcurrency)
	}
}

func TestApplyTunnelOverridesSplitCapsRemainExplicit(t *testing.T) {
	cfg := &skirk.Config{
		Secret: "test-secret",
		Auth:   skirk.AuthConfig{AccessToken: "token"},
		Route:  skirk.RouteConfig{Mode: "direct"},
		Drive:  skirk.DriveConfig{Space: "appDataFolder"},
		Tunnel: skirk.TunnelConfig{Profile: "auto", ChunkSize: 16 * 1024 * 1024, PollIntervalMS: 100, BurstPollMS: 75, BurstPollWindowMS: 5000, Concurrency: 8},
	}
	if err := applyTunnelOverrides(cfg, 0, 0, 0, 12, 48); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Tunnel.UploadConcurrency, 12; got != want {
		t.Fatalf("upload cap = %d, want %d", got, want)
	}
	if got, want := cfg.Tunnel.DownloadConcurrency, 48; got != want {
		t.Fatalf("download cap = %d, want %d", got, want)
	}
}

func TestWriteSetupReadmeDocumentsCurrentCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	err := writeSetupReadme(path, setupSummary{
		Title:             "test-kit",
		ADCPath:           "/tmp/adc.json",
		Account:           "user@example.com",
		ClientPath:        "skirk-kit/client.json",
		ClientTextPath:    "skirk-kit/client.skirk",
		ClientCommandPath: "skirk-kit/client-command.txt",
		ExitPath:          "skirk-kit/exit.json",
		DriveFolderID:     "appDataFolder",
		Transport:         "drive_appdata",
		ClientRoute:       "google_front",
		ExitRoute:         "direct",
		Listen:            "127.0.0.1:18080",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"skirk service install --config skirk-kit/exit.json --name skirk-exit",
		"skirk serve-client --config skirk-kit/client.json --listen 127.0.0.1:18080",
		"skirk cleanup --config skirk-kit/exit.json --older-than 2h",
		"skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000",
		"skirk revoke --config skirk-kit/exit.json --revoke-oauth",
		"skirk uninstall --yes --name skirk-exit",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated README missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "%!") {
		t.Fatalf("generated README has fmt mismatch:\n%s", text)
	}
}

func TestWriteSetupReadmeDocumentsStartedServiceName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	err := writeSetupReadme(path, setupSummary{
		Title:             "test-kit",
		ADCPath:           "/tmp/adc.json",
		Account:           "user@example.com",
		ClientPath:        "skirk-kit/client.json",
		ClientTextPath:    "skirk-kit/client.skirk",
		ClientCommandPath: "skirk-kit/client-command.txt",
		ExitPath:          "skirk-kit/exit.json",
		DriveFolderID:     "folder",
		Transport:         "drive_folder",
		ClientRoute:       "google_front",
		ExitRoute:         "direct",
		Listen:            "127.0.0.1:18080",
		StartExit:         true,
		ServiceName:       "skirk-custom",
		Platform:          "linux",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"Setup starts the exit as skirk-custom.service",
		"skirk service status --name skirk-custom",
		"skirk service restart --name skirk-custom",
		"skirk uninstall --yes --name skirk-custom",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated README missing %q:\n%s", want, text)
		}
	}
}
