package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	defaultWireproxyService = "wireproxy"
	defaultWireproxyUser    = "skirk-wireproxy"
	wireproxyManifestName   = "skirk-managed.manifest"
)

var (
	defaultWireproxyDir = "/etc/wireproxy"
	defaultWireproxyBin = "/usr/local/bin/wireproxy"
	defaultWGCFBin      = "/usr/local/bin/wgcf"
)

type wireproxyManifest struct {
	ConfigDir         string
	WireproxyBin      string
	WireproxySHA256   string
	WGCFBin           string
	WGCFSHA256        string
	Service           string
	HasManagedBySkirk bool
}

func uninstallCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	service := fs.Bool("service", runtime.GOOS == "linux", "stop, disable, and remove the Linux exit systemd service")
	serviceName := fs.String("name", defaultServiceName, "systemd service name to remove")
	binary := fs.Bool("binary", true, "remove the installed skirk binary")
	binPath := fs.String("bin", defaultUninstallBinaryPath(), "installed skirk binary path")
	configPath := fs.String("config", "skirk-kit/exit.json", "exit config path used for Drive cleanup or OAuth revoke")
	deleteDrive := fs.Bool("delete-drive", false, "delete Drive mailbox objects before revoking or deleting local files")
	revokeOAuth := fs.Bool("revoke-oauth", false, "revoke the Google OAuth token embedded in the exit config")
	deleteKit := fs.Bool("delete-kit", false, "delete the generated local kit directory")
	kitDir := fs.String("kit", "skirk-kit", "generated kit directory to delete when --delete-kit is set")
	wireproxy := fs.Bool("wireproxy", false, "also remove Skirk-installed WARP wireproxy service, config directory, and helper binaries")
	dryRun := fs.Bool("dry-run", false, "print the uninstall plan without removing anything")
	yes := fs.Bool("yes", false, "confirm destructive uninstall actions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dryRun {
		printUninstallPlan(uninstallPlan{
			Service:      *service,
			ServiceName:  *serviceName,
			Binary:       *binary,
			BinPath:      *binPath,
			ConfigPath:   *configPath,
			DeleteDrive:  *deleteDrive,
			RevokeOAuth:  *revokeOAuth,
			DeleteKit:    *deleteKit,
			KitDir:       *kitDir,
			Wireproxy:    *wireproxy,
			WireproxyDir: defaultWireproxyDir,
		}, true)
		return nil
	}
	if !*yes {
		return fmt.Errorf("refusing to uninstall without --yes; run skirk for the interactive menu or pass --yes after reviewing options")
	}
	printUninstallPlan(uninstallPlan{
		Service:      *service,
		ServiceName:  *serviceName,
		Binary:       *binary,
		BinPath:      *binPath,
		ConfigPath:   *configPath,
		DeleteDrive:  *deleteDrive,
		RevokeOAuth:  *revokeOAuth,
		DeleteKit:    *deleteKit,
		KitDir:       *kitDir,
		Wireproxy:    *wireproxy,
		WireproxyDir: defaultWireproxyDir,
	}, false)
	if *service {
		if err := uninstallServiceIfAvailable(ctx, *serviceName); err != nil {
			return err
		}
	}
	if *deleteDrive {
		if err := cleanup(ctx, []string{"--config", *configPath, "--all", "--older-than", "1ns", "--delete", "--max-pages", "20000"}); err != nil {
			return fmt.Errorf("delete Drive mailbox objects: %w", err)
		}
	}
	if *revokeOAuth {
		if err := revoke(ctx, []string{"--config", *configPath, "--revoke-oauth"}); err != nil {
			return fmt.Errorf("revoke OAuth token: %w", err)
		}
	}
	if *deleteKit {
		if err := deleteKitDirectory(filepath.Join(*kitDir, "exit.json")); err != nil {
			return err
		}
	}
	if *wireproxy {
		if err := uninstallWireproxy(ctx); err != nil {
			return err
		}
	}
	if *binary {
		if err := removeInstalledBinary(ctx, *binPath); err != nil {
			return err
		}
	}
	fmt.Println("Skirk uninstall complete.")
	return nil
}

