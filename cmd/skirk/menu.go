package main

import (
	"bufio"
	"context"
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
		choice := prompt(reader, "Select", "1")
		switch choice {
		case "1":
			out := prompt(reader, "Output directory", "skirk-kit")
			title := prompt(reader, "Workspace title", "")
			args := []string{"--out", out}
			if title != "" {
				args = append(args, "--title", title)
			}
			return setupInit(ctx, args)
		case "2":
			config := prompt(reader, "Exit config", "skirk-kit/exit.json")
			return serveExit(ctx, []string{"--config", config})
		case "3":
			config := prompt(reader, "Client config", "skirk-kit/client.json")
			listen := prompt(reader, "SOCKS listen", "127.0.0.1:18080")
			return serveClient(ctx, []string{"--config", config, "--listen", listen})
		case "4":
			config := prompt(reader, "Client config", "skirk-kit/client.json")
			socks := prompt(reader, "SOCKS listen", "127.0.0.1:18080")
			ui := prompt(reader, "UI listen", "127.0.0.1:18280")
			return clientUI(ctx, []string{"--config", config, "--socks", socks, "--ui", ui})
		case "5":
			config := prompt(reader, "Exit config", "skirk-kit/exit.json")
			revokeOAuth := prompt(reader, "Also revoke Google OAuth token? Type yes to revoke all configs from this login", "no")
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

func prompt(reader *bufio.Reader, label, fallback string) string {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", label, fallback)
	} else {
		fmt.Printf("%s: ", label)
	}
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	if text == "" {
		return fallback
	}
	return text
}
