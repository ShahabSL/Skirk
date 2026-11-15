// Bounded, isolated files.watch latency experiment. Never use with production accounts.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	patchCount     = 10
	maxUnits       = 4000
	activeMaxUnits = 2500
)

type sample struct {
	PatchRequestMS               float64 `json:"patch_request_ms"`
	NotificationFromPatchStartMS float64 `json:"notification_from_patch_start_ms,omitempty"`
	NotificationFromPatchDoneMS  float64 `json:"notification_from_patch_done_ms,omitempty"`
	PollFromPatchDoneMS          float64 `json:"poll_from_patch_done_ms,omitempty"`
	PollGets                     int     `json:"poll_gets"`
	NotificationSeen             bool    `json:"notification_seen"`
	PollSeen                     bool    `json:"poll_seen"`
}

type report struct {
	SchemaVersion           int      `json:"schema_version"`
	Route                   string   `json:"route"`
	CallbackPreflightStatus int      `json:"callback_preflight_status,omitempty"`
	WatchStatus             int      `json:"watch_status,omitempty"`
	WatchExpirationSeconds  int64    `json:"watch_expiration_seconds,omitempty"`
	SyncMessages            int      `json:"sync_messages"`
	UpdateMessages          int      `json:"update_messages"`
	RejectedCallbacks       int      `json:"rejected_callbacks"`
	Samples                 []sample `json:"samples,omitempty"`
	NotificationMedianMS    float64  `json:"notification_median_ms,omitempty"`
	NotificationP95MS       float64  `json:"notification_p95_ms,omitempty"`
	PollingMedianMS         float64  `json:"polling_median_ms,omitempty"`
	PollingP95MS            float64  `json:"polling_p95_ms,omitempty"`
	StopStatus              int      `json:"stop_status,omitempty"`
	Stopped                 bool     `json:"stopped"`
	CleanupDeletes          int      `json:"cleanup_deletes"`
	ExactID404Checks        int      `json:"exact_id_404_checks"`
	ImmediatePrefixObjects  int      `json:"immediate_prefix_objects"`
	DelayedPrefixObjects    int      `json:"delayed_prefix_objects"`
	CleanupVerified         bool     `json:"cleanup_verified"`
	DriveUnitsEstimated     int64    `json:"drive_units_estimated"`
	StoreErrors             int64    `json:"store_errors"`
	Failure                 string   `json:"failure,omitempty"`
	OK                      bool     `json:"ok"`
}

type notification struct {
	At         time.Time
	State      string
	ResourceID string
}

type probe struct {
	cfg                                 *skirk.Config
	tokens                              *skirk.AccessTokenSource
	client                              *skirk.GoogleHTTPClient
	writer                              *skirk.DriveStore
	reader                              *skirk.DriveStore
	out                                 string
	recovery                            string
	id, name, prefix                    string
	channelID, channelToken, resourceID string
	manualUnits                         int64
	server                              *http.Server
	listener                            net.Listener
	tunnel                              *exec.Cmd
	callbackPath                        string
	events                              chan notification
	mu                                  sync.Mutex
	rejected                            int
	syncMessages                        int
	updateMessages                      int
	result                              report
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func parseOptions() (string, string, error) {
	var config, out string
	var live bool
	flag.StringVar(&config, "config", "", "isolated personal-test kit exit.json")
	flag.StringVar(&out, "out", "", "new ignored .skirk-runs JSON report")
	flag.BoolVar(&live, "live", false, "permit bounded live calls")
	flag.Parse()
	if !live || config == "" || out == "" || len(flag.Args()) != 0 {
		return "", "", errors.New("requires --live, --config, and --out")
	}
	config, err := filepath.EvalSymlinks(config)
	if err != nil {
		return "", "", errors.New("test config missing")
	}
	config, err = filepath.Abs(config)
	if err != nil || !strings.HasSuffix(config, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		return "", "", errors.New("config must be isolated personal-test kit exit.json")
	}
	out, err = filepath.Abs(out)
	if err != nil || filepath.Ext(out) != ".json" {
		return "", "", errors.New("invalid report path")
	}
	privateRoot, err := filepath.Abs(".skirk-runs")
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(privateRoot, 0700); err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(privateRoot, out)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, string(filepath.Separator)) {
		return "", "", errors.New("report must be directly under .skirk-runs")
	}
	for _, path := range []string{out, out + ".recovery-id"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return "", "", errors.New("report or recovery record already exists")
		}
	}
	return config, out, nil
}

