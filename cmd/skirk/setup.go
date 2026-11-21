package main

import (
	"bufio"
	"bytes"
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

	"github.com/ShahabSL/Skirk/internal/skirk"
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
	ClientSecret string `json:"client_secret,omitempty"`
	Flow         string `json:"skirk_oauth_flow,omitempty"`
}

const generatedClientCommand = "skirk serve-client --config ./client.skirk\n"

const defaultCustomOAuthScopes = "https://www.googleapis.com/auth/drive.file"

var (
	defaultOAuthClientID     string
	defaultOAuthClientSecret string
)

func setup(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("setup needs init or renew-oauth")
	}
	switch args[0] {
	case "init":
		return setupInit(ctx, args[1:])
	case "renew-oauth":
		return setupRenewOAuth(ctx, args[1:])
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
	resetGoogleLogin := fs.Bool("reset-google-login", false, "start a new Google login for this run")
	oauthMode := fs.String("oauth-mode", "personal", "Google OAuth mode: personal, easy (shared), or auto (interactive choice)")
	oauthFlow := fs.String("oauth-flow", "auto", "Google OAuth flow: auto, device, or desktop")
	oauthClientFile := fs.String("oauth-client-file", "", "override Google OAuth client JSON")
	quotaProject := fs.String("quota-project", "", "optional user-owned Google Cloud project for Drive API quota; requires enabled Drive API and serviceusage.services.use")
	oauthScopes := fs.String("oauth-scopes", defaultCustomOAuthScopes, "comma- or space-separated scopes used for Google OAuth login")
	clientRoute := fs.String("client-route", "google_front_pinned", "client Google API route: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
	exitRoute := fs.String("exit-route", "google_front_pinned", "exit Google API route: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
	clientProxy := fs.String("client-proxy", "", "optional upstream SOCKS5 URL for client Google API traffic")
	exitProxy := fs.String("exit-proxy", "", "optional outbound proxy URL for exit target traffic, for example socks5h://127.0.0.1:40000")
	enableUDP := fs.Bool("udp", true, "enable shared UDP and exit DNS using a dedicated Drive folder")
	var discovery kitDiscoveryOptions
	fs.BoolVar(&discovery.groupDrivePolls, "group-drive-polls", false, "combine TCP discovery lists and let a shared UDP exit poll clients together")
	fs.BoolVar(&discovery.conserveIdleUDP, "conserve-idle-udp", false, "check for incoming UDP less often on quiet clients (up to one extra poll tick after silence)")
	ipv6Mode := setupIPv6Mode("auto")
	fs.Var(&ipv6Mode, "ipv6", "IPv6 for UDP kits: auto, on, or off")
	googleIP := fs.String("google-ip", "216.239.38.120", "Google edge IP for pinned routes")
	listen := fs.String("listen", "127.0.0.1:18080", "client SOCKS5 listen address")
	chunkSize := fs.Int("chunk-size", 8*1024*1024, "maximum tunnel chunk size")
	pollMS := fs.Int("poll-ms", 1000, "Drive mailbox poll interval in milliseconds")
	clientUploadConcurrency := fs.Int("client-upload-concurrency", 0, "client Drive upload concurrency; 0 uses auto profile")
	clientDownloadConcurrency := fs.Int("client-download-concurrency", 0, "client Drive download concurrency; 0 uses auto profile")
	exitUploadConcurrency := fs.Int("exit-upload-concurrency", 0, "exit Drive upload concurrency; 0 uses auto profile")
	exitDownloadConcurrency := fs.Int("exit-download-concurrency", 0, "exit Drive download concurrency; 0 uses auto profile")
	startExit := fs.Bool("start-exit", runtime.GOOS == "linux", "install and start the exit service after generating the kit on Linux")
	exitServiceName := fs.String("exit-service-name", defaultServiceName, "systemd service name used when --start-exit is true")
	exitServiceUser := fs.String("exit-service-user", "", "user to run the exit service as; defaults to the current user")
	exitServiceEnable := fs.Bool("exit-service-enable", true, "enable the exit service at boot when --start-exit is true")
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
	if ipv6Mode == "on" && !*enableUDP {
		return fmt.Errorf("--ipv6 requires --udp")
	}
	explicitUDP := false
	fs.Visit(func(option *flag.Flag) { explicitUDP = explicitUDP || option.Name == "udp" })
	requireUDP := explicitUDP || ipv6Mode == "on"
	if *enableUDP && usesHTTPExitProxy(*exitProxy) {
		if requireUDP {
			return fmt.Errorf("HTTP exit proxies carry TCP only; use SOCKS5 with UDP support or --udp=false")
		}
		*enableUDP = false
		if !*jsonOut {
			fmt.Println("HTTP exit proxy selected. The kit will use TCP.")
		}
	}
	if value := strings.TrimSpace(*quotaProject); value != "" {
		if err := skirk.ValidateGoogleProjectID(value); err != nil {
			return fmt.Errorf("--quota-project: %w", err)
		}
	}
	if err := checkSetupOutput(*outDir); err != nil {
		return err
	}

	selectedOAuthMode, err := normalizeOAuthMode(*oauthMode)
	if err != nil {
		return err
	}
	selectedOAuthFlow, err := normalizeOAuthFlow(*oauthFlow)
	if err != nil {
		return err
	}
	var setupReader *bufio.Reader
	getSetupReader := func() *bufio.Reader {
		if setupReader == nil {
			setupReader = bufio.NewReader(os.Stdin)
		}
		return setupReader
	}
	oauthClientFileValue := strings.TrimSpace(*oauthClientFile)
	if shouldPromptOAuthMode(*adcPath, *noLogin, *jsonOut, selectedOAuthMode, oauthClientFileValue) {
		selectedOAuthMode, oauthClientFileValue, err = promptSetupOAuthMode(ctx, getSetupReader())
		if err != nil {
			return err
		}
	}
	var oauthClient oauthClientCredentials
	var oauthSource string
	if *adcPath == "" && !*noLogin {
		if selectedOAuthMode == "personal" && oauthClientFileValue == "" && os.Getenv("SKIRK_OAUTH_CLIENT_ID") == "" {
			const defaultFile = "oauth-client.json"
			if _, statErr := os.Stat(defaultFile); statErr == nil {
				oauthClientFileValue = defaultFile
			} else {
				if !isInteractiveTerminal() || *jsonOut {
					return errors.New("run skirk setup init in an interactive terminal to paste your Desktop app client ID and client secret. Alternatively, pass --oauth-client-file PATH or set SKIRK_OAUTH_CLIENT_ID and SKIRK_OAUTH_CLIENT_SECRET (see docs/personal-google-setup.md)")
				}
				oauthClientFileValue, err = promptPersonalOAuthClientFile(ctx, getSetupReader(), defaultFile)
				if err != nil {
					return err
				}
			}
		}
		oauthClient, oauthSource, err = resolveOAuthClientCredentialsForMode(oauthClientFileValue, selectedOAuthMode != "personal")
		if err != nil {
			return err
		}
		selectedOAuthFlow, err = resolveSetupOAuthFlow(selectedOAuthMode, selectedOAuthFlow, oauthClient)
		if err != nil {
			return err
		}
		if selectedOAuthMode == "personal" && !*jsonOut {
			printPersonalOAuthPublishingNote()
		}
	}
	credsPath := firstNonEmpty(*adcPath, defaultADCPath())
	creds, err := readADCCredentials(credsPath)
	loginRequested := *googleLogin || *resetGoogleLogin || oauthSource != ""
	if loginRequested || err != nil {
		if *noLogin {
			return fmt.Errorf("google ADC unavailable at %s: %w", credsPath, err)
		}
		if oauthSource != "" {
			fmt.Printf("Google login will use %s (%s OAuth flow).\n\n", oauthSource, selectedOAuthFlow)
			creds, err = runGoogleOAuth(ctx, oauthClient, *oauthScopes, selectedOAuthFlow, getSetupReader())
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
	exitGoogleRoute := skirk.RouteConfig{Mode: *exitRoute, GoogleIP: *googleIP, TimeoutSeconds: 240}
	clientTunnel := setupTunnelConfig(*listen, *chunkSize, *pollMS, *clientUploadConcurrency, *clientDownloadConcurrency)
	clientCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     skirk.RouteConfig{Mode: *clientRoute, Proxy: *clientProxy, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Tunnel:    clientTunnel,
	}
	exitTunnel := setupTunnelConfig(*listen, *chunkSize, *pollMS, *exitUploadConcurrency, *exitDownloadConcurrency)
	exitTunnel.ExitProxy = strings.TrimSpace(*exitProxy)
	exitCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      auth,
		Route:     exitGoogleRoute,
		Tunnel:    exitTunnel,
	}
	quotaProjectID := strings.TrimSpace(*quotaProject)
	if *enableUDP {
		configureSetupUDP(&clientCfg.Tunnel, ipv6Mode == "on")
		configureSetupUDP(&exitCfg.Tunnel, ipv6Mode == "on")
	}
	for _, candidate := range []skirk.Config{clientCfg, exitCfg} {
		candidate.Drive = skirk.DriveConfig{FolderID: "setup-validation", QuotaProject: quotaProjectID}
		candidate.ApplyDefaults()
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("invalid setup options: %w", err)
		}
	}
	if *enableUDP && exitTunnel.ExitProxy != "" {
		proxyUDP, err := setupExitProxyUDP(ctx, exitTunnel.ExitProxy, requireUDP)
		if err != nil {
			return err
		}
		if !proxyUDP {
			*enableUDP = false
			clientCfg.Tunnel, exitCfg.Tunnel = clientTunnel, exitTunnel
			if !*jsonOut {
				fmt.Println("The SOCKS5 exit proxy does not support UDP. The kit will use TCP.")
			}
		}
	}
	if *enableUDP {
		if err := skirk.ProbeExitDNS(ctx, exitTunnel.ExitProxy); err != nil {
			return errors.New("exit DNS check failed: allow DNS over TLS to dns.google on TCP port 853 through the exit route, then retry. Use --udp=false for a TCP kit")
		}
		if ipv6Mode == "auto" {
			ipv6 := skirk.ProbeExitIPv6(ctx, exitTunnel.ExitProxy)
			clientCfg.Tunnel.UDPPreviewIPv6, exitCfg.Tunnel.UDPPreviewIPv6 = ipv6, ipv6
			if !*jsonOut && !ipv6 {
				fmt.Println("The exit IPv6 probe did not pass. The kit will use IPv4. Use --ipv6=on if you have verified the exit separately.")
			}
		}
	}
	if err := os.MkdirAll(*outDir, 0700); err != nil {
		return err
	}
	lockPath := filepath.Join(*outDir, ".setup-in-progress")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("another setup is using this directory; use a fresh --out directory if the earlier setup was interrupted")
		}
		return err
	}
	defer os.Remove(lockPath)
	if err := lock.Close(); err != nil {
		return err
	}
	if err := checkSetupOutput(*outDir); err != nil {
		return err
	}
	var baseDrive skirk.DriveConfig
	var folderID string
	if *enableUDP {
		baseDrive, folderID, err = createVisibleDriveMailbox(ctx, auth, exitGoogleRoute, "skirk-mailbox-"+sessionID, sessionID, quotaProjectID)
	} else {
		baseDrive, folderID, err = setupDriveMailbox(ctx, auth, exitGoogleRoute, sessionID, quotaProjectID)
	}
	if err != nil {
		return err
	}
	configureKitDiscovery(baseDrive, &clientCfg, &exitCfg, discovery)
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
	if err := writeTextFile(clientCommandPath, generatedClientCommand); err != nil {
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
		StartExit:         *startExit,
		ServiceName:       *exitServiceName,
		Platform:          runtime.GOOS,
	}); err != nil {
		return err
	}

	result := setupResult{
		Account:           creds.Account,
		ClientPath:        clientPath,
		ClientTextPath:    clientTextPath,
		ClientCommandPath: clientCommandPath,
		ClientText:        clientText,
		ClientCommand:     strings.TrimSpace(generatedClientCommand),
		ExitPath:          exitPath,
		ReadmePath:        readmePath,
		DriveFolderID:     folderID,
		Transport:         driveTransportName(baseDrive),
		ClientRoute:       *clientRoute,
		ExitRoute:         *exitRoute,
		Listen:            *listen,
	}
	if *startExit {
		runtimeStatus, err := startExitAfterSetup(ctx, result.ExitPath, *exitServiceName, *exitServiceUser, *exitServiceEnable, *jsonOut)
		if err != nil {
			return err
		}
		result.ExitRuntime = runtimeStatus
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
			"udp_enabled":         clientCfg.Tunnel.UDPPreviewSharedUDP,
			"ipv6_enabled":        clientCfg.Tunnel.UDPPreviewIPv6,
			"note":                "generated configs contain Google refresh credentials; treat them like passwords",
			"transport":           result.Transport,
			"exit_runtime":        result.ExitRuntime,
		})
	}
	printSetupResult(result)
	return nil
}

