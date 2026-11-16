// about_watermark_probe compares v2 About.largestChangeId with v3
// changes.getStartPageToken as advisory idle Drive mailbox watermarks.
// It never changes Skirk product code.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
	"github.com/ShahabSL/Skirk/tools/internal/probeledger"
)

const (
	activeUpperLimit  = int64(120000)
	cleanupUpperLimit = int64(12000)
	defaultPoll       = 250 * time.Millisecond
	phaseTimeout      = 5 * time.Second
)

type actor uint8

const (
	client actor = iota
	exit
)

type caller func(context.Context, actor, string, string, map[string]string, []byte) (*skirk.HTTPResult, error)

type target struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Actor actor  `json:"actor"`
}

type fileMeta struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Parents     []string `json:"parents"`
	OwnedByMe   bool     `json:"ownedByMe"`
}

type phaseResult struct {
	Name                          string  `json:"name"`
	MutationMS                    float64 `json:"mutation_ms"`
	FirstWatermarkMS              float64 `json:"first_watermark_change_ms,omitempty"`
	FirstTokenMS                  float64 `json:"first_start_page_token_change_ms,omitempty"`
	FirstListMS                   float64 `json:"first_list_target_ms,omitempty"`
	WatermarkObserved             bool    `json:"watermark_observed"`
	TokenObserved                 bool    `json:"start_page_token_observed"`
	ListObserved                  bool    `json:"list_observed"`
	ListVisibleWatermarkUnchanged bool    `json:"list_visible_watermark_still_unchanged"`
	ListVisibleTokenUnchanged     bool    `json:"list_visible_start_page_token_still_unchanged"`
	WatermarkRegressed            bool    `json:"watermark_regressed"`
	WatermarkDelta                uint64  `json:"watermark_delta"`
	PrimaryStayedEmpty            bool    `json:"primary_stayed_empty,omitempty"`
	Cycles                        int     `json:"cycles"`
	AboutCalls                    int     `json:"about_calls"`
	TokenCalls                    int     `json:"start_page_token_calls"`
	ListCalls                     int     `json:"list_calls"`
	NominalUnits                  int64   `json:"nominal_units"`
	UpperUnits                    int64   `json:"eight_attempt_upper_units"`
	Failure                       string  `json:"failure,omitempty"`
}

type report struct {
	Schema                    int                  `json:"schema_version"`
	Route                     string               `json:"route"`
	IdentityScope             string               `json:"identity_scope"`
	QuiescentSamples          int                  `json:"quiescent_about_samples"`
	QuiescentChanges          int                  `json:"quiescent_watermark_changes"`
	QuiescentTokenSamples     int                  `json:"quiescent_start_page_token_samples"`
	QuiescentTokenChanges     int                  `json:"quiescent_start_page_token_changes"`
	V2PreflightOK             bool                 `json:"v2_about_preflight_ok"`
	V3PreflightOK             bool                 `json:"v3_start_page_token_preflight_ok"`
	Phases                    []phaseResult        `json:"phases"`
	Calls                     map[string]int       `json:"calls"`
	CallDurationsMS           map[string][]float64 `json:"call_durations_ms"`
	Protocols                 map[string]int       `json:"protocols"`
	NominalUnits              int64                `json:"nominal_units"`
	UpperUnits                int64                `json:"eight_attempt_upper_units"`
	ResponseBytes             int64                `json:"response_bytes"`
	RequestBodyBytes          int64                `json:"request_body_bytes"`
	CleanupExact404           bool                 `json:"cleanup_exact_ids_404_twice"`
	CleanupImmediate          bool                 `json:"cleanup_prefixes_empty_immediately"`
	CleanupDelayed            bool                 `json:"cleanup_prefixes_empty_after_31s"`
	Failure                   string               `json:"failure,omitempty"`
	Completed                 bool                 `json:"completed"`
	AdvisorySafeInSample      bool                 `json:"advisory_safe_in_sample"`
	TokenAdvisorySafeInSample bool                 `json:"start_page_token_advisory_safe_in_sample"`
}

