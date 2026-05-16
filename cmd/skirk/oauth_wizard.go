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
		fmt.Println("Personal OAuth needs a Google OAuth client of type \"TVs and Limited Input devices\".")
		fmt.Println("Create one in Google Cloud, download the JSON, or paste the client ID and secret now.")
		fmt.Println("Google Cloud clients page: https://console.cloud.google.com/auth/clients")
		fmt.Println()
		fmt.Println("1. Enter another JSON path")
		fmt.Println("2. Paste client ID and client secret now")
		fmt.Println("0. Cancel")
		choice, err := prompt(ctx, reader, "OAuth client setup", "2")
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
			savePath, err := prompt(ctx, reader, "Save OAuth client JSON as", path)
			if err != nil {
				return "", err
			}
			savePath = strings.TrimSpace(savePath)
			if savePath == "" {
				savePath = fallback
			}
			if err := writeOAuthClientJSON(savePath, creds); err != nil {
				return "", err
			}
			fmt.Printf("Saved personal OAuth client JSON to %s.\n\n", savePath)
			return savePath, nil
		case "0", "q", "quit", "cancel":
			return "", errors.New("personal OAuth setup canceled")
		default:
			fmt.Println("Unknown selection")
		}
	}
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
