// comment_thread_probe tests a known Drive comment as a bounded append-only
// metadata carrier. It is a research primitive, not a Skirk transport.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
	"github.com/ShahabSL/Skirk/tools/internal/probeledger"
)

const (
	marker            = "skirk-comment-probe-v1:"
	activeUpperLimit  = int64(70000)
	cleanupUpperLimit = int64(10000)
	maxResponseBytes  = 512 << 10
	defaultPollDelay  = 100 * time.Millisecond
)

type actor uint8

const (
	owner actor = iota
	peer
)

type caller func(context.Context, actor, string, string, map[string]string, []byte) (*skirk.HTTPResult, error)

type reply struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Deleted bool   `json:"deleted"`
}

type comment struct {
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Replies []reply `json:"replies"`
}

type sample struct {
	Sequence         uint64  `json:"sequence"`
	StartToVisibleMS float64 `json:"start_to_visible_ms"`
	PostMS           float64 `json:"post_ms"`
	ResponseToSeenMS float64 `json:"response_to_seen_ms"`
}

type arm struct {
	Concurrency   int      `json:"concurrency"`
	Verified      int      `json:"verified"`
	GetCalls      int      `json:"comment_get_calls"`
	ReplyCreates  int      `json:"reply_create_calls"`
	NominalUnits  int64    `json:"nominal_units"`
	UpperUnits    int64    `json:"eight_attempt_upper_units"`
	ResponseBytes int64    `json:"response_bytes"`
	P50MS         float64  `json:"start_to_visible_p50_ms"`
	P95MS         float64  `json:"start_to_visible_p95_ms"`
	Samples       []sample `json:"samples"`
}

type sizeCase struct {
	PlaintextBytes int  `json:"plaintext_bytes"`
	CreateStatus   int  `json:"create_status"`
	Verified       bool `json:"verified"`
}

type report struct {
	Schema             int            `json:"schema_version"`
	Route              string         `json:"route"`
	Arms               []arm          `json:"arms"`
	LostResponseFound  bool           `json:"lost_response_found"`
	DuplicateRawCount  int            `json:"duplicate_raw_count"`
	DuplicateDelivered int            `json:"duplicate_logical_deliveries"`
	RotationVerified   bool           `json:"rotation_verified"`
	Sizes              []sizeCase     `json:"size_cases"`
	Calls              map[string]int `json:"calls"`
	NominalUnits       int64          `json:"nominal_units"`
	UpperUnits         int64          `json:"eight_attempt_upper_units"`
	ResponseBytes      int64          `json:"response_bytes"`
	RequestBodyBytes   int64          `json:"request_body_bytes"`
	CleanupExact404    bool           `json:"cleanup_exact_404_twice"`
	CleanupComments404 bool           `json:"cleanup_comment_ids_404"`
	CleanupImmediate   bool           `json:"cleanup_immediate_prefix_empty"`
	CleanupDelayed     bool           `json:"cleanup_delayed_prefix_empty"`
	Failure            string         `json:"failure,omitempty"`
	OK                 bool           `json:"ok"`
}

type probe struct {
	call         caller
	folder       string
	name         string
	out          string
	key          [32]byte
	session      [16]byte
	fileID       string
	commentIDs   []string
	known        map[uint64]sealedRecord
	pollDelay    time.Duration
	cleanupDelay time.Duration
	mu           sync.Mutex
	active       int64
	cleanup      int64
	result       report
}

type sealedRecord struct {
	seq   uint64
	body  string
	plain []byte
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 256 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func cost(method, path string) (string, int64, error) {
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/generateIds?"):
		return "files.generateIds", 5, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files?"):
		return "files.list", 100, nil
	case method == http.MethodGet && strings.Contains(path, "/comments/"):
		return "comments.get", 5, nil
	case method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/"):
		return "files.get", 5, nil
	case method == http.MethodPost && strings.HasSuffix(path, "/replies?fields=id,content"):
		return "replies.create", 50, nil
	case method == http.MethodPost && strings.HasSuffix(path, "/comments?fields=id,content"):
		return "comments.create", 50, nil
	case method == http.MethodPost && strings.HasPrefix(path, "/drive/v3/files?fields="):
		return "files.create", 50, nil
	case method == http.MethodDelete && strings.HasPrefix(path, "/drive/v3/files/"):
		return "files.delete", 50, nil
	default:
		return "", 0, errors.New("probe method/path outside allowlist")
	}
}

