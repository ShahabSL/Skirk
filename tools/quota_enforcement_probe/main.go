// quota_enforcement_probe performs a bounded, read-only contrast between an
// explicitly selected Drive quota project and the OAuth publisher project.
// It never changes Cloud quotas: an operator must set and restore the test
// project's limit separately, verifying the effective value each time.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

var projectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
var safeReason = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,79}$`)

type observation struct {
	Consumer string  `json:"consumer"`
	Status   int     `json:"status"`
	Reason   string  `json:"reason"`
	MS       float64 `json:"elapsed_ms"`
}

type report struct {
	Phase     string        `json:"phase"`
	Route     string        `json:"route"`
	Method    string        `json:"method"`
	Project   string        `json:"quota_project"`
	Results   []observation `json:"results"`
	AllExact  bool          `json:"all_successful_reads_exact"`
	Pass      bool          `json:"phase_pass"`
	Error     string        `json:"error,omitempty"`
}

func phasePass(phase string, v []observation) bool {
	exact := func(i int, who string) bool {
		return i < len(v) && v[i].Consumer == who && v[i].Status == 200 && v[i].Reason == "ok_exact_id"
	}
	denied := func(i int) bool {
		return i < len(v) && v[i].Consumer == "selected" && (v[i].Status == 403 || v[i].Status == 429) && (v[i].Reason == "RATE_LIMIT_EXCEEDED" || v[i].Reason == "rateLimitExceeded" || v[i].Reason == "quotaExceeded")
	}
	switch phase {
	case "baseline":
		return len(v) == 2 && exact(0, "selected") && exact(1, "publisher")
	case "challenged":
		return len(v) == 4 && exact(0, "publisher") && denied(1) && denied(2) && exact(3, "publisher")
	case "restored":
		return len(v) == 1 && exact(0, "selected")
	default:
		return false
	}
}

func reason(body []byte) string {
	var payload struct {
		Error struct {
			Status  string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return "unclassified"
	}
	for _, d := range payload.Error.Details {
		if safeReason.MatchString(d.Reason) {
			return d.Reason
		}
	}
	for _, e := range payload.Error.Errors {
		if safeReason.MatchString(e.Reason) {
			return e.Reason
		}
	}
	if safeReason.MatchString(payload.Error.Status) {
		return payload.Error.Status
	}
	return "unclassified"
}

func main() {
	phase := flag.String("phase", "", "baseline, challenged, or restored")
	selected := flag.String("quota-project", "", "disposable selected project ID")
	configPath := flag.String("config", "", "isolated personal-test client.json")
	outPath := flag.String("out", "", "fresh JSON output directly under .skirk-runs")
	live := flag.Bool("live", false, "permit bounded Drive reads")
	flag.Parse()
	if !*live || !projectID.MatchString(*selected) || (*phase != "baseline" && *phase != "challenged" && *phase != "restored") {
		fmt.Fprintln(os.Stderr, "live phase and valid quota project required")
		os.Exit(2)
	}
	configAbs, err := filepath.Abs(*configPath)
	if err == nil {
		configAbs, err = filepath.EvalSymlinks(configAbs)
	}
	if err != nil || filepath.Base(configAbs) != "client.json" || filepath.Base(filepath.Dir(configAbs)) != "kit" || filepath.Base(filepath.Dir(filepath.Dir(configAbs))) != "personal-test" || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(configAbs)))) != ".skirk-runs" {
		fmt.Fprintln(os.Stderr, "isolated personal-test client config required")
		os.Exit(2)
	}
	root, err := filepath.Abs(".skirk-runs")
	if err != nil {
		os.Exit(2)
	}
	out, err := filepath.Abs(*outPath)
	if err != nil || filepath.Dir(out) != root || filepath.Ext(out) != ".json" {
		fmt.Fprintln(os.Stderr, "fresh .skirk-runs JSON output required")
		os.Exit(2)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "output already exists or cannot be inspected")
		os.Exit(2)
	}
	cfg, err := skirk.LoadConfig(configAbs)
	if err != nil || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" || cfg.Route.GoogleIP == "" {
		fmt.Fprintln(os.Stderr, "valid visible-folder pinned-front kit required")
		os.Exit(2)
	}
	cfg.Route.Mode = "google_front_pinned"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	source := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	token, err := source.Token(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "isolated OAuth refresh failed")
		os.Exit(1)
	}
	client := skirk.NewGoogleHTTPClient(cfg.Route)
	sequence := []string{"selected", "publisher"}
	if *phase == "challenged" {
		sequence = []string{"publisher", "selected", "selected", "publisher"}
	} else if *phase == "restored" {
		sequence = []string{"selected"}
	}
	r := report{Phase: *phase, Route: "google_front_pinned", Method: "files.get(id_only)", Project: *selected, AllExact: true}
	path := "/drive/v3/files/" + url.PathEscape(cfg.Drive.FolderID) + "?fields=id"
	for _, consumer := range sequence {
		headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache", "Accept-Encoding": "identity"}
		if consumer == "selected" {
			headers["X-Goog-User-Project"] = *selected
		}
		start := time.Now()
		callCtx, done := context.WithTimeout(ctx, 10*time.Second)
		response, callErr := client.RequestSingleAttempt(callCtx, http.MethodGet, "www.googleapis.com", path, headers, nil)
		done()
		entry := observation{Consumer: consumer, MS: float64(time.Since(start).Microseconds()) / 1000}
		if callErr != nil {
			entry.Reason = "network_or_context_error"
			r.AllExact = false
		} else if response == nil {
			entry.Reason = "missing_response"
			r.AllExact = false
		} else {
			entry.Status = response.Status
			if response.Status == 200 {
				var v struct{ ID string `json:"id"` }
				if json.Unmarshal(response.Body, &v) != nil || v.ID != cfg.Drive.FolderID {
					entry.Reason = "wrong_or_invalid_id"
					r.AllExact = false
				} else {
					entry.Reason = "ok_exact_id"
				}
			} else {
				entry.Reason = reason(response.Body)
			}
		}
		r.Results = append(r.Results, entry)
		if callErr != nil || response == nil || (response.Status == 200 && entry.Reason != "ok_exact_id") {
			break
		}
	}
	r.Pass = r.AllExact && phasePass(r.Phase, r.Results)
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(out, append(data, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "cannot persist sanitized report")
		os.Exit(1)
	}
	fmt.Printf("phase=%s reads=%d pass=%t\n", r.Phase, len(r.Results), r.Pass)
	if !r.Pass {
		os.Exit(1)
	}
}
