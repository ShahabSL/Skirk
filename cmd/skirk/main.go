package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"skirk/internal/skirk"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(os.Args); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	defer cancel()
	go func() {
		<-signals
		cancel()
		<-signals
		os.Exit(130)
	}()
	if len(args) < 2 {
		return menu(ctx)
	}
	switch args[1] {
	case "help", "--help", "-h":
		usage()
		return nil
	case "version":
		fmt.Printf("skirk %s commit=%s date=%s\n", version, commit, date)
		return nil
	case "keygen":
		secret, err := skirk.RandomSecret()
		if err != nil {
			return err
		}
		fmt.Println(secret)
		return nil
	case "workspace":
		return workspace(ctx, args[2:])
	case "setup":
		return setup(ctx, args[2:])
	case "revoke":
		return revoke(ctx, args[2:])
	case "config":
		return configCommand(args[2:])
	case "hybrid-send":
		return hybridSend(ctx, args[2:])
	case "hybrid-recv":
		return hybridRecv(ctx, args[2:])
	case "e2e":
		return e2e(ctx, args[2:])
	case "bench":
		return bench(ctx, args[2:])
	case "serve-client":
		return serveClient(ctx, args[2:])
	case "client":
		return serveClient(ctx, args[2:])
	case "client-ui":
		return clientUI(ctx, args[2:])
	case "serve-exit":
		return serveExit(ctx, args[2:])
	case "exit":
		return serveExit(ctx, args[2:])
	case "sample-config":
		return sampleConfig(args[2:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[1])
	}
}

func usage() {
	fmt.Println(`skirk commands:
  help
  version
  keygen
  sample-config --out skirk.json --spreadsheet-id SHEET_ID --secret SECRET
  setup init --out skirk-kit
  config export --config skirk-kit/client.json [--out client.skirk]
  config decode --config client.skirk --out client.json
  revoke --config skirk-kit/exit.json [--revoke-oauth]
  workspace create --config skirk.json --title TITLE --sheet skirk
  workspace delete --config skirk.json --spreadsheet-id SHEET_ID [--delete-drive-folder]
  hybrid-send --config skirk.json --input file.bin [--session SESSION]
  hybrid-recv --config skirk.json --output file.bin --session SESSION [--delete-after]
  e2e --config skirk.json [--bytes 2048] [--delete-after]
  bench --config skirk.json --sizes 8192,65536 --chunk-sizes 8192,65536 [--temp-workspace]
  serve-exit --config skirk.json
  serve-client --config skirk.json [--listen 127.0.0.1:18080]
  client-ui --config skirk.json [--socks 127.0.0.1:18080] [--ui 127.0.0.1:18280]`)
}

func configCommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("config needs export or decode")
	}
	switch args[0] {
	case "export":
		fs := flag.NewFlagSet("config export", flag.ExitOnError)
		configPath := fs.String("config", "skirk-kit/client.json", "config path or inline config text")
		out := fs.String("out", "", "optional output file for one-line text config")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := skirk.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		text, err := skirk.EncodeConfigText(cfg)
		if err != nil {
			return err
		}
		if strings.TrimSpace(*out) == "" {
			fmt.Println(text)
			return nil
		}
		return os.WriteFile(*out, []byte(text+"\n"), 0600)
	case "decode":
		fs := flag.NewFlagSet("config decode", flag.ExitOnError)
		configText := fs.String("config", "", "config path or inline config text")
		out := fs.String("out", "client.json", "output JSON path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*configText) == "" {
			return fmt.Errorf("--config is required")
		}
		cfg, err := skirk.LoadConfig(*configText)
		if err != nil {
			return err
		}
		return writeJSONFile(*out, cfg)
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func load(path string) (*skirk.Config, *skirk.DriveStore, *skirk.SheetsLog, *skirk.Workspace, error) {
	cfg, err := skirk.LoadConfig(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	drive, sheets, workspace, err := skirk.StoresFromConfig(context.Background(), cfg)
	return cfg, drive, sheets, workspace, err
}

func workspace(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("workspace needs create or delete")
	}
	fs := flag.NewFlagSet("workspace "+args[0], flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	title := fs.String("title", "skirk-workspace", "spreadsheet title")
	sheet := fs.String("sheet", "skirk", "sheet title")
	spreadsheetID := fs.String("spreadsheet-id", "", "spreadsheet id")
	driveFolderID := fs.String("drive-folder-id", "", "Drive folder id")
	deleteDriveFolder := fs.Bool("delete-drive-folder", false, "also delete the Drive folder from config or --drive-folder-id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_, _, _, workspace, err := load(*configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		id, err := workspace.CreateSpreadsheet(ctx, *title, *sheet)
		if err != nil {
			return err
		}
		return printJSON(map[string]string{"spreadsheet_id": id})
	case "delete":
		cfg, err := skirk.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		deleted := map[string]string{}
		id := *spreadsheetID
		if id == "" {
			id = cfg.Sheets.SpreadsheetID
		}
		if id != "" {
			if err := workspace.DeleteSpreadsheet(ctx, id); err != nil {
				return err
			}
			deleted["deleted_spreadsheet_id"] = id
		}
		if *deleteDriveFolder {
			folderID := firstNonEmpty(*driveFolderID, cfg.Drive.FolderID)
			if folderID != "" && folderID != "appDataFolder" {
				if err := workspace.DeleteDriveFile(ctx, folderID); err != nil {
					return err
				}
				deleted["deleted_drive_folder_id"] = folderID
			} else if cfg.Drive.Space == "appDataFolder" {
				deleted["appdata"] = "not deleted; revoke the OAuth app to disconnect appDataFolder access"
			}
		}
		if len(deleted) == 0 {
			deleted["result"] = "nothing to delete"
		}
		return printJSON(deleted)
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func revoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/exit.json", "config path")
	revokeOAuth := fs.Bool("revoke-oauth", false, "also revoke the Google OAuth refresh/access token in this config")
	keepWorkspace := fs.Bool("keep-workspace", false, "do not delete the Sheet and Drive folder")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skirk.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	_, _, workspace, err := skirk.StoresFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	result := map[string]any{"config": *configPath}
	if !*keepWorkspace {
		if cfg.Sheets.SpreadsheetID != "" {
			if err := workspace.DeleteSpreadsheet(ctx, cfg.Sheets.SpreadsheetID); err != nil {
				return err
			}
			result["deleted_spreadsheet_id"] = cfg.Sheets.SpreadsheetID
		}
		if cfg.Drive.FolderID != "" {
			if err := workspace.DeleteDriveFile(ctx, cfg.Drive.FolderID); err != nil {
				return err
			}
			result["deleted_drive_folder_id"] = cfg.Drive.FolderID
		}
	}
	if *revokeOAuth {
		if err := cfg.Auth.Revoke(ctx, cfg.Route); err != nil {
			return err
		}
		result["oauth_revoked"] = true
	}
	return printJSON(result)
}

func hybridSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hybrid-send", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	input := fs.String("input", "", "input file")
	session := fs.String("session", "", "session id")
	chunkSize := fs.Int("chunk-size", 0, "chunk size")
	concurrency := fs.Int("concurrency", 0, "Drive upload concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return fmt.Errorf("--input is required")
	}
	cfg, drive, sheets, _, err := load(*configPath)
	if err != nil {
		return err
	}
	size := cfg.Tunnel.ChunkSize
	if *chunkSize > 0 {
		size = *chunkSize
	}
	workers := cfg.Tunnel.Concurrency
	if *concurrency > 0 {
		workers = *concurrency
	}
	var result skirk.HybridSendResult
	if cfg.Sheets.SpreadsheetID == "" {
		result, err = skirk.HybridSendFile(ctx, drive, controlStore(drive, sheets, cfg), *input, cfg.Secret, firstNonEmpty(*session, cfg.SessionID), skirk.DirectionUp, size, false)
	} else {
		result, err = skirk.HybridSendFileBulk(ctx, drive, sheets, *input, cfg.Secret, firstNonEmpty(*session, cfg.SessionID), skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: size, Concurrency: workers})
	}
	if err != nil {
		return err
	}
	return printJSON(result)
}

func hybridRecv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hybrid-recv", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	output := fs.String("output", "", "output file")
	session := fs.String("session", "", "session id")
	deleteAfter := fs.Bool("delete-after", false, "delete data/control after receive")
	concurrency := fs.Int("concurrency", 0, "Drive download/cleanup concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *output == "" || *session == "" {
		return fmt.Errorf("--output and --session are required")
	}
	cfg, drive, sheets, _, err := load(*configPath)
	if err != nil {
		return err
	}
	workers := cfg.Tunnel.Concurrency
	if *concurrency > 0 {
		workers = *concurrency
	}
	var result skirk.HybridReceiveResult
	if cfg.Sheets.SpreadsheetID == "" {
		result, err = skirk.HybridReceiveFile(ctx, drive, controlStore(drive, sheets, cfg), *output, cfg.Secret, *session, skirk.DirectionUp, *deleteAfter)
	} else {
		result, err = skirk.HybridReceiveFileBulk(ctx, drive, sheets, *output, cfg.Secret, *session, skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: cfg.Tunnel.ChunkSize, Concurrency: workers})
	}
	if err != nil {
		return err
	}
	if *deleteAfter && cfg.Sheets.SpreadsheetID != "" {
		if err := cleanupHybrid(ctx, drive, sheets, result.DriveFileIDs, result.DriveObjects, result.ControlRows, workers); err != nil {
			return err
		}
	}
	return printJSON(result)
}