func (p *probe) request(ctx context.Context, cleanup bool, who actor, method, path string, body []byte) (*skirk.HTTPResult, error) {
	label, units, err := cost(method, path)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if cleanup {
		if p.cleanup+8*units > cleanupUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("cleanup quota reserve exhausted")
		}
		p.cleanup += 8 * units
	} else {
		if p.active+8*units > activeUpperLimit {
			p.mu.Unlock()
			return nil, errors.New("active quota guard")
		}
		p.active += 8 * units
	}
	p.result.NominalUnits += units
	p.result.UpperUnits += 8 * units
	p.result.RequestBodyBytes += int64(len(body))
	p.result.Calls[label]++
	p.mu.Unlock()
	headers := map[string]string{"Cache-Control": "no-cache", "Accept-Encoding": "identity"}
	if len(body) > 0 {
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := p.call(requestCtx, who, method, path, headers, body)
	if err != nil {
		return r, err
	}
	if r == nil || len(r.Body) > maxResponseBytes {
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

func parseOne[T any](body []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	if err := dec.Decode(new(any)); err != nil {
		if !errors.Is(err, io.EOF) {
			return v, err
		}
	} else {
		return v, errors.New("trailing JSON")
	}
	return v, nil
}

func (p *probe) persistRecovery() error {
	if p.out == "" {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"file_id": p.fileID, "name": p.name, "folder": p.folder, "comment_ids": p.commentIDs})
	path := p.out + ".recovery.json"
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return probeledger.SyncParent(path)
}

func (p *probe) reserveFile(ctx context.Context) error {
	r, err := p.request(ctx, false, owner, http.MethodGet, "/drive/v3/files/generateIds?count=1&fields=ids", nil)
	if err != nil {
		return err
	}
	if err = requireStatus(r, 200); err != nil {
		return err
	}
	v, err := parseOne[struct {
		IDs []string `json:"ids"`
	}](r.Body)
	if err != nil || len(v.IDs) != 1 || len(v.IDs[0]) < 16 || len(v.IDs[0]) > 128 || !validID(v.IDs[0]) {
		return errors.New("invalid generated Drive file ID")
	}
	p.fileID = v.IDs[0]
	return p.persistRecovery() // durable exact ID precedes any create
}

func (p *probe) filePath() string { return "/drive/v3/files/" + url.PathEscape(p.fileID) }
func (p *probe) commentPath(id string) string {
	return p.filePath() + "/comments/" + url.PathEscape(id)
}

func (p *probe) createFile(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{"id": p.fileID, "name": p.name, "mimeType": "application/octet-stream", "parents": []string{p.folder}})
	r, err := p.request(ctx, false, owner, http.MethodPost, "/drive/v3/files?fields=id,name,parents,spaces,ownedByMe", body)
	if err != nil {
		return err
	}
	if err = requireStatus(r, 200, 201); err != nil {
		return err
	}
	v, err := parseOne[struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Parents   []string `json:"parents"`
		Spaces    []string `json:"spaces"`
		OwnedByMe bool     `json:"ownedByMe"`
	}](r.Body)
	if err != nil || v.ID != p.fileID || v.Name != p.name || len(v.Parents) != 1 || v.Parents[0] != p.folder || len(v.Spaces) != 1 || v.Spaces[0] != "drive" || !v.OwnedByMe {
		return errors.New("created file identity/ownership mismatch")
	}
	return nil
}