type probe struct {
	call         caller
	folder       string
	out          string
	targets      [2]target
	mu           sync.Mutex
	activeUpper  int64
	cleanupUpper int64
	pollDelay    time.Duration
	cleanupDelay time.Duration
	result       report
}

func validID(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func methodCost(method, path string) (string, int64, error) {
	switch {
	case method == http.MethodGet && path == "/drive/v2/about?fields=largestChangeId":
		return "about.get", 5, nil
	case method == http.MethodGet && path == "/drive/v3/changes/startPageToken?fields=startPageToken":
		return "changes.getStartPageToken", 5, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/generateIds?"):
		return "files.generateIds", 5, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files?"):
		return "files.list", 100, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/"):
		return "files.get", 5, nil
	case method == http.MethodPost && strings.HasPrefix(path, "/drive/v3/files?fields="):
		return "files.create", 50, nil
	case method == http.MethodPatch && strings.HasPrefix(path, "/drive/v3/files/"):
		return "files.update", 50, nil
	case method == http.MethodDelete && strings.HasPrefix(path, "/drive/v3/files/"):
		return "files.delete", 50, nil
	default:
		return "", 0, errors.New("method or path outside probe allowlist")
	}
}

func (p *probe) request(ctx context.Context, cleanup bool, who actor, method, path string, body []byte) (*skirk.HTTPResult, error) {
	label, units, err := methodCost(method, path)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if cleanup {
		if p.cleanupUpper+8*units > cleanupUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("cleanup quota reserve exhausted")
		}
		p.cleanupUpper += 8 * units
	} else {
		if p.activeUpper+8*units > activeUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("active quota guard")
		}
		p.activeUpper += 8 * units
	}
	p.result.Calls[label]++
	p.result.NominalUnits += units
	p.result.UpperUnits += 8 * units
	p.result.RequestBodyBytes += int64(len(body))
	p.mu.Unlock()
	headers := map[string]string{"Accept-Encoding": "identity", "Cache-Control": "no-cache"}
	if len(body) != 0 {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	started := time.Now()
	r, err := p.call(requestCtx, who, method, path, headers, body)
	durationMS := float64(time.Since(started).Microseconds()) / 1000
	p.mu.Lock()
	if p.result.CallDurationsMS == nil {
		p.result.CallDurationsMS = map[string][]float64{}
	}
	p.result.CallDurationsMS[label] = append(p.result.CallDurationsMS[label], durationMS)
	if r != nil {
		if p.result.Protocols == nil {
			p.result.Protocols = map[string]int{}
		}
		proto := r.Proto
		if proto != "HTTP/1.1" && proto != "HTTP/2.0" && proto != "HTTP/3.0" {
			proto = "other"
		}
		p.result.Protocols[proto]++
	}
	p.mu.Unlock()
	if err != nil {
		return r, err
	}
	if r == nil || len(r.Body) > 64<<10 {
		return r, errors.New("missing or oversized Drive response")
	}
	p.mu.Lock()
	p.result.ResponseBytes += int64(len(r.Body))
	p.mu.Unlock()
	return r, nil
}

func requireStatus(r *skirk.HTTPResult, want ...int) error {
	if r == nil {
		return errors.New("missing Drive response")
	}
	for _, status := range want {
		if r.Status == status {
			return nil
		}
	}
	return fmt.Errorf("unexpected Drive status %d", r.Status)
}

func decode[T any](body []byte) (T, error) {
	var v T
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return v, errors.New("trailing or invalid JSON")
	}
	return v, nil
}

func (p *probe) about(ctx context.Context) (uint64, error) {
	r, err := p.request(ctx, false, exit, http.MethodGet, "/drive/v2/about?fields=largestChangeId", nil)
	if err != nil {
		return 0, err
	}
	if err = requireStatus(r, 200); err != nil {
		return 0, err
	}
	v, err := decode[struct {
		LargestChangeID string `json:"largestChangeId"`
	}](r.Body)
	if err != nil || v.LargestChangeID == "" {
		return 0, errors.New("v2 About lacks largestChangeId")
	}
	id, err := strconv.ParseUint(v.LargestChangeID, 10, 64)
	if err != nil {
		return 0, errors.New("invalid largestChangeId")
	}
	return id, nil
}

