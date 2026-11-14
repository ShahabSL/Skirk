// Bounded authenticated HTTP/2 vs HTTP/3 Google-front reachability probe.
// It reads one existing test-folder ID; it creates or modifies no Drive files.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type sample struct {
	Protocol  string  `json:"protocol"`
	WireProto string  `json:"wire_proto,omitempty"`
	Status    int     `json:"status"`
	TTFBMS    float64 `json:"ttfb_ms"`
	TotalMS   float64 `json:"total_ms"`
	ExactID   bool    `json:"exact_id"`
	OK        bool    `json:"ok"`
}

func main() {
	config := flag.String("config", "", "private isolated test config")
	out := flag.String("out", "", "private output JSON (must not exist)")
	live := flag.Bool("live", false, "authorize eight read-only Drive metadata calls")
	nativeH3Only := flag.Bool("native-h3-only", false, "make one authenticated read using Skirk's Go HTTP/3 transport")
	flag.Parse()
	if !*live || *config == "" || *out == "" {
		panic("requires --live, --config and --out")
	}
	if _, err := os.Stat(*out); !os.IsNotExist(err) {
		panic("output already exists or inaccessible")
	}
	cfg, err := skirk.LoadConfig(*config)
	if err != nil || cfg.Drive.FolderID == "" || cfg.Drive.Space == "appDataFolder" || cfg.Route.Mode != "google_front_pinned" || cfg.Route.GoogleIP == "" {
		panic("requires isolated visible test folder and pinned Google front")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	token, err := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route).Token(ctx)
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`).MatchString(token) {
		panic("cannot obtain safe test token")
	}
	uri := "https://www.google.com/drive/v3/files/" + url.PathEscape(cfg.Drive.FolderID) + "?fields=id"
	if *nativeH3Only {
		row := runNativeH3(ctx, cfg, token)
		writeSamples(*out, "google_front_h3_pinned", []sample{row}, 1, "Skirk Go transport; HTTP/3 wire protocol required")
		fmt.Printf("protocol=%s wire_proto=%s status=%d total_ms=%.3f exact_id=%t\n", row.Protocol, row.WireProto, row.Status, row.TotalMS, row.ExactID)
		if !row.OK {
			panic("native HTTP/3 read did not complete over HTTP/3 with exact folder ID")
		}
		return
	}
	sequence := []string{"h2", "h3", "h3", "h2", "h3", "h2", "h2", "h3"}
	rows := make([]sample, 0, len(sequence))
	for _, protocol := range sequence {
		row := run(ctx, cfg, token, uri, protocol)
		rows = append(rows, row)
		fmt.Printf("protocol=%s status=%d ttfb_ms=%.3f total_ms=%.3f exact_id=%t\n", row.Protocol, row.Status, row.TTFBMS, row.TotalMS, row.ExactID)
		if !row.OK {
			break
		}
	}
	writeSamples(*out, cfg.Route.Mode, rows, 8, "fresh curl connection per call; not persistent Skirk throughput")
}

func writeSamples(out, route string, rows []sample, maxRequests int, note string) {
	encoded, _ := json.MarshalIndent(map[string]any{"route": route, "samples": rows, "expected_max_requests": maxRequests, "note": note}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o600); err != nil {
		panic(err)
	}
}

func runNativeH3(parent context.Context, cfg *skirk.Config, token string) sample {
	row := sample{Protocol: "go-h3"}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	route := cfg.Route
	route.Mode = "google_front_h3_pinned"
	client := skirk.NewGoogleHTTPClient(route)
	headers := map[string]string{"Authorization": "Bearer " + token}
	if cfg.Drive.QuotaProject != "" {
		headers["X-Goog-User-Project"] = cfg.Drive.QuotaProject
	}
	started := time.Now()
	result, err := client.RequestNoRateLimitRetry(ctx, http.MethodGet, "www.googleapis.com", "/drive/v3/files/"+url.PathEscape(cfg.Drive.FolderID)+"?fields=id", headers, nil)
	row.TotalMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil || result == nil || len(result.Body) > 4096 {
		return row
	}
	row.Status, row.WireProto = result.Status, result.Proto
	var body struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(result.Body, &body) == nil {
		row.ExactID = body.ID == cfg.Drive.FolderID
	}
	row.OK = row.Status == 200 && row.ExactID && row.WireProto == "HTTP/3.0"
	return row
}

func run(parent context.Context, cfg *skirk.Config, token, uri, protocol string) sample {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	file, err := os.CreateTemp("", "skirk-h3-body-*")
	if err != nil {
		return sample{Protocol: protocol}
	}
	defer os.Remove(file.Name())
	defer file.Close()
	mode := "--http2"
	if protocol == "h3" {
		mode = "--http3-only"
	}
	args := []string{mode, "--resolve", "www.google.com:443:" + cfg.Route.GoogleIP, "-H", "Host: www.googleapis.com", "-sS", "-o", file.Name(), "-w", "%{http_version} %{http_code} %{time_starttransfer} %{time_total}", "--max-time", "11", "-K", "-", uri}
	command := exec.CommandContext(ctx, "/opt/homebrew/opt/curl/bin/curl", args...)
	secretConfig := "header = \"Authorization: Bearer " + token + "\"\n"
	if cfg.Drive.QuotaProject != "" {
		secretConfig += "header = \"X-Goog-User-Project: " + cfg.Drive.QuotaProject + "\"\n"
	}
	command.Stdin = strings.NewReader(secretConfig)
	command.Stderr = nil
	output, err := command.Output()
	row := sample{Protocol: protocol}
	if err != nil {
		return row
	}
	fields := strings.Fields(string(output))
	if len(fields) != 4 {
		return row
	}
	row.Status, _ = strconv.Atoi(fields[1])
	ttfb, _ := strconv.ParseFloat(fields[2], 64)
	total, _ := strconv.ParseFloat(fields[3], 64)
	row.TTFBMS, row.TotalMS = ttfb*1000, total*1000
	if (protocol == "h3" && fields[0] != "3") || (protocol == "h2" && fields[0] != "2") {
		return row
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || len(data) > 4096 {
		return row
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &body) == nil {
		row.ExactID = body.ID == cfg.Drive.FolderID
	}
	row.OK = row.Status == 200 && row.ExactID
	return row
}