func (p *probe) createComment(ctx context.Context) (string, error) {
	body := []byte(`{"content":"skirk-comment-anchor-v1"}`)
	r, err := p.request(ctx, false, owner, http.MethodPost, p.filePath()+"/comments?fields=id,content", body)
	if err != nil {
		return "", err
	}
	if err = requireStatus(r, 200, 201); err != nil {
		return "", err
	}
	v, err := parseOne[comment](r.Body)
	if err != nil || !validID(v.ID) || v.Content != "skirk-comment-anchor-v1" {
		return "", errors.New("created comment identity mismatch")
	}
	p.commentIDs = append(p.commentIDs, v.ID)
	if err := p.persistRecovery(); err != nil {
		return "", err
	}
	return v.ID, nil
}

func (p *probe) newRecord(seq uint64, size int) (sealedRecord, error) {
	if size < 1 || size > 16<<10 {
		return sealedRecord{}, errors.New("record size outside probe cap")
	}
	plain := make([]byte, size)
	if _, err := rand.Read(plain); err != nil {
		return sealedRecord{}, err
	}
	wire, err := skirk.Seal(p.key[:], p.session, skirk.DirectionUp, seq, plain, false)
	if err != nil {
		return sealedRecord{}, err
	}
	rec := sealedRecord{seq: seq, body: marker + base64.RawURLEncoding.EncodeToString(wire), plain: plain}
	p.mu.Lock()
	if p.known == nil {
		p.known = map[uint64]sealedRecord{}
	}
	p.known[seq] = rec
	p.mu.Unlock()
	return rec, nil
}

func (p *probe) openReply(body string) (uint64, []byte, bool, error) {
	if !strings.HasPrefix(body, marker) {
		return 0, nil, false, nil
	}
	wire, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(body, marker))
	if err != nil || len(wire) > 20<<10 {
		return 0, nil, true, errors.New("invalid reply encoding")
	}
	env, plain, err := skirk.OpenEnvelope(p.key[:], wire)
	if err != nil || env.SessionID != p.session || env.Direction != skirk.DirectionUp {
		return 0, nil, true, errors.New("reply authentication/session mismatch")
	}
	return env.Sequence, plain, true, nil
}

func (p *probe) postReply(ctx context.Context, commentID string, rec sealedRecord) (*skirk.HTTPResult, error) {
	body, _ := json.Marshal(map[string]string{"content": rec.body})
	r, err := p.request(ctx, false, owner, http.MethodPost, p.commentPath(commentID)+"/replies?fields=id,content", body)
	if err != nil {
		return r, err
	}
	if err = requireStatus(r, 200, 201); err != nil {
		return r, err
	}
	v, err := parseOne[reply](r.Body)
	if err != nil || !validID(v.ID) || v.Content != rec.body || v.Deleted {
		return r, errors.New("reply create did not return exact content/ID")
	}
	return r, nil
}

// postReplyBlind discards the provider's new reply ID. Recovery must prove
// publication by the authenticated application sequence in comments.get.
func (p *probe) postReplyBlind(ctx context.Context, commentID string, rec sealedRecord) error {
	body, _ := json.Marshal(map[string]string{"content": rec.body})
	r, err := p.request(ctx, false, owner, http.MethodPost, p.commentPath(commentID)+"/replies?fields=id,content", body)
	if err != nil {
		return err
	}
	return requireStatus(r, 200, 201)
}

func (p *probe) getComment(ctx context.Context, commentID string) (comment, error) {
	fields := url.QueryEscape("id,content,replies(id,content,deleted,createdTime),modifiedTime")
	r, err := p.request(ctx, false, peer, http.MethodGet, p.commentPath(commentID)+"?fields="+fields, nil)
	if err != nil {
		return comment{}, err
	}
	if err = requireStatus(r, 200); err != nil {
		return comment{}, err
	}
	var v struct {
		ID           string  `json:"id"`
		Content      string  `json:"content"`
		Replies      []reply `json:"replies"`
		ModifiedTime string  `json:"modifiedTime"`
	}
	if err = json.Unmarshal(r.Body, &v); err != nil || v.ID != commentID || v.Content != "skirk-comment-anchor-v1" {
		return comment{}, errors.New("comment identity or body mismatch")
	}
	return comment{ID: v.ID, Content: v.Content, Replies: v.Replies}, nil
}

