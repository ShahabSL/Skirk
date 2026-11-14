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
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type oauthRenewKit struct {
	kitDir           string
	exitPath         string
	exitConfig       *skirk.Config
	exitJSON         []byte
	clientConfig     *skirk.Config
	clientTextConfig *skirk.Config
	clientJSON       []byte
	clientText       []byte
	clientCommand    []byte
	hasClientText    bool
	hasClientCommand bool
}

type oauthRenewFile struct {
	path        string
	original    []byte
	replacement []byte
	staged      string
}

func setupRenewOAuth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup renew-oauth", flag.ExitOnError)
	kitFlag := fs.String("kit", "", "kit directory; defaults to skirk-kit or the --config directory")
	configFlag := fs.String("config", "", "existing exit.json path")
	clientFile := fs.String("oauth-client-file", "", "OAuth client JSON; its client ID must match the existing kit")
	flowFlag := fs.String("oauth-flow", "auto", "Google OAuth flow: auto, device, or desktop")
	restartExit := fs.Bool("restart-exit", false, "restart the exit service after safely updating the kit")
	serviceName := fs.String("exit-service-name", defaultServiceName, "exit service to restart with --restart-exit")
	jsonOut := fs.Bool("json", false, "print machine-readable result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("setup renew-oauth does not accept positional arguments")
	}
	if *restartExit && runtime.GOOS != "linux" {
		return fmt.Errorf("--restart-exit is only supported on Linux; renew the kit and restart its exit manually on %s", runtime.GOOS)
	}
	kitDir := strings.TrimSpace(*kitFlag)
	exitPath := strings.TrimSpace(*configFlag)
	if kitDir == "" {
		if exitPath == "" {
			kitDir = "skirk-kit"
		} else {
			kitDir = filepath.Dir(exitPath)
		}
	}
	if exitPath == "" {
		exitPath = filepath.Join(kitDir, "exit.json")
	}
	kit, err := loadOAuthRenewKit(kitDir, exitPath)
	if err != nil {
		return err
	}
	client, err := resolveRenewOAuthClient(kit.exitConfig.Auth, strings.TrimSpace(*clientFile))
	if err != nil {
		return err
	}
	requestedFlow, err := normalizeOAuthFlow(*flowFlag)
	if err != nil {
		return err
	}
	mode := "personal"
	if defaultOAuthClientID != "" && client.ClientID == defaultOAuthClientID {
		mode = "easy"
	}
	flow, err := resolveSetupOAuthFlow(mode, requestedFlow, client)
	if err != nil {
		return err
	}
	if flow == "device" && client.ClientSecret == "" {
		return errors.New("this OAuth client has no client secret; use --oauth-flow desktop or provide its matching --oauth-client-file")
	}
	creds, err := runGoogleOAuth(ctx, client, defaultCustomOAuthScopes, flow, bufio.NewReader(os.Stdin))
	if err != nil {
		return err
	}
	newAuth := creds.AuthConfig()
	if newAuth.ClientID != kit.exitConfig.Auth.ClientID || newAuth.RefreshToken == "" {
		return errors.New("Google returned credentials for a different OAuth client or without a refresh token; the kit was not changed")
	}
	if err := validateDriveMailbox(ctx, newAuth, kit.exitConfig.Drive, kit.exitConfig.Route, kit.exitConfig.SessionID); err != nil {
		return fmt.Errorf("new Google credentials cannot use the existing Drive mailbox; the kit was not changed: %w", err)
	}
	files, err := kit.updatedFiles(newAuth)
	if err != nil {
		return err
	}
	backupDir, err := replaceOAuthRenewFiles(kit.kitDir, files)
	if err != nil {
		return err
	}
	if *restartExit {
		if err := serviceCommand(ctx, []string{"restart", "--name", *serviceName}); err != nil {
			return fmt.Errorf("OAuth renewal was saved (backup: %s), but the exit service restart failed: %w", backupDir, err)
		}
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"result":         "ok",
			"exit_config":    kit.exitPath,
			"client_updated": kit.clientConfig != nil,
			"client_config":  filepath.Join(kit.kitDir, "client.skirk"),
			"backup_dir":     backupDir,
			"exit_restarted": *restartExit,
			"client_action":  "Re-import the updated client.skirk on each device; previously imported profiles still contain the old refresh token.",
		})
	}
	fmt.Printf("Renewed Google OAuth for %s.\n", kit.exitPath)
	fmt.Printf("Backup: %s\n", backupDir)
	if !*restartExit {
		fmt.Printf("Restart the exit service to load the new credentials.\n")
	}
	if kit.clientConfig != nil {
		fmt.Printf("Re-import %s on each client device. Previously imported profiles still contain the old refresh token.\n", filepath.Join(kit.kitDir, "client.skirk"))
	} else {
		fmt.Printf("This kit has no client.json. Update every client profile with the new OAuth credentials before its old token expires.\n")
	}
	return nil
}

