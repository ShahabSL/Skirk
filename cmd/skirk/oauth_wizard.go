package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func normalizeOAuthMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "", "auto":
		return "auto", nil
	case "easy", "builtin", "built-in", "skirk":
		return "easy", nil
	case "personal", "own", "custom":
		return "personal", nil
	default:
		return "", fmt.Errorf("--oauth-mode must be auto, easy, or personal; got %q", raw)
	}
}

func shouldPromptOAuthMode(adcPath string, noLogin, jsonOut bool, mode, oauthClientFile string) bool {
	if jsonOut || noLogin || strings.TrimSpace(adcPath) != "" || strings.TrimSpace(oauthClientFile) != "" {
		return false
	}
	if mode != "auto" {
		return false
	}
	return isInteractiveTerminal()
}

func isInteractiveTerminal() bool {
	stdin, err := os.Stdin.Stat()
	if err != nil || stdin.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	stdout, err := os.Stdout.Stat()
	return err == nil && stdout.Mode()&os.ModeCharDevice != 0
}

func sharedOAuthAvailable() bool {
	return strings.TrimSpace(defaultOAuthClientID) != "" || strings.TrimSpace(os.Getenv("SKIRK_OAUTH_CLIENT_ID")) != ""
}

func promptSetupOAuthMode(ctx context.Context, reader *bufio.Reader) (string, string, error) {
	if !sharedOAuthAvailable() {
		path, err := promptPersonalOAuthClientFile(ctx, reader, "oauth-client.json")
		return "personal", path, err
	}
	fmt.Println("Google OAuth mode:")
	fmt.Println("1. Shared Skirk OAuth project")
	fmt.Println("2. Your own Google Cloud project (recommended)")
	fmt.Println()
	fmt.Println("Use personal mode if you expect sustained video, multiple clients, or public use.")
	choice, err := prompt(ctx, reader, "Select OAuth mode", "2")
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "1", "easy", "skirk":
		return "easy", "", nil
	case "", "2", "personal", "own", "custom":
		path, err := promptPersonalOAuthClientFile(ctx, reader, "oauth-client.json")
		return "personal", path, err
	default:
		return "", "", fmt.Errorf("unknown OAuth mode selection %q", choice)
	}
}

