package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func menu(ctx context.Context) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println()
		fmt.Print(skirkBanner)
		fmt.Println("1. Create Google kit (easy Skirk OAuth)")
		fmt.Println("2. Create Google kit (personal Google OAuth project)")
		fmt.Println("3. Run exit in this terminal")
		fmt.Println("4. Run client SOCKS in this terminal")
		fmt.Println("5. Run optional desktop dashboard")
		fmt.Println("6. Manage exit services and instances")
		fmt.Println("7. Revoke, clean, or delete kit")
		fmt.Println("8. Update installed Skirk")
		fmt.Println("9. Show commands")
		if runtime.GOOS == "linux" {
			fmt.Println("10. Uninstall Skirk from this Linux machine")
		}
		fmt.Println("0. Quit")
		choice, err := prompt(ctx, reader, "Select", "1")
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		switch choice {
		case "1":
			return createGoogleKitFromMenu(ctx, reader, "easy")
		case "2":
			return createGoogleKitFromMenu(ctx, reader, "personal")
		case "3":
			config, err := prompt(ctx, reader, "Exit config", defaultKitFile("exit.json"))
			if err != nil {
				return err
			}
			return serveExit(ctx, []string{"--config", config})
		case "4":
			config, err := prompt(ctx, reader, "Client config or pasted text", defaultKitFile("client.skirk"))
			if err != nil {
				return err
			}
			listen, err := prompt(ctx, reader, "SOCKS listen", "127.0.0.1:18080")
			if err != nil {
				return err
			}
			return serveClient(ctx, []string{"--config", config, "--listen", listen})
		case "5":
			config, err := prompt(ctx, reader, "Client config or pasted text", defaultKitFile("client.skirk"))
			if err != nil {
				return err
			}
			socks, err := prompt(ctx, reader, "SOCKS listen", "127.0.0.1:18080")
			if err != nil {
				return err
			}
			ui, err := prompt(ctx, reader, "UI listen", "127.0.0.1:18280")
			if err != nil {
				return err
			}
			return clientUI(ctx, []string{"--config", config, "--socks", socks, "--ui", ui})
		case "6":
			if err := instancesMenu(ctx, reader); err != nil {
				return err
			}
		case "7":
			config, err := prompt(ctx, reader, "Exit config", defaultKitFile("exit.json"))
			if err != nil {
				return err
			}
			stopService, err := promptYesNo(ctx, reader, "Stop exit service first", false)
			if err != nil {
				return err
			}
			if stopService {
				name, err := prompt(ctx, reader, "Service name", defaultServiceName)
				if err != nil {
					return err
				}
				if err := serviceCommand(ctx, []string{"stop", "--name", name}); err != nil {
					return err
				}
			}
			deleteDrive, err := promptYesNo(ctx, reader, "Delete stale Drive mailbox objects now", false)
			if err != nil {
				return err
			}
			if deleteDrive {
				if err := cleanup(ctx, []string{"--config", config, "--all", "--older-than", "1ns", "--delete", "--max-pages", "20000"}); err != nil {
					return err
				}
			}
			revokeOAuth, err := promptYesNo(ctx, reader, "Revoke Google OAuth token and invalidate generated configs", false)
			if err != nil {
				return err
			}
			if revokeOAuth {
				if err := revoke(ctx, []string{"--config", config, "--revoke-oauth"}); err != nil {
					return err
				}
			}
			deleteLocal, err := promptYesNo(ctx, reader, "Delete local kit directory", false)
			if err != nil {
				return err
			}
			if deleteLocal {
				if err := deleteKitDirectory(config); err != nil {
					return err
				}
			}
		case "8":
			if err := updateFromMenu(ctx, reader); err != nil {
				return err
			}
		case "9":
			usage()
		case "10":
			if runtime.GOOS != "linux" {
				fmt.Println("Unknown selection")
				continue
			}
			return uninstallFromMenu(ctx, reader)
		case "0", "q", "quit", "exit":
			return nil
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func uninstallFromMenu(ctx context.Context, reader *bufio.Reader) error {
	fmt.Println()
	fmt.Println("Linux uninstall can remove the exit service and installed binary.")
	fmt.Println("Drive cleanup, OAuth revocation, local kit deletion, and WARP wireproxy removal are optional.")
	serviceName, err := prompt(ctx, reader, "Service name", defaultServiceName)
	if err != nil {
		return err
	}
	removeService, err := promptYesNo(ctx, reader, "Remove exit systemd service", true)
	if err != nil {
		return err
	}
	binPath, err := prompt(ctx, reader, "Installed binary path", defaultUninstallBinaryPath())
	if err != nil {
		return err
	}
	removeBinary, err := promptYesNo(ctx, reader, "Remove installed binary", true)
	if err != nil {
		return err
	}
	configPath, err := prompt(ctx, reader, "Exit config for optional cleanup/revoke", defaultKitFile("exit.json"))
	if err != nil {
		return err
	}
	deleteDrive, err := promptYesNo(ctx, reader, "Delete Drive mailbox objects", false)
	if err != nil {
		return err
	}
	revokeOAuth, err := promptYesNo(ctx, reader, "Revoke Google OAuth token", false)
	if err != nil {
		return err
	}
	deleteKit, err := promptYesNo(ctx, reader, "Delete local kit directory", false)
	if err != nil {
		return err
	}
	kitDir := "skirk-kit"
	if deleteKit {
		kitDir, err = prompt(ctx, reader, "Kit directory", filepath.Dir(configPath))
		if err != nil {
			return err
		}
	}
	removeWireproxy, err := promptYesNo(ctx, reader, "Remove Skirk-installed WARP wireproxy", false)
	if err != nil {
		return err
	}
	confirm, err := promptYesNo(ctx, reader, "Proceed with uninstall", false)
	if err != nil {
		return err
	}
	if !confirm {
		return nil
	}

	args := []string{"--yes", "--name", serviceName, "--bin", binPath, "--config", configPath, "--kit", kitDir}
	if !removeService {
		args = append(args, "--service=false")
	}
	if !removeBinary {
		args = append(args, "--binary=false")
	}
	if deleteDrive {
		args = append(args, "--delete-drive")
	}
	if revokeOAuth {
		args = append(args, "--revoke-oauth")
	}
	if deleteKit {
		args = append(args, "--delete-kit")
	}
	if removeWireproxy {
		args = append(args, "--wireproxy")
	}
	return uninstallCommand(ctx, args)
}

func createGoogleKitFromMenu(ctx context.Context, reader *bufio.Reader, oauthMode string) error {
	out, err := prompt(ctx, reader, "Output directory", "skirk-kit")
	if err != nil {
		return err
	}
	title, err := prompt(ctx, reader, "Kit title", "")
	if err != nil {
		return err
	}
	args := []string{"--out", out}
	if title != "" {
		args = append(args, "--title", title)
	}
	reset, err := promptYesNo(ctx, reader, "Reset Google login before setup", true)
	if err != nil {
		return err
	}
	if reset {
		args = append(args, "--reset-google-login")
	}
	switch oauthMode {
	case "easy":
		fmt.Println("Google OAuth: easy Skirk OAuth client")
	case "personal":
		fmt.Println("Google OAuth: personal Google OAuth project")
		oauthFile, err := promptPersonalOAuthClientFile(ctx, reader, "oauth-client.json")
		if err != nil {
			return err
		}
		args = append(args, "--oauth-client-file", oauthFile)
	default:
		return fmt.Errorf("unknown OAuth mode %q", oauthMode)
	}
	startExit, err := promptYesNo(ctx, reader, "Install and start exit service after setup", runtime.GOOS == "linux")
	if err != nil {
		return err
	}
	if !startExit {
		args = append(args, "--start-exit=false")
	}
	if runtime.GOOS == "linux" {
		configureProxy, err := promptYesNo(ctx, reader, "Set outbound exit proxy now", false)
		if err != nil {
			return err
		}
		if configureProxy {
			proxyURL, err := promptProxyURL(ctx, reader, "Outbound proxy URL", "socks5h://127.0.0.1:40000")
			if err != nil {
				return err
			}
			args = append(args, "--exit-proxy", proxyURL)
		}
	}
	return setupInit(ctx, args)
}

func serviceMenu(ctx context.Context, reader *bufio.Reader) error {
	fmt.Println()
	fmt.Println("1. Install or update exit service")
	fmt.Println("2. Configure outbound proxy")
	fmt.Println("3. Service status")
	fmt.Println("4. Start service")
	fmt.Println("5. Stop service")
	fmt.Println("6. Restart service")
	fmt.Println("7. Uninstall exit service only")
	fmt.Println("0. Back")
	choice, err := prompt(ctx, reader, "Service action", "1")
	if err != nil {
		return err
	}
	if choice == "0" || strings.EqualFold(choice, "back") {
		return nil
	}
	name, err := prompt(ctx, reader, "Service name", defaultServiceName)
	if err != nil {
		return err
	}
	switch choice {
	case "1":
		config, err := prompt(ctx, reader, "Exit config", defaultKitFile("exit.json"))
		if err != nil {
			return err
		}
		user, err := prompt(ctx, reader, "Run service as user (blank=current user)", "")
		if err != nil {
			return err
		}
		args := []string{"install", "--name", name, "--config", config}
		if strings.TrimSpace(user) != "" {
			args = append(args, "--user", user)
		}
		return serviceCommand(ctx, args)
	case "2":
		return outboundProxyMenu(ctx, reader, name)
	case "3":
		return serviceCommand(ctx, []string{"status", "--name", name})
	case "4":
		return serviceCommand(ctx, []string{"start", "--name", name})
	case "5":
		return serviceCommand(ctx, []string{"stop", "--name", name})
	case "6":
		return serviceCommand(ctx, []string{"restart", "--name", name})
	case "7":
		confirm, err := promptYesNo(ctx, reader, "Uninstall service", false)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		return serviceCommand(ctx, []string{"uninstall", "--name", name})
	default:
		return fmt.Errorf("unknown service action %q", choice)
	}
}

func outboundProxyMenu(ctx context.Context, reader *bufio.Reader, serviceName string) error {
	configPath, err := prompt(ctx, reader, "Exit config", defaultKitFile("exit.json"))
	if err != nil {
		return err
	}
	return outboundProxyMenuForConfig(ctx, reader, serviceName, configPath)
}

func outboundProxyMenuForConfig(ctx context.Context, reader *bufio.Reader, serviceName, configPath string) error {
	cfg, err := skirk.LoadConfig(configPath)
	if err != nil {
		return err
	}
	current := strings.TrimSpace(cfg.Tunnel.ExitProxy)
	fmt.Printf("Current outbound proxy: %s\n", firstNonEmpty(current, "direct"))
	fmt.Println("1. Set custom proxy URL")
	if runtime.GOOS == "linux" {
		fmt.Println("2. Install WARP wireproxy and use it")
		fmt.Println("3. Uninstall WARP wireproxy and use direct exit")
		fmt.Println("4. Unset proxy and use direct exit")
	} else {
		fmt.Println("2. Unset proxy and use direct exit")
	}
	fmt.Println("0. Back")
	choice, err := prompt(ctx, reader, "Outbound proxy action", "1")
	if err != nil {
		return err
	}
	switch choice {
	case "0", "back":
		return nil
	case "1":
		proxyURL, err := promptProxyURL(ctx, reader, "Proxy URL", firstNonEmpty(current, "socks5h://127.0.0.1:40000"))
		if err != nil {
			return err
		}
		if err := updateExitProxyConfig(configPath, proxyURL); err != nil {
			return err
		}
		fmt.Printf("Updated %s: outbound proxy is %s\n", configPath, proxyURL)
	case "2":
		if runtime.GOOS != "linux" {
			if err := setDirectExitProxy(ctx, serviceName, configPath); err != nil {
				return err
			}
			break
		}
		bind, err := prompt(ctx, reader, "WARP SOCKS listen", "127.0.0.1:40000")
		if err != nil {
			return err
		}
		if err := validateProxyListenAddr(bind); err != nil {
			return err
		}
		accepted, err := promptYesNo(ctx, reader, "Install WARP wireproxy and accept Cloudflare WARP terms", false)
		if err != nil {
			return err
		}
		if !accepted {
			return fmt.Errorf("WARP wireproxy install cancelled")
		}
		if err := installWarpWireproxyFromMenu(ctx, bind); err != nil {
			return err
		}
		proxyURL := "socks5h://" + bind
		if err := updateExitProxyConfig(configPath, proxyURL); err != nil {
			return err
		}
		if err := installWarpServiceDependency(ctx, serviceName); err != nil {
			return err
		}
		fmt.Printf("Updated %s: outbound proxy is %s\n", configPath, proxyURL)
	case "3":
		if runtime.GOOS != "linux" {
			return fmt.Errorf("unknown outbound proxy action %q", choice)
		}
		confirm, err := promptYesNo(ctx, reader, "Uninstall WARP wireproxy and use direct exit", false)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		if err := preflightRemoveWarpServiceDependency(serviceName); err != nil {
			return err
		}
		if err := preflightUninstallWireproxy(); err != nil {
			return err
		}
		if err := updateExitProxyConfig(configPath, ""); err != nil {
			return err
		}
		if err := removeWarpServiceDependency(ctx, serviceName); err != nil {
			return err
		}
		if err := uninstallWireproxy(ctx); err != nil {
			return err
		}
		fmt.Printf("Updated %s: outbound proxy is direct\n", configPath)
	case "4":
		if runtime.GOOS != "linux" {
			return fmt.Errorf("unknown outbound proxy action %q", choice)
		}
		if err := setDirectExitProxy(ctx, serviceName, configPath); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown outbound proxy action %q", choice)
	}
	restart, err := promptYesNo(ctx, reader, "Restart exit service now", true)
	if err != nil {
		return err
	}
	if restart {
		return serviceCommand(ctx, []string{"restart", "--name", serviceName})
	}
	return nil
}

func setDirectExitProxy(ctx context.Context, serviceName, configPath string) error {
	if err := updateExitProxyConfig(configPath, ""); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		if err := removeWarpServiceDependency(ctx, serviceName); err != nil {
			return err
		}
	}
	fmt.Printf("Updated %s: outbound proxy is direct\n", configPath)
	return nil
}