func resolveRenewOAuthClient(old skirk.AuthConfig, path string) (oauthClientCredentials, error) {
	if strings.TrimSpace(old.ClientID) == "" || strings.TrimSpace(old.RefreshToken) == "" {
		return oauthClientCredentials{}, errors.New("existing exit.json has no Google OAuth client ID and refresh token; setup renew-oauth requires an OAuth kit")
	}
	client := oauthClientCredentials{ClientID: old.ClientID, ClientSecret: old.ClientSecret}
	if path == "" {
		return client, nil
	}
	provided, err := readOAuthClientCredentials(path)
	if err != nil {
		return oauthClientCredentials{}, err
	}
	if provided.ClientID != old.ClientID {
		return oauthClientCredentials{}, errors.New("--oauth-client-file belongs to a different Google OAuth client; use the original project and client ID so the existing Drive mailbox remains accessible")
	}
	if provided.ClientSecret != "" {
		client.ClientSecret = provided.ClientSecret
	}
	client.Flow = provided.Flow
	return client, nil
}

func loadOAuthRenewKit(kitDir, exitPath string) (*oauthRenewKit, error) {
	exitJSON, err := readRegularFile(exitPath)
	if err != nil {
		return nil, fmt.Errorf("read exit config: %w", err)
	}
	exitConfig, err := skirk.ParseConfig(exitJSON)
	if err != nil {
		return nil, fmt.Errorf("parse exit config: %w", err)
	}
	if _, err := resolveRenewOAuthClient(exitConfig.Auth, ""); err != nil {
		return nil, err
	}
	kit := &oauthRenewKit{kitDir: kitDir, exitPath: exitPath, exitConfig: exitConfig, exitJSON: exitJSON}
	clientPath := filepath.Join(kitDir, "client.json")
	clientJSON, err := readRegularFile(clientPath)
	if errors.Is(err, os.ErrNotExist) {
		for _, name := range []string{"client.skirk", "client-command.txt"} {
			if _, statErr := os.Lstat(filepath.Join(kitDir, name)); statErr == nil {
				return nil, fmt.Errorf("%s exists but client.json is missing; refusing to leave an exported profile with stale OAuth credentials", name)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return nil, statErr
			}
		}
		return kit, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}
	clientConfig, err := skirk.ParseConfig(clientJSON)
	if err != nil {
		return nil, fmt.Errorf("parse client config: %w", err)
	}
	if err := checkOAuthRenewClientIdentity(exitConfig, clientConfig); err != nil {
		return nil, fmt.Errorf("client.json: %w", err)
	}
	kit.clientConfig, kit.clientJSON = clientConfig, clientJSON
	textPath := filepath.Join(kitDir, "client.skirk")
	kit.clientText, err = readRegularFile(textPath)
	if err == nil {
		kit.hasClientText = true
		textConfig, parseErr := skirk.ParseConfig(kit.clientText)
		if parseErr != nil {
			return nil, fmt.Errorf("parse client.skirk: %w", parseErr)
		}
		if identityErr := checkOAuthRenewClientIdentity(exitConfig, textConfig); identityErr != nil {
			return nil, fmt.Errorf("client.skirk: %w", identityErr)
		}
		kit.clientTextConfig = textConfig
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read client.skirk: %w", err)
	}
	commandPath := filepath.Join(kitDir, "client-command.txt")
	kit.clientCommand, err = readRegularFile(commandPath)
	if err == nil {
		kit.hasClientCommand = true
		if !kit.hasClientText || !strings.Contains(string(kit.clientCommand), strings.TrimSpace(string(kit.clientText))) {
			return nil, errors.New("client-command.txt does not contain client.skirk; refusing to overwrite a custom command")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read client-command.txt: %w", err)
	}
	return kit, nil
}

func checkOAuthRenewClientIdentity(exit, client *skirk.Config) error {
	if client.Secret != exit.Secret || client.SessionID != exit.SessionID || client.Drive != exit.Drive || client.Auth.ClientID != exit.Auth.ClientID {
		return errors.New("client profile does not match the exit secret, session, Drive mailbox, and OAuth client ID")
	}
	return nil
}

func (kit *oauthRenewKit) updatedFiles(auth skirk.AuthConfig) ([]oauthRenewFile, error) {
	exitJSON, err := replaceAuthJSON(kit.exitJSON, auth)
	if err != nil {
		return nil, fmt.Errorf("rewrite exit.json: %w", err)
	}
	files := []oauthRenewFile{{path: kit.exitPath, original: kit.exitJSON, replacement: exitJSON}}
	if kit.clientConfig == nil {
		return files, nil
	}
	clientJSON, err := replaceAuthJSON(kit.clientJSON, auth)
	if err != nil {
		return nil, fmt.Errorf("rewrite client.json: %w", err)
	}
	clientConfig, err := skirk.ParseConfig(clientJSON)
	if err != nil {
		return nil, fmt.Errorf("validate renewed client config: %w", err)
	}
	profileConfig := clientConfig
	if kit.clientTextConfig != nil {
		copy := *kit.clientTextConfig
		copy.Auth = auth
		profileConfig = &copy
	}
	clientText, err := skirk.EncodeConfigText(profileConfig)
	if err != nil {
		return nil, err
	}
	clientPath := filepath.Join(kit.kitDir, "client.json")
	files = append(files, oauthRenewFile{path: clientPath, original: kit.clientJSON, replacement: clientJSON})
	textFile := oauthRenewFile{path: filepath.Join(kit.kitDir, "client.skirk"), replacement: []byte(clientText + "\n")}
	if kit.hasClientText {
		textFile.original = kit.clientText
	}
	files = append(files, textFile)
	command := fmt.Sprintf("skirk serve-client --config '%s' --listen %s\n", clientText, clientConfig.Tunnel.Listen)
	if kit.hasClientCommand {
		command = strings.ReplaceAll(string(kit.clientCommand), strings.TrimSpace(string(kit.clientText)), clientText)
	}
	commandFile := oauthRenewFile{path: filepath.Join(kit.kitDir, "client-command.txt"), replacement: []byte(command)}
	if kit.hasClientCommand {
		commandFile.original = kit.clientCommand
	}
	return append(files, commandFile), nil
}

func replaceAuthJSON(original []byte, auth skirk.AuthConfig) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(original, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("config must be a JSON object")
	}
	encodedAuth, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	document["auth"] = encodedAuth
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
}

