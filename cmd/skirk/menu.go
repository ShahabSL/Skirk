package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func menu(ctx context.Context) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println()
		fmt.Print(skirkBanner)
		fmt.Println("1. Create Google kit (easy Skirk OAuth)")
		fmt.Println("2. Create Google kit (personal Google OAuth project)")
		fmt.Println("3. Run exit in this terminal")
		fmt.Println("4. Run client SOCKS in this terminal")
		fmt.Println("5. Run optional desktop dashboard")
		fmt.Println("6. Manage exit service")
		fmt.Println("7. Revoke, clean, or delete kit")
		fmt.Println("8. Show commands")
		fmt.Println("0. Quit")
		choice, err := prompt(ctx, reader, "Select", "1")
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		switch choice {
		case "1":
			return createGoogleKitFromMenu(ctx, reader, "easy")
		case "2":
			return createGoogleKitFromMenu(ctx, reader, "personal")
		case "3":
			config, err := prompt(ctx, reader, "Exit config", "skirk-kit/exit.json")
			if err != nil {
				return err
			}
			return serveExit(ctx, []string{"--config", config})
		case "4":
			config, err := prompt(ctx, reader, "Client config or pasted text", "skirk-kit/client.skirk")
			if err != nil {
				return err
			}
			listen, err := prompt(ctx, reader, "SOCKS listen", "127.0.0.1:18080")
			if err != nil {
				return err
			}
			return serveClient(ctx, []string{"--config", config, "--listen", listen})
		case "5":
			config, err := prompt(ctx, reader, "Client config or pasted text", "skirk-kit/client.skirk")
			if err != nil {
				return err
			}
			socks, err := prompt(ctx, reader, "SOCKS listen", "127.0.0.1:18080")
			if err != nil {
				return err
			}
			ui, err := prompt(ctx, reader, "UI listen", "127.0.0.1:18280")
			if err != nil {
				return err
			}
			return clientUI(ctx, []string{"--config", config, "--socks", socks, "--ui", ui})
		case "6":
			if err := serviceMenu(ctx, reader); err != nil {
				return err
			}
		case "7":
			config, err := prompt(ctx, reader, "Exit config", "skirk-kit/exit.json")
			if err != nil {
				return err
			}
			stopService, err := promptYesNo(ctx, reader, "Stop exit service first", false)
			if err != nil {
				return err
			}
			if stopService {
				name, err := prompt(ctx, reader, "Service name", defaultServiceName)
				if err != nil {
					return err
				}
				if err := serviceCommand(ctx, []string{"stop", "--name", name}); err != nil {
					return err
				}
			}
			deleteDrive, err := promptYesNo(ctx, reader, "Delete stale Drive mailbox objects now", false)
			if err != nil {
				return err
			}
			if deleteDrive {
				if err := cleanup(ctx, []string{"--config", config, "--older-than", "0s", "--delete"}); err != nil {
					return err
				}
			}
			revokeOAuth, err := promptYesNo(ctx, reader, "Revoke Google OAuth token and invalidate generated configs", false)
			if err != nil {
				return err
			}
			if revokeOAuth {
				if err := revoke(ctx, []string{"--config", config, "--revoke-oauth"}); err != nil {
					return err
				}
			}
			deleteLocal, err := promptYesNo(ctx, reader, "Delete local kit directory", false)
			if err != nil {
				return err
			}
			if deleteLocal {
				if err := deleteKitDirectory(config); err != nil {
					return err
				}
			}
		case "8":
			usage()
		case "0", "q", "quit", "exit":
			return nil
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func createGoogleKitFromMenu(ctx context.Context, reader *bufio.Reader, oauthMode string) error {
	out, err := prompt(ctx, reader, "Output directory", "skirk-kit")
	if err != nil {
		return err
	}
	title, err := prompt(ctx, reader, "Kit title", "")
	if err != nil {
		return err
	}
	args := []string{"--out", out}
	if title != "" {
		args = append(args, "--title", title)
	}
	reset, err := promptYesNo(ctx, reader, "Reset Google login before setup", true)
	if err != nil {
		return err
	}
	if reset {
		args = append(args, "--reset-google-login")
	}
	switch oauthMode {
	case "easy":
		fmt.Println("Google OAuth: easy Skirk OAuth client")
	case "personal":
		fmt.Println("Google OAuth: personal Google OAuth project")
		oauthFile, err := prompt(ctx, reader, "OAuth client JSON", "oauth-client.json")
		if err != nil {
			return err
		}
		args = append(args, "--oauth-client-file", oauthFile)
	default:
		return fmt.Errorf("unknown OAuth mode %q", oauthMode)
	}
	startExit, err := promptYesNo(ctx, reader, "Install and start exit service after setup", runtime.GOOS == "linux")
	if err != nil {
		return err
	}
	if !startExit {
		args = append(args, "--start-exit=false")
	}
	return setupInit(ctx, args)
}

func serviceMenu(ctx context.Context, reader *bufio.Reader) error {
	fmt.Println()
	fmt.Println("1. Install or update exit service")
	fmt.Println("2. Service status")
	fmt.Println("3. Start service")
	fmt.Println("4. Stop service")
	fmt.Println("5. Restart service")
	fmt.Println("6. Uninstall service")
	fmt.Println("0. Back")
	choice, err := prompt(ctx, reader, "Service action", "1")
	if err != nil {
		return err
	}
	if choice == "0" || strings.EqualFold(choice, "back") {
		return nil
	}
	name, err := prompt(ctx, reader, "Service name", defaultServiceName)
	if err != nil {
		return err
	}
	switch choice {
	case "1":
		config, err := prompt(ctx, reader, "Exit config", "skirk-kit/exit.json")
		if err != nil {
			return err
		}
		user, err := prompt(ctx, reader, "Run service as user (blank=current user)", "")
		if err != nil {
			return err
		}
		args := []string{"install", "--name", name, "--config", config}
		if strings.TrimSpace(user) != "" {
			args = append(args, "--user", user)
		}
		return serviceCommand(ctx, args)
	case "2":
		return serviceCommand(ctx, []string{"status", "--name", name})
	case "3":
		return serviceCommand(ctx, []string{"start", "--name", name})
	case "4":
		return serviceCommand(ctx, []string{"stop", "--name", name})
	case "5":
		return serviceCommand(ctx, []string{"restart", "--name", name})
	case "6":
		confirm, err := promptYesNo(ctx, reader, "Uninstall service", false)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		return serviceCommand(ctx, []string{"uninstall", "--name", name})
	default:
		return fmt.Errorf("unknown service action %q", choice)
	}
}

func promptYesNo(ctx context.Context, reader *bufio.Reader, label string, fallback bool) (bool, error) {
	fallbackText := "no"
	if fallback {
		fallbackText = "yes"
	}
	for {
		text, err := prompt(ctx, reader, label+" (yes/no)", fallbackText)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Println("Please answer yes or no.")
		}
	}
}

func deleteKitDirectory(configPath string) error {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if filepath.Base(absConfig) != "exit.json" {
		return fmt.Errorf("refusing to delete kit directory: %s is not an exit.json path", absConfig)
	}
	dir := filepath.Dir(absConfig)
	if dir == string(filepath.Separator) || dir == "." {
		return fmt.Errorf("refusing to delete unsafe kit directory %q", dir)
	}
	required := []string{"exit.json", "client.skirk", "client.json"}
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("refusing to delete %s: expected generated kit file %s is missing: %w", dir, name, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("Deleted local kit directory: %s\n", dir)
	return nil
}

const skirkBanner = `             ##################
            ####################
            ####            ####
            ####            ###
            ####
            ####    ####
            #########  ########
            #########  #########
                    ####    ####
                            ####
             ###            ####
            ####            ####
            ####################
             ##################

Skirk
`

func prompt(ctx context.Context, reader *bufio.Reader, label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", label, fallback)
	} else {
		fmt.Printf("%s: ", label)
	}
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := reader.ReadString('\n')
		ch <- result{text: text, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return "", result.err
		}
		text := strings.TrimSpace(result.text)
		if text == "" {
			return fallback, nil
		}
		return text, nil
	}
}