func newProbe(cfg *skirk.Config, out string) *probe {
	cfg.Route.Mode = "google_front_pinned"
	cfg.Drive.MetadataSlotV1 = true
	tokens := skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	return &probe{cfg: cfg, tokens: tokens, client: skirk.NewGoogleHTTPClient(cfg.Route), writer: skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(cfg.Route), tokens, cfg.Drive), reader: skirk.NewDriveStoreWithTokenSource(skirk.NewGoogleHTTPClient(cfg.Route), tokens, cfg.Drive), out: out, recovery: out + ".recovery-id", events: make(chan notification, 64), result: report{SchemaVersion: 1, Route: "google_front_pinned"}}
}

func (p *probe) units() int64 {
	return p.writer.QuotaSnapshot().Units + p.reader.QuotaSnapshot().Units + p.manualUnits
}
func (p *probe) guard(active bool) error {
	limit := int64(maxUnits)
	if active {
		limit = activeMaxUnits
	}
	if p.units() > limit {
		return errors.New("Drive quota guard")
	}
	if active && p.writer.QuotaSnapshot().Errors+p.reader.QuotaSnapshot().Errors > 0 {
		return errors.New("Drive API error")
	}
	return nil
}

func (p *probe) request(ctx context.Context, method, path string, units int64, body []byte) (*skirk.HTTPResult, error) {
	if p.units()+units > maxUnits {
		return nil, errors.New("Drive quota guard")
	}
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Cache-Control": "no-cache"}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	if project := p.cfg.Drive.QuotaProject; project != "" {
		headers["X-Goog-User-Project"] = project
	}
	res, err := p.client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	p.manualUnits += units
	return res, err
}

func (p *probe) startCallback() (string, error) {
	path, err := randomHex(16)
	if err != nil {
		return "", err
	}
	p.callbackPath = "/watch/" + path
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	p.listener = listener
	mux := http.NewServeMux()
	mux.HandleFunc(p.callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Goog-Channel-Id")), []byte(p.channelID)) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Goog-Channel-Token")), []byte(p.channelToken)) != 1 {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		state := r.Header.Get("X-Goog-Resource-State")
		if state != "sync" && state != "update" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ev := notification{At: time.Now(), State: state, ResourceID: r.Header.Get("X-Goog-Resource-Id")}
		p.mu.Lock()
		if state == "sync" {
			p.syncMessages++
		} else {
			p.updateMessages++
		}
		if p.resourceID == "" && ev.ResourceID != "" {
			p.resourceID = ev.ResourceID
		}
		p.mu.Unlock()
		select {
		case p.events <- ev:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	p.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	cmd := exec.Command("cloudflared", "tunnel", "--url", "http://"+listener.Addr().String(), "--no-autoupdate", "--protocol", "http2", "--loglevel", "info")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		return "", err
	}
	p.tunnel = cmd
	urls := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		regex := regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
		for scanner.Scan() {
			if found := regex.FindString(scanner.Text()); found != "" {
				select {
				case urls <- found:
				default:
				}
			}
		}
	}()
	select {
	case base := <-urls:
		// Verify the temporary public route before creating any watched Drive resource.
		verify := http.Client{Timeout: 3 * time.Second}
		for attempt := 0; attempt < 12; attempt++ {
			res, err := verify.Get(base + p.callbackPath)
			if err == nil {
				p.result.CallbackPreflightStatus = res.StatusCode
				_ = res.Body.Close()
				if res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusForbidden {
					return base + p.callbackPath, nil
				}
			}
			time.Sleep(time.Second)
		}
		return "", errors.New("temporary HTTPS route unreachable")
	case <-time.After(45 * time.Second):
		return "", errors.New("temporary HTTPS tunnel unavailable")
	}
}

