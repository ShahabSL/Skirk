package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ShahabSL/Skirk/internal/skirk"
	"github.com/skip2/go-qrcode"
	"golang.org/x/term"
)

func showProfileCommand(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	path := fs.String("config", defaultKitFile("client.skirk"), "path to the client.skirk profile")
	qrPath := fs.String("qr-out", "", "save a private QR code PNG")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("use --config PATH to select the client profile file")
	}
	if *qrPath == "" {
		return showClientProfile(os.Stdout, *path)
	}
	profile, err := readShareableProfile(*path)
	if err != nil {
		return err
	}
	qr, err := qrcode.New(profile, qrcode.Low)
	if err != nil {
		return errors.New("the profile is too large for one QR code")
	}
	png, err := qr.PNG(-4)
	if err != nil {
		return err
	}
	if err := writePrivateConfigOutput(*qrPath, png); err != nil {
		return err
	}
	fmt.Printf("Saved the profile QR code to %s. Keep it private.\n", *qrPath)
	return nil
}

func readShareableProfile(path string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(path), skirk.ConfigTextPrefix) {
		return "", errors.New("select the client.skirk file by its path")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Match the limit used by config decode for encoded profile input.
	const maxProfileSize = 8 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxProfileSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxProfileSize {
		return "", errors.New("profile input exceeds 8 MiB")
	}
	profile := strings.TrimSpace(string(data))
	normalized, ok := skirk.NormalizeConfigText(profile)
	if !ok || profile != normalized {
		return "", errors.New("select a client.skirk file containing one complete skirk: profile")
	}
	if _, err := skirk.DecodeConfigText(profile); err != nil {
		return "", errors.New("the client profile is invalid. Choose a client.skirk file from a generated kit")
	}
	return profile, nil
}

func showClientProfile(stdout *os.File, path string) error {
	profile, err := readShareableProfile(path)
	if err != nil {
		return err
	}
	columns, rows, _ := term.GetSize(int(stdout.Fd()))
	return printClientProfile(stdout, profile, columns, rows)
}

func printClientProfile(out io.Writer, profile string, columns, rows int) error {
	var panel strings.Builder
	fmt.Fprintf(&panel, "\nClient profile\nKeep this text and QR code private.\n\n%s\n\n", profile)

	qr, err := qrcode.New(profile, qrcode.Low)
	if err != nil {
		panel.WriteString("This profile is too large for one QR code. Copy the text above.\n")
	} else {
		// Pair vertical modules to fit normal terminal cell proportions. The
		// encoder retains the white border required by camera scanners.
		width := len(qr.Bitmap())
		height := (width + 1) / 2
		if (columns > 0 && columns < width) || (rows > 0 && rows < height+2) {
			fmt.Fprintf(&panel, "Zoom out or resize the terminal to at least %d columns by %d rows, then show the profile again to scan.\n", width, height+2)
		} else {
			panel.WriteString("Open Skirk on your phone and choose Scan QR code.\n")
			// Force dark modules on a light background in either terminal theme.
			for _, row := range strings.Split(strings.TrimSuffix(qr.ToSmallString(true), "\n"), "\n") {
				fmt.Fprintf(&panel, "\x1b[30;107m%s\x1b[0m\n", row)
			}
		}
	}
	_, err = io.WriteString(out, panel.String())
	return err
}

func showClientProfileFromMenu(ctx context.Context, reader *bufio.Reader, path string) error {
	if err := showClientProfile(os.Stdout, path); err != nil {
		fmt.Printf("Couldn't open the client profile: %v\n", err)
		return nil
	}
	_, err := prompt(ctx, reader, "Press Enter to return", "")
	return err
}