func (p *probe) startPageToken(ctx context.Context) (string, error) {
	r, err := p.request(ctx, false, exit, http.MethodGet, "/drive/v3/changes/startPageToken?fields=startPageToken", nil)
	if err != nil {
		return "", err
	}
	if err = requireStatus(r, 200); err != nil {
		return "", err
	}
	v, err := decode[struct {
		StartPageToken string `json:"startPageToken"`
	}](r.Body)
	if err != nil || v.StartPageToken == "" || len(v.StartPageToken) > 1024 {
		return "", errors.New("invalid v3 startPageToken")
	}
	return v.StartPageToken, nil
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}

func (p *probe) list(ctx context.Context, cleanup bool, t target) (*fileMeta, error) {
	q := "trashed=false and '" + escape(p.folder) + "' in parents and name contains '" + escape(t.Name) + "'"
	params := url.Values{"q": {q}, "fields": {"nextPageToken,incompleteSearch,files(id,name,description,parents,ownedByMe)"}, "pageSize": {"100"}}
	r, err := p.request(ctx, cleanup, exit, http.MethodGet, "/drive/v3/files?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err = requireStatus(r, 200); err != nil {
		return nil, err
	}
	v, err := decode[struct {
		Files      []fileMeta `json:"files"`
		Next       string     `json:"nextPageToken"`
		Incomplete bool       `json:"incompleteSearch"`
	}](r.Body)
	if err != nil || v.Next != "" || v.Incomplete {
		return nil, errors.New("incomplete or invalid Drive list")
	}
	var found *fileMeta
	for _, f := range v.Files {
		if f.Name != t.Name {
			continue // Drive name-contains can match more than exact name.
		}
		if f.ID != t.ID || len(f.Parents) != 1 || f.Parents[0] != p.folder || found != nil {
			return nil, errors.New("ambiguous or foreign exact-name result")
		}
		copy := f
		found = &copy
	}
	return found, nil
}

func (p *probe) persistRecovery() error {
	if p.out == "" {
		return nil
	}
	path := p.out + ".recovery.json"
	tmp := path + ".tmp"
	b, _ := json.Marshal(map[string]any{"folder": p.folder, "targets": p.targets})
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return probeledger.SyncParent(path)
}

func (p *probe) reserve(ctx context.Context, index int) error {
	who := p.targets[index].Actor
	r, err := p.request(ctx, false, who, http.MethodGet, "/drive/v3/files/generateIds?count=1&fields=ids", nil)
	if err != nil {
		return err
	}
	if err = requireStatus(r, 200); err != nil {
		return err
	}
	v, err := decode[struct {
		IDs []string `json:"ids"`
	}](r.Body)
	if err != nil || len(v.IDs) != 1 || !validID(v.IDs[0]) {
		return errors.New("invalid generated file ID")
	}
	p.targets[index].ID = v.IDs[0]
	if index == 1 && p.targets[0].ID == p.targets[1].ID {
		return errors.New("duplicate generated IDs")
	}
	return p.persistRecovery() // precedes every file create
}

func path(t target) string { return "/drive/v3/files/" + url.PathEscape(t.ID) }

func (p *probe) create(ctx context.Context, t target, description string) error {
	b, _ := json.Marshal(map[string]any{"id": t.ID, "name": t.Name, "description": description, "mimeType": "application/octet-stream", "parents": []string{p.folder}})
	r, err := p.request(ctx, false, t.Actor, http.MethodPost, "/drive/v3/files?fields=id,name,description,parents,ownedByMe", b)
	if err != nil {
		return err
	}
	if err = requireStatus(r, 200, 201); err != nil {
		return err
	}
	f, err := decode[fileMeta](r.Body)
	if err != nil || f.ID != t.ID || f.Name != t.Name || f.Description != description || len(f.Parents) != 1 || f.Parents[0] != p.folder || !f.OwnedByMe {
		return errors.New("created file identity mismatch")
	}
	return nil
}

