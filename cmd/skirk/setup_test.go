package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
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

func TestProfileDecoderPrivatePipe(t *testing.T) {
	cfg := skirk.Config{Secret: "private-test-value", Drive: skirk.DriveConfig{FolderID: "fixture"}}
	profile, err := skirk.EncodeConfigText(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := decodeConfig([]string{"--stdin", "--out", "-"}, strings.NewReader(profile), &output); err != nil {
		t.Fatal(err)
	}
	var got skirk.Config
	if err := json.Unmarshal(output.Bytes(), &got); err != nil || got.Secret != cfg.Secret {
		t.Fatal("private pipe did not round-trip the profile")
	}
	// A user may have left an earlier export with permissive file permissions.
	path := filepath.Join(t.TempDir(), "private profile.json")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := decodeConfig([]string{"--stdin", "--out", path}, strings.NewReader(profile), io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("decoded profile did not receive private file permissions")
	}
	for _, tc := range []struct {
		args []string
		data string
	}{
		{[]string{"--stdin", "--out", "-"}, "invalid"},
		{[]string{"--stdin", "--out", "-"}, strings.Repeat("x", (8<<20)+1)},
		{[]string{"--stdin", "--config", profile, "--out", "-"}, profile},
		{[]string{"--out", "-"}, ""},
	} {
		output.Reset()
		err := decodeConfig(tc.args, strings.NewReader(tc.data), &output)
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), profile) {
			t.Fatal("invalid import must fail without writing profile data")
		}
	}
}

func TestGeneratedClientCommandReadsCopiedProfile(t *testing.T) {
	kit := t.TempDir()
	_, client, profile := writeTestOAuthRenewKit(t, kit, "test-secret")
	commandPath := filepath.Join(kit, "client-command.txt")
	for _, tc := range []struct {
		name    string
		command string
		write   bool
		reject  bool
	}{
		{name: "missing", write: true},
		{name: "already migrated", command: generatedClientCommand},
		{name: "custom file command", command: "custom-client --profile /private/selected-profile"},
		{name: "legacy", command: "skirk serve-client --config '" + profile + "' --listen " + client.Tunnel.Listen + "\n", write: true},
		{name: "unrecognized inline command", command: "custom-client --profile '" + profile + "'", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.command == "" {
				if err := os.Remove(commandPath); err != nil {
					t.Fatal(err)
				}
			} else if err := writeTextFile(commandPath, tc.command); err != nil {
				t.Fatal(err)
			}
			plan, err := planClientCommandUpdate(kit, client.Tunnel.Listen)
			if (err != nil) != tc.reject || (plan != nil) != tc.write {
				t.Fatalf("unexpected command migration: planned=%t err=%v", plan != nil, err)
			}
			if plan != nil {
				if string(plan.replacement) != generatedClientCommand || string(plan.original) != tc.command {
					t.Fatal("incorrect migration or backup")
				}
				fields := strings.Fields(generatedClientCommand)
				if len(fields) != 4 || strings.Join(fields[:3], " ") != "skirk serve-client --config" {
					t.Fatal("client command must take a single private profile path")
				}
				cfg, err := skirk.LoadConfig(filepath.Join(kit, fields[3]))
				if err != nil || cfg.Tunnel.Listen != client.Tunnel.Listen || cfg.Auth.RefreshToken != client.Auth.RefreshToken {
					t.Fatal("copied file does not supply the intended settings and grant")
				}
			}
		})
	}
}

func TestKitDiscoveryOptions(t *testing.T) {
	mailbox := skirk.DriveConfig{FolderID: "kit-folder", QuotaProject: "kit-project"}
	for _, group := range []bool{false, true} {
		for _, udp := range []bool{false, true} {
			client := skirk.Config{Secret: "test-kit-discovery"}
			var exit skirk.Config
			if udp {
				configureSetupUDP(&client.Tunnel, false)
				configureSetupUDP(&exit.Tunnel, false)
			}
			configureKitDiscovery(mailbox, &client, &exit, kitDiscoveryOptions{groupDrivePolls: group})
			encoded, err := skirk.EncodeConfigText(&client)
			if err != nil {
				t.Fatal(err)
			}
			imported, err := skirk.DecodeConfigText(encoded)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []*skirk.Config{imported, &exit} {
				if cfg.Drive.FolderID != mailbox.FolderID || cfg.Drive.QuotaProject != mailbox.QuotaProject || cfg.Drive.AdaptiveMuxDiscovery != group {
					t.Fatalf("unexpected mailbox discovery settings for group=%t udp=%t", group, udp)
				}
			}
			if imported.Tunnel.UDPPreviewGroupedDiscovery || exit.Tunnel.UDPPreviewGroupedDiscovery != (group && udp) {
				t.Fatalf("unexpected UDP discovery on client or exit for group=%t udp=%t", group, udp)
			}
		}
	}
}

