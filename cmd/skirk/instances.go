package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type exitInstance struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	KitDir      string `json:"kit_dir"`
	ConfigPath  string `json:"config_path"`
	ServiceName string `json:"service_name"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	Legacy       bool   `json:"-"`
	ManifestPath string `json:"-"`
}

func instancesMenu(ctx context.Context, reader *bufio.Reader) error {
	for {
		instances, err := discoverExitInstances()
		if err != nil {
			return err
		}
		fmt.Println()
		fmt.Println("1. Manage default exit service")
		fmt.Println("2. Create another Skirk exit instance")
		fmt.Println("3. Manage an existing exit instance")
		fmt.Println("4. List exit instances")
		fmt.Println("0. Back")
		choice, err := prompt(ctx, reader, "Exit management", "3")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "0", "back":
			return nil
		case "1":
			if err := serviceMenu(ctx, reader); err != nil {
				return err
			}
		case "2":
			if err := createExitInstanceFromMenu(ctx, reader); err != nil {
				return err
			}
		case "3":
			instance, ok, err := selectExitInstance(ctx, reader, instances)
			if err != nil || !ok {
				return err
			}
			if err := manageExitInstanceMenu(ctx, reader, instance); err != nil {
				return err
			}
		case "4":
			printExitInstances(instances)
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func createExitInstanceFromMenu(ctx context.Context, reader *bufio.Reader) error {
	root, err := skirkInstancesRoot()
	if err != nil {
		return err
	}
	title, err := prompt(ctx, reader, "Instance name", "Skirk exit")
	if err != nil {
		return err
	}
	idDefault := defaultInstanceID(title)
	id, err := prompt(ctx, reader, "Instance ID", idDefault)
	if err != nil {
		return err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if err := validateInstanceID(id); err != nil {
		return err
	}
	kitDir := filepath.Join(root, id)
	if _, err := os.Stat(kitDir); err == nil {
		return fmt.Errorf("instance %q already exists at %s", id, kitDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	serviceName, err := prompt(ctx, reader, "Service name", "skirk-exit-"+id)
	if err != nil {
		return err
	}
	if _, err := normalizeSystemdServiceName(serviceName); err != nil {
		return err
	}
	fmt.Println("OAuth mode:")
	fmt.Println("1. Easy Skirk OAuth")
	fmt.Println("2. Personal Google OAuth project")
	oauthChoice, err := prompt(ctx, reader, "OAuth mode", "1")
	if err != nil {
		return err
	}
	args := []string{"--out", kitDir, "--title", firstNonEmpty(title, id), "--exit-service-name", serviceName}
	switch strings.TrimSpace(oauthChoice) {
	case "1", "easy":
		args = append(args, "--oauth-mode", "easy")
	case "2", "personal":
		oauthFile, err := promptPersonalOAuthClientFile(ctx, reader, "oauth-client.json")
		if err != nil {
			return err
		}
		args = append(args, "--oauth-mode", "personal", "--oauth-client-file", oauthFile)
	default:
		return fmt.Errorf("unknown OAuth mode %q", oauthChoice)
	}
	resetLogin, err := promptYesNo(ctx, reader, "Reset Google login before setup", true)
	if err != nil {
		return err
	}
	if resetLogin {
		args = append(args, "--reset-google-login")
	}
	startExit, err := promptYesNo(ctx, reader, "Install and start this exit service", true)
	if err != nil {
		return err
	}
	if !startExit {
		args = append(args, "--start-exit=false")
	}
	serviceUser, err := prompt(ctx, reader, "Run service as user (blank=current user)", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(serviceUser) != "" {
		args = append(args, "--exit-service-user", serviceUser)
	}
	if err := setupInit(ctx, args); err != nil {
		return err
	}
	instance := exitInstance{
		ID:          id,
		Title:       firstNonEmpty(title, id),
		KitDir:      kitDir,
		ConfigPath:  filepath.Join(kitDir, "exit.json"),
		ServiceName: serviceName,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveExitInstance(instance); err != nil {
		return err
	}
	configureProxy, err := promptYesNo(ctx, reader, "Configure outbound proxy or WARP now", false)
	if err != nil {
		return err
	}
	if configureProxy {
		return outboundProxyMenuForConfig(ctx, reader, serviceName, instance.ConfigPath)
	}
	return nil
}

func manageExitInstanceMenu(ctx context.Context, reader *bufio.Reader, instance exitInstance) error {
	for {
		fmt.Println()
		printExitInstanceDetails(instance)
		fmt.Println("1. Service status")
		fmt.Println("2. Start service")
		fmt.Println("3. Stop service")
		fmt.Println("4. Restart service")
		fmt.Println("5. Install or update service")
		fmt.Println("6. Configure outbound proxy or WARP")
		fmt.Println("7. Tune Drive usage and concurrency")
		fmt.Println("8. Cleanup stale Drive objects")
		fmt.Println("9. Show generated client files")
		fmt.Println("10. Delete this instance")
		fmt.Println("0. Back")
		choice, err := prompt(ctx, reader, "Instance action", "1")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "0", "back":
			return nil
		case "1":
			return serviceCommand(ctx, []string{"status", "--name", instance.ServiceName})
		case "2":
			return serviceCommand(ctx, []string{"start", "--name", instance.ServiceName})
		case "3":
			return serviceCommand(ctx, []string{"stop", "--name", instance.ServiceName})
		case "4":
			return serviceCommand(ctx, []string{"restart", "--name", instance.ServiceName})
		case "5":
			user, err := prompt(ctx, reader, "Run service as user (blank=current user)", "")
			if err != nil {
				return err
			}
			args := []string{"install", "--name", instance.ServiceName, "--config", instance.ConfigPath}
			if strings.TrimSpace(user) != "" {
				args = append(args, "--user", user)
			}
			return serviceCommand(ctx, args)
		case "6":
			return outboundProxyMenuForConfig(ctx, reader, instance.ServiceName, instance.ConfigPath)
		case "7":
			if err := exitPerformanceMenu(ctx, reader, instance.ConfigPath); err != nil {
				return err
			}
			restart, err := promptYesNo(ctx, reader, "Restart exit service now", true)
			if err != nil {
				return err
			}
			if restart {
				return serviceCommand(ctx, []string{"restart", "--name", instance.ServiceName})
			}
		case "8":
			return cleanupInstanceFromMenu(ctx, reader, instance)
		case "9":
			fmt.Printf("Client JSON: %s\n", filepath.Join(instance.KitDir, "client.json"))
			fmt.Printf("Client profile: %s\n", filepath.Join(instance.KitDir, "client.skirk"))
			fmt.Printf("Client command: %s\n", filepath.Join(instance.KitDir, "client-command.txt"))
		case "10":
			return deleteExitInstanceFromMenu(ctx, reader, instance)
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func exitPerformanceMenu(ctx context.Context, reader *bufio.Reader, configPath string) error {
	cfg, err := skirk.LoadConfig(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("Current: poll=%dms upload=%s download=%s burst=%v\n",
		cfg.Tunnel.PollIntervalMS,
		concurrencyLabel(cfg.Tunnel.UploadConcurrency),
		concurrencyLabel(cfg.Tunnel.DownloadConcurrency),
		cfg.Tunnel.BurstPoll,
	)
	fmt.Println("1. Recommended server default")
	fmt.Println("2. Lower Drive usage")
	fmt.Println("3. Responsive")
	fmt.Println("4. Bulk transfer")
	fmt.Println("5. Custom")
	fmt.Println("0. Back")
	choice, err := prompt(ctx, reader, "Performance preset", "1")
	if err != nil {
		return err
	}
	switch strings.TrimSpace(choice) {
	case "0", "back":
		return nil
	case "1":
		cfg.Tunnel.PollIntervalMS = 1000
		cfg.Tunnel.UploadConcurrency = 0
		cfg.Tunnel.DownloadConcurrency = 0
		cfg.Tunnel.BurstPoll = false
	case "2":
		cfg.Tunnel.PollIntervalMS = 2000
		cfg.Tunnel.UploadConcurrency = 8
		cfg.Tunnel.DownloadConcurrency = 8
		cfg.Tunnel.BurstPoll = false
	case "3":
		cfg.Tunnel.PollIntervalMS = 1000
		cfg.Tunnel.UploadConcurrency = 16
		cfg.Tunnel.DownloadConcurrency = 16
		cfg.Tunnel.BurstPoll = true
	case "4":
		cfg.Tunnel.PollIntervalMS = 1000
		cfg.Tunnel.UploadConcurrency = 32
		cfg.Tunnel.DownloadConcurrency = 32
		cfg.Tunnel.BurstPoll = false
	case "5":
		fmt.Println("Warning: low poll intervals, high worker counts, and burst polling can burn Drive quota quickly.")
		pollMS, err := promptBoundedInt(ctx, reader, "Poll interval ms", cfg.Tunnel.PollIntervalMS, 250, 60000)
		if err != nil {
			return err
		}
		upload, err := promptBoundedInt(ctx, reader, "Upload workers (0=auto)", cfg.Tunnel.UploadConcurrency, 0, 64)
		if err != nil {
			return err
		}
		download, err := promptBoundedInt(ctx, reader, "Download workers (0=auto)", cfg.Tunnel.DownloadConcurrency, 0, 64)
		if err != nil {
			return err
		}
		burst, err := promptYesNo(ctx, reader, "Enable burst polling", cfg.Tunnel.BurstPoll)
		if err != nil {
			return err
		}
		cfg.Tunnel.PollIntervalMS = pollMS
		cfg.Tunnel.UploadConcurrency = upload
		cfg.Tunnel.DownloadConcurrency = download
		cfg.Tunnel.BurstPoll = burst
	default:
		return fmt.Errorf("unknown performance preset %q", choice)
	}
	if cfg.Tunnel.BurstPoll {
		fmt.Println("Warning: burst polling improves responsiveness after traffic but can spend Drive list quota faster.")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := writeJSONFile(configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Updated %s\n", configPath)
	return nil
}

func cleanupInstanceFromMenu(ctx context.Context, reader *bufio.Reader, instance exitInstance) error {
	fmt.Println("Cleanup first runs a dry-run unless you explicitly choose delete.")
	allObjects, err := promptYesNo(ctx, reader, "Scan every object in this mailbox folder", true)
	if err != nil {
		return err
	}
	olderThan, err := promptDuration(ctx, reader, "Objects older than", "10m")
	if err != nil {
		return err
	}
	deleteObjects, err := promptYesNo(ctx, reader, "Actually delete matching stale objects", false)
	if err != nil {
		return err
	}
	args := []string{"--config", instance.ConfigPath, "--older-than", olderThan.String(), "--max-pages", "20000"}
	if allObjects {
		args = append(args, "--all")
	}
	if deleteObjects {
		args = append(args, "--delete")
	}
	return cleanup(ctx, args)
}

func deleteExitInstanceFromMenu(ctx context.Context, reader *bufio.Reader, instance exitInstance) error {
	fmt.Println("Deleting an instance can remove its systemd service, Drive mailbox objects, OAuth token, and local kit.")
	fmt.Println("It never removes the Skirk binary from this machine.")
	stopService, err := promptYesNo(ctx, reader, "Stop service first", true)
	if err != nil {
		return err
	}
	uninstallService, err := promptYesNo(ctx, reader, "Uninstall service unit", true)
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
	deleteLocal, err := promptYesNo(ctx, reader, "Delete local kit directory", false)
	if err != nil {
		return err
	}
	confirm, err := prompt(ctx, reader, "Type instance ID to confirm", "")
	if err != nil {
		return err
	}
	if confirm != instance.ID {
		return fmt.Errorf("confirmation did not match %q", instance.ID)
	}
	if stopService {
		_ = serviceCommand(ctx, []string{"stop", "--name", instance.ServiceName})
	}
	if deleteDrive && !stopService {
		return fmt.Errorf("stop the service before deleting every Drive mailbox object")
	}
	if uninstallService {
		if err := serviceCommand(ctx, []string{"uninstall", "--name", instance.ServiceName}); err != nil {
			return err
		}
	}
	if deleteDrive {
		if err := cleanup(ctx, []string{"--config", instance.ConfigPath, "--all", "--older-than", "1ns", "--delete", "--max-pages", "20000"}); err != nil {
			return err
		}
	}
	if revokeOAuth {
		if err := revoke(ctx, []string{"--config", instance.ConfigPath, "--revoke-oauth"}); err != nil {
			return err
		}
	}
	if deleteLocal {
		if err := deleteKitDirectory(instance.ConfigPath); err != nil {
			return err
		}
	}
	if !instance.Legacy && instance.ManifestPath != "" {
		_ = os.Remove(instance.ManifestPath)
	}
	return nil
}

func discoverExitInstances() ([]exitInstance, error) {
	var out []exitInstance
	seen := map[string]bool{}
	if kitDir, ok, err := existingDefaultKitDir(); err != nil {
		return nil, err
	} else if ok {
		configPath, _ := filepath.Abs(filepath.Join(kitDir, "exit.json"))
		out = append(out, exitInstance{
			ID:          "default",
			Title:       "Default skirk-kit",
			KitDir:      kitDir,
			ConfigPath:  configPath,
			ServiceName: defaultServiceName,
			Legacy:      true,
		})
		seen["default"] = true
	}
	root, err := skirkInstancesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateInstanceID(id); err != nil {
			continue
		}
		if seen[id] {
			continue
		}
		dir := filepath.Join(root, id)
		manifestPath := filepath.Join(dir, "instance.json")
		instance, err := readExitInstanceManifest(manifestPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			if _, statErr := os.Stat(filepath.Join(dir, "exit.json")); statErr != nil {
				continue
			}
			instance = exitInstance{ID: id, Title: id, KitDir: dir, ConfigPath: filepath.Join(dir, "exit.json")}
		}
		instance.ID = firstNonEmpty(instance.ID, id)
		instance.Title = firstNonEmpty(instance.Title, instance.ID)
		instance.KitDir = firstNonEmpty(instance.KitDir, dir)
		instance.ConfigPath = firstNonEmpty(instance.ConfigPath, filepath.Join(instance.KitDir, "exit.json"))
		instance.ServiceName = firstNonEmpty(instance.ServiceName, "skirk-exit-"+instance.ID)
		instance.ManifestPath = manifestPath
		out = append(out, instance)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Legacy != out[j].Legacy {
			return out[i].Legacy
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

var defaultKitDirCandidates = func() []string {
	candidates := []string{"skirk-kit"}
	if runtime.GOOS == "linux" {
		candidates = append(candidates, "/opt/skirk-kit")
	}
	return candidates
}

func existingDefaultKitDir() (string, bool, error) {
	for _, dir := range defaultKitDirCandidates() {
		exitPath := filepath.Join(dir, "exit.json")
		if _, err := os.Stat(exitPath); err == nil {
			kitDir, absErr := filepath.Abs(dir)
			if absErr != nil {
				return "", false, absErr
			}
			return kitDir, true, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
	}
	return "", false, nil
}

func defaultKitFile(name string) string {
	if kitDir, ok, err := existingDefaultKitDir(); err == nil && ok {
		return filepath.Join(kitDir, name)
	}
	return filepath.Join("skirk-kit", name)
}

func readExitInstanceManifest(path string) (exitInstance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return exitInstance{}, err
	}
	var instance exitInstance
	if err := json.Unmarshal(data, &instance); err != nil {
		return exitInstance{}, err
	}
	instance.ManifestPath = path
	return instance, nil
}

func saveExitInstance(instance exitInstance) error {
	root, err := skirkInstancesRoot()
	if err != nil {
		return err
	}
	if err := validateInstanceID(instance.ID); err != nil {
		return err
	}
	instance.KitDir, _ = filepath.Abs(instance.KitDir)
	instance.ConfigPath, _ = filepath.Abs(instance.ConfigPath)
	instance.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(root, instance.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "instance.json"), instance)
}

func selectExitInstance(ctx context.Context, reader *bufio.Reader, instances []exitInstance) (exitInstance, bool, error) {
	if len(instances) == 0 {
		fmt.Println("No exit instances found. Create one first.")
		return exitInstance{}, false, nil
	}
	printExitInstances(instances)
	value, err := prompt(ctx, reader, "Select instance number", "1")
	if err != nil {
		return exitInstance{}, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 || n > len(instances) {
		return exitInstance{}, false, fmt.Errorf("invalid instance selection %q", value)
	}
	return instances[n-1], true, nil
}

func printExitInstances(instances []exitInstance) {
	if len(instances) == 0 {
		fmt.Println("No exit instances found.")
		return
	}
	for i, instance := range instances {
		fmt.Printf("%d. %s [%s] service=%s config=%s\n", i+1, firstNonEmpty(instance.Title, instance.ID), instance.ID, instance.ServiceName, instance.ConfigPath)
	}
}

func printExitInstanceDetails(instance exitInstance) {
	fmt.Printf("Instance: %s [%s]\n", firstNonEmpty(instance.Title, instance.ID), instance.ID)
	fmt.Printf("Service:  %s\n", instance.ServiceName)
	fmt.Printf("Config:   %s\n", instance.ConfigPath)
	if cfg, err := skirk.LoadConfig(instance.ConfigPath); err == nil {
		fmt.Printf("Drive:    %s %s\n", firstNonEmpty(cfg.Drive.Space, "drive"), firstNonEmpty(cfg.Drive.FolderID, "(app folder)"))
		fmt.Printf("Proxy:    %s\n", firstNonEmpty(cfg.Tunnel.ExitProxy, "direct"))
		fmt.Printf("Tuning:   poll=%dms upload=%s download=%s burst=%v\n",
			cfg.Tunnel.PollIntervalMS,
			concurrencyLabel(cfg.Tunnel.UploadConcurrency),
			concurrencyLabel(cfg.Tunnel.DownloadConcurrency),
			cfg.Tunnel.BurstPoll,
		)
	}
}

func skirkInstancesRoot() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			if err != nil {
				return "", err
			}
			return "", homeErr
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "skirk", "instances"), nil
}

func validateInstanceID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || len(id) > 64 {
		return fmt.Errorf("instance ID must be 1-64 characters")
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("instance ID may contain only lowercase letters, digits, dot, underscore, and hyphen")
	}
	return nil
}

func defaultInstanceID(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	value := strings.Trim(b.String(), "-")
	if value == "" {
		value = "instance-" + time.Now().UTC().Format("20060102-150405")
	}
	if len(value) > 48 {
		value = strings.Trim(value[:48], "-")
	}
	return value
}

func concurrencyLabel(value int) string {
	if value == 0 {
		return "auto"
	}
	return strconv.Itoa(value)
}

func promptBoundedInt(ctx context.Context, reader *bufio.Reader, label string, fallback, minValue, maxValue int) (int, error) {
	for {
		value, err := prompt(ctx, reader, label, strconv.Itoa(fallback))
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && n >= minValue && n <= maxValue {
			return n, nil
		}
		fmt.Printf("Enter a number between %d and %d.\n", minValue, maxValue)
	}
}

func promptDuration(ctx context.Context, reader *bufio.Reader, label, fallback string) (time.Duration, error) {
	for {
		value, err := prompt(ctx, reader, label, fallback)
		if err != nil {
			return 0, err
		}
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err == nil && duration > 0 {
			return duration, nil
		}
		fmt.Println("Enter a duration like 10m, 2h, or 24h.")
	}
}
