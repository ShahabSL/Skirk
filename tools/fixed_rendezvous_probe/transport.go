package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

const (
	activeUpperLimit  = int64(80000)
	cleanupUpperLimit = int64(16000)
	maxResponseBytes  = 64 << 10
)

type actor uint8

const (
	writer actor = iota
	reader
)

type caller func(context.Context, actor, string, string, map[string]string, []byte) (*skirk.HTTPResult, error)

type parentRef struct {
	ID string
}

type driveFile struct {
	ID              string
	Title           string
	Description     string
	ETag            string
	Version         string
	Parents         []parentRef
	OwnedByMe       bool
	IsAppAuthorized bool
}

type ownedTarget struct {
	ID    string
	Title string
}

type apiMetrics struct {
	Calls         map[string]int
	Statuses      map[string]int
	NominalUnits  int64
	UpperUnits    int64
	ResponseBytes int64
	RequestBytes  int64
}

type probe struct {
	call    caller
	folder  string
	out     string
	stem    string
	targets []ownedTarget
	key     [32]byte
	session [16]byte
	mu      sync.Mutex
	active  int64
	cleanup int64
	metrics apiMetrics
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

func validTag(s string) bool {
	if len(s) < 3 || len(s) > 512 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for _, c := range s[1 : len(s)-1] {
		if c < 0x21 || c > 0x7e || c == '"' {
			return false
		}
	}
	return true
}

func checkStatus(r *skirk.HTTPResult, want ...int) error {
	if r == nil {
		return errors.New("missing Drive response")
	}
	for _, code := range want {
		if r.Status == code {
			return nil
		}
	}
	return fmt.Errorf("unexpected Drive status %d", r.Status)
}

func parseOne[T any](body []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return v, errors.New("trailing or invalid JSON")
	}
	return v, nil
}

func apiCost(method, path string) (string, int64, error) {
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v2/files/generateIds?"):
		return "files.generateIds", 5, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v2/files?"):
		return "files.list", 100, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v2/files/"):
		return "files.get", 5, nil
	case method == http.MethodPost && strings.HasPrefix(path, "/drive/v2/files?"):
		return "files.insert", 50, nil
	case method == http.MethodPatch && strings.HasPrefix(path, "/drive/v2/files/"):
		return "files.patch", 50, nil
	case method == http.MethodDelete && strings.HasPrefix(path, "/drive/v2/files/"):
		return "files.delete", 50, nil
	default:
		return "", 0, errors.New("method/path outside fixed probe allowlist")
	}
}

func (p *probe) request(ctx context.Context, cleanup bool, who actor, method, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
	label, units, err := apiCost(method, path)
	if err != nil {
		return nil, err
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Cache-Control"] = "no-cache"
	headers["Accept-Encoding"] = "identity"
	if len(body) != 0 {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	p.mu.Lock()
	if cleanup {
		if p.cleanup+8*units > cleanupUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("cleanup quota guard")
		}
		p.cleanup += 8 * units
	} else {
		if p.active+8*units > activeUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("active quota guard")
		}
		p.active += 8 * units
	}
	if p.metrics.Calls == nil {
		p.metrics.Calls = map[string]int{}
		p.metrics.Statuses = map[string]int{}
	}
	p.metrics.Calls[label]++
	p.metrics.NominalUnits += units
	p.metrics.UpperUnits += 8 * units
	p.metrics.RequestBytes += int64(len(body))
	p.mu.Unlock()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := p.call(reqCtx, who, method, path, headers, body)
	p.mu.Lock()
	if r != nil {
		p.metrics.Statuses[fmt.Sprintf("%s:%d", label, r.Status)]++
		if len(r.Body) <= maxResponseBytes {
			p.metrics.ResponseBytes += int64(len(r.Body))
		}
	}
	p.mu.Unlock()
	if err != nil {
		return r, err
	}
	if r == nil || len(r.Body) > maxResponseBytes {
		return r, errors.New("missing or oversized Drive response")
	}
	return r, nil
}

func (p *probe) metricCopy() apiMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.metrics
	m.Calls = make(map[string]int, len(p.metrics.Calls))
	m.Statuses = make(map[string]int, len(p.metrics.Statuses))
	for k, v := range p.metrics.Calls {
		m.Calls[k] = v
	}
	for k, v := range p.metrics.Statuses {
		m.Statuses[k] = v
	}
	return m
}

