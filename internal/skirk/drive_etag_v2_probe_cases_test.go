package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// V2 is modelled independently: title and parent refs are not v3 name/IDs,
// and the resource etag can exist without an HTTP ETag response header.
type driveETagV2Fake struct {
	p                                            *driveETagProbe
	exists, honor, bodyTag, headerTag, staticTag bool
	fullOnly, conflictingTags, malformedTag      bool
	foreignTitle                                 bool
	description                                  string
	revision                                     int
	writes, deletes                              int
	paths, methods                               []string
}

func newDriveETagV2Fake(method string) (*driveETagProbe, *driveETagV2Fake) {
	p := &driveETagProbe{apiVersion: 2, writeMethod: method, parent: "owned-parent", values: map[string]string{"A": "opaque-A", "B": "opaque-B", "C": "opaque-C", "D": "opaque-D"}}
	p.report = driveETagReport{APIVersion: "v2", WriteMethod: method, FileName: "skirk-etag-probe-v2-public-nonce"}
	f := &driveETagV2Fake{p: p, honor: true, bodyTag: true, headerTag: true}
	p.do = f.request
	return p, f
}

func (f *driveETagV2Fake) tag() string {
	revision := f.revision
	if f.staticTag {
		revision = 1
	}
	return fmt.Sprintf("\"v2-revision-%d\"", revision)
}
func (f *driveETagV2Fake) request(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.paths, f.methods = append(f.paths, path), append(f.methods, method)
	u, err := url.ParseRequestURI(path)
	if err != nil || !strings.HasPrefix(u.Path, "/drive/v2/files") {
		return nil, errors.New("fixture_v2_path")
	}
	r := &HTTPResult{Status: 200, Proto: "HTTP/2.0", Header: make(http.Header)}
	if u.Path == "/drive/v2/files/generateIds" {
		q := u.Query()
		if method != http.MethodGet || q.Get("maxResults") != "1" || q.Get("count") != "" || q.Get("fields") != "ids" {
			return nil, errors.New("fixture_v2_generate")
		}
		r.Body = []byte(`{"ids":["reserved-v2-public-id"]}`)
		return r, nil
	}
	if method == http.MethodPost {
		var value struct {
			ID, Title, Name, Description string
			Parents                      []map[string]string
		}
		if u.Path != "/drive/v2/files" || json.Unmarshal(body, &value) != nil || value.ID != "reserved-v2-public-id" || value.Title != f.p.report.FileName || value.Name != "" || len(value.Parents) != 1 {
			return nil, errors.New("fixture_v2_insert")
		}
		expectedParent := f.p.parent
		if expectedParent == "appDataFolder" {
			expectedParent = "appdata"
		}
		if value.Parents[0]["id"] != expectedParent || len(value.Parents[0]) != 1 {
			return nil, errors.New("fixture_v2_parent_reference")
		}
		f.exists, f.description, f.revision = true, value.Description, 1
	} else if !f.exists {
		r.Status = 404
		return r, nil
	}
	if u.Query().Get("fields") != "id,title,description,version,etag" && u.Query().Get("fields") != "*" {
		return nil, errors.New("fixture_v2_projection")
	}
	if method == http.MethodGet && headers["If-None-Match"] == f.tag() {
		r.Status = 304
		return r, nil
	}
	if method == http.MethodPatch || method == http.MethodPut {
		if method != f.p.writeMethod {
			return nil, errors.New("fixture_v2_selected_method_only")
		}
		f.writes++
		if f.honor && headers["If-Match"] != f.tag() {
			r.Status = 412
			return r, nil
		}
		var update map[string]string
		if json.Unmarshal(body, &update) != nil || len(update) != 1 || update["description"] == "" {
			return nil, errors.New("fixture_v2_description_only")
		}
		f.description, f.revision = update["description"], f.revision+1
	}
	if method == http.MethodDelete {
		f.deletes++
		f.exists, r.Status = false, 204
		return r, nil
	}
	metadata := map[string]string{"id": "reserved-v2-public-id", "title": f.p.report.FileName, "description": f.description, "version": fmt.Sprint(f.revision)}
	if f.foreignTitle {
		metadata["title"] = "foreign-fixture-title"
	}
	if !f.fullOnly || u.Query().Get("fields") == "*" {
		if f.headerTag {
			r.Header.Set("ETag", f.tag())
		}
		if f.bodyTag {
			metadata["etag"] = f.tag()
			if f.conflictingTags {
				metadata["etag"] = "\"other-v2-etag\""
			}
			if f.malformedTag {
				metadata["etag"] = "unquoted-fixture-tag"
			}
		}
	}
	r.Body, _ = json.Marshal(metadata)
	return r, nil
}

