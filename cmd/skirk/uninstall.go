package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	defaultWireproxyService = "wireproxy"
	defaultWireproxyDir     = "/etc/wireproxy"
	defaultWireproxyBin     = "/usr/local/bin/wireproxy"
	defaultWGCFBin          = "/usr/local/bin/wgcf"
)

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
	wireproxyUnitPath := filepath.Join("/etc/systemd/system", defaultWireproxyService+".service")
	if _, err := os.Lstat(wireproxyUnitPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: skipping wireproxy path removal because %s is absent\n", wireproxyUnitPath)
			return nil
		}
		return err
	}
	if err := uninstallServiceIfAvailable(ctx, defaultWireproxyService); err != nil {
		return fmt.Errorf("remove wireproxy service: %w", err)
	}
	for _, path := range []string{defaultWireproxyDir, defaultWireproxyBin, defaultWGCFBin} {
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := runPrivileged(ctx, "rm", "-rf", path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		fmt.Printf("Removed wireproxy path: %s\n", path)
	}
	return nil
}