func e2e(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("e2e", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	byteCount := fs.Int("bytes", 2048, "random payload size")
	deleteAfter := fs.Bool("delete-after", true, "delete data/control after receive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, drive, sheets, _, err := load(*configPath)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "skirk-e2e-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	input := filepath.Join(tmpDir, "input.bin")
	output := filepath.Join(tmpDir, "output.bin")
	payload := make([]byte, *byteCount)
	if _, err := rand.Read(payload); err != nil {
		return err
	}
	if err := os.WriteFile(input, payload, 0600); err != nil {
		return err
	}
	start := time.Now()
	var send skirk.HybridSendResult
	var recv skirk.HybridReceiveResult
	if cfg.Sheets.SpreadsheetID == "" {
		control := controlStore(drive, sheets, cfg)
		send, err = skirk.HybridSendFile(ctx, drive, control, input, cfg.Secret, cfg.SessionID, skirk.DirectionUp, cfg.Tunnel.ChunkSize, false)
		if err == nil {
			recv, err = skirk.HybridReceiveFile(ctx, drive, control, output, cfg.Secret, send.SessionID, skirk.DirectionUp, *deleteAfter)
		}
	} else {
		send, err = skirk.HybridSendFileBulk(ctx, drive, sheets, input, cfg.Secret, cfg.SessionID, skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: cfg.Tunnel.ChunkSize, Concurrency: cfg.Tunnel.Concurrency})
		if err == nil {
			recv, err = skirk.HybridReceiveFileBulk(ctx, drive, sheets, output, cfg.Secret, send.SessionID, skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: cfg.Tunnel.ChunkSize, Concurrency: cfg.Tunnel.Concurrency})
		}
	}
	if err != nil {
		return err
	}
	if *deleteAfter && cfg.Sheets.SpreadsheetID != "" {
		if err := cleanupHybrid(ctx, drive, sheets, recv.DriveFileIDs, recv.DriveObjects, recv.ControlRows, cfg.Tunnel.Concurrency); err != nil {
			return err
		}
	}
	roundtrip, err := os.ReadFile(output)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, roundtrip) {
		return fmt.Errorf("payload mismatch")
	}
	return printJSON(map[string]any{
		"result":         "pass",
		"session_id":     send.SessionID,
		"bytes":          len(payload),
		"send_chunks":    send.Chunks,
		"receive_chunks": recv.Chunks,
		"duration_ms":    time.Since(start).Milliseconds(),
		"delete_after":   *deleteAfter,
		"chunk_size":     cfg.Tunnel.ChunkSize,
		"spreadsheet_id": cfg.Sheets.SpreadsheetID,
		"transport":      driveTransportName(cfg.Drive, cfg.Sheets),
	})
}