func TestDriveETagV2ProbeMethodAndTagSourceMatrix(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		for _, mode := range []string{"honored", "ignored", "resource_only", "header_only", "no_etag", "full_only", "static_read_etag"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				p, f := newDriveETagV2Fake(method)
				switch mode {
				case "ignored":
					f.honor = false
				case "resource_only":
					f.headerTag = false
				case "header_only":
					f.bodyTag = false
				case "no_etag":
					f.headerTag, f.bodyTag = false, false
				case "full_only":
					f.fullOnly = true
				case "static_read_etag":
					f.staticTag = true
				}
				if err := p.run(context.Background()); err != nil {
					t.Fatal(err)
				}
				wantCAS := mode != "ignored" && mode != "no_etag" && mode != "static_read_etag"
				if p.report.WriteCASObserved != wantCAS {
					t.Fatal("wrong v2 write-fence conclusion", p.report.WriteCASObserved)
				}
				if mode == "resource_only" && (p.report.SelectedETagSource != "v2_resource_etag" || p.report.Calls[2].MetadataETag == "" || p.report.Calls[2].ETag != "") {
					t.Fatal("resource-only ETag was lost or invented as an HTTP header")
				}
				if mode == "header_only" && p.report.SelectedETagSource != "http_header" {
					t.Fatal("HTTP header source not identified")
				}
				if mode == "static_read_etag" && (!p.report.Cache304 || p.report.StaleRejected || p.report.StalePreserved) {
					t.Fatal("read cache was mistaken for a description fence")
				}
				if err := p.cleanup(context.Background(), 0); err != nil || !p.report.CleanupVerified || f.exists || f.deletes != 1 || p.report.VisibleCalls > 20 {
					t.Fatal("bounded v2 owned cleanup failed", err)
				}
				for _, got := range f.methods {
					if got == http.MethodPatch && method != http.MethodPatch || got == http.MethodPut && method != http.MethodPut {
						t.Fatal("one experiment invoked the other mutable method")
					}
				}
			})
		}
	}
}

func TestDriveETagV2ProbeRejectsInconsistentOrMalformedTagsBeforeWrite(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		p, f := newDriveETagV2Fake(http.MethodPatch)
		f.malformedTag, f.conflictingTags = !conflict, conflict
		if err := p.run(context.Background()); err == nil || f.writes != 0 || p.report.WriteCASObserved {
			t.Fatal("untrusted v2 tag authorized a write fence")
		}
		if err := p.cleanup(context.Background(), 0); err != nil || !p.report.CleanupVerified {
			t.Fatal("tag rejection lost owned cleanup", err)
		}
	}
	for _, tag := range []string{"bare", "\"x\r\nAuthorization: fake\"", "\"x y\"", "\"x\"y\"", "\"" + strings.Repeat("x", 512) + "\""} {
		if driveETagTagValid(tag) {
			t.Fatal("unsafe ETag accepted")
		}
	}
}

func TestDriveETagV2ProbeExactDialectAndForeignCleanupFence(t *testing.T) {
	p, f := newDriveETagV2Fake(http.MethodPut)
	p.space, p.parent = "appDataFolder", "appDataFolder"
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(f.paths)
	for _, request := range []struct{ method, path string }{
		{http.MethodPatch, p.filePath(false)},
		{http.MethodPut, strings.Replace(p.filePath(false), "/v2/", "/v3/", 1)},
		{http.MethodPut, p.filePath(false) + "&addParents=foreign"},
		{http.MethodDelete, "/drive/v2/files/root?fields=*"},
	} {
		if _, err := p.call(context.Background(), "forbidden", request.method, request.path, nil, nil, true); err == nil || len(f.paths) != before {
			t.Fatal("unselected method/foreign dialect reached transport")
		}
	}
	f.foreignTitle = true
	if err := p.cleanup(context.Background(), 0); err == nil || f.deletes != 0 || !f.exists || p.report.CleanupVerified {
		t.Fatal("v2 foreign title authorized deletion")
	}
}

func TestDriveETagV2ProbePUTConditionalHeaderKeepsPinnedRoute(t *testing.T) {
	p, _ := newDriveETagV2Fake(http.MethodPut)
	p.report.FileID = "reserved-v2-public-id"
	client := NewGoogleHTTPClient(RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"})
	defer client.client.CloseIdleConnections()
	client.client.Transport = driveETagRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.google.com" || r.Host != "www.googleapis.com" || r.Method != http.MethodPut || r.URL.Path != "/drive/v2/files/reserved-v2-public-id" || r.URL.Query().Get("fields") != p.projection() || r.Header.Get("If-Match") != "\"v2-provider-etag\"" {
			t.Fatal("v2 conditional PUT changed its method, projection or front")
		}
		header := make(http.Header)
		header.Set("ETag", "\"v2-current\"")
		return &http.Response{StatusCode: 412, Proto: "HTTP/2.0", ProtoMajor: 2, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
		return client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	}
	if r, err := p.patch(context.Background(), "v2_put_header", p.values["B"], "\"v2-provider-etag\""); err != nil || r.Status != 412 || p.report.Calls[0].ETag != "\"v2-current\"" {
		t.Fatal("v2 actual client conditional evidence lost", err)
	}
}
