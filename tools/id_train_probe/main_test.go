package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func testProbe(t *testing.T) *probe {
	t.Helper()
	p := &probe{folder: "test-folder", prefix: "idtrain/test/", out: filepath.Join(t.TempDir(), "report.json"), key: make([]byte, 32), calls: map[string]int{}, names: map[string]string{}}
	p.sid[0] = 1
	return p
}

func TestPacketAuthenticatesIdentitySessionSequenceAndBytes(t *testing.T) {
	p := testProbe(t)
	want := []byte("hello")
	sealed, e := p.sealed(7, want)
	if e != nil {
		t.Fatal(e)
	}
	f := packet{ID: "id", Name: "name", Description: sealed, Parents: []string{p.folder}, Spaces: []string{"drive"}}
	if e = p.verify(f, "id", "name", 7, want); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name string
		file packet
		seq  int
		want []byte
	}{
		{"wrong-sequence", f, 8, want}, {"wrong-body", f, 7, []byte("other")},
		{"wrong-parent", packet{ID: f.ID, Name: f.Name, Description: f.Description, Parents: []string{"other"}, Spaces: f.Spaces}, 7, want},
		{"wrong-id", packet{ID: "other", Name: f.Name, Description: f.Description, Parents: f.Parents, Spaces: f.Spaces}, 7, want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p.verify(tc.file, "id", "name", tc.seq, tc.want) == nil {
				t.Fatal("accepted corrupt or foreign packet")
			}
		})
	}
	p.sid[0] = 2
	if p.verify(f, "id", "name", 7, want) == nil {
		t.Fatal("accepted old session")
	}
}

