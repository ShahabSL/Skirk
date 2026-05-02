package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

func menu(ctx context.Context) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println()
		fmt.Print(skirkBanner)
		fmt.Println("1. Create Google kit")
		fmt.Println("2. Run exit")
		fmt.Println("3. Run client SOCKS")
		fmt.Println("4. Run optional desktop dashboard")
		fmt.Println("5. Revoke/delete kit")
		fmt.Println("6. Show commands")
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
			out, err := prompt(ctx, reader, "Output directory", "skirk-kit")
			if err != nil {
				return err
			}
			title, err := prompt(ctx, reader, "Workspace title", "")
			if err != nil {
				return err
			}
			args := []string{"--out", out}
			if title != "" {
				args = append(args, "--title", title)
			}
			return setupInit(ctx, args)
		case "2":
			config, err := prompt(ctx, reader, "Exit config", "skirk-kit/exit.json")
			if err != nil {
				return err
			}
			return serveExit(ctx, []string{"--config", config})
		case "3":
			config, err := prompt(ctx, reader, "Client config or pasted text", "skirk-kit/client.skirk")
			if err != nil {
				return err
			}
			listen, err := prompt(ctx, reader, "SOCKS listen", "127.0.0.1:18080")
			if err != nil {
				return err
			}
			return serveClient(ctx, []string{"--config", config, "--listen", listen})
		case "4":
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
		case "5":
			config, err := prompt(ctx, reader, "Exit config", "skirk-kit/exit.json")
			if err != nil {
				return err
			}
			revokeOAuth, err := prompt(ctx, reader, "Also revoke Google OAuth token? Type yes to revoke all configs from this login", "no")
			if err != nil {
				return err
			}
			args := []string{"--config", config}
			if strings.EqualFold(revokeOAuth, "yes") {
				args = append(args, "--revoke-oauth")
			}
			return revoke(ctx, args)
		case "6":
			usage()
		case "0", "q", "quit", "exit":
			return nil
		default:
			fmt.Println("Unknown selection")
		}
	}
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