func TestSetupConserveIdleUDPProfile(t *testing.T) {
	mailbox := skirk.DriveConfig{FolderID: "kit-folder", QuotaProject: "kit-project"}
	for _, udp := range []bool{false, true} {
		client := skirk.Config{Secret: "test-kit-discovery", Route: skirk.RouteConfig{Mode: "google_front_pinned"}}
		exit := skirk.Config{Route: skirk.RouteConfig{Mode: "direct"}}
		if udp {
			configureSetupUDP(&client.Tunnel, false)
			configureSetupUDP(&exit.Tunnel, false)
		}
		for _, options := range []kitDiscoveryOptions{{conserveIdleUDP: true}, {groupDrivePolls: true, conserveIdleUDP: true}, {}} {
			configureKitDiscovery(mailbox, &client, &exit, options)
			profile, err := skirk.EncodeConfigText(&client)
			if err != nil {
				t.Fatal(err)
			}
			imported, err := skirk.DecodeConfigText(profile)
			if err != nil {
				t.Fatal(err)
			}
			if imported.Tunnel.ConserveIdleUDP != (udp && options.conserveIdleUDP) || exit.Tunnel.ConserveIdleUDP {
				t.Fatalf("unexpected polling configuration: udp=%t options=%+v", udp, options)
			}
			if imported.Route.Mode != "google_front_pinned" || exit.Route.Mode != "direct" ||
				imported.Drive.AdaptiveMuxDiscovery != options.groupDrivePolls || exit.Tunnel.UDPPreviewGroupedDiscovery != (udp && options.groupDrivePolls) {
				t.Fatal("idle UDP option changed a separate route or discovery setting")
			}
		}
	}
}

func TestSetupKitUsesMuxV5AndRenewRejectsMixedTransport(t *testing.T) {
	created := setupTunnelConfig("127.0.0.1:18080", 1024, 100, 0, 0)
	if created.Transport != "muxv5" {
		t.Fatalf("new kit transport = %q, want muxv5", created.Transport)
	}
	exit := &skirk.Config{Tunnel: created}
	client := &skirk.Config{Tunnel: created}
	if err := checkOAuthRenewClientIdentity(exit, client); err != nil {
		t.Fatalf("matching kit transports rejected: %v", err)
	}
	client.Tunnel.Transport = "muxv4"
	if err := checkOAuthRenewClientIdentity(exit, client); err == nil || !strings.Contains(err.Error(), "transport version") {
		t.Fatalf("mixed kit transports did not fail with upgrade guidance: %v", err)
	}
}

func TestSetupUDPProfileValidatesAndRequiresDriveFolder(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		cfg := skirk.Config{
			Secret: "synthetic-secret",
			Drive:  skirk.DriveConfig{FolderID: "test-mailbox"},
			Tunnel: setupTunnelConfig("127.0.0.1:18080", 8<<20, 1000, 0, 0),
		}
		configureSetupUDP(&cfg.Tunnel, ipv6)
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if !cfg.Tunnel.UDPPreviewSharedUDP || !cfg.Tunnel.UDPPreviewRemoteDNS || cfg.Tunnel.UDPPreviewIPv6 != ipv6 {
			t.Fatalf("unexpected UDP capabilities for IPv6=%t", ipv6)
		}
		cfg.Drive = skirk.DriveConfig{Space: "appDataFolder"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("UDP setup accepted appDataFolder")
		}
	}
}

func TestSetupWithExplicitADCValidatesWithoutGoogleLogin(t *testing.T) {
	t.Setenv("SKIRK_OAUTH_CLIENT_ID", "unused-oauth-client")
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user","client_id":"synthetic","refresh_token":"synthetic"}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := setupInit(t.Context(), []string{"--adc", adc, "--no-gcloud-login", "--udp", "--exit-proxy", "socks5h://127.0.0.1:40000?unexpected=1"})
	if err == nil || !strings.Contains(err.Error(), "invalid setup options") {
		t.Fatalf("explicit ADC must validate UDP proxy options before Google access, got %v", err)
	}
}