func checkSetupOutput(directory string) error {
	for _, name := range []string{"client.json", "client.skirk", "client-command.txt", "exit.json", "README.md"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			return fmt.Errorf("setup output %s already contains %s; choose a fresh --out directory or use setup renew-oauth", directory, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func startExitAfterSetup(ctx context.Context, exitPath, serviceName, serviceUser string, enable bool, quiet bool) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("--start-exit is only supported on Linux; run serve-exit manually on %s", runtime.GOOS)
	}
	name := strings.TrimSpace(serviceName)
	if name == "" {
		name = defaultServiceName
	}
	if err := installSystemdService(ctx, serviceInstallOptions{
		Name:       name,
		ConfigPath: exitPath,
		User:       serviceUser,
		Start:      true,
		Enable:     enable,
		Quiet:      quiet,
	}); err != nil {
		return "", fmt.Errorf("exit service start failed after setup: %w", err)
	}
	unit, err := normalizeSystemdServiceName(name)
	if err != nil {
		return "", err
	}
	return unit + " started", nil
}

func repairMailbox(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair-mailbox", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json and client.json")
	configPath := fs.String("config", "", "exit config path; defaults to <kit>/exit.json")
	force := fs.Bool("force", false, "create a fresh mailbox even if the current mailbox validates")
	startExit := fs.Bool("start-exit", false, "restart the exit service after rewriting the kit")
	exitServiceName := fs.String("exit-service-name", defaultServiceName, "systemd service name used with --start-exit")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	kit := strings.TrimSpace(*kitDir)
	if kit == "" {
		kit = "skirk-kit"
	}
	exitPath := strings.TrimSpace(*configPath)
	if exitPath == "" {
		exitPath = filepath.Join(kit, "exit.json")
	}
	exitCfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return err
	}

	validationErr := validateDriveMailbox(ctx, exitCfg.Auth, exitCfg.Drive, exitCfg.Route, exitCfg.SessionID)
	if validationErr == nil && !*force {
		result := map[string]any{
			"result":  "ok",
			"changed": false,
			"message": "mailbox already validates",
			"config":  exitPath,
		}
		if *jsonOut {
			return printJSON(result)
		}
		fmt.Printf("Mailbox already validates for %s\n", exitPath)
		return nil
	}

	clientPath := filepath.Join(kit, "client.json")
	clientTextPath := filepath.Join(kit, "client.skirk")
	configs, err := loadConfigKit(kit, exitPath, checkRepairClientIdentity)
	if err != nil {
		return err
	}
	exitCfg = configs.exitConfig
	folderName := "skirk-mailbox-" + exitCfg.SessionID
	_, folderID, err := createVisibleDriveMailbox(ctx, exitCfg.Auth, exitCfg.Route, folderName, exitCfg.SessionID, exitCfg.Drive.QuotaProject)
	if err != nil {
		return fmt.Errorf("repair mailbox failed after validation error (%v): %w", validationErr, err)
	}

	if err := configs.writeRepairedMailbox(folderID); err != nil {
		return err
	}
	clientUpdated := configs.clientConfig != nil

	runtimeStatus := ""
	if *startExit {
		if err := serviceCommand(ctx, []string{"restart", "--name", *exitServiceName}); err != nil {
			return err
		}
		runtimeStatus = *exitServiceName + " restarted"
	}

	result := map[string]any{
		"result":             "ok",
		"changed":            true,
		"previous_valid":     validationErr == nil,
		"exit_config":        exitPath,
		"client_config":      clientPath,
		"client_updated":     clientUpdated,
		"drive_folder_id":    folderID,
		"validation_error":   "",
		"exit_runtime":       runtimeStatus,
		"client_config_text": clientTextPath,
	}
	if validationErr != nil {
		result["validation_error"] = validationErr.Error()
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("Repaired Drive mailbox for %s\n", exitPath)
	fmt.Printf("Data folder: %s\n", folderID)
	if clientUpdated {
		fmt.Printf("Updated client config: %s\n", clientPath)
		fmt.Printf("Updated client text config: %s\n", clientTextPath)
	}
	if runtimeStatus != "" {
		fmt.Printf("Exit runtime: %s\n", runtimeStatus)
	}
	return nil
}