type benchCase struct {
	SizeBytes     int     `json:"size_bytes"`
	ChunkSize     int     `json:"chunk_size"`
	Concurrency   int     `json:"concurrency"`
	SendChunks    int     `json:"send_chunks"`
	ReceiveChunks int     `json:"receive_chunks"`
	SendMS        int64   `json:"send_ms"`
	ReceiveMS     int64   `json:"receive_ms"`
	VerifyMS      int64   `json:"verify_ms"`
	CleanupMS     int64   `json:"cleanup_ms"`
	SendMbps      float64 `json:"send_mbps"`
	ReceiveMbps   float64 `json:"receive_mbps"`
	RoundTripMbps float64 `json:"roundtrip_mbps"`
	SessionID     string  `json:"session_id"`
}

func bench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	sizesText := fs.String("sizes", "8192,65536", "comma-separated payload sizes in bytes")
	chunksText := fs.String("chunk-sizes", "8192,65536", "comma-separated chunk sizes in bytes")
	concurrency := fs.Int("concurrency", 0, "Drive upload/download/cleanup concurrency")
	tempWorkspace := fs.Bool("temp-workspace", false, "create and delete a temporary control spreadsheet")
	title := fs.String("title", "skirk-bench", "temporary spreadsheet title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sizes, err := parseIntList(*sizesText)
	if err != nil {
		return err
	}
	chunkSizes, err := parseIntList(*chunksText)
	if err != nil {
		return err
	}
	cfg, drive, sheets, workspace, err := load(*configPath)
	if err != nil {
		return err
	}
	tempSheetID := ""
	if *tempWorkspace {
		tempSheetID, err = workspace.CreateSpreadsheet(ctx, *title, "skirk")
		if err != nil {
			return err
		}
		cfg.Sheets.SpreadsheetID = tempSheetID
		drive, sheets, _, err = skirk.StoresFromConfig(ctx, cfg)
		if err != nil {
			_ = workspace.DeleteSpreadsheet(ctx, tempSheetID)
			return err
		}
		defer workspace.DeleteSpreadsheet(context.Background(), tempSheetID)
	}
	if cfg.Sheets.SpreadsheetID == "" {
		return fmt.Errorf("config.sheets.spreadsheet_id is required unless --temp-workspace is used")
	}
	workers := cfg.Tunnel.Concurrency
	if *concurrency > 0 {
		workers = *concurrency
	}

	tmpDir, err := os.MkdirTemp("", "skirk-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	var cases []benchCase
	for _, size := range sizes {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			return err
		}
		input := filepath.Join(tmpDir, fmt.Sprintf("input-%d.bin", size))
		if err := os.WriteFile(input, payload, 0600); err != nil {
			return err
		}
		for _, chunkSize := range chunkSizes {
			output := filepath.Join(tmpDir, fmt.Sprintf("output-%d-%d.bin", size, chunkSize))
			startSend := time.Now()
			send, err := skirk.HybridSendFileBulk(ctx, drive, sheets, input, cfg.Secret, "", skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: chunkSize, Concurrency: workers})
			sendDuration := time.Since(startSend)
			if err != nil {
				return err
			}

			startReceive := time.Now()
			recv, err := skirk.HybridReceiveFileBulk(ctx, drive, sheets, output, cfg.Secret, send.SessionID, skirk.DirectionUp, skirk.HybridBulkOptions{ChunkSize: chunkSize, Concurrency: workers})
			receiveDuration := time.Since(startReceive)
			if err != nil {
				_ = cleanupHybrid(ctx, drive, sheets, send.DriveFileIDs, send.DriveObjects, send.ControlRows, workers)
				return err
			}

			startVerify := time.Now()
			roundtrip, err := os.ReadFile(output)
			if err != nil {
				_ = cleanupHybrid(ctx, drive, sheets, recv.DriveFileIDs, recv.DriveObjects, recv.ControlRows, workers)
				return err
			}
			if !bytes.Equal(payload, roundtrip) {
				_ = cleanupHybrid(ctx, drive, sheets, recv.DriveFileIDs, recv.DriveObjects, recv.ControlRows, workers)
				return fmt.Errorf("payload mismatch for size=%d chunk=%d", size, chunkSize)
			}
			verifyDuration := time.Since(startVerify)

			startCleanup := time.Now()
			if err := cleanupHybrid(ctx, drive, sheets, recv.DriveFileIDs, recv.DriveObjects, recv.ControlRows, workers); err != nil {
				return err
			}
			cleanupDuration := time.Since(startCleanup)
			sendSeconds := sendDuration.Seconds()
			receiveSeconds := receiveDuration.Seconds()
			roundTripSeconds := sendSeconds + receiveSeconds
			cases = append(cases, benchCase{
				SizeBytes:     size,
				ChunkSize:     chunkSize,
				Concurrency:   workers,
				SendChunks:    send.Chunks,
				ReceiveChunks: recv.Chunks,
				SendMS:        sendDuration.Milliseconds(),
				ReceiveMS:     receiveDuration.Milliseconds(),
				VerifyMS:      verifyDuration.Milliseconds(),
				CleanupMS:     cleanupDuration.Milliseconds(),
				SendMbps:      mbps(size, sendSeconds),
				ReceiveMbps:   mbps(size, receiveSeconds),
				RoundTripMbps: mbps(size, roundTripSeconds),
				SessionID:     send.SessionID,
			})
		}
	}
	return printJSON(map[string]any{
		"result":              "pass",
		"temp_spreadsheet_id": tempSheetID,
		"cases":               cases,
	})
}

