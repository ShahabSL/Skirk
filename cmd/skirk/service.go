package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const defaultServiceName = "skirk-exit"

func serviceCommand(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("service needs install, status, start, stop, restart, or uninstall")
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ExitOnError)
		configPath := fs.String("config", "skirk-kit/exit.json", "exit config path")
		name := fs.String("name", defaultServiceName, "systemd service name")
		user := fs.String("user", "", "user to run the exit service as; defaults to the current user")
		start := fs.Bool("start", true, "start or restart the service after installing")
		enable := fs.Bool("enable", true, "enable the service at boot")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return installSystemdService(ctx, serviceInstallOptions{
			Name:       *name,
			ConfigPath: *configPath,
			User:       *user,
			Start:      *start,
			Enable:     *enable,
		})
	case "status", "start", "stop", "restart", "uninstall":
		fs := flag.NewFlagSet("service "+args[0], flag.ExitOnError)
		name := fs.String("name", defaultServiceName, "systemd service name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		unit, err := normalizeSystemdServiceName(*name)
		if err != nil {
			return err
		}
		switch args[0] {
		case "status":
			return runCommand(ctx, "systemctl", "status", unit, "--no-pager")
		case "uninstall":
			return uninstallSystemdService(ctx, unit)
		default:
			return runPrivileged(ctx, "systemctl", args[0], unit)
		}
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

type serviceInstallOptions struct {
	Name       string
	ConfigPath string
	User       string
	Start      bool
	Enable     bool
	Quiet      bool
}

func installSystemdService(ctx context.Context, opts serviceInstallOptions) error {
	unit, err := normalizeSystemdServiceName(opts.Name)
	if err != nil {
		return err
	}
	if err := requireSystemd(); err != nil {
		return err
	}
	configPath, err := filepath.Abs(strings.TrimSpace(opts.ConfigPath))
	if err != nil {
		return err
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("exit config is not readable at %s: %w", configPath, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	user := strings.TrimSpace(opts.User)
	if user == "" {
		user, err = currentUsername(ctx)
		if err != nil {
			return err
		}
	}
	if err := validateSystemdUser(user); err != nil {
		return err
	}
	unitText := systemdUnitText(exe, configPath, user)
	tmp, err := os.CreateTemp("", "skirk-*.service")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(unitText); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := verifySystemdUnit(ctx, tmpPath); err != nil {
		return err
	}

	unitPath := filepath.Join("/etc/systemd/system", unit)
	if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "install", "-m", "0644", tmpPath, unitPath); err != nil {
		return err
	}
	if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if opts.Enable {
		if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "systemctl", "enable", unit); err != nil {
			return err
		}
	}
	if opts.Start {
		if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "systemctl", "restart", unit); err != nil {
			return err
		}
	}
	fmt.Fprintf(commandStdout(opts.Quiet), "Installed systemd service %s using %s\n", unit, configPath)
	return nil
}

func uninstallSystemdService(ctx context.Context, unit string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "systemctl", "disable", "--now", unit); err != nil {
		fmt.Fprintf(os.Stderr, "warning: systemctl disable --now failed for %s: %v\n", unit, err)
	}
	if err := runPrivileged(ctx, "rm", "-f", filepath.Join("/etc/systemd/system", unit)); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Removed systemd service %s\n", unit)
	return nil
}

func requireSystemd() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd service management is only available on Linux")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl was not found; run serve-exit manually or install systemd")
	}
	return nil
}

func normalizeSystemdServiceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("service name is required")
	}
	name = strings.TrimSuffix(name, ".service")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '@':
		default:
			return "", fmt.Errorf("service name %q contains unsupported character %q", name, r)
		}
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("service name %q must not contain '..'", name)
	}
	return name + ".service", nil
}

func systemdUnitText(exePath, configPath, serviceUser string) string {
	workDir := filepath.Dir(configPath)
	return fmt.Sprintf(`[Unit]
Description=Skirk exit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s serve-exit --config %s
Restart=always
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, systemdUnitValue(serviceUser), systemdUnitValue(workDir), systemdExecArg(exePath), systemdExecArg(configPath))
}

func verifySystemdUnit(ctx context.Context, path string) error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		return nil
	}
	if err := runCommandWithStdout(ctx, os.Stderr, "systemd-analyze", "verify", path); err != nil {
		return fmt.Errorf("generated systemd service unit is invalid: %w", err)
	}
	return nil
}

func validateSystemdUser(user string) error {
	if user == "" {
		return fmt.Errorf("systemd service user is required")
	}
	for _, r := range user {
		if r <= ' ' || r == '"' || r == '\'' || r == '\\' {
			return fmt.Errorf("systemd service user %q contains unsupported character %q", user, r)
		}
	}
	return nil
}

func systemdUnitValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case ' ':
			b.WriteString(`\s`)
		case '\t':
			b.WriteString(`\t`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			b.WriteString(`%%`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func systemdExecArg(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func currentUsername(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "id", "-un").Output()
	if err != nil {
		return "", err
	}
	user := strings.TrimSpace(string(output))
	if user == "" {
		return "", fmt.Errorf("current user is empty")
	}
	return user, nil
}

func runCommand(ctx context.Context, name string, args ...string) error {
	return runCommandWithStdout(ctx, os.Stdout, name, args...)
}

func runCommandWithStdout(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runPrivileged(ctx context.Context, name string, args ...string) error {
	return runPrivilegedWithStdout(ctx, os.Stdout, name, args...)
}

func runPrivilegedWithStdout(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	if os.Geteuid() == 0 {
		return runCommandWithStdout(ctx, stdout, name, args...)
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("root privileges are required for %s; rerun as root or install sudo", name)
	}
	return runCommandWithStdout(ctx, stdout, "sudo", append([]string{name}, args...)...)
}

func commandStdout(quiet bool) io.Writer {
	if quiet {
		return os.Stderr
	}
	return os.Stdout
}