func (p *probe) inspect(c comment, expected map[uint64]sealedRecord) (map[uint64]int, error) {
	counts := map[uint64]int{}
	ids := map[string]bool{}
	for _, r := range c.Replies {
		if !validID(r.ID) || ids[r.ID] || r.Deleted {
			return nil, errors.New("missing, duplicate or deleted reply identity")
		}
		ids[r.ID] = true
		seq, body, ours, err := p.openReply(r.Content)
		if err != nil {
			return nil, err
		}
		if !ours {
			continue
		}
		p.mu.Lock()
		want, ok := p.known[seq]
		p.mu.Unlock()
		if !ok || !bytes.Equal(body, want.plain) || r.Content != want.body {
			return nil, errors.New("unexpected or altered authenticated reply")
		}
		if _, wanted := expected[seq]; wanted {
			counts[seq]++
		}
	}
	return counts, nil
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

func (p *probe) pollUntil(ctx context.Context, commentID string, expected map[uint64]sealedRecord, needed int) (map[uint64]int, map[uint64]time.Time, error) {
	seen := map[uint64]time.Time{}
	for {
		c, err := p.getComment(ctx, commentID)
		if err != nil {
			return nil, nil, err
		}
		counts, err := p.inspect(c, expected)
		if err != nil {
			return nil, nil, err
		}
		now := time.Now()
		for seq := range counts {
			if seen[seq].IsZero() {
				seen[seq] = now
			}
		}
		if len(seen) == len(expected) {
			complete := true
			for _, count := range counts {
				if count < needed {
					complete = false
				}
			}
			if complete {
				return counts, seen, nil
			}
		}
		delay := p.pollDelay
		if delay == 0 {
			delay = defaultPollDelay
		}
		if err := wait(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
}

func percentile(v []float64, n int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[(len(s)*n+99)/100-1]
}

func (p *probe) runArm(parent context.Context, commentID string, concurrency int, startSeq uint64) (arm, error) {
	a := arm{Concurrency: concurrency}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	records := make([]sealedRecord, 8)
	expected := make(map[uint64]sealedRecord, len(records))
	for i := range records {
		rec, err := p.newRecord(startSeq+uint64(i), 64)
		if err != nil {
			return a, err
		}
		records[i] = rec
		expected[rec.seq] = rec
	}
	type timing struct {
		start, end time.Time
		err        error
	}
	times := make([]timing, len(records))
	p.mu.Lock()
	beginGet := p.result.Calls["comments.get"]
	beginCreate := p.result.Calls["replies.create"]
	beginNominal, beginUpper, beginBytes := p.result.NominalUnits, p.result.UpperUnits, p.result.ResponseBytes
	p.mu.Unlock()
	gate := make(chan struct{})
	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range records {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				times[i].err = ctx.Err()
				return
			}
			defer func() { <-slots }()
			select {
			case <-gate:
			case <-ctx.Done():
				times[i].err = ctx.Err()
				return
			}
			times[i].start = time.Now()
			_, times[i].err = p.postReply(ctx, commentID, records[i])
			times[i].end = time.Now()
		}(i)
	}
	close(gate)
	counts, seen, pollErr := p.pollUntil(ctx, commentID, expected, 1)
	if pollErr != nil {
		cancel()
	}
	wg.Wait() // no cleanup can race an unfinished create
	if pollErr != nil {
		return a, pollErr
	}
	for i, rec := range records {
		if times[i].err != nil || times[i].start.IsZero() || counts[rec.seq] != 1 {
			return a, errors.New("reply publication failed or was duplicated")
		}
		s := sample{Sequence: rec.seq, StartToVisibleMS: float64(seen[rec.seq].Sub(times[i].start).Microseconds()) / 1000, PostMS: float64(times[i].end.Sub(times[i].start).Microseconds()) / 1000, ResponseToSeenMS: float64(seen[rec.seq].Sub(times[i].end).Microseconds()) / 1000}
		a.Samples = append(a.Samples, s)
	}
	vals := make([]float64, len(a.Samples))
	for i, s := range a.Samples {
		vals[i] = s.StartToVisibleMS
	}
	a.P50MS, a.P95MS = percentile(vals, 50), percentile(vals, 95)
	a.Verified = len(records)
	p.mu.Lock()
	a.GetCalls = p.result.Calls["comments.get"] - beginGet
	a.ReplyCreates = p.result.Calls["replies.create"] - beginCreate
	a.NominalUnits = p.result.NominalUnits - beginNominal
	a.UpperUnits = p.result.UpperUnits - beginUpper
	a.ResponseBytes = p.result.ResponseBytes - beginBytes
	p.mu.Unlock()
	return a, nil
}

func (p *probe) lostResponse(ctx context.Context, commentID string) error {
	rec, err := p.newRecord(1000, 64)
	if err != nil {
		return err
	}
	// Deliberately discard a successful create response. The protocol may only
	// retry after polling the known thread for this exact authenticated record.
	if err = p.postReplyBlind(ctx, commentID, rec); err != nil {
		return err
	}
	expected := map[uint64]sealedRecord{rec.seq: rec}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	counts, _, err := p.pollUntil(checkCtx, commentID, expected, 1)
	if err != nil || counts[rec.seq] != 1 {
		return errors.New("lost response readback did not prove commitment")
	}
	p.result.LostResponseFound = true
	if _, err = p.postReply(ctx, commentID, rec); err != nil {
		return err
	}
	counts, _, err = p.pollUntil(checkCtx, commentID, expected, 2)
	if err != nil || counts[rec.seq] != 2 {
		return errors.New("duplicate replay not observed exactly twice")
	}
	p.result.DuplicateRawCount = 2
	p.result.DuplicateDelivered = len(counts)
	return nil
}

func (p *probe) sizeBounds(ctx context.Context, commentID string) error {
	for i, size := range []int{2048, 8192, 16384} {
		rec, err := p.newRecord(1100+uint64(i), size)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]string{"content": rec.body})
		r, err := p.request(ctx, false, owner, http.MethodPost, p.commentPath(commentID)+"/replies?fields=id,content", body)
		if err != nil {
			return err
		}
		row := sizeCase{PlaintextBytes: size, CreateStatus: r.Status}
		if r.Status == 400 || r.Status == 413 {
			p.result.Sizes = append(p.result.Sizes, row)
			continue
		}
		if err = requireStatus(r, 200, 201); err != nil {
			p.result.Sizes = append(p.result.Sizes, row)
			return err
		}
		v, err := parseOne[reply](r.Body)
		if err != nil || v.Content != rec.body || !validID(v.ID) {
			return errors.New("size create did not return exact ciphertext")
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		counts, _, e := p.pollUntil(checkCtx, commentID, map[uint64]sealedRecord{rec.seq: rec}, 1)
		cancel()
		if e != nil || counts[rec.seq] != 1 {
			return errors.New("accepted size was not visible exactly")
		}
		row.Verified = true
		p.result.Sizes = append(p.result.Sizes, row)
	}
	return nil
}