func validateProxyListenAddr(value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("WARP SOCKS listen must be host:port: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("WARP SOCKS listen host is required")
	}
	if strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("WARP SOCKS listen must be a single-line address")
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("WARP SOCKS listen must bind loopback only; use 127.0.0.1:40000")
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("WARP SOCKS listen port is invalid: %w", err)
	}
	return nil
}

func promptProxyURL(ctx context.Context, reader *bufio.Reader, label, fallback string) (string, error) {
	for {
		value, err := prompt(ctx, reader, label, fallback)
		if err != nil {
			return "", err
		}
		cfg := &skirk.Config{Secret: "validate-only", Tunnel: skirk.TunnelConfig{ExitProxy: strings.TrimSpace(value)}}
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err == nil {
			return strings.TrimSpace(value), nil
		}
		fmt.Println("Proxy must be a valid socks5, socks5h, http, or https URL.")
	}
}

func updateExitProxyConfig(path, proxyURL string) error {
	cfg, err := skirk.LoadConfig(path)
	if err != nil {
		return err
	}
	cfg.Tunnel.ExitProxy = strings.TrimSpace(proxyURL)
	if err := cfg.Validate(); err != nil {
		return err
	}
	return writeJSONFile(path, cfg)
}

func installWarpWireproxyFromMenu(ctx context.Context, bind string) error {
	script := `set -e
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT INT TERM
curl -fsSL "$1" -o "$tmp"
SKIRK_WIREPROXY_ONLY=1 SKIRK_INSTALL_WIREPROXY=1 SKIRK_ACCEPT_WARP_TOS=1 SKIRK_WIREPROXY_BIND="$2" sh "$tmp"`
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "skirk-warp", installerScriptURL(), bind)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func installerScriptURL() string {
	ref := "main"
	if safeInstallerRef(version) {
		ref = version
	}
	return "https://raw.githubusercontent.com/ShahabSL/Skirk/" + ref + "/install.sh"
}

