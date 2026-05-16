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

func promptSetupOAuthMode(ctx context.Context, reader *bufio.Reader) (string, string, error) {
	fmt.Println("Google OAuth mode:")
	fmt.Println("1. Easy Skirk OAuth client")
	fmt.Println("2. Personal Google OAuth project")
	choice, err := prompt(ctx, reader, "Select OAuth mode", "1")
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "", "1", "easy", "skirk":
		return "easy", "", nil
	case "2", "personal", "own", "custom":
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
		printPersonalOAuthGuide(fallback)
		fmt.Println("How do you want to provide the OAuth client?")
		fmt.Println("1. Paste client ID and client secret")
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

func printPersonalOAuthGuide(defaultPath string) {
	fmt.Println()
	fmt.Println("Personal Google OAuth project wizard")
	fmt.Println()
	fmt.Println("This mode uses your own Google Cloud project for Drive API quota.")
	fmt.Println("You only need a normal Google account and a browser. Keep this terminal open.")
	fmt.Println()
	fmt.Println("In your browser, do these steps:")
	fmt.Println()
	fmt.Println("1. Create or select a Google Cloud project")
	fmt.Println("   Open: https://console.cloud.google.com/projectcreate")
	fmt.Println("   Name example: Skirk Personal")
	fmt.Println()
	fmt.Println("2. Enable Google Drive API for that project")
	fmt.Println("   Open: https://console.cloud.google.com/apis/library/drive.googleapis.com")
	fmt.Println("   Select your project if asked, then click Enable.")
	fmt.Println()
	fmt.Println("3. Configure the OAuth consent screen")
	fmt.Println("   Open: https://console.cloud.google.com/auth/overview")
	fmt.Println("   Choose External unless this is a Google Workspace-only project.")
	fmt.Println("   App name example: Skirk Personal")
	fmt.Println("   Support email and developer contact: your Google email.")
	fmt.Println("   If the app stays in Testing, add the Google account you will log in with")
	fmt.Println("   under Audience/Test users.")
	fmt.Println()
	fmt.Println("4. Create the OAuth client")
	fmt.Println("   Open: https://console.cloud.google.com/auth/clients")
	fmt.Println("   Click Create client.")
	fmt.Println("   Application type: TVs and Limited Input devices")
	fmt.Println("   Name example: Skirk Personal Device")
	fmt.Println("   Click Create.")
	fmt.Println()
	fmt.Println("5. Bring the client back to Skirk")
	fmt.Println("   Easiest: copy the Client ID and Client secret and paste them here.")
	fmt.Println("   Alternative: download the JSON and save it as " + defaultPath + ".")
	fmt.Println()
	fmt.Println("Skirk will then open Google's device-code approval flow and generate the kit.")
	fmt.Println()
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
		fmt.Println("2. Paste client ID and client secret now")
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
			return promptAndSaveOAuthClient(ctx, reader, path)
		case "3", "help", "guide":
			printPersonalOAuthGuide(fallback)
		case "0", "q", "quit", "cancel":
			return "", errors.New("personal OAuth setup canceled")
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func promptAndSaveOAuthClient(ctx context.Context, reader *bufio.Reader, fallbackPath string) (string, error) {
	fmt.Println()
	fmt.Println("Paste the values from the Google Cloud OAuth client page.")
	fmt.Println("They are labels for your Google Cloud project; by themselves they do not grant Drive access.")
	clientID, err := prompt(ctx, reader, "OAuth client ID", "")
	if err != nil {
		return "", err
	}
	clientSecret, err := prompt(ctx, reader, "OAuth client secret", "")
	if err != nil {
		return "", err
	}
	creds, ok, err := oauthClientFromPair(clientID, clientSecret, "pasted OAuth client")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("pasted OAuth client ID and secret cannot both be empty")
	}
	savePath, err := prompt(ctx, reader, "Save OAuth client JSON as", fallbackPath)
	if err != nil {
		return "", err
	}
	savePath = strings.TrimSpace(savePath)
	if savePath == "" {
		savePath = fallbackPath
	}
	if err := writeOAuthClientJSON(savePath, creds); err != nil {
		return "", err
	}
	fmt.Printf("Saved personal OAuth client JSON to %s.\n\n", savePath)
	return savePath, nil
}

func writeOAuthClientJSON(path string, creds oauthClientCredentials) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	payload := map[string]any{
		"installed": map[string]string{
			"client_id":     creds.ClientID,
			"client_secret": creds.ClientSecret,
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0600)
}