func checkRepairClientIdentity(exit, client *skirk.Config) error {
	if exit.SessionID != client.SessionID || exit.Secret != client.Secret {
		return errors.New("client profile does not match the exit session and secret")
	}
	return nil
}

func (kit *configKit) writeRepairedMailbox(folderID string) error {
	files, err := kit.rewriteFiles(func(data []byte) ([]byte, error) {
		return replaceMailboxFolderJSON(data, folderID)
	}, func(profile *skirk.Config) {
		profile.Drive.FolderID = folderID
		profile.Drive.Space = ""
	})
	if err != nil {
		return err
	}
	backupDir, err := replaceKitFiles(filepath.Dir(kit.exitPath), ".mailbox-repair-backup-", files)
	if err != nil {
		return err
	}
	return os.RemoveAll(backupDir)
}

// Repair creates a visible folder. Keep every other field, including settings
// written by a newer client, and retain each peer's independent quota project.
func replaceMailboxFolderJSON(data []byte, folderID string) ([]byte, error) {
	if cfg, inline, err := skirk.ParseInlineConfig(string(data)); inline {
		if err != nil {
			return nil, err
		}
		data, err = json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("config must be a JSON object")
	}
	drive := map[string]json.RawMessage{}
	found := false
	for key, raw := range config {
		if strings.EqualFold(key, "drive") {
			if found {
				return nil, errors.New("config contains multiple Drive sections")
			}
			found = true
			if err := json.Unmarshal(raw, &drive); err != nil {
				return nil, err
			}
			delete(config, key)
		}
	}
	if drive == nil {
		drive = make(map[string]json.RawMessage)
	}
	for key := range drive {
		if strings.EqualFold(key, "space") || strings.EqualFold(key, "folder_id") {
			delete(drive, key)
		}
	}
	drive["folder_id"], _ = json.Marshal(folderID)
	encodedDrive, err := json.Marshal(drive)
	if err != nil {
		return nil, err
	}
	config["drive"] = encodedDrive
	updated, err := json.MarshalIndent(config, "", "  ")
	return append(updated, '\n'), err
}