func (p *probe) update(ctx context.Context, t target, description string) error {
	b, _ := json.Marshal(map[string]string{"description": description})
	r, err := p.request(ctx, false, t.Actor, http.MethodPatch, path(t)+"?fields=id,name,description", b)
	if err != nil {
		return err
	}
	if err = requireStatus(r, 200); err != nil {
		return err
	}
	f, err := decode[fileMeta](r.Body)
	if err != nil || f.ID != t.ID || f.Name != t.Name || f.Description != description {
		return errors.New("updated file identity mismatch")
	}
	return nil
}

func (p *probe) delete(ctx context.Context, t target) error {
	r, err := p.request(ctx, false, t.Actor, http.MethodDelete, path(t), nil)
	if err != nil {
		return err
	}
	return requireStatus(r, 204)
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func matches(f *fileMeta, state string) bool {
	if state == "absent" {
		return f == nil
	}
	return f != nil && f.Description == state
}

func (p *probe) phase(parent context.Context, name string, t target, before, after string, mutate func(context.Context) error, unrelated bool) (phaseResult, error) {
	row := phaseResult{Name: name}
	ctx, cancel := context.WithTimeout(parent, phaseTimeout+12*time.Second)
	defer cancel()
	base, err := p.about(ctx)
	if err != nil {
		return row, err
	}
	baseToken, err := p.startPageToken(ctx)
	if err != nil {
		return row, err
	}
	f, err := p.list(ctx, false, t)
	if err != nil {
		return row, err
	}
	if !matches(f, before) {
		return row, errors.New("phase precondition list mismatch")
	}
	p.mu.Lock()
	startAbout, startToken, startList := p.result.Calls["about.get"], p.result.Calls["changes.getStartPageToken"], p.result.Calls["files.list"]
	startNominal, startUpper := p.result.NominalUnits, p.result.UpperUnits
	p.mu.Unlock()
	start := time.Now()
	if err = mutate(ctx); err != nil {
		return row, err
	}
	row.MutationMS = float64(time.Since(start).Microseconds()) / 1000
	deadline := time.Now().Add(phaseTimeout)
	for time.Now().Before(deadline) {
		row.Cycles++
		value, e := p.about(ctx)
		if e != nil {
			return row, e
		}
		if value < base {
			row.WatermarkRegressed = true
			return row, errors.New("largestChangeId regressed")
		}
		if value > base && !row.WatermarkObserved {
			row.WatermarkObserved = true
			row.FirstWatermarkMS = float64(time.Since(start).Microseconds()) / 1000
			row.WatermarkDelta = value - base
		}
		token, e := p.startPageToken(ctx)
		if e != nil {
			return row, e
		}
		if token != baseToken && !row.TokenObserved {
			row.TokenObserved = true
			row.FirstTokenMS = float64(time.Since(start).Microseconds()) / 1000
		}
		f, e = p.list(ctx, false, t)
		if e != nil {
			return row, e
		}
		if matches(f, after) && !row.ListObserved {
			row.ListObserved = true
			row.FirstListMS = float64(time.Since(start).Microseconds()) / 1000
		}
		if row.ListObserved && (value == base || token == baseToken) {
			// Recheck both advisory signals after the list-visible state. The
			// second read is a stronger lag witness than a pre-list read alone.
			confirm, e := p.about(ctx)
			if e != nil {
				return row, e
			}
			if confirm == base {
				row.ListVisibleWatermarkUnchanged = true
			} else if confirm < base {
				row.WatermarkRegressed = true
				return row, errors.New("largestChangeId regressed")
			} else if !row.WatermarkObserved {
				row.WatermarkObserved = true
				row.FirstWatermarkMS = float64(time.Since(start).Microseconds()) / 1000
				row.WatermarkDelta = confirm - base
			}
			confirmToken, e := p.startPageToken(ctx)
			if e != nil {
				return row, e
			}
			if confirmToken == baseToken {
				row.ListVisibleTokenUnchanged = true
			} else if !row.TokenObserved {
				row.TokenObserved = true
				row.FirstTokenMS = float64(time.Since(start).Microseconds()) / 1000
			}
		}
		if row.ListObserved && row.WatermarkObserved && row.TokenObserved {
			break
		}
		delay := p.pollDelay
		if delay == 0 {
			delay = defaultPoll
		}
		if e := wait(ctx, delay); e != nil {
			return row, e
		}
	}
	if !row.ListObserved {
		return row, errors.New("mutated file never reached corroborating list")
	}
	if unrelated {
		primary, e := p.list(ctx, false, p.targets[0])
		if e != nil {
			return row, e
		}
		row.PrimaryStayedEmpty = primary == nil
		if !row.PrimaryStayedEmpty {
			return row, errors.New("unrelated phase polluted primary mailbox")
		}
	}
	p.mu.Lock()
	row.AboutCalls = p.result.Calls["about.get"] - startAbout
	row.TokenCalls = p.result.Calls["changes.getStartPageToken"] - startToken
	row.ListCalls = p.result.Calls["files.list"] - startList
	row.NominalUnits = p.result.NominalUnits - startNominal
	row.UpperUnits = p.result.UpperUnits - startUpper
	p.mu.Unlock()
	return row, nil
}

func (p *probe) quiescent(ctx context.Context) error {
	first, err := p.about(ctx)
	if err != nil {
		return err
	}
	firstToken, err := p.startPageToken(ctx)
	if err != nil {
		return err
	}
	if f, err := p.list(ctx, false, p.targets[0]); err != nil {
		return err
	} else if f != nil {
		return errors.New("primary prefix not empty at baseline")
	}
	last := first
	lastToken := firstToken
	for i := 0; i < 8; i++ {
		if err := wait(ctx, 150*time.Millisecond); err != nil {
			return err
		}
		value, err := p.about(ctx)
		if err != nil {
			return err
		}
		if value < last {
			return errors.New("quiescent largestChangeId regressed")
		}
		if value > last {
			p.result.QuiescentChanges++
		}
		last = value
		p.result.QuiescentSamples++
		token, err := p.startPageToken(ctx)
		if err != nil {
			return err
		}
		if token != lastToken {
			p.result.QuiescentTokenChanges++
		}
		lastToken = token
		p.result.QuiescentTokenSamples++
	}
	if f, err := p.list(ctx, false, p.targets[0]); err != nil {
		return err
	} else if f != nil {
		return errors.New("primary prefix changed during quiescent baseline")
	}
	return nil
}

func (p *probe) run(ctx context.Context) error {
	// Both read methods must be checked before creating any file, even if
	// the first one fails on the live grant or front route.
	_, v2err := p.about(ctx)
	p.result.V2PreflightOK = v2err == nil
	_, v3err := p.startPageToken(ctx)
	p.result.V3PreflightOK = v3err == nil
	if v2err != nil || v3err != nil {
		return fmt.Errorf("read preflight failed: v2=%v; v3=%v", v2err, v3err)
	}
	for i := range p.targets {
		if err := p.reserve(ctx, i); err != nil {
			return err
		}
	}
	if err := p.quiescent(ctx); err != nil {
		return err
	}
	primary, unrelated := p.targets[0], p.targets[1]
	steps := []struct {
		name          string
		target        target
		before, after string
		unrelated     bool
		mutate        func(context.Context) error
	}{
		{"primary_create", primary, "absent", "created", false, func(ctx context.Context) error { return p.create(ctx, primary, "created") }},
		{"primary_update", primary, "created", "updated", false, func(ctx context.Context) error { return p.update(ctx, primary, "updated") }},
		{"primary_delete", primary, "updated", "absent", false, func(ctx context.Context) error { return p.delete(ctx, primary) }},
		{"unrelated_create", unrelated, "absent", "created", true, func(ctx context.Context) error { return p.create(ctx, unrelated, "created") }},
		{"unrelated_update", unrelated, "created", "updated", true, func(ctx context.Context) error { return p.update(ctx, unrelated, "updated") }},
		{"unrelated_delete", unrelated, "updated", "absent", true, func(ctx context.Context) error { return p.delete(ctx, unrelated) }},
	}
	for _, step := range steps {
		row, err := p.phase(ctx, step.name, step.target, step.before, step.after, step.mutate, step.unrelated)
		if err != nil {
			row.Failure = err.Error()
		}
		p.result.Phases = append(p.result.Phases, row)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *probe) cleanupOwned() {
	if p.targets[0].ID == "" && p.targets[1].ID == "" {
		// Preflight failed before ID reservation and before any mutation.
		p.result.CleanupExact404 = true
		p.result.CleanupImmediate = true
		p.result.CleanupDelayed = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	checkExact := true
	for _, t := range p.targets {
		if t.ID == "" {
			continue
		}
		r, err := p.request(ctx, true, t.Actor, http.MethodGet, path(t)+"?fields=id,name,description,parents,ownedByMe", nil)
		if err != nil {
			checkExact = false
			continue
		}
		if r.Status == 200 {
			f, e := decode[fileMeta](r.Body)
			if e != nil || f.ID != t.ID || f.Name != t.Name || len(f.Parents) != 1 || f.Parents[0] != p.folder || !f.OwnedByMe {
				checkExact = false // never delete an unproven file
				continue
			}
			r, err = p.request(ctx, true, t.Actor, http.MethodDelete, path(t), nil)
			if err != nil || requireStatus(r, 204, 404) != nil {
				checkExact = false
				continue
			}
		} else if r.Status != 404 {
			checkExact = false
			continue
		}
		for i := 0; i < 2; i++ {
			r, err = p.request(ctx, true, t.Actor, http.MethodGet, path(t)+"?fields=id", nil)
			if err != nil || requireStatus(r, 404) != nil {
				checkExact = false
			}
			if i == 0 {
				_ = wait(ctx, time.Second)
			}
		}
	}
	p.result.CleanupExact404 = checkExact
	checkLists := func() bool {
		ok := true
		for _, t := range p.targets {
			if t.ID == "" {
				continue
			}
			f, err := p.list(ctx, true, t)
			if err != nil || f != nil {
				ok = false
			}
		}
		return ok
	}
	p.result.CleanupImmediate = checkLists()
	delay := p.cleanupDelay
	if delay == 0 {
		delay = 31 * time.Second
	}
	if wait(ctx, delay) == nil {
		p.result.CleanupDelayed = checkLists()
	}
	if p.result.CleanupExact404 && p.result.CleanupImmediate && p.result.CleanupDelayed {
		_ = os.Remove(p.out + ".recovery.json")
	}
}

func main() {
	clientPath := flag.String("client-config", "", "isolated personal-test kit client.json")
	exitPath := flag.String("exit-config", "", "isolated personal-test kit exit.json")
	outPath := flag.String("out", "", "fresh JSON result directly under .skirk-runs")
	routeChoice := flag.String("route", "front", "front or explicit direct")
	live := flag.Bool("live", false, "permit bounded live Google calls")
	flag.Parse()
	if !*live || (*routeChoice != "front" && *routeChoice != "direct") {
		fmt.Fprintln(os.Stderr, "bounded live mode and a valid route are required")
		os.Exit(2)
	}
	clientAbs, e1 := filepath.EvalSymlinks(*clientPath)
	exitAbs, e2 := filepath.EvalSymlinks(*exitPath)
	if e1 != nil || e2 != nil || !strings.HasSuffix(clientAbs, filepath.Join(".skirk-runs", "personal-test", "kit", "client.json")) || !strings.HasSuffix(exitAbs, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		fmt.Fprintln(os.Stderr, "isolated personal-test kit configs required")
		os.Exit(2)
	}
	root, _ := filepath.Abs(".skirk-runs")
	_ = os.MkdirAll(root, 0700)
	out, _ := filepath.Abs(*outPath)
	if filepath.Dir(out) != root || filepath.Ext(out) != ".json" {
		fmt.Fprintln(os.Stderr, "fresh result directly under .skirk-runs required")
		os.Exit(2)
	}
	for _, path := range []string{out, out + ".recovery.json", out + ".recovery.json.tmp"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "result or recovery ledger exists")
			os.Exit(2)
		}
	}
	clientCfg, e1 := skirk.LoadConfig(clientAbs)
	exitCfg, e2 := skirk.LoadConfig(exitAbs)
	if e1 != nil || e2 != nil || clientCfg.Drive.FolderID == "" || clientCfg.Drive.FolderID == "appDataFolder" || clientCfg.Drive.FolderID != exitCfg.Drive.FolderID {
		fmt.Fprintln(os.Stderr, "shared visible isolated folder required")
		os.Exit(2)
	}
	if clientCfg.Auth.ClientID == "" || clientCfg.Auth.RefreshToken == "" || clientCfg.Auth.ClientID != exitCfg.Auth.ClientID || clientCfg.Auth.RefreshToken != exitCfg.Auth.RefreshToken {
		fmt.Fprintln(os.Stderr, "same-grant personal-test kit required")
		os.Exit(2)
	}
	if *routeChoice == "front" {
		if clientCfg.Route.GoogleIP == "" || exitCfg.Route.GoogleIP == "" {
			fmt.Fprintln(os.Stderr, "pinned Google IP required")
			os.Exit(2)
		}
		clientCfg.Route.Mode, exitCfg.Route.Mode = "google_front_pinned", "google_front_pinned"
	} else {
		clientCfg.Route.Mode, exitCfg.Route.Mode = "direct", "direct"
		clientCfg.Route.GoogleIP, exitCfg.Route.GoogleIP = "", ""
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		fmt.Fprintln(os.Stderr, "nonce generation failed")
		os.Exit(1)
	}
	stem := "skirk-about-wm-" + hex.EncodeToString(nonce[:])
	p := &probe{folder: clientCfg.Drive.FolderID, out: out, result: report{Schema: 1, Route: *routeChoice, IdentityScope: "same Google account; client and exit process configs", Calls: map[string]int{}}}
	p.targets[0] = target{Name: stem + "-primary", Actor: client}
	p.targets[1] = target{Name: stem + "-unrelated", Actor: exit}
	sources := [2]*skirk.AccessTokenSource{skirk.NewAccessTokenSource(clientCfg.Auth, clientCfg.Route), skirk.NewAccessTokenSource(exitCfg.Auth, exitCfg.Route)}
	clients := [2]*skirk.GoogleHTTPClient{skirk.NewGoogleHTTPClient(clientCfg.Route), skirk.NewGoogleHTTPClient(exitCfg.Route)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	for _, source := range sources {
		if _, err := source.Token(ctx); err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "token preload failed")
			os.Exit(1)
		}
	}
	p.call = func(ctx context.Context, who actor, method, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
		token, err := sources[who].Token(ctx)
		if err != nil {
			return nil, err
		}
		headers["Authorization"] = "Bearer " + token
		return clients[who].RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	}
	err := p.run(ctx)
	cancel()
	if err != nil {
		p.result.Failure = err.Error()
	}
	p.cleanupOwned()
	p.result.Completed = p.result.Failure == "" && len(p.result.Phases) == 6 && p.result.CleanupExact404 && p.result.CleanupImmediate && p.result.CleanupDelayed
	p.result.AdvisorySafeInSample = p.result.Completed
	p.result.TokenAdvisorySafeInSample = p.result.Completed
	for _, phase := range p.result.Phases {
		if !phase.WatermarkObserved || phase.ListVisibleWatermarkUnchanged || phase.WatermarkRegressed {
			p.result.AdvisorySafeInSample = false
		}
		if !phase.TokenObserved || phase.ListVisibleTokenUnchanged {
			p.result.TokenAdvisorySafeInSample = false
		}
	}
	b, _ := json.MarshalIndent(p.result, "", "  ")
	if err := os.WriteFile(out, append(b, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "result write failed")
		os.Exit(1)
	}
	fmt.Printf("completed=%t v2_advisory_safe_in_sample=%t v3_advisory_safe_in_sample=%t upper_units=%d cleanup=%t\n", p.result.Completed, p.result.AdvisorySafeInSample, p.result.TokenAdvisorySafeInSample, p.result.UpperUnits, p.result.CleanupExact404 && p.result.CleanupImmediate && p.result.CleanupDelayed)
	if !p.result.Completed {
		os.Exit(1)
	}
}
