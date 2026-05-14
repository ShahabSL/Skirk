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
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	case "bench-drive":
		return benchDrive(ctx, args[2:])
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
  bench-drive --config skirk-kit/client.skirk [--mode lifecycle|known-id|range] [--sizes 256K,1M,2M] [--concurrency 4,8,16]
  revoke --config skirk-kit/exit.json [--revoke-oauth]
  serve-exit --config skirk.json [--exit-proxy socks5h://127.0.0.1:40000]
  serve-client --config skirk.json [--listen 127.0.0.1:18080] [--client-id my-device]
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
	prefix := fs.String("prefix", "", "optional mailbox object prefix; defaults to muxv4/<session>/")
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
		cleanupPrefix = "muxv4/" + skirk.SessionString(sid) + "/"
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
	burstPoll := fs.Bool("burst-poll", false, "enable bounded adaptive burst polling after local uploads")
	burstPollMS := fs.Int("burst-poll-ms", 0, "override burst poll interval in milliseconds")
	burstPollWindowMS := fs.Int("burst-poll-window-ms", 0, "override burst poll warm window in milliseconds")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	observe := fs.Bool("observe", false, "enable verbose mux observability logs")
	clientID := fs.String("client-id", "", "stable per-device client id; generated automatically when omitted")
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
	if strings.TrimSpace(*clientID) != "" {
		cfg.Client.ID = strings.TrimSpace(*clientID)
	}
	if strings.TrimSpace(cfg.Client.ID) == "" {
		generated, err := skirk.RandomClientID()
		if err != nil {
			return err
		}
		cfg.Client.ID = generated
	}
	runID, err := skirk.RandomRunID()
	if err != nil {
		return err
	}
	cfg.Client.RunID = runID
	if *observe {
		cfg.Tunnel.Observe = true
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
	if err := cfg.Validate(); err != nil {
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
	log.Printf("skirk client SOCKS5 listening on %s session=%s client=%s run=%s route=%s upstream=%s", addr, skirk.SessionString(tunnel.SessionID), cfg.Client.ID, cfg.Client.RunID, cfg.Route.Mode, firstNonEmpty(cfg.Route.Proxy, "none"))
	errCh := make(chan error, 2)
	go func() { errCh <- tunnel.ServeClient(ctx, addr) }()
	if strings.TrimSpace(*httpProxyListen) != "" {
		log.Printf("skirk client HTTP proxy listening on %s session=%s client=%s run=%s", *httpProxyListen, skirk.SessionString(tunnel.SessionID), cfg.Client.ID, cfg.Client.RunID)
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
	exitIPFamily := fs.String("exit-ip-family", "", "exit target dial family: auto, prefer_ipv4, ipv4_only, prefer_ipv6, or ipv6_only")
	observe := fs.Bool("observe", false, "enable verbose mux observability logs")
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
	if strings.TrimSpace(*exitIPFamily) != "" {
		cfg.Tunnel.ExitIPFamily = strings.TrimSpace(*exitIPFamily)
	}
	if *observe {
		cfg.Tunnel.Observe = true
	}
	if err := applyTunnelOverrides(cfg, *chunkSize, *pollMS, *concurrency, *uploadConcurrency, *downloadConcurrency); err != nil {
		return err
	}
	tunnel, err := skirk.NewTunnel(drive, cfg)
	if err != nil {
		return err
	}
	lock, err := acquireExitLock(tunnel.SessionID)
	if err != nil {
		return err
	}
	defer lock.Close()
	startMailboxJanitor(ctx, drive)
	log.Printf("skirk exit polling session=%s exit_proxy=%s exit_ip_family=%s", skirk.SessionString(tunnel.SessionID), firstNonEmpty(tunnel.ExitProxy, "none"), firstNonEmpty(tunnel.ExitIPFamily, "prefer_ipv4"))
	return tunnel.ServeExit(ctx)
}

const mailboxJanitorDefaultOlderThan = 24 * time.Hour
const mailboxJanitorDefaultInterval = 6 * time.Hour

var mailboxJanitorPrefixes = []string{"muxv4/"}

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

type benchDriveResult struct {
	RouteMode      string                   `json:"route_mode"`
	Prefix         string                   `json:"prefix"`
	StartedUTC     string                   `json:"started_utc"`
	DurationMS     int64                    `json:"duration_ms"`
	VisibilityPoll int64                    `json:"visibility_poll_ms"`
	Matrix         []benchDriveMatrixResult `json:"matrix"`
	Quota          skirk.DriveQuotaSnapshot `json:"quota"`
	QuotaOps       string                   `json:"quota_ops"`
}

type benchDriveMatrixResult struct {
	Mode           string             `json:"mode"`
	SizeBytes      int64              `json:"size_bytes"`
	RangeBytes     int64              `json:"range_bytes,omitempty"`
	Concurrency    int                `json:"concurrency"`
	Objects        int                `json:"objects"`
	SetupMS        int64              `json:"setup_ms,omitempty"`
	Successes      int                `json:"successes"`
	Failures       int                `json:"failures"`
	Bytes          int64              `json:"bytes"`
	DurationMS     int64              `json:"duration_ms"`
	MeanMBps       float64            `json:"mean_MBps"`
	MeanMbps       float64            `json:"mean_mbps"`
	DownloadMbps   float64            `json:"download_mbps"`
	UploadMbps     float64            `json:"upload_mbps,omitempty"`
	P50TotalMS     int64              `json:"p50_total_ms"`
	P95TotalMS     int64              `json:"p95_total_ms"`
	P50UploadMS    int64              `json:"p50_upload_ms"`
	P95UploadMS    int64              `json:"p95_upload_ms"`
	P50VisibleMS   int64              `json:"p50_visible_ms"`
	P95VisibleMS   int64              `json:"p95_visible_ms"`
	P50DownloadMS  int64              `json:"p50_download_ms"`
	P95DownloadMS  int64              `json:"p95_download_ms"`
	P50DeleteMS    int64              `json:"p50_delete_ms"`
	P95DeleteMS    int64              `json:"p95_delete_ms"`
	ListPollsTotal int                `json:"list_polls_total"`
	ListPagesTotal int                `json:"list_pages_total"`
	Samples        []benchDriveSample `json:"samples"`
	Errors         map[string]int     `json:"errors,omitempty"`
}

type benchDriveSample struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	SizeBytes   int64  `json:"size_bytes"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	UploadMS    int64  `json:"upload_ms"`
	VisibleMS   int64  `json:"visible_ms"`
	DownloadMS  int64  `json:"download_ms"`
	DeleteMS    int64  `json:"delete_ms"`
	TotalMS     int64  `json:"total_ms"`
	ListCalls   int    `json:"list_calls"`
	ListPages   int    `json:"list_pages"`
	ListPartial bool   `json:"list_partial"`
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
	burstPoll := fs.Bool("burst-poll", false, "enable bounded adaptive burst polling after local uploads")
	burstPollMS := fs.Int("burst-poll-ms", 0, "override burst poll interval in milliseconds")
	burstPollWindowMS := fs.Int("burst-poll-window-ms", 0, "override burst poll warm window in milliseconds")
	chunkSize := fs.Int("chunk-size", 0, "override tunnel chunk size in bytes")
	pollMS := fs.Int("poll-ms", 0, "override mailbox poll interval in milliseconds")
	concurrency := fs.Int("concurrency", 0, "override Drive upload/download concurrency")
	uploadConcurrency := fs.Int("upload-concurrency", 0, "override Drive upload concurrency")
	downloadConcurrency := fs.Int("download-concurrency", 0, "override Drive download concurrency")
	observe := fs.Bool("observe", false, "enable verbose mux observability logs")
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
	if strings.TrimSpace(cfg.Client.ID) == "" {
		generated, err := skirk.RandomClientID()
		if err != nil {
			return err
		}
		cfg.Client.ID = generated
	}
	runID, err := skirk.RandomRunID()
	if err != nil {
		return err
	}
	cfg.Client.RunID = runID
	if *observe {
		cfg.Tunnel.Observe = true
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

func benchDrive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench-drive", flag.ExitOnError)
	configPath := fs.String("config", "skirk-kit/client.skirk", "config path or inline config text")
	routeMode := fs.String("route-mode", "", "override config route mode")
	googleIP := fs.String("google-ip", "", "override config Google edge IP for pinned route modes")
	sizesValue := fs.String("sizes", "256K,512K,1M,2M,4M", "comma-separated object sizes")
	concurrencyValue := fs.String("concurrency", "4,8,16", "comma-separated Drive lifecycle concurrency levels")
	objects := fs.Int("objects", 32, "objects per size/concurrency matrix cell")
	mode := fs.String("mode", "lifecycle", "benchmark mode: lifecycle, known-id, or range")
	rangeSizeValue := fs.String("range-size", "256K", "byte range size for --mode range")
	visibilityPoll := fs.Duration("visibility-poll", 100*time.Millisecond, "Drive discovery poll interval")
	visibilityTimeout := fs.Duration("visibility-timeout", 30*time.Second, "timeout waiting for files.list discovery")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall benchmark timeout")
	cleanupObjects := fs.Bool("cleanup", true, "delete benchmark objects after each sample")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *objects < 1 {
		return fmt.Errorf("--objects must be at least 1")
	}
	if *visibilityPoll <= 0 {
		return fmt.Errorf("--visibility-poll must be positive")
	}
	if *visibilityTimeout <= 0 {
		return fmt.Errorf("--visibility-timeout must be positive")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	sizes, err := parseSizeList(*sizesValue)
	if err != nil {
		return err
	}
	concurrencyLevels, err := parsePositiveIntList(*concurrencyValue)
	if err != nil {
		return err
	}
	rangeSize, err := parseSizeValue(*rangeSizeValue)
	if err != nil {
		return err
	}
	cfg, err := skirk.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*routeMode) != "" {
		cfg.Route.Mode = strings.TrimSpace(*routeMode)
	}
	if strings.TrimSpace(*googleIP) != "" {
		cfg.Route.GoogleIP = strings.TrimSpace(*googleIP)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	benchCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	drive, err := skirk.StoresFromConfig(benchCtx, cfg)
	if err != nil {
		return err
	}
	drive.ResetTelemetry()
	started := time.Now()
	prefix := fmt.Sprintf("bench-drive/%s/", started.UTC().Format("20060102T150405.000000000Z"))
	result := benchDriveResult{
		RouteMode:      cfg.Route.Mode,
		Prefix:         prefix,
		StartedUTC:     started.UTC().Format(time.RFC3339Nano),
		VisibilityPoll: visibilityPoll.Milliseconds(),
	}
	for _, size := range sizes {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			return err
		}
		for _, concurrency := range concurrencyLevels {
			var matrix benchDriveMatrixResult
			switch strings.TrimSpace(strings.ToLower(*mode)) {
			case "", "lifecycle":
				matrix, err = runBenchDriveMatrix(benchCtx, drive, prefix, payload, concurrency, *objects, *visibilityPoll, *visibilityTimeout, *cleanupObjects)
			case "known-id", "known_id", "download", "known-id-download":
				matrix, err = runBenchDriveKnownIDMatrix(benchCtx, drive, prefix, payload, concurrency, *objects, *cleanupObjects, 0)
			case "range", "known-id-range", "known_id_range":
				matrix, err = runBenchDriveKnownIDMatrix(benchCtx, drive, prefix, payload, concurrency, *objects, *cleanupObjects, rangeSize)
			default:
				err = fmt.Errorf("unknown --mode %q", *mode)
			}
			if err != nil {
				return err
			}
			result.Matrix = append(result.Matrix, matrix)
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	result.Quota = drive.QuotaSnapshot()
	result.QuotaOps = result.Quota.OpSummary()
	return printJSON(result)
}

func runBenchDriveMatrix(ctx context.Context, drive *skirk.DriveStore, prefix string, payload []byte, concurrency, objects int, pollInterval, visibilityTimeout time.Duration, cleanupObjects bool) (benchDriveMatrixResult, error) {
	started := time.Now()
	jobs := make(chan int)
	results := make(chan benchDriveSample, objects)
	var wg sync.WaitGroup
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > objects {
		concurrency = objects
	}
	since := started.UTC().Add(-time.Minute)
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results <- runBenchDriveSample(ctx, drive, prefix, payload, concurrency, index, since, pollInterval, visibilityTimeout, cleanupObjects)
			}
		}()
	}
	for index := 0; index < objects; index++ {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(results)
			return benchDriveMatrixResult{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	matrix := benchDriveMatrixResult{
		Mode:        "lifecycle",
		SizeBytes:   int64(len(payload)),
		Concurrency: concurrency,
		Objects:     objects,
		Errors:      map[string]int{},
	}
	for sample := range results {
		matrix.Samples = append(matrix.Samples, sample)
		matrix.ListPollsTotal += sample.ListCalls
		matrix.ListPagesTotal += sample.ListPages
		if sample.OK {
			matrix.Successes++
			matrix.Bytes += int64(len(payload))
		} else {
			matrix.Failures++
			matrix.Errors[sample.Error]++
		}
	}
	sort.Slice(matrix.Samples, func(i, j int) bool { return matrix.Samples[i].Index < matrix.Samples[j].Index })
	matrix.DurationMS = time.Since(started).Milliseconds()
	if matrix.DurationMS > 0 {
		matrix.MeanMBps = float64(matrix.Bytes) / (float64(matrix.DurationMS) / 1000) / 1_000_000
		matrix.MeanMbps = matrix.MeanMBps * 8
	}
	matrix.DownloadMbps = throughputMbps(matrix.Samples, int64(len(payload)), func(s benchDriveSample) int64 { return s.DownloadMS })
	matrix.UploadMbps = throughputMbps(matrix.Samples, int64(len(payload)), func(s benchDriveSample) int64 { return s.UploadMS })
	matrix.P50TotalMS, matrix.P95TotalMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.TotalMS })
	matrix.P50UploadMS, matrix.P95UploadMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.UploadMS })
	matrix.P50VisibleMS, matrix.P95VisibleMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.VisibleMS })
	matrix.P50DownloadMS, matrix.P95DownloadMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.DownloadMS })
	matrix.P50DeleteMS, matrix.P95DeleteMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.DeleteMS })
	if len(matrix.Errors) == 0 {
		matrix.Errors = nil
	}
	return matrix, nil
}

type benchDriveKnownObject struct {
	index int
	name  string
	id    string
}

func runBenchDriveKnownIDMatrix(ctx context.Context, drive *skirk.DriveStore, prefix string, payload []byte, concurrency, objects int, cleanupObjects bool, rangeBytes int) (benchDriveMatrixResult, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > objects {
		concurrency = objects
	}
	mode := "known-id"
	verifiedBytes := int64(len(payload))
	if rangeBytes > 0 {
		mode = "range"
		if rangeBytes > len(payload) {
			rangeBytes = len(payload)
		}
		verifiedBytes = int64(rangeBytes)
	}
	setupStart := time.Now()
	records := make([]benchDriveKnownObject, objects)
	uploadJobs := make(chan int)
	uploadErrs := make(chan error, objects)
	var uploadWG sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		uploadWG.Add(1)
		go func() {
			defer uploadWG.Done()
			for index := range uploadJobs {
				name := fmt.Sprintf("%sknown-id/%d/%d/%08d.bin", prefix, len(payload), concurrency, index)
				info, err := drive.PutObject(ctx, name, payload)
				if err != nil {
					uploadErrs <- fmt.Errorf("preload %s: %w", name, err)
					continue
				}
				records[index] = benchDriveKnownObject{index: index, name: name, id: info.ID}
			}
		}()
	}
	for index := 0; index < objects; index++ {
		select {
		case uploadJobs <- index:
		case <-ctx.Done():
			close(uploadJobs)
			uploadWG.Wait()
			return benchDriveMatrixResult{}, ctx.Err()
		}
	}
	close(uploadJobs)
	uploadWG.Wait()
	close(uploadErrs)
	var preloadErrors []string
	for err := range uploadErrs {
		if err != nil {
			preloadErrors = append(preloadErrors, cliErrorSummary(err))
		}
	}
	if len(preloadErrors) > 0 {
		if cleanupObjects {
			ids := make([]string, 0, len(records))
			for _, record := range records {
				if record.id != "" {
					ids = append(ids, record.id)
				}
			}
			_ = drive.DeleteIDs(context.Background(), ids, concurrency)
		}
		return benchDriveMatrixResult{}, fmt.Errorf("known-id preload failed: %s", strings.Join(preloadErrors, "; "))
	}
	setupMS := time.Since(setupStart).Milliseconds()
	defer func() {
		if cleanupObjects {
			ids := make([]string, 0, len(records))
			for _, record := range records {
				if record.id != "" {
					ids = append(ids, record.id)
				}
			}
			_ = drive.DeleteIDs(context.Background(), ids, concurrency)
		}
	}()

	started := time.Now()
	downloadJobs := make(chan benchDriveKnownObject)
	results := make(chan benchDriveSample, objects)
	var downloadWG sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		downloadWG.Add(1)
		go func() {
			defer downloadWG.Done()
			for record := range downloadJobs {
				results <- runBenchDriveKnownIDDownload(ctx, drive, record, payload, rangeBytes)
			}
		}()
	}
	for _, record := range records {
		select {
		case downloadJobs <- record:
		case <-ctx.Done():
			close(downloadJobs)
			downloadWG.Wait()
			close(results)
			return benchDriveMatrixResult{}, ctx.Err()
		}
	}
	close(downloadJobs)
	downloadWG.Wait()
	close(results)

	matrix := benchDriveMatrixResult{
		Mode:        mode,
		SizeBytes:   int64(len(payload)),
		RangeBytes:  int64(rangeBytes),
		Concurrency: concurrency,
		Objects:     objects,
		SetupMS:     setupMS,
		Errors:      map[string]int{},
	}
	for sample := range results {
		matrix.Samples = append(matrix.Samples, sample)
		if sample.OK {
			matrix.Successes++
			matrix.Bytes += verifiedBytes
		} else {
			matrix.Failures++
			matrix.Errors[sample.Error]++
		}
	}
	sort.Slice(matrix.Samples, func(i, j int) bool { return matrix.Samples[i].Index < matrix.Samples[j].Index })
	matrix.DurationMS = time.Since(started).Milliseconds()
	if matrix.DurationMS > 0 {
		matrix.MeanMBps = float64(matrix.Bytes) / (float64(matrix.DurationMS) / 1000) / 1_000_000
		matrix.MeanMbps = matrix.MeanMBps * 8
	}
	matrix.DownloadMbps = throughputMbps(matrix.Samples, verifiedBytes, func(s benchDriveSample) int64 { return s.DownloadMS })
	matrix.P50TotalMS, matrix.P95TotalMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.TotalMS })
	matrix.P50DownloadMS, matrix.P95DownloadMS = sampleDurationPercentiles(matrix.Samples, func(s benchDriveSample) int64 { return s.DownloadMS })
	if len(matrix.Errors) == 0 {
		matrix.Errors = nil
	}
	return matrix, nil
}

func runBenchDriveKnownIDDownload(ctx context.Context, drive *skirk.DriveStore, record benchDriveKnownObject, payload []byte, rangeBytes int) (sample benchDriveSample) {
	sample = benchDriveSample{Index: record.index, Name: record.name, SizeBytes: int64(len(payload))}
	started := time.Now()
	defer func() {
		sample.TotalMS = time.Since(started).Milliseconds()
	}()
	downloadStart := time.Now()
	var data []byte
	var err error
	if rangeBytes > 0 {
		if rangeBytes > len(payload) {
			rangeBytes = len(payload)
		}
		sample.SizeBytes = int64(rangeBytes)
		data, _, err = drive.GetRangeByID(ctx, record.id, 0, int64(rangeBytes-1))
	} else {
		data, err = drive.GetByID(ctx, record.id)
	}
	sample.DownloadMS = time.Since(downloadStart).Milliseconds()
	if err != nil {
		sample.Error = "download:" + cliErrorSummary(err)
		return sample
	}
	expectedLen := len(payload)
	if rangeBytes > 0 {
		expectedLen = rangeBytes
	}
	if len(data) != expectedLen {
		sample.Error = fmt.Sprintf("download_size:%d", len(data))
		return sample
	}
	if rangeBytes > 0 {
		if !bytes.Equal(data, payload[:rangeBytes]) {
			sample.Error = "download_mismatch"
			return sample
		}
	} else if !bytes.Equal(data, payload) {
		sample.Error = "download_mismatch"
		return sample
	}
	sample.OK = true
	return sample
}

func throughputMbps(samples []benchDriveSample, bytesPerSample int64, duration func(benchDriveSample) int64) float64 {
	if bytesPerSample <= 0 {
		return 0
	}
	var bytes int64
	var milliseconds int64
	for _, sample := range samples {
		if !sample.OK {
			continue
		}
		ms := duration(sample)
		if ms <= 0 {
			continue
		}
		bytes += bytesPerSample
		milliseconds += ms
	}
	if milliseconds <= 0 {
		return 0
	}
	return float64(bytes*8) / (float64(milliseconds) / 1000) / 1_000_000
}

func runBenchDriveSample(ctx context.Context, drive *skirk.DriveStore, prefix string, payload []byte, concurrency, index int, since time.Time, pollInterval, visibilityTimeout time.Duration, cleanupObjects bool) (sample benchDriveSample) {
	name := fmt.Sprintf("%slifecycle/%d/%d/%08d-%d.bin", prefix, len(payload), concurrency, index, len(payload))
	sample = benchDriveSample{Index: index, Name: name, SizeBytes: int64(len(payload))}
	started := time.Now()
	defer func() {
		sample.TotalMS = time.Since(started).Milliseconds()
	}()
	info, err := drive.PutObject(ctx, name, payload)
	sample.UploadMS = time.Since(started).Milliseconds()
	if err != nil {
		sample.Error = "upload:" + cliErrorSummary(err)
		return sample
	}
	visibleStart := time.Now()
	deadline := time.NewTimer(visibilityTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		sample.ListCalls++
		listInfo, listErr := drive.ListFreshStatus(ctx, prefix, since)
		if listErr != nil {
			sample.Error = "list:" + cliErrorSummary(listErr)
			return sample
		}
		if listInfo.Pages > 0 {
			sample.ListPages += listInfo.Pages
		} else {
			sample.ListPages++
		}
		sample.ListPartial = sample.ListPartial || listInfo.Truncated
		for _, object := range listInfo.Objects {
			if object.ID == info.ID {
				sample.VisibleMS = time.Since(visibleStart).Milliseconds()
				goto visible
			}
		}
		select {
		case <-ctx.Done():
			sample.Error = "context:" + cliErrorSummary(ctx.Err())
			return sample
		case <-deadline.C:
			sample.VisibleMS = time.Since(visibleStart).Milliseconds()
			sample.Error = "visibility_timeout"
			return sample
		case <-ticker.C:
		}
	}
visible:
	downloadStart := time.Now()
	data, err := drive.GetByID(ctx, info.ID)
	sample.DownloadMS = time.Since(downloadStart).Milliseconds()
	if err != nil {
		sample.Error = "download:" + cliErrorSummary(err)
		return sample
	}
	if len(data) != len(payload) {
		sample.Error = fmt.Sprintf("download_size:%d", len(data))
		return sample
	}
	if !bytes.Equal(data, payload) {
		sample.Error = "download_mismatch"
		return sample
	}
	if cleanupObjects {
		deleteStart := time.Now()
		err = drive.DeleteID(ctx, info.ID)
		sample.DeleteMS = time.Since(deleteStart).Milliseconds()
		if err != nil {
			sample.Error = "delete:" + cliErrorSummary(err)
			return sample
		}
	}
	sample.OK = true
	return sample
}

func parseSizeList(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	var out []int
	for _, part := range parts {
		size, err := parseSizeValue(part)
		if err != nil {
			return nil, err
		}
		out = append(out, size)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no sizes configured")
	}
	return out, nil
}

func parseSizeValue(value string) (int, error) {
	value = strings.TrimSpace(strings.ToUpper(value))
	if value == "" {
		return 0, fmt.Errorf("empty size")
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "KIB"):
		multiplier, value = 1024, strings.TrimSuffix(value, "KIB")
	case strings.HasSuffix(value, "KB"):
		multiplier, value = 1000, strings.TrimSuffix(value, "KB")
	case strings.HasSuffix(value, "K"):
		multiplier, value = 1024, strings.TrimSuffix(value, "K")
	case strings.HasSuffix(value, "MIB"):
		multiplier, value = 1024*1024, strings.TrimSuffix(value, "MIB")
	case strings.HasSuffix(value, "MB"):
		multiplier, value = 1000*1000, strings.TrimSuffix(value, "MB")
	case strings.HasSuffix(value, "M"):
		multiplier, value = 1024*1024, strings.TrimSuffix(value, "M")
	case strings.HasSuffix(value, "GIB"):
		multiplier, value = 1024*1024*1024, strings.TrimSuffix(value, "GIB")
	case strings.HasSuffix(value, "GB"):
		multiplier, value = 1000*1000*1000, strings.TrimSuffix(value, "GB")
	case strings.HasSuffix(value, "G"):
		multiplier, value = 1024*1024*1024, strings.TrimSuffix(value, "G")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	if n > int64(^uint(0)>>1)/multiplier {
		return 0, fmt.Errorf("size too large")
	}
	return int(n * multiplier), nil
}

func parsePositiveIntList(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	var out []int
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid positive integer %q", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no positive integers configured")
	}
	return out, nil
}

func sampleDurationPercentiles(samples []benchDriveSample, get func(benchDriveSample) int64) (int64, int64) {
	values := make([]int64, 0, len(samples))
	for _, sample := range samples {
		if sample.OK {
			values = append(values, get(sample))
		}
	}
	if len(values) == 0 {
		return 0, 0
	}
	return percentileMS(values, 0.50), percentileMS(values, 0.95)
}

func cliErrorSummary(err error) string {
	if err == nil {
		return "none"
	}
	text := strings.TrimSpace(err.Error())
	if text == "" {
		return err.Error()
	}
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 240 {
		return text[:240]
	}
	return text
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