func setupDriveMailbox(ctx context.Context, auth skirk.AuthConfig, route skirk.RouteConfig, sessionID, quotaProject string) (skirk.DriveConfig, string, error) {
	appDataDrive := skirk.DriveConfig{Space: "appDataFolder", QuotaProject: quotaProject}
	if err := validateDriveMailbox(ctx, auth, appDataDrive, route, sessionID); err != nil {
		if !isAppDataScopeError(err) {
			return skirk.DriveConfig{}, "", err
		}
		fmt.Printf("Google OAuth cannot access Drive appDataFolder with the current scope; creating a Skirk-owned Drive mailbox folder instead.\n\n")
		folderName := "skirk-mailbox-" + sessionID
		folderDrive, folderID, fallbackErr := createVisibleDriveMailbox(ctx, auth, route, folderName, sessionID, quotaProject)
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

func createVisibleDriveMailbox(ctx context.Context, auth skirk.AuthConfig, route skirk.RouteConfig, folderName, sessionID, quotaProject string) (skirk.DriveConfig, string, error) {
	cfg := skirk.Config{
		Secret: "setup-only",
		Auth:   auth,
		Route:  route,
		Drive:  skirk.DriveConfig{QuotaProject: quotaProject},
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
	driveCfg := skirk.DriveConfig{FolderID: folderID, QuotaProject: quotaProject}
	if err := validateDriveMailbox(ctx, auth, driveCfg, route, sessionID); err != nil {
		return skirk.DriveConfig{}, "", err
	}
	return driveCfg, folderID, nil
}

func validateDriveMailbox(ctx context.Context, auth skirk.AuthConfig, driveCfg skirk.DriveConfig, route skirk.RouteConfig, sessionID string) error {
	cfg := skirk.Config{
		Secret: "setup-only",
		Auth:   auth,
		Route:  route,
		Drive:  driveCfg,
		Tunnel: skirk.TunnelConfig{Profile: "fixed", ChunkSize: 4096, PollIntervalMS: 1200, Concurrency: 1, CleanupProcessed: true},
	}
	cfg.ApplyDefaults()
	drive, err := skirk.StoresFromConfig(ctx, &cfg)
	if err != nil {
		return err
	}
	name := "setup/" + sessionID + "/marker.json"
	marker := []byte(`{"ok":true}`)
	info, err := drive.PutObject(ctx, name, marker)
	if err != nil {
		return fmt.Errorf("drive mailbox validation upload failed: %w", err)
	}
	if strings.TrimSpace(info.ID) == "" {
		return errors.New("drive mailbox validation upload returned no file id")
	}
	got, readErr := drive.GetByID(ctx, info.ID)
	cleanupErr := drive.DeleteID(ctx, info.ID)
	if readErr != nil {
		return fmt.Errorf("drive mailbox validation download failed: %w", errors.Join(readErr, cleanupErr))
	}
	if !bytes.Equal(got, marker) {
		return errors.Join(errors.New("drive mailbox validation downloaded different content"), cleanupErr)
	}
	if cleanupErr != nil {
		return fmt.Errorf("drive mailbox validation cleanup failed: %w", cleanupErr)
	}
	return nil
}

func setupTunnelConfig(listen string, chunkSize, pollMS int, uploadConcurrency, downloadConcurrency int) skirk.TunnelConfig {
	cfg := skirk.TunnelConfig{
		Listen:           listen,
		Profile:          "auto",
		Transport:        "muxv5",
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

type kitDiscoveryOptions struct {
	groupDrivePolls bool
	conserveIdleUDP bool
}

func configureKitDiscovery(mailbox skirk.DriveConfig, client, exit *skirk.Config, options kitDiscoveryOptions) {
	mailbox.AdaptiveMuxDiscovery = options.groupDrivePolls
	client.Drive, exit.Drive = mailbox, mailbox
	exit.Tunnel.UDPPreviewGroupedDiscovery = options.groupDrivePolls && exit.Tunnel.UDPPreviewSharedUDP
	client.Tunnel.ConserveIdleUDP = options.conserveIdleUDP && client.Tunnel.UDPPreviewSharedUDP
}

func configureSetupUDP(cfg *skirk.TunnelConfig, ipv6 bool) {
	cfg.UDPPreviewPublicIPv4 = true
	cfg.UDPPreviewIPv6 = ipv6
	cfg.UDPPreviewRemoteDNS = true
	cfg.UDPPreviewNamedUDP = true
	cfg.UDPPreviewStableUDP = true
	cfg.UDPPreviewSharedUDP = true
	cfg.UDPPreviewSharedAssociations = 32
	cfg.UDPPreviewBatchBytes = 16 * 1024
	cfg.UDPPreviewMedia = true
	cfg.UDPPreviewMediaBytes = 128 * 1024
	cfg.UDPPreviewAdaptiveUploads = true
}

type setupIPv6Mode string

func (mode *setupIPv6Mode) String() string { return string(*mode) }
func (*setupIPv6Mode) IsBoolFlag() bool    { return true }

func (mode *setupIPv6Mode) Set(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto":
		*mode = "auto"
	case "on", "true", "1":
		*mode = "on"
	case "off", "false", "0":
		*mode = "off"
	default:
		return fmt.Errorf("IPv6 mode must be auto, on, or off")
	}
	return nil
}

func usesHTTPExitProxy(address string) bool {
	u, err := url.Parse(strings.TrimSpace(address))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

func setupExitProxyUDP(ctx context.Context, proxyURL string, required bool) (bool, error) {
	capable, err := skirk.ProbeExitProxyUDP(ctx, proxyURL)
	if err != nil {
		return false, fmt.Errorf("could not verify UDP through the SOCKS5 exit proxy. Check UDP forwarding or use --udp=false for a TCP-only kit: %w", err)
	}
	if !capable && required {
		return false, errors.New("the SOCKS5 exit proxy does not support UDP ASSOCIATE. Use a SOCKS5 proxy with UDP support or --udp=false for a TCP-only kit")
	}
	return capable, nil
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
		"Create a Desktop app OAuth client in your Google project and paste its Client ID and Client secret at the interactive skirk setup init prompts. Alternatively, pass --oauth-client-file PATH or set SKIRK_OAUTH_CLIENT_ID, SKIRK_OAUTH_CLIENT_SECRET, and SKIRK_OAUTH_FLOW=desktop. See docs/personal-google-setup.md.\n"+
		"Current ADC path was %s; original credential error: %w", credsPath, cause)
}

func resolveOAuthClientCredentials(path string) (oauthClientCredentials, string, error) {
	return resolveOAuthClientCredentialsForMode(path, true)
}

func resolveOAuthClientCredentialsForMode(path string, allowBuiltIn bool) (oauthClientCredentials, string, error) {
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
		source := "the OAuth client configured in SKIRK_OAUTH_CLIENT_ID"
		if strings.TrimSpace(creds.ClientSecret) != "" {
			source += "/SKIRK_OAUTH_CLIENT_SECRET"
		}
		return creds, source, nil
	}
	if allowBuiltIn {
		if creds, ok, err := builtInOAuthClient(); err != nil || ok {
			if err != nil {
				return oauthClientCredentials{}, "", err
			}
			return creds, "Skirk's built-in OAuth client", nil
		}
	}
	if _, statErr := os.Stat("oauth-client.json"); statErr == nil {
		creds, err := readOAuthClientCredentials("oauth-client.json")
		if err != nil {
			return oauthClientCredentials{}, "", err
		}
		return creds, "the OAuth client file oauth-client.json", nil
	}
	if !allowBuiltIn {
		return oauthClientCredentials{}, "", errors.New("personal OAuth mode needs --oauth-client-file, oauth-client.json, or SKIRK_OAUTH_CLIENT_ID")
	}
	return oauthClientCredentials{}, "", nil
}

func oauthClientFromEnv() (oauthClientCredentials, bool, error) {
	creds, ok, err := oauthClientFromPair(os.Getenv("SKIRK_OAUTH_CLIENT_ID"), os.Getenv("SKIRK_OAUTH_CLIENT_SECRET"), "SKIRK_OAUTH_CLIENT_ID/SKIRK_OAUTH_CLIENT_SECRET")
	if err != nil || !ok {
		return creds, ok, err
	}
	flow, err := normalizeOAuthFlow(os.Getenv("SKIRK_OAUTH_FLOW"))
	if err != nil {
		return oauthClientCredentials{}, true, err
	}
	if flow != "auto" {
		creds.Flow = flow
	}
	return creds, true, nil
}

func builtInOAuthClient() (oauthClientCredentials, bool, error) {
	creds, ok, err := oauthClientFromPair(defaultOAuthClientID, defaultOAuthClientSecret, "built-in OAuth client")
	if err != nil || !ok {
		return creds, ok, err
	}
	creds.Flow = "device"
	return creds, true, nil
}

func oauthClientFromPair(clientID, clientSecret, source string) (oauthClientCredentials, bool, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" && clientSecret == "" {
		return oauthClientCredentials{}, false, nil
	}
	if clientID == "" {
		return oauthClientCredentials{}, true, fmt.Errorf("%s must provide client_id when client_secret is set", source)
	}
	return oauthClientCredentials{ClientID: clientID, ClientSecret: clientSecret}, true, nil
}

func readOAuthClientCredentials(path string) (oauthClientCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return oauthClientCredentials{}, err
	}
	type oauthClientJSON struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		Flow         string   `json:"skirk_oauth_flow"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	var raw struct {
		Installed *oauthClientJSON `json:"installed"`
		Web       *oauthClientJSON `json:"web"`
		ClientID  string           `json:"client_id"`
		Secret    string           `json:"client_secret"`
		Flow      string           `json:"skirk_oauth_flow"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return oauthClientCredentials{}, err
	}
	creds := oauthClientCredentials{ClientID: raw.ClientID, ClientSecret: raw.Secret, Flow: raw.Flow}
	if raw.Installed != nil {
		creds = oauthClientCredentials{ClientID: raw.Installed.ClientID, ClientSecret: raw.Installed.ClientSecret, Flow: firstNonEmpty(raw.Flow, raw.Installed.Flow)}
		if creds.Flow == "" && hasLoopbackRedirect(raw.Installed.RedirectURIs) {
			creds.Flow = "desktop"
		}
	}
	if raw.Web != nil && creds.ClientID == "" {
		creds = oauthClientCredentials{ClientID: raw.Web.ClientID, ClientSecret: raw.Web.ClientSecret, Flow: firstNonEmpty(raw.Flow, raw.Web.Flow)}
	}
	creds.ClientID = strings.TrimSpace(creds.ClientID)
	creds.ClientSecret = strings.TrimSpace(creds.ClientSecret)
	creds.Flow = strings.TrimSpace(creds.Flow)
	if creds.ClientID == "" {
		return oauthClientCredentials{}, errors.New("OAuth client JSON must contain client_id")
	}
	if creds.Flow != "" {
		flow, err := normalizeOAuthFlow(creds.Flow)
		if err != nil {
			return oauthClientCredentials{}, err
		}
		if flow != "auto" {
			creds.Flow = flow
		}
	}
	return creds, nil
}

