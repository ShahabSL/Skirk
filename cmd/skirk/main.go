package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

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
	case "setup":
		return setup(ctx, args[2:])
	case "revoke":
		return revoke(ctx, args[2:])
	case "config":
		return configCommand(args[2:])
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
  sample-config --out skirk.json --secret SECRET
  setup init --out skirk-kit
  config export --config skirk-kit/client.json [--out client.skirk]
  config decode --config client.skirk --out client.json
  revoke --config skirk-kit/exit.json [--revoke-oauth]
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

func load(path string) (*skirk.Config, *skirk.DriveStore, error) {
	cfg, err := skirk.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	drive, err := skirk.StoresFromConfig(context.Background(), cfg)
	return cfg, drive, err
}

func revoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/exit.json", "config path")
	revokeOAuth := fs.Bool("revoke-oauth", false, "also revoke the Google OAuth refresh/access token in this config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := skirk.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	result := map[string]any{"config": *configPath}
	if *revokeOAuth {
		if err := cfg.Auth.Revoke(ctx, cfg.Route); err != nil {
			return err
		}
		result["oauth_revoked"] = true
	}
	return printJSON(result)
}

func serveClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-client", flag.ExitOnError)
	configPath := fs.String("config", "skirk.json", "config path")
	listen := fs.String("listen", "", "SOCKS5 listen address")
	httpProxyListen := fs.String("http-proxy-listen", "", "optional HTTP/HTTPS proxy listen address")
	upstreamProxy := fs.String("upstream-proxy", "", "override config route proxy, for example socks5h://127.0.0.1:11093")
	routeMode := fs.String("route-mode", "", "override config route mode: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
	googleIP := fs.String("google-ip", "", "override config Google edge IP for pinned route modes")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	watchParentPID := fs.Int("watch-parent-pid", 0, "exit when this parent process disappears")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *watchParentPID > 0 {
		enableParentDeathSignal()
		watchParentProcess(ctx, *watchParentPID, cancel)
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
	drive, err := skirk.StoresFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	tunnel, err := skirk.NewTunnel(drive, cfg)
	if err != nil {
		return err
	}
	addr := firstNonEmpty(*listen, cfg.Tunnel.Listen)
	log.Printf("skirk client SOCKS5 listening on %s session=%s route=%s upstream=%s", addr, skirk.SessionString(tunnel.SessionID), cfg.Route.Mode, firstNonEmpty(cfg.Route.Proxy, "none"))
	errCh := make(chan error, 2)
	go func() { errCh <- tunnel.ServeClient(ctx, addr) }()
	if strings.TrimSpace(*httpProxyListen) != "" {
		log.Printf("skirk client HTTP proxy listening on %s session=%s", *httpProxyListen, skirk.SessionString(tunnel.SessionID))
		go func() { errCh <- tunnel.ServeHTTPProxyClient(ctx, strings.TrimSpace(*httpProxyListen)) }()
	}
	return <-errCh
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
	cfg, drive, err := load(*configPath)
	if err != nil {
		return err
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	tunnel, err := skirk.NewTunnel(drive, cfg)
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

func sampleConfig(args []string) error {
	fs := flag.NewFlagSet("sample-config", flag.ExitOnError)
	out := fs.String("out", "skirk.json", "output path")
	secret := fs.String("secret", "", "secret from keygen")
	session := fs.String("session", "", "fixed 32-hex session id")
	proxy := fs.String("proxy", "socks5h://127.0.0.1:1080", "upstream restricted-network proxy")
	routeMode := fs.String("route-mode", "google_front", "route mode: direct, real_pinned, google_front, google_front_pinned, google_front_h1, google_front_h1_pinned")
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
		Drive:     skirk.DriveConfig{Space: "appDataFolder"},
		Tunnel:    skirk.TunnelConfig{Listen: "127.0.0.1:18080", Profile: "auto", ChunkSize: 8 * 1024 * 1024, PollIntervalMS: 250, Concurrency: *concurrency, CleanupProcessed: true},
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