func TestTrainAndListDeliverEveryScheduledPayloadInOrder(t *testing.T) {
	for armIndex, mode := range []string{"id-train", "list"} {
		t.Run(mode, func(t *testing.T) {
			p := testProbe(t)
			var mu sync.Mutex
			files := map[string]packet{}
			p.call = func(ctx context.Context, method, path string, _ map[string]string, body []byte) (*skirk.HTTPResult, error) {
				mu.Lock()
				defer mu.Unlock()
				r := &skirk.HTTPResult{Status: 200}
				if strings.Contains(path, "generateIds") {
					r.Body = []byte(`{"ids":["id-a","id-b","id-c"]}`)
					return r, nil
				}
				if method == http.MethodPost {
					var f packet
					if e := json.Unmarshal(body, &f); e != nil {
						return nil, e
					}
					f.Spaces = []string{"drive"}
					files[f.ID] = f
					r.Body = []byte(`{"id":"` + f.ID + `"}`)
					return r, nil
				}
				if strings.HasPrefix(path, "/drive/v3/files?") {
					v := struct {
						Files []packet `json:"files"`
					}{}
					for _, f := range files {
						v.Files = append(v.Files, f)
					}
					r.Body, _ = json.Marshal(v)
					return r, nil
				}
				id := strings.Split(strings.TrimPrefix(path, "/drive/v3/files/"), "?")[0]
				f, ok := files[id]
				if !ok {
					r.Status = 404
					return r, nil
				}
				r.Body, _ = json.Marshal(f)
				return r, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, e := p.runArm(ctx, mode, armIndex, 3, 256, 100, 100)
			if e != nil {
				t.Fatal(e)
			}
			if a.Verified != 3 || len(a.Samples) != 3 {
				t.Fatalf("lost frames: %+v", a)
			}
			for i, s := range a.Samples {
				if s.Sequence != i {
					t.Fatal("reordered delivery")
				}
			}
			if mode == "id-train" && a.Expected404 == 0 {
				t.Fatal("did not exercise precreate404")
			}
		})
	}
}

func TestFailedReadWaitsForCanceledWriterBeforeReturn(t *testing.T) {
	p := testProbe(t)
	entered := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	p.call = func(ctx context.Context, method, path string, _ map[string]string, body []byte) (*skirk.HTTPResult, error) {
		if strings.Contains(path, "generateIds") {
			return &skirk.HTTPResult{Status: 200, Body: []byte(`{"ids":["id"]}`)}, nil
		}
		if method == http.MethodPost {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			close(finished)
			return nil, ctx.Err()
		}
		select {
		case <-entered:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &skirk.HTTPResult{Status: 403}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, e := p.runArm(ctx, "id-train", 0, 1, 64, 100, 100)
	if e == nil {
		t.Fatal("expected error")
	}
	select {
	case <-finished:
	default:
		t.Fatal("returned with outstanding writer; cleanup could race")
	}
}

func TestPrefixListRejectsIncompleteOrForeignResult(t *testing.T) {
	for _, body := range []string{`{"nextPageToken":"next","files":[]}`, `{"incompleteSearch":true,"files":[]}`} {
		p := testProbe(t)
		p.call = func(context.Context, string, string, map[string]string, []byte) (*skirk.HTTPResult, error) {
			return &skirk.HTTPResult{Status: 200, Body: []byte(body)}, nil
		}
		if _, e := p.list(context.Background(), p.prefix); e == nil {
			t.Fatal("accepted partial or foreign discovery")
		}
	}
	p := testProbe(t)
	deletes := 0
	p.call = func(_ context.Context, method string, _ string, _ map[string]string, _ []byte) (*skirk.HTTPResult, error) {
		if method == http.MethodDelete {
			deletes++
		}
		return &skirk.HTTPResult{Status: 200, Body: []byte(`{"files":[{"id":"foreign-id","name":"foreign"}]}`)}, nil
	}
	files, e := p.list(context.Background(), p.prefix)
	if e != nil || len(files) != 0 || p.listFalseMatches != 1 || deletes != 0 || len(p.ids) != 0 {
		t.Fatalf("foreign result processed or cleanup-claimed: files=%v error=%v matches=%d deletes=%d", files, e, p.listFalseMatches, deletes)
	}
}

func TestCostAndEscapedPrefix(t *testing.T) {
	if unitsFor("GET", "/drive/v3/files/generateIds?count=2") != 5 || unitsFor("GET", "/drive/v3/files?q=x") != 100 || unitsFor("GET", "/drive/v3/files/id?fields=id") != 5 || unitsFor("DELETE", "/drive/v3/files/id") != 50 {
		t.Fatal("wrong unit model")
	}
	p := testProbe(t)
	p.folder = "a'b"
	p.call = func(_ context.Context, _ string, path string, _ map[string]string, _ []byte) (*skirk.HTTPResult, error) {
		u, e := url.Parse(path)
		if e != nil {
			return nil, e
		}
		if !strings.Contains(u.Query().Get("q"), "'a\\'b' in parents") {
			return nil, fmt.Errorf("unescaped query")
		}
		return &skirk.HTTPResult{Status: 200, Body: []byte(`{"files":[]}`)}, nil
	}
	if _, e := p.list(context.Background(), "idtrain/test/"); e != nil {
		t.Fatal(e)
	}
}

func TestParallelWriterPreservesOrderAcrossReorderedCreates(t *testing.T) {
	for _, mode := range []string{"id-train", "list"} {
		t.Run(mode, func(t *testing.T) {
			p := testProbe(t)
			p.writerConcurrency = 2
			p.idTrainCatchup = true
			var mu sync.Mutex
			files := map[string]packet{}
			var createOrder []string
			p.call = func(ctx context.Context, method, path string, _ map[string]string, body []byte) (*skirk.HTTPResult, error) {
				if strings.Contains(path, "generateIds") {
					return &skirk.HTTPResult{Status: 200, Body: []byte(`{"ids":["id-a","id-b","id-c"]}`)}, nil
				}
				if method == http.MethodPost {
					var f packet
					if e := json.Unmarshal(body, &f); e != nil {
						return nil, e
					}
					delay := 10 * time.Millisecond
					if f.ID == "id-a" {
						delay = 450 * time.Millisecond
					}
					if e := wait(ctx, delay); e != nil {
						return nil, e
					}
					f.Spaces = []string{"drive"}
					mu.Lock()
					files[f.ID] = f
					createOrder = append(createOrder, f.ID)
					mu.Unlock()
					return &skirk.HTTPResult{Status: 200, Body: []byte(`{}`)}, nil
				}
				mu.Lock()
				defer mu.Unlock()
				if strings.HasPrefix(path, "/drive/v3/files?") {
					var v struct {
						Files []packet `json:"files"`
					}
					for _, f := range files {
						v.Files = append(v.Files, f)
					}
					b, _ := json.Marshal(v)
					return &skirk.HTTPResult{Status: 200, Body: b}, nil
				}
				id := strings.Split(strings.TrimPrefix(path, "/drive/v3/files/"), "?")[0]
				f, ok := files[id]
				if !ok {
					return &skirk.HTTPResult{Status: 404}, nil
				}
				b, _ := json.Marshal(f)
				return &skirk.HTTPResult{Status: 200, Body: b}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, e := p.runArm(ctx, mode, 0, 3, 256, 100, 100)
			if e != nil {
				t.Fatal(e)
			}
			if len(createOrder) != 3 || createOrder[0] == "id-a" {
				t.Fatalf("reordering was not exercised: %v", createOrder)
			}
			if a.Verified != 3 {
				t.Fatalf("lost reordered packet: %+v", a)
			}
			for i, s := range a.Samples {
				if s.Sequence != i {
					t.Fatal("out-of-order delivery")
				}
				if s.ScheduledMS+float64(i*100) < 450 {
					t.Fatal("delivered past missing first record")
				}
			}
		})
	}
}
