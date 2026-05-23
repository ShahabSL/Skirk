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
	if err := validateServicePathsForUser(exe, configPath, user); err != nil {
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

	unitPath := filepath.Join("/etc/systemd/system", unit)
	if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "install", "-m", "0644", tmpPath, unitPath); err != nil {
		return err
	}
	if err := runPrivilegedWithStdout(ctx, commandStdout(opts.Quiet), "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := verifySystemdUnit(ctx, unitPath); err != nil {
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

func installSystemdDropIn(ctx context.Context, unit, name, text string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	normalized, err := normalizeSystemdServiceName(unit)
	if err != nil {
		return err
	}
	unit = normalized
	if strings.TrimSpace(name) == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("unsafe systemd drop-in name %q", name)
	}
	if err := assertSkirkSystemdUnit(unit); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "skirk-*.conf")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dir := filepath.Join("/etc/systemd/system", unit+".d")
	path := filepath.Join(dir, name)
	if err := runPrivileged(ctx, "mkdir", "-p", dir); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "install", "-m", "0644", tmpPath, path); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Installed systemd drop-in %s\n", path)
	return nil
}

func removeSystemdDropIn(ctx context.Context, unit, name string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	normalized, err := normalizeSystemdServiceName(unit)
	if err != nil {
		return err
	}
	unit = normalized
	if strings.TrimSpace(name) == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("unsafe systemd drop-in name %q", name)
	}
	if err := assertSkirkSystemdUnit(unit); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	dir := filepath.Join("/etc/systemd/system", unit+".d")
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := assertSkirkDropInFile(path); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "rm", "-f", path); err != nil {
		return err
	}
	_ = runPrivileged(ctx, "rmdir", dir)
	if err := runPrivileged(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Removed systemd drop-in %s\n", path)
	return nil
}

func assertSkirkSystemdUnit(unit string) error {
	unitPath := filepath.Join("/etc/systemd/system", unit)
	owned, err := isSkirkSystemdUnitFile(unitPath)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("refusing to modify %s: unit file is not managed by Skirk", unitPath)
	}
	return nil
}

func removeSystemdUnitDependency(ctx context.Context, unit, dependency string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	normalized, err := normalizeSystemdServiceName(unit)
	if err != nil {
		return err
	}
	unit = normalized
	unitPath := filepath.Join("/etc/systemd/system", unit)
	data, err := os.ReadFile(unitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if owned, err := isSkirkSystemdUnitFile(unitPath); err != nil {
		return err
	} else if !owned {
		return fmt.Errorf("refusing to edit %s: unit file is not managed by Skirk", unitPath)
	}
	next, changed := removeSystemdDependencyFromUnitText(string(data), dependency)
	if !changed {
		return nil
	}
	tmp, err := os.CreateTemp("", "skirk-*.service")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(next); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "install", "-m", "0644", tmpPath, unitPath); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Removed %s dependency from %s\n", dependency, unitPath)
	return nil
}

func removeSystemdDependencyFromUnitText(text, dependency string) (string, bool) {
	var out []string
	changed := false
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimSuffix(line, "\n")
		suffix := ""
		if strings.HasSuffix(line, "\n") {
			suffix = "\n"
		}
		key, value, ok := strings.Cut(body, "=")
		if !ok || (key != "After" && key != "Wants") {
			out = append(out, line)
			continue
		}
		fields := strings.Fields(value)
		nextFields := fields[:0]
		for _, field := range fields {
			if field == dependency {
				changed = true
				continue
			}
			nextFields = append(nextFields, field)
		}
		if len(nextFields) == 0 {
			if len(fields) != 0 {
				changed = true
			}
			continue
		}
		out = append(out, key+"="+strings.Join(nextFields, " ")+suffix)
	}
	return strings.Join(out, ""), changed
}

func uninstallSystemdService(ctx context.Context, unit string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	unitPath := filepath.Join("/etc/systemd/system", unit)
	dropInDir := unitPath + ".d"
	owned, err := isSkirkSystemdUnitFile(unitPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := removeSkirkSystemdDropInDir(ctx, dropInDir); err != nil {
				return err
			}
			fmt.Printf("Systemd service file already absent: %s\n", unitPath)
			return nil
		}
		return err
	}
	if !owned {
		return fmt.Errorf("refusing to remove %s: unit file is not managed by Skirk", unitPath)
	}
	if err := runPrivileged(ctx, "systemctl", "disable", "--now", unit); err != nil {
		return fmt.Errorf("stop and disable %s: %w", unit, err)
	}
	if err := runPrivileged(ctx, "rm", "-f", unitPath); err != nil {
		return err
	}
	if err := removeSkirkSystemdDropInDir(ctx, dropInDir); err != nil {
		return err
	}
	if err := runPrivileged(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Removed systemd service %s\n", unit)
	return nil
}

func removeSkirkSystemdDropInDir(ctx context.Context, dir string) error {
	if _, err := os.Lstat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := assertSkirkDropInDir(dir); err != nil {
		return err
	}
	return runPrivileged(ctx, "rm", "-rf", dir)
}

func assertSkirkDropInDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("refusing to remove %s: contains nested directory %s", dir, entry.Name())
		}
		if !strings.HasSuffix(entry.Name(), ".conf") {
			return fmt.Errorf("refusing to remove %s: contains non-drop-in file %s", dir, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !isSkirkDropInText(string(data)) {
			return fmt.Errorf("refusing to remove %s: drop-in %s is not managed by Skirk", dir, entry.Name())
		}
	}
	return nil
}

func assertSkirkDropInFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !isSkirkDropInText(string(data)) {
		return fmt.Errorf("refusing to remove %s: drop-in is not managed by Skirk", path)
	}
	return nil
}

func isSkirkDropInText(text string) bool {
	if strings.Contains(text, "Managed by Skirk") {
		return true
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n") == "[Unit]\nAfter=wireproxy.service\nWants=wireproxy.service"
}

func isSkirkSystemdUnitFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return isSkirkSystemdUnitText(string(data)), nil
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
# Managed by Skirk
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

func isSkirkSystemdUnitText(text string) bool {
	if strings.Contains(text, "Managed by Skirk") {
		return true
	}
	if strings.Contains(text, "Wireproxy WARP SOCKS proxy for Skirk exit") {
		return true
	}
	return strings.Contains(text, "ExecStart=") &&
		strings.Contains(text, " serve-exit ") &&
		strings.Contains(text, " --config ")
}

func verifySystemdUnit(ctx context.Context, path string) error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		return nil
	}
	output, err := exec.CommandContext(ctx, "systemd-analyze", "verify", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("generated systemd service unit is invalid: %w\n%s", err, strings.TrimSpace(string(output)))
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

func validateServicePathsForUser(exePath, configPath, serviceUser string) error {
	if serviceUser == "root" {
		return nil
	}
	if isRootPrivatePath(exePath) {
		return fmt.Errorf("systemd service user %q cannot execute Skirk from %s; install Skirk under /usr/local/bin or run the service as root", serviceUser, exePath)
	}
	if isRootPrivatePath(configPath) {
		return fmt.Errorf("systemd service user %q cannot read Skirk config from %s; move the kit outside /root or run the service as root", serviceUser, configPath)
	}
	return nil
}

func isRootPrivatePath(path string) bool {
	clean := filepath.Clean(path)
	return clean == "/root" || strings.HasPrefix(clean, "/root/")
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