func safeInstallerRef(value string) bool {
	return validMenuUpdateVersion(value) && strings.HasPrefix(value, "v")
}

func installWarpServiceDependency(ctx context.Context, serviceName string) error {
	unit, err := normalizeSystemdServiceName(serviceName)
	if err != nil {
		return err
	}
	return installSystemdDropIn(ctx, unit, "10-wireproxy.conf", `[Unit]
# Managed by Skirk
After=wireproxy.service
Wants=wireproxy.service
`)
}

func removeWarpServiceDependency(ctx context.Context, serviceName string) error {
	unit, err := normalizeSystemdServiceName(serviceName)
	if err != nil {
		return err
	}
	if err := removeSystemdDropIn(ctx, unit, "10-wireproxy.conf"); err != nil {
		return err
	}
	return removeSystemdUnitDependency(ctx, unit, "wireproxy.service")
}

func preflightRemoveWarpServiceDependency(serviceName string) error {
	unit, err := normalizeSystemdServiceName(serviceName)
	if err != nil {
		return err
	}
	unitPath := filepath.Join("/etc/systemd/system", unit)
	if _, err := os.Lstat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return assertSkirkSystemdUnit(unit)
}

func updateFromMenu(ctx context.Context, reader *bufio.Reader) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("menu update is currently available on Linux installs")
	}
	versionValue, err := prompt(ctx, reader, "Version", "latest")
	if err != nil {
		return err
	}
	if !validMenuUpdateVersion(versionValue) {
		return fmt.Errorf("update version must be latest or a vX.Y.Z tag")
	}
	serviceName, err := prompt(ctx, reader, "Service name", defaultServiceName)
	if err != nil {
		return err
	}
	restart, err := promptYesNo(ctx, reader, "Restart exit service after update", true)
	if err != nil {
		return err
	}
	script := `set -e
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT INT TERM
case "$1" in
  latest) ref=main ;;
  v*) ref="$1" ;;
  *) echo "error: update version must be latest or a vX.Y.Z tag" >&2; exit 1 ;;
esac
curl -fsSL "https://raw.githubusercontent.com/ShahabSL/Skirk/$ref/install.sh" -o "$tmp"
sh "$tmp" --version "$1"`
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "skirk-update", versionValue)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = updateInstallerEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return err
	}
	if restart {
		if err := serviceCommand(ctx, []string{"restart", "--name", serviceName}); err != nil {
			return err
		}
	}
	return execUpdatedMenu()
}

