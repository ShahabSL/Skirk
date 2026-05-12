package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"sort"
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
	case "setup":
		return setup(ctx, args[2:])
	case "revoke":
		return revoke(ctx, args[2:])
	case "cleanup":
		return cleanup(ctx, args[2:])
	case "config":
		return configCommand(args[2:])
	case "bench-live":
		return benchLive(ctx, args[2:])
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
  cleanup --config skirk-kit/exit.json --older-than 2h [--delete]
  bench-live --config skirk-kit/client.skirk [--small-url http://example.com/] [--bulk-url URL]
  revoke --config skirk-kit/exit.json [--revoke-oauth]
  serve-exit --config skirk.json [--exit-proxy socks5h://127.0.0.1:40000]
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

func cleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/exit.json", "config path")
	prefix := fs.String("prefix", "", "optional mailbox object prefix; defaults to muxv3/<session>/")
	olderThan := fs.Duration("older-than", 2*time.Hour, "delete/list objects older than this duration")
	deleteObjects := fs.Bool("delete", false, "actually delete matched objects; default is dry-run")
	concurrency := fs.Int("concurrency", 4, "delete concurrency")
	maxPages := fs.Int("max-pages", 256, "maximum Drive list pages to scan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, drive, err := load(*configPath)
	if err != nil {
		return err
	}
	cleanupPrefix := strings.TrimSpace(*prefix)
	if cleanupPrefix == "" {
		if strings.TrimSpace(cfg.SessionID) == "" {
			return fmt.Errorf("config session_id is required when --prefix is not set")
		}
		sid, err := skirk.ParseSessionID(cfg.SessionID)
		if err != nil {
			return err
		}
		cleanupPrefix = "muxv3/" + skirk.SessionString(sid) + "/"
	}
	result, err := drive.Cleanup(ctx, skirk.DriveCleanupOptions{
		Prefix:            cleanupPrefix,
		OlderThan:         *olderThan,
		DryRun:            !*deleteObjects,
		DeleteConcurrency: *concurrency,
		MaxPages:          *maxPages,
	})
	if err != nil {
		return err
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
	burstPoll := fs.Bool("burst-poll", false, "enable bounded adaptive burst polling experiment")
	burstPollMS := fs.Int("burst-poll-ms", 0, "override burst poll interval in milliseconds")
	burstPollWindowMS := fs.Int("burst-poll-window-ms", 0, "override burst poll warm window in milliseconds")
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
	if *burstPoll {
		cfg.Tunnel.BurstPoll = true
	}
	if *burstPollMS > 0 {
		cfg.Tunnel.BurstPollMS = *burstPollMS
	}
	if *burstPollWindowMS > 0 {
		cfg.Tunnel.BurstPollWindowMS = *burstPollWindowMS
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
	exitProxy := fs.String("exit-proxy", "", "optional outbound proxy for exit traffic, for example socks5h://127.0.0.1:40000")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, drive, err := load(*configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*exitProxy) != "" {
		cfg.Tunnel.ExitProxy = strings.TrimSpace(*exitProxy)
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	startMailboxJanitor(ctx, drive)
	tunnel, err := skirk.NewTunnel(drive, cfg)
	if err != nil {
		return err
	}
	log.Printf("skirk exit polling session=%s exit_proxy=%s", skirk.SessionString(tunnel.SessionID), firstNonEmpty(tunnel.ExitProxy, "none"))
	return tunnel.ServeExit(ctx)
}

const mailboxJanitorDefaultOlderThan = 24 * time.Hour
const mailboxJanitorDefaultInterval = 6 * time.Hour

var mailboxJanitorPrefixes = []string{"muxv3/", "control/", "data/"}

func startMailboxJanitor(ctx context.Context, drive *skirk.DriveStore) {
	if drive == nil || envBool("SKIRK_DISABLE_JANITOR") {
		return
	}
	olderThan := envDuration("SKIRK_JANITOR_OLDER_THAN", mailboxJanitorDefaultOlderThan)
	interval := envDuration("SKIRK_JANITOR_INTERVAL", mailboxJanitorDefaultInterval)
	go func() {
		runMailboxJanitor(ctx, drive, olderThan)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runMailboxJanitor(ctx, drive, olderThan)
			}
		}
	}()
}

func runMailboxJanitor(ctx context.Context, drive *skirk.DriveStore, olderThan time.Duration) {
	if drive == nil || olderThan <= 0 {
		return
	}
	for _, prefix := range mailboxJanitorPrefixes {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		result, err := drive.Cleanup(cleanupCtx, skirk.DriveCleanupOptions{
			Prefix:            prefix,
			OlderThan:         olderThan,
			DeleteConcurrency: 4,
			MaxPages:          1000,
		})
		cancel()
		if err != nil {
			log.Printf("mailbox janitor prefix=%s older_than=%s error=%s", prefix, olderThan, err)
			continue
		}
		if result.Matched > 0 || result.Deleted > 0 || result.Failed > 0 {
			log.Printf("mailbox janitor prefix=%s older_than=%s scanned=%d matched=%d deleted=%d failed=%d bytes=%d",
				prefix, olderThan, result.Scanned, result.Matched, result.Deleted, result.Failed, result.MatchedSize)
		}
	}
}

type benchHTTPResult struct {
	URL         string  `json:"url"`
	Status      int     `json:"status"`
	Bytes       int64   `json:"bytes"`
	TTFBMS      int64   `json:"ttfb_ms"`
	TotalMS     int64   `json:"total_ms"`
	Mbps        float64 `json:"mbps"`
	ContentType string  `json:"content_type,omitempty"`
}

type benchHTTPSummary struct {
	Samples      int     `json:"samples"`
	Successes    int     `json:"successes"`
	Bytes        int64   `json:"bytes"`
	P50TTFBMS    int64   `json:"p50_ttfb_ms"`
	P95TTFBMS    int64   `json:"p95_ttfb_ms"`
	P50TotalMS   int64   `json:"p50_total_ms"`
	P95TotalMS   int64   `json:"p95_total_ms"`
	MeanMbps     float64 `json:"mean_mbps"`
	PeakMbps     float64 `json:"peak_mbps"`
	LastHTTPCode int     `json:"last_http_code"`
}

type benchLiveResult struct {
	Listen          string                                `json:"listen"`
	RouteMode       string                                `json:"route_mode"`
	UpstreamProxy   string                                `json:"upstream_proxy,omitempty"`
	DurationSeconds float64                               `json:"duration_seconds"`
	Small           benchHTTPSummary                      `json:"small"`
	Bulk            *benchHTTPSummary                     `json:"bulk,omitempty"`
	Quota           skirk.DriveQuotaSnapshot              `json:"quota"`
	QuotaPerMinute  benchQuotaMinuteSummary               `json:"quota_per_minute"`
	QuotaPerRequest benchQuotaRequestSummary              `json:"quota_per_request"`
	DriveOps        map[string]skirk.DriveQuotaOpSnapshot `json:"drive_ops"`
	QuotaOps        string                                `json:"quota_ops"`
}

type benchQuotaMinuteSummary struct {
	Calls         float64 `json:"calls"`
	Units         float64 `json:"units"`
	Errors        float64 `json:"errors"`
	ResponseBytes float64 `json:"response_bytes"`
}

type benchQuotaRequestSummary struct {
	Calls         float64 `json:"calls"`
	Units         float64 `json:"units"`
	Errors        float64 `json:"errors"`
	ResponseBytes float64 `json:"response_bytes"`
}

func benchLive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench-live", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/client.skirk", "config path or inline config text")
	listen := fs.String("listen", "127.0.0.1:0", "temporary SOCKS5 listen address")
	smallURL := fs.String("small-url", "http://example.com/", "small request URL")
	bulkURL := fs.String("bulk-url", "", "optional bulk request URL")
	samples := fs.Int("samples", 3, "small request samples")
	timeout := fs.Duration("timeout", 180*time.Second, "per-request timeout")
	upstreamProxy := fs.String("upstream-proxy", "", "override config route proxy, for example socks5h://127.0.0.1:11093")
	routeMode := fs.String("route-mode", "", "override config route mode")
	googleIP := fs.String("google-ip", "", "override config Google edge IP for pinned route modes")
	burstPoll := fs.Bool("burst-poll", false, "enable bounded adaptive burst polling experiment")
	burstPollMS := fs.Int("burst-poll-ms", 0, "override burst poll interval in milliseconds")
	burstPollWindowMS := fs.Int("burst-poll-window-ms", 0, "override burst poll warm window in milliseconds")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *samples < 1 {
		return fmt.Errorf("--samples must be at least 1")
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
	if *burstPoll {
		cfg.Tunnel.BurstPoll = true
	}
	if *burstPollMS > 0 {
		cfg.Tunnel.BurstPollMS = *burstPollMS
	}
	if *burstPollWindowMS > 0 {
		cfg.Tunnel.BurstPollWindowMS = *burstPollWindowMS
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	addr, err := benchListenAddress(*listen)
	if err != nil {
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
	benchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- tunnel.ServeClient(benchCtx, addr) }()
	if err := waitForTCP(ctx, addr, errCh); err != nil {
		return err
	}
	drive.ResetTelemetry()
	started := time.Now()
	smallSamples, err := runHTTPSamples(ctx, addr, strings.TrimSpace(*smallURL), *samples, *timeout)
	if err != nil {
		return err
	}
	var bulkSummary *benchHTTPSummary
	if strings.TrimSpace(*bulkURL) != "" {
		bulkSamples, err := runHTTPSamples(ctx, addr, strings.TrimSpace(*bulkURL), 1, *timeout)
		if err != nil {
			return err
		}
		summary := summarizeHTTPSamples(bulkSamples)
		bulkSummary = &summary
	}
	duration := time.Since(started)
	quota := drive.QuotaSnapshot()
	totalRequests := len(smallSamples)
	if bulkSummary != nil {
		totalRequests++
	}
	return printJSON(benchLiveResult{
		Listen:          addr,
		RouteMode:       cfg.Route.Mode,
		UpstreamProxy:   cfg.Route.Proxy,
		DurationSeconds: duration.Seconds(),
		Small:           summarizeHTTPSamples(smallSamples),
		Bulk:            bulkSummary,
		Quota:           quota,
		QuotaPerMinute:  quotaPerMinute(quota, duration),
		QuotaPerRequest: quotaPerRequest(quota, totalRequests),
		DriveOps:        quota.Ops,
		QuotaOps:        quota.OpSummary(),
	})
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

func benchListenAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", err
	}
	if port != "0" {
		return value, nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

func waitForTCP(ctx context.Context, addr string, errCh <-chan error) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err == nil {
				return fmt.Errorf("client listener exited before accepting connections")
			}
			return fmt.Errorf("client listener exited before accepting connections: %w", err)
		case <-deadline.C:
			return fmt.Errorf("client listener did not become ready on %s", addr)
		case <-ticker.C:
		}
	}
}

func runHTTPSamples(ctx context.Context, socksAddr, rawURL string, samples int, timeout time.Duration) ([]benchHTTPResult, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("benchmark URL is required")
	}
	results := make([]benchHTTPResult, 0, samples)
	for i := 0; i < samples; i++ {
		sample, err := runHTTPSample(ctx, socksAddr, rawURL, timeout)
		if err != nil {
			return results, err
		}
		results = append(results, sample)
	}
	return results, nil
}