func TestSetupExitProxyUDPUsesTCPForDefinitiveRefusalOnly(t *testing.T) {
	for _, required := range []bool{false, true} {
		name := "automatic UDP"
		if required {
			name = "explicit UDP"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var greeting [3]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting != [3]byte{5, 1, 0} {
					done <- errors.New("invalid SOCKS5 greeting")
					return
				}
				if _, err := conn.Write([]byte{5, 0}); err != nil {
					done <- err
					return
				}
				var request [10]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil || !bytes.Equal(request[:4], []byte{5, 3, 0, 1}) {
					done <- errors.New("expected SOCKS5 UDP ASSOCIATE")
					return
				}
				_, err = conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
				done <- err
			}()
			proxy := "socks5h://" + listener.Addr().String()
			if required {
				adc := filepath.Join(t.TempDir(), "adc.json")
				if err := os.WriteFile(adc, []byte(`{"type":"authorized_user","client_id":"synthetic","refresh_token":"synthetic"}`), 0600); err != nil {
					t.Fatal(err)
				}
				err := setupInit(t.Context(), []string{"--adc", adc, "--no-gcloud-login", "--udp", "--exit-proxy", proxy, "--out", t.TempDir()})
				if err == nil || !strings.Contains(err.Error(), "does not support UDP ASSOCIATE") || !strings.Contains(err.Error(), "--udp=false") {
					t.Fatalf("explicit UDP denied without guidance: %v", err)
				}
			} else if ok, err := setupExitProxyUDP(t.Context(), proxy, false); ok || err != nil {
				t.Fatalf("automatic UDP must select TCP after explicit refusal, got (%t, %v)", ok, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupRefusesToOverwriteExistingKit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exit.json")
	if err := os.WriteFile(path, []byte("existing kit"), 0600); err != nil {
		t.Fatal(err)
	}
	err := setupInit(t.Context(), []string{"--out", dir})
	if err == nil || !strings.Contains(err.Error(), "setup renew-oauth") {
		t.Fatalf("expected existing kit guidance, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing kit" {
		t.Fatalf("existing setup config changed: %v", err)
	}
}

func TestSetupIPv6SwitchAcceptsLegacyAndAutomaticValues(t *testing.T) {
	for input, want := range map[string]setupIPv6Mode{"true": "on", "false": "off", "off": "off", "on": "on", "auto": "auto"} {
		var got setupIPv6Mode
		if err := got.Set(input); err != nil || got != want {
			t.Fatalf("IPv6 switch %q = %q (%v), want %q", input, got, err, want)
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
		"", // Enter selects paste
		"client-id.apps.googleusercontent.com",
		"client-secret",
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
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
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

func TestPersonalOAuthPastePreservesExistingClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-client.json")
	existing := []byte(`{"installed":{"client_id":"previous-project"}}`)
	if err := os.WriteFile(path, existing, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := promptPersonalOAuthClientFile(t.Context(), bufio.NewReader(strings.NewReader("\nnew-project\nnew-secret\n")), path)
	if err != nil {
		t.Fatal(err)
	}
	if got == path || filepath.Dir(got) != filepath.Dir(path) {
		t.Fatalf("expected a separate file alongside %q, got %q", path, got)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, existing) {
		t.Fatalf("previous client file changed: %v", err)
	}
	creds, err := readOAuthClientCredentials(got)
	if err != nil || creds.ClientID != "new-project" || creds.ClientSecret != "new-secret" || creds.Flow != "desktop" {
		t.Fatalf("separately saved client did not round-trip: %v", err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("separately saved client file mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestPersonalOAuthDownloadedFileRemainsAnOption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "downloaded.json")
	if err := os.WriteFile(path, []byte(`{"installed":{"client_id":"existing-client"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(strings.NewReader("2\n" + path + "\n"))
	got, err := promptPersonalOAuthClientFile(t.Context(), reader, filepath.Join(dir, "oauth-client.json"))
	if err != nil || got != path {
		t.Fatalf("downloaded file selection = %q, %v", got, err)
	}
}

func TestPersonalOAuthMissingDownloadFallsBackToPaste(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "oauth-client.json")
	missing := filepath.Join(dir, "mistyped", "download.json")
	input := "2\n" + missing + "\n\nnew-project\nnew-secret\n"
	got, err := promptPersonalOAuthClientFile(t.Context(), bufio.NewReader(strings.NewReader(input)), defaultPath)
	if err != nil || got != defaultPath {
		t.Fatalf("paste after missing download saved to %q, %v", got, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected file at mistyped path: %v", err)
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

func TestSetupRejectsInvalidQuotaProjectBeforeGoogleLogin(t *testing.T) {
	err := setupInit(context.Background(), []string{"--quota-project", "invalid_project", "--no-gcloud-login"})
	if err == nil || !strings.Contains(err.Error(), "--quota-project") {
		t.Fatalf("setup error = %v, want quota-project validation error", err)
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
		ClientRoute:       "google_front_pinned",
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
		"skirk serve-client --config ./client.skirk",
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
		ClientRoute:       "google_front_pinned",
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