func (p *probe) rotate(ctx context.Context, oldID string) error {
	newID, err := p.createComment(ctx)
	if err != nil {
		return err
	}
	// A new thread ID is sent through the old authenticated thread. Full
	// transport would need a reciprocal ACK before retiring the old thread.
	rec, err := p.newRecord(2000, len(newID))
	if err != nil {
		return err
	}
	copy(rec.plain, newID)
	wire, err := skirk.Seal(p.key[:], p.session, skirk.DirectionUp, rec.seq, rec.plain, false)
	if err != nil {
		return err
	}
	rec.body = marker + base64.RawURLEncoding.EncodeToString(wire)
	p.mu.Lock()
	p.known[rec.seq] = rec
	p.mu.Unlock()
	if _, err = p.postReply(ctx, oldID, rec); err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	counts, _, err := p.pollUntil(checkCtx, oldID, map[uint64]sealedRecord{rec.seq: rec}, 1)
	cancel()
	if err != nil || counts[rec.seq] != 1 {
		return errors.New("rotation manifest not visible")
	}
	if _, err = p.getComment(ctx, newID); err != nil {
		return err
	}
	first, err := p.newRecord(2001, 64)
	if err != nil {
		return err
	}
	if _, err = p.postReply(ctx, newID, first); err != nil {
		return err
	}
	checkCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	counts, _, err = p.pollUntil(checkCtx, newID, map[uint64]sealedRecord{first.seq: first}, 1)
	cancel()
	if err != nil || counts[first.seq] != 1 {
		return errors.New("rotated thread did not deliver first record")
	}
	p.result.RotationVerified = true
	return nil
}

