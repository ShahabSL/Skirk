package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type fakeDrive struct {
	mu                   sync.Mutex
	fileID               string
	name                 string
	folder               string
	fileExists           bool
	comments             map[string]*comment
	nextComment          int
	nextReply            int
	postDelay            time.Duration
	inflight             int
	peak                 int
	denyPeer             bool
	failPeerGet          bool
	waitForPostBeforeGet bool
}

func newFakeDrive() *fakeDrive { return &fakeDrive{comments: map[string]*comment{}} }

func fakeResult(status int, v any) (*skirk.HTTPResult, error) {
	b, err := json.Marshal(v)
	return &skirk.HTTPResult{Status: status, Body: b, Proto: "HTTP/2.0"}, err
}

func (f *fakeDrive) call(ctx context.Context, who actor, method, path string, _ map[string]string, body []byte) (*skirk.HTTPResult, error) {
	if method == http.MethodPost && strings.HasSuffix(path, "/replies?fields=id,content") {
		f.mu.Lock()
		f.inflight++
		if f.inflight > f.peak {
			f.peak = f.inflight
		}
		f.mu.Unlock()
		if f.postDelay > 0 {
			if err := wait(ctx, f.postDelay); err != nil {
				f.mu.Lock()
				f.inflight--
				f.mu.Unlock()
				return nil, err
			}
		}
		defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	}
	if who == peer && method == http.MethodGet && strings.Contains(path, "/comments/") && f.waitForPostBeforeGet {
		for {
			f.mu.Lock()
			started := f.inflight > 0
			f.mu.Unlock()
			if started {
				break
			}
			if err := wait(ctx, time.Millisecond); err != nil {
				return nil, err
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if who == peer && f.denyPeer || who == peer && method == http.MethodGet && f.failPeerGet {
		return fakeResult(403, map[string]any{"error": "denied"})
	}
	if method == http.MethodGet && strings.Contains(path, "generateIds") {
		return fakeResult(200, map[string]any{"ids": []string{"generated_file_001"}})
	}
	if method == http.MethodPost && strings.HasPrefix(path, "/drive/v3/files?fields=") {
		var v struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		if f.fileExists || len(v.Parents) != 1 {
			return fakeResult(400, map[string]any{})
		}
		f.fileID, f.name, f.folder, f.fileExists = v.ID, v.Name, v.Parents[0], true
		return fakeResult(200, map[string]any{"id": v.ID, "name": v.Name, "parents": v.Parents, "spaces": []string{"drive"}, "ownedByMe": true})
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files?") {
		q, err := url.Parse(path)
		if err != nil || q.Query().Get("q") == "" {
			return nil, errors.New("missing list query")
		}
		files := []map[string]string{}
		if f.fileExists {
			files = append(files, map[string]string{"id": f.fileID, "name": f.name})
		}
		return fakeResult(200, map[string]any{"files": files})
	}
	if method == http.MethodDelete && path == "/drive/v3/files/"+f.fileID {
		if !f.fileExists {
			return fakeResult(404, map[string]any{})
		}
		f.fileExists = false
		f.comments = map[string]*comment{}
		return &skirk.HTTPResult{Status: 204, Proto: "HTTP/2.0"}, nil
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/") && !strings.Contains(path, "/comments/") {
		if !f.fileExists {
			return fakeResult(404, map[string]any{})
		}
		return fakeResult(200, map[string]any{"id": f.fileID, "name": f.name, "parents": []string{f.folder}, "ownedByMe": true})
	}
	if !f.fileExists || !strings.HasPrefix(path, "/drive/v3/files/"+f.fileID+"/comments") {
		return fakeResult(404, map[string]any{})
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/comments?fields=id,content") {
		var v comment
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		f.nextComment++
		v.ID = fmt.Sprintf("comment_%03d", f.nextComment)
		f.comments[v.ID] = &v
		return fakeResult(200, v)
	}
	parts := strings.Split(strings.Split(path, "?")[0], "/")
	if len(parts) < 7 {
		return fakeResult(404, map[string]any{})
	}
	commentID := parts[6]
	c, ok := f.comments[commentID]
	if !ok {
		return fakeResult(404, map[string]any{})
	}
	if method == http.MethodGet {
		return fakeResult(200, c)
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/replies?fields=id,content") {
		var v reply
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		f.nextReply++
		v.ID = fmt.Sprintf("reply_%03d", f.nextReply)
		c.Replies = append(c.Replies, v)
		return fakeResult(200, v)
	}
	return fakeResult(400, map[string]any{})
}

func testProbe(t *testing.T, f *fakeDrive) *probe {
	t.Helper()
	p := &probe{folder: "shared-folder", name: "skirk-comment-probe-test", out: filepath.Join(t.TempDir(), "result.json"), call: f.call, pollDelay: time.Millisecond, cleanupDelay: time.Millisecond, result: report{Schema: 1, Calls: map[string]int{}}}
	p.session[0] = 1
	p.key[0] = 42
	return p
}

func TestFullPrimitiveAndExactCleanup(t *testing.T) {
	f := newFakeDrive()
	f.postDelay = 5 * time.Millisecond
	p := testProbe(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.result.Arms) != 4 || !p.result.LostResponseFound || p.result.DuplicateRawCount != 2 || p.result.DuplicateDelivered != 1 || !p.result.RotationVerified || len(p.result.Sizes) != 3 {
		t.Fatalf("missing protocol phases: %+v", p.result)
	}
	for i, a := range p.result.Arms {
		if a.Verified != 8 || a.Concurrency != 1<<i || len(a.Samples) != 8 || a.GetCalls == 0 || a.ReplyCreates != 8 || a.NominalUnits != int64(400+5*a.GetCalls) || a.UpperUnits != 8*a.NominalUnits {
			t.Fatalf("incomplete arm: %+v", a)
		}
	}
	if f.peak < 8 {
		t.Fatalf("eight-writer phase never overlapped: peak=%d", f.peak)
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupComments404 || !p.result.CleanupImmediate || !p.result.CleanupDelayed {
		t.Fatalf("cleanup failed: %+v", p.result)
	}
	if _, err := os.Stat(p.out + ".recovery.json"); !os.IsNotExist(err) {
		t.Fatal("recovery ledger retained after exact cleanup", err)
	}
	if p.result.Calls["comments.list"] != 0 || p.result.Calls["replies.list"] != 0 || p.result.Calls["comments.get"] == 0 {
		t.Fatalf("probe used list discovery: %+v", p.result.Calls)
	}
}

func TestPeerPermissionFailureStillCleansOwnedFile(t *testing.T) {
	f := newFakeDrive()
	f.denyPeer = true
	p := testProbe(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.run(ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("did not reject cross-account failure: %v", err)
	}
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupComments404 || !p.result.CleanupDelayed {
		t.Fatal("failed setup left owned file")
	}
}

func TestBadReplyAndDuplicateContent(t *testing.T) {
	p := testProbe(t, newFakeDrive())
	rec, err := p.newRecord(1, 64)
	if err != nil {
		t.Fatal(err)
	}
	good := comment{ID: "comment_001", Replies: []reply{{ID: "reply_001", Content: rec.body}}}
	count, err := p.inspect(good, map[uint64]sealedRecord{1: rec})
	if err != nil || count[1] != 1 {
		t.Fatal("good AEAD reply rejected", err)
	}
	bad := good
	bad.Replies = append([]reply(nil), good.Replies...)
	bad.Replies[0].Content += "!"
	if _, err := p.inspect(bad, map[uint64]sealedRecord{1: rec}); err == nil {
		t.Fatal("altered reply accepted")
	}
	bad = good
	bad.Replies = append(bad.Replies, bad.Replies[0])
	if _, err := p.inspect(bad, map[uint64]sealedRecord{1: rec}); err == nil {
		t.Fatal("same provider reply ID accepted twice")
	}
	good.Replies = append(good.Replies, reply{ID: "reply_002", Content: rec.body})
	count, err = p.inspect(good, map[uint64]sealedRecord{1: rec})
	if err != nil || count[1] != 2 {
		t.Fatal("authenticated duplicate not recognized", err)
	}
}

func TestGuardAndAllowlist(t *testing.T) {
	p := testProbe(t, newFakeDrive())
	if _, _, err := cost("POST", "/drive/v3/files/x/permissions"); err == nil {
		t.Fatal("out-of-scope method admitted")
	}
	if label, units, err := cost("GET", "/drive/v3/files/x/comments/y?fields=id"); err != nil || label != "comments.get" || units != 5 {
		t.Fatal("known comment priced as a list", label, units, err)
	}
	p.active = activeUpperLimit
	if _, err := p.request(context.Background(), false, owner, "GET", "/drive/v3/files/generateIds?count=1&fields=ids", nil); err == nil {
		t.Fatal("active guard ignored")
	}
	p.fileID = "generated_file_001"
	if _, err := p.request(context.Background(), true, owner, "DELETE", p.filePath(), nil); err != nil {
		t.Fatal("active exhaustion blocked reserved cleanup", err)
	}
}

func TestReadFailureJoinsOutstandingCreates(t *testing.T) {
	f := newFakeDrive()
	f.postDelay = time.Second
	p := testProbe(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.reserveFile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.createFile(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := p.createComment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.failPeerGet = true
	f.waitForPostBeforeGet = true
	if _, err := p.runArm(ctx, id, 8, 1); err == nil {
		t.Fatal("peer read failure accepted")
	}
	f.mu.Lock()
	inflight := f.inflight
	f.mu.Unlock()
	if inflight != 0 {
		t.Fatal("returned before canceled creates joined", inflight)
	}
	f.waitForPostBeforeGet = false
	f.failPeerGet = false
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupComments404 || !p.result.CleanupDelayed {
		t.Fatal("failed arm left owned file")
	}
}

func TestCleanupRefusesUnprovenFile(t *testing.T) {
	f := newFakeDrive()
	p := testProbe(t, f)
	ctx := context.Background()
	if err := p.reserveFile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.createFile(ctx); err != nil {
		t.Fatal(err)
	}
	original := p.name
	p.name = "different-name"
	p.cleanupOwned()
	f.mu.Lock()
	stillThere := f.fileExists
	f.mu.Unlock()
	if !stillThere || p.result.CleanupExact404 {
		t.Fatal("cleanup deleted a file without exact identity proof")
	}
	p.name = original
	p.cleanupOwned()
	if !p.result.CleanupExact404 || !p.result.CleanupDelayed {
		t.Fatal("proven owned file did not clean")
	}
}