type uninstallPlan struct {
	Service      bool
	ServiceName  string
	Binary       bool
	BinPath      string
	ConfigPath   string
	DeleteDrive  bool
	RevokeOAuth  bool
	DeleteKit    bool
	KitDir       string
	Wireproxy    bool
	WireproxyDir string
}

func printUninstallPlan(plan uninstallPlan, dryRun bool) {
	if dryRun {
		fmt.Println("Skirk uninstall dry run:")
	} else {
		fmt.Println("Skirk uninstall plan:")
	}
	if plan.Service {
		fmt.Printf("- remove exit service: %s\n", plan.ServiceName)
	}
	if plan.DeleteDrive {
		fmt.Printf("- delete Drive mailbox objects using config: %s\n", plan.ConfigPath)
	}
	if plan.RevokeOAuth {
		fmt.Printf("- revoke OAuth token using config: %s\n", plan.ConfigPath)
	}
	if plan.DeleteKit {
		fmt.Printf("- delete local kit directory: %s\n", plan.KitDir)
	}
	if plan.Wireproxy {
		fmt.Printf("- remove wireproxy service and paths under: %s\n", plan.WireproxyDir)
	}
	if plan.Binary {
		fmt.Printf("- remove installed binary: %s\n", plan.BinPath)
	}
	if !plan.Service && !plan.DeleteDrive && !plan.RevokeOAuth && !plan.DeleteKit && !plan.Wireproxy && !plan.Binary {
		fmt.Println("- no actions selected")
	}
}

func defaultUninstallBinaryPath() string {
	exe, err := os.Executable()
	if err == nil && strings.TrimSpace(exe) != "" {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
		return exe
	}
	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "bin", "skirk")
	}
	return "skirk"
}

func uninstallServiceIfAvailable(ctx context.Context, name string) error {
	unit, err := normalizeSystemdServiceName(name)
	if err != nil {
		return err
	}
	if err := requireSystemd(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: skipping service removal: %v\n", err)
		return nil
	}
	return uninstallSystemdService(ctx, unit)
}