func hasLoopbackRedirect(redirectURIs []string) bool {
	for _, raw := range redirectURIs {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		host := strings.Trim(u.Hostname(), "[]")
		if host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1" {
			return true
		}
	}
	return false
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
	if strings.TrimSpace(client.ClientSecret) == "" {
		return adcCredentials{}, errors.New("Google device-code OAuth requires a client_secret. Create a Google OAuth client with application type \"Desktop app\" and use Skirk's personal desktop flow, or use a TVs and Limited Input client only if Google provides its client secret.")
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
		values := deviceTokenForm(client, code.DeviceCode)
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
			return out, deviceTokenError(out)
		}
		if err != nil {
			return out, err
		}
		return out, errors.New("device token response did not include a refresh token")
	}
}

func deviceTokenForm(client oauthClientCredentials, deviceCode string) url.Values {
	values := url.Values{}
	values.Set("client_id", strings.TrimSpace(client.ClientID))
	if secret := strings.TrimSpace(client.ClientSecret); secret != "" {
		values.Set("client_secret", secret)
	}
	values.Set("device_code", deviceCode)
	values.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	return values
}

func deviceTokenError(out deviceTokenResponse) error {
	if out.Error == "access_denied" {
		return fmt.Errorf("device token request failed: access_denied %s\n\n"+
			"Google blocked the OAuth consent step. If your personal OAuth app is in Testing, open https://console.cloud.google.com/auth/audience and add the exact Google account you used at google.com/device under Test users, then rerun setup. You can also publish the app to Production. This is not fixed by adding more scopes; Skirk requests drive.file automatically.", out.ErrorDesc)
	}
	return fmt.Errorf("device token request failed: %s %s", out.Error, out.ErrorDesc)
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

// Migrate only the exact command emitted by older kits. A user-owned command
// using a file stays untouched, including its options and permissions.
func planClientCommandUpdate(kitDir, listen string) (*kitFileUpdate, error) {
	path := filepath.Join(kitDir, "client-command.txt")
	command, err := readRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &kitFileUpdate{path: path, replacement: []byte(generatedClientCommand)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read client-command.txt: %w", err)
	}
	if !bytes.Contains(command, []byte(skirk.ConfigTextPrefix)) {
		return nil, nil
	}
	profile, err := readRegularFile(filepath.Join(kitDir, "client.skirk"))
	if err != nil {
		return nil, fmt.Errorf("read client.skirk for command migration: %w", err)
	}
	legacy := fmt.Sprintf("skirk serve-client --config '%s' --listen %s", bytes.TrimSpace(profile), listen)
	if strings.TrimSpace(string(command)) != legacy {
		return nil, errors.New("client-command.txt contains a custom inline profile; change its --config argument to ./client.skirk before updating this kit")
	}
	return &kitFileUpdate{path: path, original: command, replacement: []byte(generatedClientCommand)}, nil
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
	StartExit         bool
	ServiceName       string
	Platform          string
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
	ExitRuntime       string
}

func printSetupResult(result setupResult) {
	fmt.Println()
	fmt.Println("Skirk kit created.")
	fmt.Printf("Exit config: %s\n", result.ExitPath)
	fmt.Printf("Client profile: %s\n", result.ClientTextPath)
	fmt.Printf("Setup guide: %s\n", result.ReadmePath)
	fmt.Println()
	if strings.TrimSpace(result.ExitRuntime) != "" {
		fmt.Printf("Exit runtime: %s\n", result.ExitRuntime)
		fmt.Println()
	} else {
		fmt.Println("Run this on the exit machine:")
		fmt.Println()
		fmt.Printf("skirk serve-exit --config %s\n", result.ExitPath)
		fmt.Println()
	}
	fmt.Println("To scan or copy the client profile, run skirk and choose 11. Show client profile and QR code.")
	fmt.Println("Keep the profile and QR code private. They both contain the credentials to connect.")
}

func writeSetupReadme(path string, summary setupSummary) error {
	serviceName := strings.TrimSpace(summary.ServiceName)
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	serviceUnit, err := normalizeSystemdServiceName(serviceName)
	if err != nil {
		return err
	}
	serviceSection := setupReadmeServiceSection(summary, serviceName, serviceUnit)
	content := fmt.Sprintf(`# Skirk Generated Kit

Created kit: %s

Google account: %s
ADC path: %s
Transport: %s
Data store: %s
Client route: %s
Exit route: %s

## What To Run

%s

On the client machine, run the SOCKS proxy:

`+"```bash"+`
skirk serve-client --config %s --listen %s
curl --socks5-hostname %s http://example.com/
`+"```"+`

Run skirk and choose **11. Show client profile and QR code**. Select %s when prompted.
Scan the code in the Skirk phone app, or copy the profile text into the client app.

For a CLI client, copy the private profile to the client machine as `+"`client.skirk`"+`.
From the directory containing that file, run the command saved in %s:

`+"```bash"+`
skirk serve-client --config ./client.skirk
`+"```"+`

## Config Handling

Send only `+"`client.skirk`"+` or `+"`client.json`"+` to client devices. Keep `+"`exit.json`"+` on the exit machine.

All generated client and exit configs contain Google refresh credentials and the Skirk tunnel secret. Treat them like passwords:

- do not commit them;
- do not paste them into logs or chats;
- regenerate the kit if one leaks.

This kit uses the `+"`muxv5`"+` transport. Upgrade the exit and all clients together: older Skirk versions watch a separate `+"`muxv4/`"+` mailbox and cannot answer this kit. If Test exit does not verify after upgrading, check both versions and the exit status before renewing Google authorization.

## Cleanup / Disconnect

Processed mailbox objects are deleted during normal runtime, and `+"`serve-exit`"+` starts a janitor for stale leftovers. Without `+"`--prefix`"+`, manual cleanup scans Skirk runtime object prefixes (`+"`muxv5/`"+`, `+"`muxv4/`"+`, `+"`bench-drive/`"+`, and `+"`setup/`"+`):

`+"```bash"+`
skirk cleanup --config %s --older-than 2h
`+"```"+`

To delete those matched stale objects:

`+"```bash"+`
skirk cleanup --config %s --older-than 2h --delete
`+"```"+`

To delete all files directly in this mailbox before removing the kit:

`+"```bash"+`
skirk cleanup --config %s --all --older-than 1ns --delete --max-pages 20000
`+"```"+`

To revoke the embedded OAuth token:

`+"```bash"+`
skirk revoke --config %s --revoke-oauth
`+"```"+`

To remove Skirk from this Linux machine:

`+"```bash"+`
skirk uninstall --dry-run
skirk uninstall --yes --name %s
`+"```"+`

Then delete the local kit directory when you no longer need it, or include
`+"`--delete-kit --kit <kit-directory>`"+` in the uninstall command.

To immediately invalidate every config generated from this OAuth login, revoke the app token from the Google account security page or run Google's OAuth revocation endpoint against the refresh token.

## Notes

The exit can be a VPS, a home server, or a laptop. It does not need an inbound port because both sides exchange encrypted chunks through Google Drive. A VPS is still best for reliability because laptops sleep, move networks, and disappear when closed.
`, summary.Title, summary.Account, summary.ADCPath, summary.Transport, summary.DriveFolderID, summary.ClientRoute, summary.ExitRoute, serviceSection, summary.ClientPath, summary.Listen, summary.Listen, summary.ClientTextPath, summary.ClientCommandPath, summary.ExitPath, summary.ExitPath, summary.ExitPath, summary.ExitPath, serviceName)
	return os.WriteFile(path, []byte(content), 0600)
}

func setupReadmeServiceSection(summary setupSummary, serviceName, serviceUnit string) string {
	if summary.Platform != "" && summary.Platform != "linux" {
		return fmt.Sprintf(`Service auto-start is only available on Linux/systemd. Run the exit manually on this machine:

`+"```bash"+`
skirk serve-exit --config %s
`+"```"+`

On a Linux exit machine, install the service with:

`+"```bash"+`
skirk service install --config %s --name %s
`+"```", summary.ExitPath, summary.ExitPath, serviceName)
	}
	if summary.StartExit {
		return fmt.Sprintf(`Setup starts the exit as %s by default. Check it with:

`+"```bash"+`
skirk service status --name %s
`+"```"+`

Restart it after changing config:

`+"```bash"+`
skirk service restart --name %s
`+"```", serviceUnit, serviceName, serviceName)
	}
	return fmt.Sprintf(`Setup was run with `+"`--start-exit=false`"+`. Start the exit later:

`+"```bash"+`
skirk service install --config %s --name %s
`+"```"+`

Or run it in the foreground:

`+"```bash"+`
skirk serve-exit --config %s
`+"```", summary.ExitPath, serviceName, summary.ExitPath)
}

func driveTransportName(drive skirk.DriveConfig) string {
	if drive.Space == "appDataFolder" {
		return "drive_appdata"
	}
	return "drive_folder"
}