func replaceOAuthRenewFiles(kitDir string, files []oauthRenewFile) (string, error) {
	backupDir, err := os.MkdirTemp(kitDir, ".oauth-renew-backup-")
	if err != nil {
		return "", err
	}
	cleanStaged := func() {
		for i := range files {
			if files[i].staged != "" {
				_ = os.Remove(files[i].staged)
			}
		}
	}
	for i := range files {
		stage, stageErr := stageOAuthRenewFile(files[i].path, files[i].replacement)
		if stageErr != nil {
			cleanStaged()
			_ = os.RemoveAll(backupDir)
			return "", stageErr
		}
		files[i].staged = stage
		if files[i].original != nil {
			backupPath := filepath.Join(backupDir, fmt.Sprintf("%02d-%s", i, filepath.Base(files[i].path)))
			if backupErr := writeSyncedFile(backupPath, files[i].original); backupErr != nil {
				cleanStaged()
				_ = os.RemoveAll(backupDir)
				return "", backupErr
			}
		}
	}
	for i := range files {
		if err := os.Rename(files[i].staged, files[i].path); err != nil {
			cleanStaged()
			var rollbackErrors []error
			for j := i - 1; j >= 0; j-- {
				if files[j].original == nil {
					if removeErr := os.Remove(files[j].path); removeErr != nil {
						rollbackErrors = append(rollbackErrors, removeErr)
					}
					continue
				}
				rollbackStage, stageErr := stageOAuthRenewFile(files[j].path, files[j].original)
				if stageErr == nil {
					stageErr = os.Rename(rollbackStage, files[j].path)
				}
				if stageErr != nil {
					rollbackErrors = append(rollbackErrors, stageErr)
				}
			}
			return "", fmt.Errorf("renewal write failed (backup: %s): %w; rollback errors: %v", backupDir, err, rollbackErrors)
		}
		files[i].staged = ""
	}
	return backupDir, nil
}

func stageOAuthRenewFile(path string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".renew-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	cleanup := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Chmod(0600); err != nil {
		return cleanup(err)
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(err)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func writeSyncedFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