func metricDiff(a, b apiMetrics) apiMetrics {
	m := apiMetrics{Calls: map[string]int{}, Statuses: map[string]int{}, NominalUnits: b.NominalUnits - a.NominalUnits, UpperUnits: b.UpperUnits - a.UpperUnits, ResponseBytes: b.ResponseBytes - a.ResponseBytes, RequestBytes: b.RequestBytes - a.RequestBytes}
	for k, v := range b.Calls {
		if v-a.Calls[k] != 0 {
			m.Calls[k] = v - a.Calls[k]
		}
	}
	for k, v := range b.Statuses {
		if v-a.Statuses[k] != 0 {
			m.Statuses[k] = v - a.Statuses[k]
		}
	}
	return m
}

func (p *probe) recoveryPath() string { return p.out + ".recovery.json" }

func (p *probe) persistRecovery() error {
	body, err := json.Marshal(map[string]any{"folder": p.folder, "targets": p.targets})
	if err != nil {
		return err
	}
	tmp := p.recoveryPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(body, '\n'))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p.recoveryPath()); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(tmp))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (p *probe) reserve(ctx context.Context, title string) (ownedTarget, error) {
	path := "/drive/v2/files/generateIds?maxResults=1&fields=ids"
	r, err := p.request(ctx, false, writer, http.MethodGet, path, nil, nil)
	if err != nil {
		return ownedTarget{}, err
	}
	if err = checkStatus(r, 200); err != nil {
		return ownedTarget{}, err
	}
	v, err := parseOne[struct{ IDs []string }](r.Body)
	if err != nil || len(v.IDs) != 1 || !validID(v.IDs[0]) {
		return ownedTarget{}, errors.New("invalid generated ID")
	}
	for _, t := range p.targets {
		if t.ID == v.IDs[0] {
			return ownedTarget{}, errors.New("generated duplicate ID")
		}
	}
	t := ownedTarget{ID: v.IDs[0], Title: title}
	p.targets = append(p.targets, t)
	if err := p.persistRecovery(); err != nil {
		return ownedTarget{}, err
	}
	return t, nil
}

func filePath(t ownedTarget) string { return "/drive/v2/files/" + url.PathEscape(t.ID) }

const fileFields = "id,title,description,parents(id),ownedByMe,isAppAuthorized,etag,version"

func (p *probe) requireOwned(f driveFile, t ownedTarget) error {
	if f.ID != t.ID || f.Title != t.Title || len(f.Parents) != 1 || f.Parents[0].ID != p.folder || !f.OwnedByMe || !f.IsAppAuthorized {
		return errors.New("Drive file identity/owner/app/parent mismatch")
	}
	return nil
}

func (p *probe) create(ctx context.Context, t ownedTarget, description string) (driveFile, error) {
	body, _ := json.Marshal(map[string]any{"id": t.ID, "title": t.Title, "description": description, "mimeType": "application/octet-stream", "parents": []map[string]string{{"id": p.folder}}})
	r, err := p.request(ctx, false, writer, http.MethodPost, "/drive/v2/files?fields="+fileFields, nil, body)
	if err != nil {
		return driveFile{}, err
	}
	if err = checkStatus(r, 200, 201); err != nil {
		return driveFile{}, err
	}
	f, err := parseOne[driveFile](r.Body)
	if err != nil || p.requireOwned(f, t) != nil || f.Description != description {
		return driveFile{}, errors.New("create exact readback mismatch")
	}
	return f, nil
}

func chooseTag(r *skirk.HTTPResult, f driveFile) (string, error) {
	h := r.Header.Get("ETag")
	if !validTag(f.ETag) && f.ETag != "" || !validTag(h) && h != "" {
		return "", errors.New("malformed ETag")
	}
	if h != "" && f.ETag != "" && h != f.ETag {
		return "", errors.New("HTTP/resource ETag mismatch")
	}
	if f.ETag != "" {
		return f.ETag, nil
	}
	if h != "" {
		return h, nil
	}
	return "", errors.New("missing ETag")
}

func (p *probe) get(ctx context.Context, t ownedTarget) (driveFile, string, error) {
	r, err := p.request(ctx, false, reader, http.MethodGet, filePath(t)+"?fields="+fileFields, nil, nil)
	if err != nil {
		return driveFile{}, "", err
	}
	if err = checkStatus(r, 200); err != nil {
		return driveFile{}, "", err
	}
	f, err := parseOne[driveFile](r.Body)
	if err != nil {
		return driveFile{}, "", err
	}
	if err = p.requireOwned(f, t); err != nil {
		return driveFile{}, "", err
	}
	tag, err := chooseTag(r, f)
	return f, tag, err
}

