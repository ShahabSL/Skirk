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
	fmt.Println("   Then open Audience/Test users and add the exact Google account")
	fmt.Println("   you will use during Google approval. This is required while")
	fmt.Println("   Publishing status is Testing.")
	fmt.Println()
	fmt.Println("4. Create the OAuth client")
	fmt.Println("   Open: https://console.cloud.google.com/auth/clients")
	fmt.Println("   Click Create client.")
	fmt.Println("   Application type: Desktop app")
	fmt.Println("   Name example: Skirk Personal Desktop")
	fmt.Println("   Click Create.")
	fmt.Println()
	fmt.Println("5. Bring the client back to Skirk")
	fmt.Println("   Easiest: copy the Client ID and Client secret and paste them here.")
	fmt.Println("   Alternative: download the JSON and save it as " + defaultPath + ".")
	fmt.Println()
	fmt.Println("Skirk will print a Google approval URL. On a VPS, approve it in your")
	fmt.Println("browser, then paste the redirected localhost URL back into this terminal.")
	fmt.Println("Do not choose TVs and Limited Input devices for personal setup unless")
	fmt.Println("Google also gives you a client secret and you explicitly use --oauth-flow device.")
	fmt.Println()
}

func confirmPersonalOAuthConsentReady(ctx context.Context, reader *bufio.Reader) error {
	fmt.Println()
	fmt.Println("Before Skirk opens Google approval:")
	fmt.Println()
	fmt.Println("Open: https://console.cloud.google.com/auth/audience")
	fmt.Println()
	fmt.Println("If Publishing status is Testing, add the exact Google account you will")
	fmt.Println("approve with under Test users. This is the fix for Google's message:")
	fmt.Println("\"the app is currently being tested and can only be accessed by")
	fmt.Println("developer-approved testers.\"")
	fmt.Println()
	fmt.Println("Alternative: publish the app to Production. Google may still show an")
	fmt.Println("unverified-app warning/user cap until verification, but the Testing")
	fmt.Println("allowlist block goes away.")
	fmt.Println()
	fmt.Println("Do not add more scopes for this error. Skirk requests drive.file during")
	fmt.Println("Google login; this error is about OAuth app audience access.")
	ready, err := promptYesNo(ctx, reader, "I added the account as a test user or published the app", false)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("finish Google OAuth Audience/Test users setup, then rerun setup")
	}
	return nil
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
	fmt.Println("Use application type \"Desktop app\". If Google only shows a Client ID, you probably created a TV/Limited Input client; create a Desktop app client instead.")
	clientID, err := prompt(ctx, reader, "OAuth client ID", "")
	if err != nil {
		return "", err
	}
	clientSecret, err := prompt(ctx, reader, "OAuth client secret (optional)", "")
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
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0600)
}