func execUpdatedMenu() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("updated Skirk was installed, but current executable path could not be resolved; restart skirk manually: %w", err)
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	fmt.Printf("Restarting updated Skirk menu from %s\n", exe)
	return syscall.Exec(exe, []string{exe}, os.Environ())
}

func updateInstallerEnv(base []string) []string {
	exe, err := os.Executable()
	if err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
	}
	installDir := ""
	if strings.TrimSpace(exe) != "" {
		installDir = filepath.Dir(exe)
	}
	blocked := map[string]bool{
		"SKIRK_SERVER_SETUP":        true,
		"SKIRK_UNINSTALL":           true,
		"SKIRK_REPO":                true,
		"SKIRK_ASSET_BASE":          true,
		"SKIRK_VERSION":             true,
		"SKIRK_DEV_INSTALL":         true,
		"SKIRK_INSTALL_SYSTEMD":     true,
		"SKIRK_INSTALL_WIREPROXY":   true,
		"SKIRK_WIREPROXY_ONLY":      true,
		"SKIRK_WIREPROXY_BIND":      true,
		"SKIRK_WIREPROXY_DIR":       true,
		"SKIRK_WIREPROXY_BIN":       true,
		"SKIRK_WIREPROXY_VERSION":   true,
		"SKIRK_WGCF_BIN":            true,
		"SKIRK_WGCF_VERSION":        true,
		"SKIRK_RESET_WARP":          true,
		"SKIRK_ACCEPT_WARP_TOS":     true,
		"SKIRK_EXIT_PROXY":          true,
		"SKIRK_ADC":                 true,
		"SKIRK_SETUP_OUT":           true,
		"SKIRK_RESET_GOOGLE_LOGIN":  true,
		"SKIRK_OAUTH_CLIENT_FILE":   true,
		"SKIRK_OAUTH_CLIENT_ID":     true,
		"SKIRK_OAUTH_CLIENT_SECRET": true,
	}
	out := make([]string, 0, len(base)+2)
	hasInstallDir := false
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if key == "SKIRK_INSTALL_DIR" {
			hasInstallDir = true
			if installDir != "" {
				out = append(out, "SKIRK_INSTALL_DIR="+installDir)
			}
			continue
		}
		if blocked[key] {
			continue
		}
		out = append(out, entry)
	}
	if !hasInstallDir && installDir != "" {
		out = append(out, "SKIRK_INSTALL_DIR="+installDir)
	}
	out = append(out, "SKIRK_REQUIRE_RELEASE_ASSET=1")
	return out
}