func promptPersonalOAuthClientFile(ctx context.Context, reader *bufio.Reader, fallback string) (string, error) {
	if strings.TrimSpace(fallback) == "" {
		fallback = "oauth-client.json"
	}
	for {
		printPersonalOAuthGuide()
		fmt.Println("How do you want to provide the OAuth client?")
		fmt.Println("1. Paste Desktop app client ID and client secret")
		fmt.Println("2. Use a downloaded OAuth client JSON file")
		fmt.Println("3. Show these instructions again")
		fmt.Println("0. Cancel")
		choice, err := prompt(ctx, reader, "Personal OAuth setup", "1")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "", "1", "paste", "manual":
			return promptAndSaveOAuthClient(ctx, reader, fallback)
		case "2", "json", "file", "path":
			return promptExistingOAuthClientFile(ctx, reader, fallback)
		case "3", "help", "guide":
			continue
		case "0", "q", "quit", "cancel":
			return "", errors.New("personal OAuth setup canceled")
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func printPersonalOAuthGuide() {
	fmt.Println()
	fmt.Println("Set up Skirk with your Google Cloud project")
	fmt.Println("1. Create a project and enable Google Drive API.")
	fmt.Println("   https://console.cloud.google.com/apis/library/drive.googleapis.com")
	fmt.Println("2. In Google Auth Platform, set up Branding and choose External for a personal Google account.")
	fmt.Println("   In Data Access, add only https://www.googleapis.com/auth/drive.file.")
	fmt.Println("   https://console.cloud.google.com/auth/overview")
	fmt.Println("3. In Audience, select Publish app and confirm In production.")
	fmt.Println("   https://console.cloud.google.com/auth/audience")
	fmt.Println("4. In Clients, create a Desktop app client. Copy its Client ID and Client secret to paste here.")
	fmt.Println("   https://console.cloud.google.com/auth/clients")
	fmt.Println()
	fmt.Println("Skirk will print a Google approval URL. On a VPS, approve it in your")
	fmt.Println("browser, then paste the redirected localhost URL into this terminal.")
	fmt.Println("Illustrated guide: docs/personal-google-setup.md")
	fmt.Println()
}

func printPersonalOAuthPublishingNote() {
	fmt.Println("Before approving Drive access, check Audience → Publishing status → In production.")
	fmt.Println("Testing grants for Drive expire after seven days. If a kit was authorized in Testing,")
	fmt.Println("publish the app and run 'skirk setup renew-oauth' with the same OAuth client.")
}

func promptExistingOAuthClientFile(ctx context.Context, reader *bufio.Reader, fallback string) (string, error) {
	path, err := prompt(ctx, reader, "OAuth client JSON path", fallback)
	if err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = fallback
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		fmt.Printf("\n%s was not found.\n", path)
		fmt.Println("1. Enter another JSON path")
		fmt.Println("2. Paste Desktop app client ID and client secret now")
		fmt.Println("3. Show setup instructions")
		fmt.Println("0. Cancel")
		choice, err := prompt(ctx, reader, "Missing OAuth JSON", "2")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "1", "path", "file":
			path, err = prompt(ctx, reader, "OAuth client JSON path", path)
			if err != nil {
				return "", err
			}
			path = strings.TrimSpace(path)
			if path == "" {
				path = fallback
			}
		case "2", "paste", "manual":
			return promptAndSaveOAuthClient(ctx, reader, fallback)
		case "3", "help", "guide":
			printPersonalOAuthGuide()
		case "0", "q", "quit", "cancel":
			return "", errors.New("personal OAuth setup canceled")
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func promptAndSaveOAuthClient(ctx context.Context, reader *bufio.Reader, path string) (string, error) {
	fmt.Println()
	fmt.Println("Paste each value from your Desktop app client in Google Cloud, then press Enter.")
	clientID, err := prompt(ctx, reader, "OAuth client ID", "")
	if err != nil {
		return "", err
	}
	clientSecret, err := promptOAuthClientSecret(ctx, reader)
	if err != nil {
		return "", err
	}
	creds, ok, err := oauthClientFromPair(clientID, clientSecret, "pasted OAuth client")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("pasted OAuth client ID cannot be empty")
	}
	creds.Flow = "desktop"
	savePath, err := writeOAuthClientJSON(path, creds)
	if err != nil {
		return "", err
	}
	fmt.Printf("Saved personal OAuth client JSON to %s.\n\n", savePath)
	return savePath, nil
}

func promptOAuthClientSecret(ctx context.Context, reader *bufio.Reader) (secret string, err error) {
	restore, err := disableInputEcho(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("hide OAuth client secret: %w", err)
	}
	label := "OAuth client secret (optional)"
	if restore != nil {
		label += " [input hidden]"
		defer func() {
			restoreErr := restore()
			fmt.Println()
			if err == nil && restoreErr != nil {
				secret, err = "", fmt.Errorf("restore terminal input: %w", restoreErr)
			}
		}()
	}
	return prompt(ctx, reader, label, "")
}

func writeOAuthClientJSON(path string, creds oauthClientCredentials) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	installed := map[string]string{
		"client_id": creds.ClientID,
	}
	if strings.TrimSpace(creds.ClientSecret) != "" {
		installed["client_secret"] = creds.ClientSecret
	}
	payload := map[string]any{
		"installed": installed,
	}
	if strings.TrimSpace(creds.Flow) != "" {
		payload["skirk_oauth_flow"] = strings.TrimSpace(creds.Flow)
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		// A second kit can use another OAuth project from the same directory.
		ext := filepath.Ext(path)
		prefix := strings.TrimSuffix(filepath.Base(path), ext)
		f, err = os.CreateTemp(dir, prefix+"-*"+ext)
	}
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(body)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