func (p *probe) stopCallback() {
	if p.tunnel != nil && p.tunnel.Process != nil {
		_ = p.tunnel.Process.Kill()
		_ = p.tunnel.Wait()
	}
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.server.Shutdown(ctx)
	}
}

func (p *probe) recordRecovery() error {
	f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(f, "file_id=%s\nname=%s\nchannel_id=%s\n", p.id, p.name, p.channelID)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (p *probe) watch(ctx context.Context, address string) error {
	body, _ := json.Marshal(map[string]any{"id": p.channelID, "type": "web_hook", "address": address, "token": p.channelToken, "expiration": time.Now().Add(5 * time.Minute).UnixMilli()})
	res, err := p.request(ctx, http.MethodPost, "/drive/v3/files/"+url.PathEscape(p.id)+"/watch", 100, body)
	if err != nil || res == nil {
		return errors.New("watch request failed")
	}
	p.result.WatchStatus = res.Status
	if res.Status != http.StatusOK {
		return fmt.Errorf("watch returned status %d", res.Status)
	}
	var channel struct {
		ID         string      `json:"id"`
		ResourceID string      `json:"resourceId"`
		Expiration json.Number `json:"expiration"`
	}
	if err := json.Unmarshal(res.Body, &channel); err != nil || channel.ID != p.channelID || channel.ResourceID == "" {
		return errors.New("watch response invalid")
	}
	p.mu.Lock()
	p.resourceID = channel.ResourceID
	p.mu.Unlock()
	if f, err := os.OpenFile(p.recovery, os.O_WRONLY|os.O_APPEND, 0600); err == nil {
		_, _ = fmt.Fprintf(f, "resource_id=%s\n", channel.ResourceID)
		_ = f.Close()
	}
	if millis, err := channel.Expiration.Int64(); err == nil {
		p.result.WatchExpirationSeconds = (millis - time.Now().UnixMilli()) / 1000
	}
	return nil
}

func (p *probe) stopChannel(ctx context.Context) {
	p.mu.Lock()
	resourceID := p.resourceID
	p.mu.Unlock()
	if p.channelID == "" || resourceID == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"id": p.channelID, "resourceId": resourceID})
	for attempt := 0; attempt < 3; attempt++ {
		res, err := p.request(ctx, http.MethodPost, "/drive/v3/channels/stop", 5, body)
		if err == nil && res != nil {
			p.result.StopStatus = res.Status
			if res.Status >= 200 && res.Status < 300 {
				p.result.Stopped = true
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
}

func (p *probe) prefixObjects(ctx context.Context) (int, error) {
	q := "trashed = false and name contains '" + p.prefix + "'"
	if folder := p.cfg.Drive.FolderID; folder != "" && p.cfg.Drive.Space != "appDataFolder" {
		q += " and '" + folder + "' in parents"
	}
	values := url.Values{}
	values.Set("q", q)
	values.Set("fields", "nextPageToken,files(id,name)")
	values.Set("pageSize", "100")
	if p.cfg.Drive.Space == "appDataFolder" {
		values.Set("spaces", "appDataFolder")
	}
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files?"+values.Encode(), 100, nil)
	if err != nil || res == nil || res.Status != http.StatusOK {
		return 0, errors.New("prefix sweep failed")
	}
	var payload struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil || payload.NextPageToken != "" {
		return 0, errors.New("prefix sweep incomplete")
	}
	count := 0
	for _, f := range payload.Files {
		if strings.HasPrefix(f.Name, p.prefix) {
			count++
			if f.ID != p.id {
				return count, errors.New("unexpected file in unique prefix")
			}
		}
	}
	return count, nil
}

func (p *probe) exactMissing(ctx context.Context) bool {
	res, err := p.request(ctx, http.MethodGet, "/drive/v3/files/"+url.PathEscape(p.id)+"?fields=id", 5, nil)
	return err == nil && res != nil && res.Status == http.StatusNotFound
}

func (p *probe) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	p.stopChannel(ctx)
	if p.id == "" {
		p.result.CleanupVerified = p.resourceID == "" || p.result.Stopped
		return
	}
	for attempt := 0; attempt < 3 && p.units()+500 < maxUnits; attempt++ {
		p.result.CleanupDeletes++
		_ = p.writer.DeleteID(ctx, p.id)
		if !p.exactMissing(ctx) {
			continue
		}
		p.result.ExactID404Checks++
		time.Sleep(2 * time.Second)
		if !p.exactMissing(ctx) {
			continue
		}
		p.result.ExactID404Checks++
		count, err := p.prefixObjects(ctx)
		p.result.ImmediatePrefixObjects = count
		if err != nil || count != 0 {
			continue
		}
		time.Sleep(30 * time.Second)
		count, err = p.prefixObjects(ctx)
		p.result.DelayedPrefixObjects = count
		if err == nil && count == 0 && (p.resourceID == "" || p.result.Stopped) {
			if err := os.Remove(p.recovery); err == nil || os.IsNotExist(err) {
				p.result.CleanupVerified = true
			}
		}
		return
	}
}

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	index := int(float64(len(values)-1)*q + 0.5)
	return values[index]
}