func validMenuUpdateVersion(value string) bool {
	value = strings.TrimSpace(value)
	if value == "latest" {
		return true
	}
	if !strings.HasPrefix(value, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func promptYesNo(ctx context.Context, reader *bufio.Reader, label string, fallback bool) (bool, error) {
	fallbackText := "no"
	if fallback {
		fallbackText = "yes"
	}
	for {
		text, err := prompt(ctx, reader, label+" (yes/no)", fallbackText)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Println("Please answer yes or no.")
		}
	}
}

func deleteKitDirectory(configPath string) error {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if filepath.Base(absConfig) != "exit.json" {
		return fmt.Errorf("refusing to delete kit directory: %s is not an exit.json path", absConfig)
	}
	dir := filepath.Dir(absConfig)
	if dir == string(filepath.Separator) || dir == "." {
		return fmt.Errorf("refusing to delete unsafe kit directory %q", dir)
	}
	if cwd, err := os.Getwd(); err == nil && sameCleanPath(dir, cwd) {
		return fmt.Errorf("refusing to delete current working directory as a kit: %s", dir)
	}
	if home, err := os.UserHomeDir(); err == nil && sameCleanPath(dir, home) {
		return fmt.Errorf("refusing to delete home directory as a kit: %s", dir)
	}
	required := []string{"exit.json", "client.skirk", "client.json"}
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("refusing to delete %s: expected generated kit file %s is missing: %w", dir, name, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("Deleted local kit directory: %s\n", dir)
	return nil
}

func sameCleanPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

const skirkBanner = `             ##################
            ####################
            ####            ####
            ####            ###
            ####
            ####    ####
            #########  ########
            #########  #########
                    ####    ####
                            ####
             ###            ####
            ####            ####
            ####################
             ##################

Skirk
`

func prompt(ctx context.Context, reader *bufio.Reader, label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", label, fallback)
	} else {
		fmt.Printf("%s: ", label)
	}
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := reader.ReadString('\n')
		ch <- result{text: text, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return "", result.err
		}
		text := strings.TrimSpace(result.text)
		if text == "" {
			return fallback, nil
		}
		return text, nil
	}
}