func runHTTPSample(ctx context.Context, socksAddr, rawURL string, timeout time.Duration) (benchHTTPResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network != "tcp" {
				return nil, fmt.Errorf("unsupported network %q", network)
			}
			return skirk.DialViaSOCKS5(ctx, "socks5h://"+socksAddr, addr)
		},
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   45 * time.Second,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       10 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return benchHTTPResult{}, err
	}
	started := time.Now()
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			firstByte = time.Now()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	if err != nil {
		return benchHTTPResult{URL: rawURL, TotalMS: time.Since(started).Milliseconds()}, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	total := time.Since(started)
	if err != nil {
		return benchHTTPResult{URL: rawURL, Status: resp.StatusCode, Bytes: n, TotalMS: total.Milliseconds()}, err
	}
	ttfb := total
	if !firstByte.IsZero() {
		ttfb = firstByte.Sub(started)
	}
	return benchHTTPResult{
		URL:         rawURL,
		Status:      resp.StatusCode,
		Bytes:       n,
		TTFBMS:      ttfb.Milliseconds(),
		TotalMS:     total.Milliseconds(),
		Mbps:        mbps(n, total),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func summarizeHTTPSamples(samples []benchHTTPResult) benchHTTPSummary {
	summary := benchHTTPSummary{Samples: len(samples)}
	if len(samples) == 0 {
		return summary
	}
	ttfb := make([]int64, 0, len(samples))
	total := make([]int64, 0, len(samples))
	var mbpsSum float64
	for _, sample := range samples {
		summary.Bytes += sample.Bytes
		if sample.Status >= 200 && sample.Status < 400 {
			summary.Successes++
		}
		ttfb = append(ttfb, sample.TTFBMS)
		total = append(total, sample.TotalMS)
		mbpsSum += sample.Mbps
		if sample.Mbps > summary.PeakMbps {
			summary.PeakMbps = sample.Mbps
		}
		summary.LastHTTPCode = sample.Status
	}
	summary.P50TTFBMS = percentileMS(ttfb, 0.50)
	summary.P95TTFBMS = percentileMS(ttfb, 0.95)
	summary.P50TotalMS = percentileMS(total, 0.50)
	summary.P95TotalMS = percentileMS(total, 0.95)
	summary.MeanMbps = mbpsSum / float64(len(samples))
	return summary
}

func percentileMS(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if len(values) == 1 {
		return values[0]
	}
	if p <= 0 {
		return values[0]
	}
	if p >= 1 {
		return values[len(values)-1]
	}
	index := int(p * float64(len(values)))
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func mbps(bytes int64, duration time.Duration) float64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return (float64(bytes) * 8) / duration.Seconds() / 1_000_000
}

func quotaPerMinute(snapshot skirk.DriveQuotaSnapshot, duration time.Duration) benchQuotaMinuteSummary {
	if duration <= 0 {
		return benchQuotaMinuteSummary{}
	}
	scale := 60 / duration.Seconds()
	return benchQuotaMinuteSummary{
		Calls:         float64(snapshot.Calls) * scale,
		Units:         float64(snapshot.Units) * scale,
		Errors:        float64(snapshot.Errors) * scale,
		ResponseBytes: float64(snapshot.ResponseBytes) * scale,
	}
}

func quotaPerRequest(snapshot skirk.DriveQuotaSnapshot, requests int) benchQuotaRequestSummary {
	if requests <= 0 {
		return benchQuotaRequestSummary{}
	}
	scale := float64(requests)
	return benchQuotaRequestSummary{
		Calls:         float64(snapshot.Calls) / scale,
		Units:         float64(snapshot.Units) / scale,
		Errors:        float64(snapshot.Errors) / scale,
		ResponseBytes: float64(snapshot.ResponseBytes) / scale,
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