func cleanupHybrid(ctx context.Context, drive *skirk.DriveStore, sheets *skirk.SheetsLog, dataIDs, dataObjects, controlRows []string, concurrency int) error {
	if len(dataIDs) > 0 {
		if err := drive.DeleteIDs(ctx, dataIDs, concurrency); err != nil {
			return err
		}
	} else {
		for _, name := range dataObjects {
			if err := drive.Delete(ctx, name); err != nil {
				return err
			}
		}
	}
	return sheets.DeleteMany(ctx, controlRows)
}

func parseIntList(value string) ([]int, error) {
	var result []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q: %w", part, err)
		}
		if parsed <= 0 {
			return nil, fmt.Errorf("sizes must be positive: %d", parsed)
		}
		result = append(result, parsed)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty integer list")
	}
	return result, nil
}

func mbps(bytes int, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(bytes*8) / seconds / 1_000_000
}

func serveClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-client", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	listen := fs.String("listen", "", "SOCKS5 listen address")
	upstreamProxy := fs.String("upstream-proxy", "", "override config route proxy, for example socks5h://127.0.0.1:11093")
	routeMode := fs.String("route-mode", "", "override config route mode: direct, real_pinned, google_front, google_front_pinned")
	googleIP := fs.String("google-ip", "", "override config Google edge IP for pinned route modes")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skirk.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*upstreamProxy) != "" {
		cfg.Route.Proxy = strings.TrimSpace(*upstreamProxy)
	}
	if strings.TrimSpace(*routeMode) != "" {
		cfg.Route.Mode = strings.TrimSpace(*routeMode)
	}
	if strings.TrimSpace(*googleIP) != "" {
		cfg.Route.GoogleIP = strings.TrimSpace(*googleIP)
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	drive, sheets, _, err := skirk.StoresFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	control := controlStore(drive, sheets, cfg)
	tunnel, err := skirk.NewTunnel(drive, control, cfg)
	if err != nil {
		return err
	}
	addr := firstNonEmpty(*listen, cfg.Tunnel.Listen)
	log.Printf("skirk client SOCKS5 listening on %s session=%s route=%s upstream=%s", addr, skirk.SessionString(tunnel.SessionID), cfg.Route.Mode, firstNonEmpty(cfg.Route.Proxy, "none"))
	return tunnel.ServeClient(ctx, addr)
}