func (p *probe) run() (result report) {
	defer func() {
		p.cleanup()
		p.stopCallback()
		p.mu.Lock()
		p.result.SyncMessages = p.syncMessages
		p.result.UpdateMessages = p.updateMessages
		p.result.RejectedCallbacks = p.rejected
		p.mu.Unlock()
		p.result.DriveUnitsEstimated = p.units()
		p.result.StoreErrors = p.writer.QuotaSnapshot().Errors + p.reader.QuotaSnapshot().Errors
		var push, poll []float64
		for _, s := range p.result.Samples {
			if s.NotificationSeen {
				push = append(push, s.NotificationFromPatchDoneMS)
			}
			if s.PollSeen {
				poll = append(poll, s.PollFromPatchDoneMS)
			}
		}
		p.result.NotificationMedianMS = quantile(push, 0.5)
		p.result.NotificationP95MS = quantile(push, 0.95)
		p.result.PollingMedianMS = quantile(poll, 0.5)
		p.result.PollingP95MS = quantile(poll, 0.95)
		p.result.OK = p.result.Failure == "" && len(p.result.Samples) == patchCount && p.result.Stopped && p.result.CleanupVerified && p.result.StoreErrors == 0 && p.result.DriveUnitsEstimated <= maxUnits
		result = p.result
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	address, err := p.startCallback()
	if err != nil {
		p.result.Failure = "temporary_https_callback_unavailable"
		return
	}
	userNonce, err := randomHex(16)
	if err != nil {
		p.result.Failure = "random_failure"
		return
	}
	p.channelID, err = randomHex(16)
	if err != nil {
		p.result.Failure = "random_failure"
		return
	}
	p.channelToken, err = randomHex(32)
	if err != nil {
		p.result.Failure = "random_failure"
		return
	}
	p.prefix = "watchv1/" + userNonce + "/"
	p.name = p.prefix + "up"
	ids, err := p.writer.GenerateObjectIDs(ctx, 1)
	if err != nil || len(ids) != 1 || p.guard(true) != nil {
		p.result.Failure = "generated_id_failed"
		return
	}
	p.id = ids[0]
	if err := p.recordRecovery(); err != nil {
		p.result.Failure = "recovery_record_failed"
		return
	}
	if _, err := p.writer.CreateMetadataSlot(ctx, p.id, p.name); err != nil || p.guard(true) != nil {
		p.result.Failure = "metadata_slot_create_failed"
		return
	}
	if _, err := p.reader.GetMetadataSlot(ctx, p.id, p.name); err != nil || p.guard(true) != nil {
		p.result.Failure = "initial_read_failed"
		return
	}
	if err := p.watch(ctx, address); err != nil {
		p.result.Failure = "files_watch_failed"
		return
	}
	// The sync may precede the watch response. Drain it before measuring updates.
	for {
		select {
		case ev := <-p.events:
			if ev.State != "sync" {
				p.result.Failure = "unexpected_prepatch_update"
				return
			}
		case <-time.After(2 * time.Second):
			goto measure
		case <-ctx.Done():
			p.result.Failure = "probe_timeout"
			return
		}
	}
measure:
	for i := 0; i < patchCount; i++ {
		if p.guard(true) != nil || p.units()+100 > activeMaxUnits {
			p.result.Failure = "quota_guard"
			return
		}
		marker := fmt.Sprintf("watch-probe:%02d:%s", i, userNonce)
		start := time.Now()
		if _, err := p.writer.PatchMetadataSlot(ctx, p.id, p.name, marker); err != nil {
			p.result.Failure = "patch_failed"
			return
		}
		done := time.Now()
		s := sample{PatchRequestMS: float64(done.Sub(start).Microseconds()) / 1000}
		deadline := time.NewTimer(10 * time.Second)
		pollTicker := time.NewTicker(250 * time.Millisecond)
		// Query once immediately after PATCH completion; this is a known-ID GET baseline.
		pollNow := true
		for !s.NotificationSeen || !s.PollSeen {
			if pollNow && !s.PollSeen {
				pollNow = false
				read, readErr := p.reader.GetMetadataSlot(ctx, p.id, p.name)
				s.PollGets++
				if readErr != nil {
					pollTicker.Stop()
					deadline.Stop()
					p.result.Failure = "poll_get_failed"
					return
				}
				if read.Description == marker {
					s.PollSeen = true
					s.PollFromPatchDoneMS = float64(time.Since(done).Microseconds()) / 1000
				}
				if p.guard(true) != nil {
					pollTicker.Stop()
					deadline.Stop()
					p.result.Failure = "quota_guard"
					return
				}
			}
			if s.NotificationSeen && s.PollSeen {
				break
			}
			select {
			case ev := <-p.events:
				if ev.State == "update" && ev.ResourceID == p.resourceID && !ev.At.Before(start) {
					s.NotificationSeen = true
					s.NotificationFromPatchStartMS = float64(ev.At.Sub(start).Microseconds()) / 1000
					s.NotificationFromPatchDoneMS = float64(ev.At.Sub(done).Microseconds()) / 1000
				}
			case <-pollTicker.C:
				pollNow = true
			case <-deadline.C:
				pollTicker.Stop()
				p.result.Samples = append(p.result.Samples, s)
				p.result.Failure = "notification_timeout"
				return
			case <-ctx.Done():
				pollTicker.Stop()
				deadline.Stop()
				p.result.Failure = "probe_timeout"
				return
			}
		}
		pollTicker.Stop()
		deadline.Stop()
		p.result.Samples = append(p.result.Samples, s)
	}
	return
}

func main() {
	config, out, err := parseOptions()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg, err := skirk.LoadConfig(config)
	if err != nil || cfg.Drive.FolderID == "" && cfg.Drive.Space != "appDataFolder" {
		fmt.Fprintln(os.Stderr, "isolated test config invalid")
		os.Exit(2)
	}
	p := newProbe(cfg, out)
	result := p.run()
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil || os.WriteFile(out, append(data, '\n'), 0600) != nil {
		fmt.Fprintln(os.Stderr, "private report write failed")
		os.Exit(2)
	}
	fmt.Printf("ok=%t watch=%d patches=%d updates=%d push_median_ms=%.1f poll_median_ms=%.1f stopped=%t cleanup=%t units=%d failure=%s\n", result.OK, result.WatchStatus, len(result.Samples), result.UpdateMessages, result.NotificationMedianMS, result.PollingMedianMS, result.Stopped, result.CleanupVerified, result.DriveUnitsEstimated, result.Failure)
	if !result.OK {
		os.Exit(1)
	}
}