func removeInstalledBinary(ctx context.Context, path string) error {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return err
	}
	if filepath.Base(abs) != "skirk" {
		return fmt.Errorf("refusing to remove installed binary %q: basename must be skirk", abs)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("Installed binary already absent: %s\n", abs)
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("refusing to remove installed binary %q: path is a directory", abs)
	}
	if err := os.Remove(abs); err != nil {
		if os.IsPermission(err) {
			if err := runPrivileged(ctx, "rm", "-f", abs); err != nil {
				return fmt.Errorf("remove installed binary %s: %w", abs, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("remove installed binary %s: %w", abs, err)
		}
	}
	fmt.Printf("Removed installed binary: %s\n", abs)
	return nil
}

func uninstallWireproxy(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("wireproxy uninstall is only available on Linux")
	}
	wireproxyUnitPath := filepath.Join("/etc/systemd/system", defaultWireproxyService+".service")
	serviceOwned := false
	if _, err := os.Lstat(wireproxyUnitPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("Wireproxy service already absent: %s\n", wireproxyUnitPath)
	} else {
		if owned, err := isSkirkSystemdUnitFile(wireproxyUnitPath); err != nil {
			return err
		} else if !owned {
			return fmt.Errorf("refusing to remove %s: unit file is not managed by Skirk", wireproxyUnitPath)
		}
		serviceOwned = true
	}

	manifest, hasManifest, err := loadWireproxyManifest(defaultWireproxyDir)
	if err != nil {
		return err
	}
	if hasManifest {
		if err := verifyWireproxyManifest(manifest); err != nil {
			return err
		}
	} else if _, err := os.Lstat(defaultWireproxyDir); err == nil {
		if !serviceOwned {
			return fmt.Errorf("refusing to remove %s: Skirk ownership manifest is absent", defaultWireproxyDir)
		}
		if err := assertSkirkWireproxyPath(defaultWireproxyDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if !serviceOwned && !hasManifest {
		fmt.Println("Wireproxy service and Skirk-owned config are absent; leaving helper binaries untouched.")
		return nil
	}
	if serviceOwned {
		if err := uninstallServiceIfAvailable(ctx, defaultWireproxyService); err != nil {
			return fmt.Errorf("remove wireproxy service: %w", err)
		}
	}

	removed := false
	if hasManifest {
		if _, err := os.Lstat(defaultWireproxyDir); err == nil {
			if err := removeWireproxyConfigDir(ctx, defaultWireproxyDir, true); err != nil {
				return err
			}
			fmt.Printf("Removed wireproxy path: %s\n", defaultWireproxyDir)
			removed = true
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, path := range []string{defaultWireproxyBin, defaultWGCFBin} {
			if _, err := os.Lstat(path); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if err := assertSkirkWireproxyPath(path); err != nil {
				return err
			}
			if err := runPrivileged(ctx, "rm", "-rf", path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
			fmt.Printf("Removed wireproxy path: %s\n", path)
			removed = true
		}
	} else if _, err := os.Lstat(defaultWireproxyDir); err == nil {
		if err := removeWireproxyConfigDir(ctx, defaultWireproxyDir, false); err != nil {
			return err
		}
		fmt.Printf("Removed wireproxy path: %s\n", defaultWireproxyDir)
		fmt.Println("Wireproxy helper binaries left untouched because the Skirk ownership manifest is absent.")
		removed = true
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !removed {
		fmt.Println("Wireproxy paths already absent.")
	}
	return nil
}

func preflightUninstallWireproxy() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("wireproxy uninstall is only available on Linux")
	}
	wireproxyUnitPath := filepath.Join("/etc/systemd/system", defaultWireproxyService+".service")
	serviceOwned := false
	if _, err := os.Lstat(wireproxyUnitPath); err == nil {
		owned, err := isSkirkSystemdUnitFile(wireproxyUnitPath)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("refusing to remove %s: unit file is not managed by Skirk", wireproxyUnitPath)
		}
		serviceOwned = true
	} else if !os.IsNotExist(err) {
		return err
	}

	manifest, hasManifest, err := loadWireproxyManifest(defaultWireproxyDir)
	if err != nil {
		return err
	}
	if hasManifest {
		return verifyWireproxyManifest(manifest)
	}
	if _, err := os.Lstat(defaultWireproxyDir); err == nil {
		if !serviceOwned {
			return fmt.Errorf("refusing to remove %s: Skirk ownership manifest is absent", defaultWireproxyDir)
		}
		return assertSkirkWireproxyPath(defaultWireproxyDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loadWireproxyManifest(dir string) (wireproxyManifest, bool, error) {
	path := filepath.Join(dir, wireproxyManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wireproxyManifest{}, false, nil
		}
		return wireproxyManifest{}, false, err
	}
	manifest := wireproxyManifest{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.Contains(line, "Managed by Skirk") {
			manifest.HasManagedBySkirk = true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "config_dir":
			manifest.ConfigDir = strings.TrimSpace(value)
		case "wireproxy_bin":
			manifest.WireproxyBin = strings.TrimSpace(value)
		case "wireproxy_sha256":
			manifest.WireproxySHA256 = strings.TrimSpace(value)
		case "wgcf_bin":
			manifest.WGCFBin = strings.TrimSpace(value)
		case "wgcf_sha256":
			manifest.WGCFSHA256 = strings.TrimSpace(value)
		case "service":
			manifest.Service = strings.TrimSpace(value)
		}
	}
	return manifest, true, nil
}

func verifyWireproxyManifest(manifest wireproxyManifest) error {
	if !manifest.HasManagedBySkirk {
		return fmt.Errorf("refusing to remove wireproxy: ownership manifest is missing Skirk marker")
	}
	expectedService := filepath.Join("/etc/systemd/system", defaultWireproxyService+".service")
	expected := map[string]string{
		"config_dir":    defaultWireproxyDir,
		"wireproxy_bin": defaultWireproxyBin,
		"wgcf_bin":      defaultWGCFBin,
		"service":       expectedService,
	}
	got := map[string]string{
		"config_dir":    manifest.ConfigDir,
		"wireproxy_bin": manifest.WireproxyBin,
		"wgcf_bin":      manifest.WGCFBin,
		"service":       manifest.Service,
	}
	for key, want := range expected {
		if got[key] != want {
			return fmt.Errorf("refusing to remove wireproxy: manifest %s=%q, want %q", key, got[key], want)
		}
	}
	for _, item := range []struct {
		path string
		sum  string
	}{
		{manifest.WireproxyBin, manifest.WireproxySHA256},
		{manifest.WGCFBin, manifest.WGCFSHA256},
	} {
		if strings.TrimSpace(item.sum) == "" {
			return fmt.Errorf("refusing to remove %s: manifest checksum is empty", item.path)
		}
		if _, err := os.Lstat(item.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := verifyFileSHA256(item.path, item.sum); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(defaultWireproxyDir); err == nil {
		if err := assertSkirkWireproxyPath(defaultWireproxyDir); err != nil {
			return err
		}
		if err := assertWireproxyConfigDirEntries(defaultWireproxyDir, true); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func assertWireproxyConfigDirEntries(path string, hasManifest bool) error {
	allowed := map[string]bool{
		"wgcf-account.toml": true,
		"wgcf-profile.conf": true,
		"wireproxy.conf":    true,
	}
	if hasManifest {
		allowed[wireproxyManifestName] = true
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("refusing to remove %s: contains unexpected directory %s", path, entry.Name())
		}
		if !allowed[entry.Name()] {
			return fmt.Errorf("refusing to remove %s: contains unexpected file %s", path, entry.Name())
		}
	}
	return nil
}

func removeWireproxyConfigDir(ctx context.Context, path string, hasManifest bool) error {
	if err := assertSkirkWireproxyPath(path); err != nil {
		return err
	}
	if err := assertWireproxyConfigDirEntries(path, hasManifest); err != nil {
		return err
	}
	for _, name := range []string{"wgcf-account.toml", "wgcf-profile.conf", "wireproxy.conf", wireproxyManifestName} {
		if !hasManifest && name == wireproxyManifestName {
			continue
		}
		target := filepath.Join(path, name)
		if _, err := os.Lstat(target); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := runPrivileged(ctx, "rm", "-f", target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	if err := runPrivileged(ctx, "rmdir", path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func verifyFileSHA256(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(want)) {
		return fmt.Errorf("refusing to remove %s: checksum mismatch", path)
	}
	return nil
}

func assertSkirkWireproxyPath(path string) error {
	switch path {
	case defaultWireproxyDir:
		profile := filepath.Join(path, "wgcf-profile.conf")
		conf := filepath.Join(path, "wireproxy.conf")
		data, err := os.ReadFile(conf)
		if err != nil {
			return fmt.Errorf("refusing to remove %s: missing Skirk wireproxy.conf: %w", path, err)
		}
		if !strings.Contains(string(data), "WGConfig = "+profile) {
			return fmt.Errorf("refusing to remove %s: wireproxy.conf does not point at Skirk profile", path)
		}
	case defaultWireproxyBin:
		if filepath.Base(path) != "wireproxy" {
			return fmt.Errorf("refusing to remove unexpected wireproxy binary path %s", path)
		}
	case defaultWGCFBin:
		if filepath.Base(path) != "wgcf" {
			return fmt.Errorf("refusing to remove unexpected wgcf binary path %s", path)
		}
	default:
		return fmt.Errorf("refusing to remove unexpected wireproxy path %s", path)
	}
	return nil
}