func serveExit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-exit", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, drive, sheets, _, err := load(*configPath)
	if err != nil {
		return err
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	control := controlStore(drive, sheets, cfg)
	tunnel, err := skirk.NewTunnel(drive, control, cfg)
	if err != nil {
		return err
	}
	log.Printf("skirk exit polling session=%s", skirk.SessionString(tunnel.SessionID))
	return tunnel.ServeExit(ctx)
}

func applyTunnelOverrides(cfg *skirk.Config, chunkSize, pollMS, concurrency, uploadConcurrency, downloadConcurrency int) error {
	if cfg == nil {
		return nil
	}
	if chunkSize > 0 {
		cfg.Tunnel.ChunkSize = chunkSize
	}
	if pollMS > 0 {
		cfg.Tunnel.PollIntervalMS = pollMS
	}
	if concurrency > 0 {
		cfg.Tunnel.Concurrency = concurrency
		cfg.Tunnel.UploadConcurrency = concurrency
		cfg.Tunnel.DownloadConcurrency = concurrency
	}
	if uploadConcurrency > 0 {
		cfg.Tunnel.UploadConcurrency = uploadConcurrency
	}
	if downloadConcurrency > 0 {
		cfg.Tunnel.DownloadConcurrency = downloadConcurrency
	}
	return cfg.Validate()
}

func controlStore(drive *skirk.DriveStore, sheets *skirk.SheetsLog, cfg *skirk.Config) skirk.BlobStore {
	return drive
}

func sampleConfig(args []string) error {
	fs := flag.NewFlagSet("sample-config", flag.ExitOnError)
	out := fs.String("out", "skirk.json", "output path")
	spreadsheetID := fs.String("spreadsheet-id", "", "spreadsheet id")
	secret := fs.String("secret", "", "secret from keygen")
	session := fs.String("session", "", "fixed 32-hex session id")
	proxy := fs.String("proxy", "socks5h://127.0.0.1:1080", "upstream restricted-network proxy")
	routeMode := fs.String("route-mode", "google_front", "route mode: direct, real_pinned, google_front, google_front_pinned")
	googleIP := fs.String("google-ip", "216.239.38.120", "Google edge IP for pinned routing")
	concurrency := fs.Int("concurrency", 8, "Drive upload/download concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	value := *secret
	if value == "" {
		generated, err := skirk.RandomSecret()
		if err != nil {
			return err
		}
		value = generated
	}
	cfg := skirk.Config{
		Secret:    value,
		SessionID: *session,
		Auth:      skirk.AuthConfig{TokenCommand: "gcloud auth print-access-token"},
		Route:     skirk.RouteConfig{Mode: *routeMode, Proxy: *proxy, GoogleIP: *googleIP, TimeoutSeconds: 240},
		Sheets:    skirk.SheetsConfig{SpreadsheetID: *spreadsheetID, Range: "skirk!A:D"},
		Tunnel:    skirk.TunnelConfig{Listen: "127.0.0.1:18080", Profile: "auto", ChunkSize: 1024 * 1024, PollIntervalMS: 250, Concurrency: *concurrency, CleanupProcessed: true},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(*out, data, 0600)
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ = io.Discard