func (p *probe) run(ctx context.Context) error {
	if err := p.reserveFile(ctx); err != nil {
		return err
	}
	if err := p.createFile(ctx); err != nil {
		return err
	}
	commentID, err := p.createComment(ctx)
	if err != nil {
		return err
	}
	if _, err = p.getComment(ctx, commentID); err != nil { // peer permission/read smoke
		return err
	}
	for i, concurrency := range []int{1, 2, 4, 8} {
		a, err := p.runArm(ctx, commentID, concurrency, uint64(i*8+1))
		p.result.Arms = append(p.result.Arms, a)
		if err != nil {
			return err
		}
	}
	if err := p.lostResponse(ctx, commentID); err != nil {
		return err
	}
	if err := p.sizeBounds(ctx, commentID); err != nil {
		return err
	}
	return p.rotate(ctx, commentID)
}

func escapeQuery(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'")
}

func (p *probe) cleanupOwned() {
	if p.fileID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := p.request(ctx, true, owner, http.MethodGet, p.filePath()+"?fields=id,name,parents,ownedByMe", nil)
	if err != nil {
		return
	}
	if r.Status == 200 {
		var v struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Parents   []string `json:"parents"`
			OwnedByMe bool     `json:"ownedByMe"`
		}
		if json.Unmarshal(r.Body, &v) != nil || v.ID != p.fileID || v.Name != p.name || len(v.Parents) != 1 || v.Parents[0] != p.folder || !v.OwnedByMe {
			return // never delete an unproven file
		}
		r, err = p.request(ctx, true, owner, http.MethodDelete, p.filePath(), nil)
		if err != nil || requireStatus(r, 204, 404) != nil {
			return
		}
	} else if r.Status != 404 {
		return
	}
	checkExact := func() bool {
		for i := 0; i < 2; i++ {
			r, e := p.request(ctx, true, owner, http.MethodGet, p.filePath()+"?fields=id", nil)
			if e != nil || requireStatus(r, 404) != nil {
				return false
			}
			if i == 0 && wait(ctx, time.Second) != nil {
				return false
			}
		}
		return true
	}
	p.result.CleanupExact404 = checkExact()
	p.result.CleanupComments404 = true
	for _, id := range p.commentIDs {
		r, e := p.request(ctx, true, owner, http.MethodGet, p.commentPath(id)+"?fields=id", nil)
		if e != nil || requireStatus(r, 404) != nil {
			p.result.CleanupComments404 = false
		}
	}
	list := func() bool {
		q := "trashed=false and '" + escapeQuery(p.folder) + "' in parents and name contains '" + escapeQuery(p.name) + "'"
		params := url.Values{"q": {q}, "fields": {"nextPageToken,incompleteSearch,files(id,name)"}, "pageSize": {"100"}}
		r, e := p.request(ctx, true, owner, http.MethodGet, "/drive/v3/files?"+params.Encode(), nil)
		if e != nil || requireStatus(r, 200) != nil {
			return false
		}
		var v struct {
			Files      []struct{ ID, Name string } `json:"files"`
			Next       string                      `json:"nextPageToken"`
			Incomplete bool                        `json:"incompleteSearch"`
		}
		if json.Unmarshal(r.Body, &v) != nil || v.Next != "" || v.Incomplete {
			return false
		}
		for _, f := range v.Files {
			if f.Name == p.name || f.ID == p.fileID {
				return false
			}
		}
		return true
	}
	p.result.CleanupImmediate = list()
	delay := p.cleanupDelay
	if delay == 0 {
		delay = 31 * time.Second
	}
	if wait(ctx, delay) == nil {
		p.result.CleanupDelayed = list()
	}
	if p.result.CleanupExact404 && p.result.CleanupComments404 && p.result.CleanupImmediate && p.result.CleanupDelayed {
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
		fmt.Fprintln(os.Stderr, "isolated personal-test kit configs are required")
		os.Exit(2)
	}
	root, _ := filepath.Abs(".skirk-runs")
	_ = os.MkdirAll(root, 0700)
	out, _ := filepath.Abs(*outPath)
	if filepath.Dir(out) != root || filepath.Ext(out) != ".json" || out == root+"/.json" {
		fmt.Fprintln(os.Stderr, "fresh result directly under .skirk-runs required")
		os.Exit(2)
	}
	for _, path := range []string{out, out + ".recovery.json", out + ".recovery.json.tmp"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "result or recovery ledger already exists")
			os.Exit(2)
		}
	}
	client, e1 := skirk.LoadConfig(clientAbs)
	exit, e2 := skirk.LoadConfig(exitAbs)
	if e1 != nil || e2 != nil || client.Drive.FolderID == "" || client.Drive.FolderID == "appDataFolder" || client.Drive.FolderID != exit.Drive.FolderID {
		fmt.Fprintln(os.Stderr, "shared visible isolated Drive folder required")
		os.Exit(2)
	}
	if *routeChoice == "front" {
		if client.Route.GoogleIP == "" || exit.Route.GoogleIP == "" {
			fmt.Fprintln(os.Stderr, "pinned Google IP required for front route")
			os.Exit(2)
		}
		client.Route.Mode, exit.Route.Mode = "google_front_pinned", "google_front_pinned"
	} else {
		client.Route.Mode, exit.Route.Mode = "direct", "direct"
		client.Route.GoogleIP, exit.Route.GoogleIP = "", ""
	}
	p := &probe{folder: client.Drive.FolderID, out: out, result: report{Schema: 1, Route: *routeChoice, Calls: map[string]int{}}}
	if _, err := rand.Read(p.key[:]); err != nil {
		fmt.Fprintln(os.Stderr, "random key generation failed")
		os.Exit(1)
	}
	if _, err := rand.Read(p.session[:]); err != nil {
		fmt.Fprintln(os.Stderr, "random session generation failed")
		os.Exit(1)
	}
	p.name = "skirk-comment-probe-" + hex.EncodeToString(p.session[:])
	sources := [2]*skirk.AccessTokenSource{skirk.NewAccessTokenSource(client.Auth, client.Route), skirk.NewAccessTokenSource(exit.Auth, exit.Route)}
	clients := [2]*skirk.GoogleHTTPClient{skirk.NewGoogleHTTPClient(client.Route), skirk.NewGoogleHTTPClient(exit.Route)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	for _, source := range sources { // refresh outside measured API phases
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
	p.result.OK = p.result.Failure == "" && len(p.result.Arms) == 4 && p.result.LostResponseFound && p.result.DuplicateRawCount == 2 && p.result.DuplicateDelivered == 1 && p.result.RotationVerified && p.result.CleanupExact404 && p.result.CleanupComments404 && p.result.CleanupImmediate && p.result.CleanupDelayed
	b, _ := json.MarshalIndent(p.result, "", "  ")
	if err := os.WriteFile(out, append(b, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "result write failed")
		os.Exit(1)
	}
	fmt.Printf("ok=%t nominal_units=%d upper_units=%d cleanup=%t\n", p.result.OK, p.result.NominalUnits, p.result.UpperUnits, p.result.CleanupExact404 && p.result.CleanupImmediate && p.result.CleanupDelayed)
	if !p.result.OK {
		os.Exit(1)
	}
}