func (p *probe) patch(ctx context.Context, t ownedTarget, tag, desc string) (driveFile, int, error) {
	if !validTag(tag) {
		return driveFile{}, 0, errors.New("invalid conditional tag")
	}
	body, _ := json.Marshal(map[string]string{"description": desc})
	r, err := p.request(ctx, false, writer, http.MethodPatch, filePath(t)+"?fields="+fileFields, map[string]string{"If-Match": tag}, body)
	if err != nil {
		return driveFile{}, 0, err
	}
	if r.Status == 412 {
		return driveFile{}, 412, nil
	}
	if err = checkStatus(r, 200); err != nil {
		return driveFile{}, r.Status, err
	}
	f, err := parseOne[driveFile](r.Body)
	if err != nil || p.requireOwned(f, t) != nil || f.Description != desc {
		return driveFile{}, 200, errors.New("conditional patch readback mismatch")
	}
	return f, 200, nil
}

func escapeQuery(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}

func (p *probe) list(ctx context.Context, cleanup bool, prefix string) ([]driveFile, error) {
	q := "trashed=false and '" + escapeQuery(p.folder) + "' in parents and title contains '" + escapeQuery(prefix) + "'"
	query := url.Values{"q": {q}, "fields": {"nextPageToken,incompleteSearch,items(" + fileFields + ")"}, "maxResults": {"100"}}
	r, err := p.request(ctx, cleanup, reader, http.MethodGet, "/drive/v2/files?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	if err = checkStatus(r, 200); err != nil {
		return nil, err
	}
	v, err := parseOne[struct {
		Items            []driveFile
		NextPageToken    string
		IncompleteSearch bool
	}](r.Body)
	if err != nil || v.NextPageToken != "" || v.IncompleteSearch {
		return nil, errors.New("invalid or incomplete Drive list")
	}
	out := make([]driveFile, 0, len(v.Items))
	seen := map[string]bool{}
	for _, f := range v.Items {
		if !strings.HasPrefix(f.Title, prefix) {
			continue
		}
		if seen[f.ID] || len(f.Parents) != 1 || f.Parents[0].ID != p.folder || !f.OwnedByMe || !f.IsAppAuthorized {
			return nil, errors.New("foreign or duplicate prefix result")
		}
		seen[f.ID] = true
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *probe) cleanupOwned(visibilityDelay time.Duration) (exact, immediate, delayed bool) {
	if len(p.targets) == 0 {
		return true, true, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exact = true
	for _, t := range p.targets {
		r, err := p.request(ctx, true, writer, http.MethodGet, filePath(t)+"?fields="+fileFields, nil, nil)
		if err != nil {
			exact = false
			continue
		}
		if r.Status == 200 {
			f, e := parseOne[driveFile](r.Body)
			if e != nil || p.requireOwned(f, t) != nil {
				exact = false
				continue // Do not delete an unproven file.
			}
			r, e = p.request(ctx, true, writer, http.MethodDelete, filePath(t), nil, nil)
			if e != nil || checkStatus(r, 204, 404) != nil {
				exact = false
				continue
			}
		} else if r.Status != 404 {
			exact = false
			continue
		}
		for i := 0; i < 2; i++ {
			r, err := p.request(ctx, true, writer, http.MethodGet, filePath(t)+"?fields=id", nil, nil)
			if err != nil || checkStatus(r, 404) != nil {
				exact = false
			}
			if i == 0 {
				_ = wait(ctx, 200*time.Millisecond)
			}
		}
	}
	check := func() bool {
		items, err := p.list(ctx, true, p.stem)
		if err != nil {
			return false
		}
		return len(items) == 0
	}
	immediate = check()
	if wait(ctx, visibilityDelay) == nil {
		delayed = check()
	}
	if exact && immediate && delayed {
		_ = os.Remove(p.recoveryPath())
	}
	return
}

func wait(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newProbe(folder, out string) (*probe, error) {
	p := &probe{folder: folder, out: out}
	if _, err := rand.Read(p.key[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(p.session[:]); err != nil {
		return nil, err
	}
	p.stem = "skirk-fixed-rdv-" + hex.EncodeToString(p.session[:])
	return p, nil
}
