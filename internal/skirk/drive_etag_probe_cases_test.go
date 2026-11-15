package skirk

import (
	"bytes"
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

type driveETagRoundTripper func(*http.Request) (*http.Response, error)

func (f driveETagRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDriveETagProbePreservesConditionalHeaderAndPinnedAuthority(t *testing.T) {
	p, _ := newDriveETagFake()
	p.report.FileID = "reserved-public-id"
	client := NewGoogleHTTPClient(RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"})
	client.client.Transport = driveETagRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.google.com" || r.Host != "www.googleapis.com" || r.Method != http.MethodPatch || r.Header.Get("If-Match") != "\"provider-etag\"" {
			t.Fatal("conditional header or route lost")
		}
		responseHeaders := make(http.Header)
		responseHeaders.Set("ETag", "\"provider-current\"")
		return &http.Response{StatusCode: 412, Proto: "HTTP/2.0", ProtoMajor: 2, Header: responseHeaders, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
		return client.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, headers, body)
	}
	r, err := p.patch(context.Background(), "header_test", p.values["B"], "\"provider-etag\"")
	if err != nil || r.Status != 412 || p.report.Calls[0].IfMatch != "\"provider-etag\"" || p.report.Calls[0].ETag != "\"provider-current\"" {
		t.Fatal("conditional status/header evidence lost", err)
	}
	raw, _ := json.Marshal(p.report)
	if bytes.Contains(raw, []byte(p.values["B"])) {
		t.Fatal("opaque payload retained in report")
	}
}

type driveETagFake struct {
	probe                             *driveETagProbe
	honor, staticTag, noTag, fullOnly bool
	exists                            bool
	description                       string
	revision                          int
	paths                             []string
	patchTags                         []string
}

func (f *driveETagFake) tag(full bool) string {
	if f.noTag || f.fullOnly && !full {
		return ""
	}
	revision := f.revision
	if f.staticTag {
		revision = 1
	}
	return fmt.Sprintf("\"revision-%d\"", revision)
}
func (f *driveETagFake) request(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.paths = append(f.paths, path)
	u, _ := url.ParseRequestURI(path)
	result := &HTTPResult{Status: 200, Proto: "HTTP/2.0", Header: make(http.Header)}
	if strings.HasSuffix(u.Path, "/generateIds") {
		result.Body = []byte(`{"ids":["reserved-public-id"]}`)
		return result, nil
	}
	if method == http.MethodPost {
		var metadata struct{ ID, Name, Description string }
		if json.Unmarshal(body, &metadata) != nil || metadata.ID != "reserved-public-id" || metadata.Name != f.probe.report.FileName {
			return nil, errors.New("fake_create_identity")
		}
		f.exists = true
		f.description = metadata.Description
		f.revision = 1
	} else if !f.exists {
		result.Status = 404
		return result, nil
	}
	full := u.RawQuery == ""
	if method == http.MethodGet && headers["If-None-Match"] != "" && headers["If-None-Match"] == f.tag(full) {
		result.Status = 304
		return result, nil
	}
	if method == http.MethodPatch {
		f.patchTags = append(f.patchTags, headers["If-Match"])
		if f.honor && headers["If-Match"] != f.tag(full) {
			result.Status = 412
			return result, nil
		}
		var update struct{ Description string }
		if json.Unmarshal(body, &update) != nil {
			return nil, errors.New("fake_update")
		}
		f.description = update.Description
		f.revision++
	}
	if method == http.MethodDelete {
		f.exists = false
		result.Status = 204
		return result, nil
	}
	result.Header.Set("ETag", f.tag(full))
	result.Body, _ = json.Marshal(map[string]string{"id": "reserved-public-id", "name": f.probe.report.FileName, "description": f.description, "version": fmt.Sprint(f.revision)})
	return result, nil
}
func newDriveETagFake() (*driveETagProbe, *driveETagFake) {
	p := &driveETagProbe{values: map[string]string{"A": "opaque-A", "B": "opaque-B", "C": "opaque-C", "D": "opaque-D"}}
	p.report = driveETagReport{FileName: "skirk-etag-probe-public-nonce"}
	f := &driveETagFake{probe: p, honor: true}
	p.do = f.request
	return p, f
}

func TestDriveETagProbeDistinguishesWriteFenceFromReadCache(t *testing.T) {
	for _, which := range []string{"honored", "ignored", "cache_etag_does_not_track_description", "no_metadata_etag", "full_metadata_only"} {
		t.Run(which, func(t *testing.T) {
			p, f := newDriveETagFake()
			switch which {
			case "ignored":
				f.honor = false
			case "cache_etag_does_not_track_description":
				f.staticTag = true
			case "no_metadata_etag":
				f.noTag = true
			case "full_metadata_only":
				f.fullOnly = true
			}
			if err := p.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantCAS := which == "honored" || which == "full_metadata_only"
			if p.report.WriteCASObserved != wantCAS {
				t.Fatalf("wrong write fence conclusion: %+v", p.report)
			}
			if which == "cache_etag_does_not_track_description" && (!p.report.Cache304 || !p.report.BogusRejected || p.report.StaleRejected || p.report.StalePreserved) {
				t.Fatal("read validation/bogus rejection confused with a description write fence")
			}
			if which == "full_metadata_only" && (!p.full || len(f.patchTags) != 3) {
				t.Fatal("full projection not reused for conditional PATCH")
			}
			if err := p.cleanup(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			if f.exists || !p.report.CleanupVerified || p.report.Immediate404 != 2 || p.report.Delayed404 != 2 || p.report.VisibleCalls > 20 || p.report.AttemptsUpper != p.report.VisibleCalls*8 {
				t.Fatal("exact immediate/delayed cleanup or guard failed")
			}
		})
	}
}

func TestDriveETagProbeGuardsOwnedPathsAndReservesCleanup(t *testing.T) {
	p, f := newDriveETagFake()
	p.report.FileID = "reserved-public-id"
	f.exists = true
	if _, err := p.call(context.Background(), "foreign", http.MethodPatch, "/drive/v3/files/other-file?fields=id", nil, nil, false); err == nil || len(f.paths) != 0 {
		t.Fatal("foreign file reached transport")
	}
	for i := 0; i < 12; i++ {
		if _, err := p.call(context.Background(), "bounded_read", http.MethodGet, p.filePath(false), nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.call(context.Background(), "excess", http.MethodGet, p.filePath(false), nil, nil, false); err == nil {
		t.Fatal("active calls consumed cleanup reserve")
	}
	if err := p.cleanup(context.Background(), 0); err != nil || !p.report.CleanupVerified {
		t.Fatal("active exhaustion blocked exact cleanup", err)
	}
}

func TestDriveETagProbeCancellationAndReportingFailureStillPermitCleanup(t *testing.T) {
	p, f := newDriveETagFake()
	p.report.FileID = "reserved-public-id"
	f.exists = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.call(ctx, "canceled", http.MethodGet, p.filePath(false), nil, nil, false); !errors.Is(err, context.Canceled) || len(f.paths) != 0 {
		t.Fatal("canceled active call dispatched")
	}
	p.save = func(driveETagReport) error { return errors.New("fake_disk_failure") }
	if err := p.cleanup(context.Background(), 0); err == nil {
		t.Fatal("lost reporting error")
	}
	if f.exists || !p.report.CleanupVerified || p.report.ReportWriteErrors == 0 {
		t.Fatal("reporting failure skipped owned cleanup")
	}
}

func TestDriveETagProbePreCreateRecoveryIdentityAndNegativeStatusPreserved(t *testing.T) {
	p, f := newDriveETagFake()
	original := p.do
	p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
		if method == http.MethodPost && p.report.FileID != "reserved-public-id" {
			t.Fatal("create preceded recovery identity")
		}
		if method == http.MethodPatch {
			return &HTTPResult{Status: 403, Proto: "HTTP/2.0", Header: make(http.Header)}, nil
		}
		return original(ctx, method, path, headers, body)
	}
	if err := p.run(context.Background()); err == nil {
		t.Fatal("auth failure treated as write-CAS negative capability")
	}
	if p.report.Calls[len(p.report.Calls)-1].Status != 403 || p.report.WriteCASObserved {
		t.Fatal("actual status lost")
	}
	if err := p.cleanup(context.Background(), 0); err != nil || f.exists {
		t.Fatal("probe error left owned file", err)
	}
}

func TestDriveETagProbeCleanupNeverDeletesForeignNameOrAbsentID(t *testing.T) {
	for _, absent := range []bool{false, true} {
		p, f := newDriveETagFake()
		p.report.FileID = "reserved-public-id"
		f.exists = true
		deletes := 0
		p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
			if method == http.MethodDelete {
				deletes++
			}
			if absent {
				return &HTTPResult{Status: 404, Proto: "HTTP/2.0", Header: make(http.Header)}, nil
			}
			r := &HTTPResult{Status: 200, Proto: "HTTP/2.0", Header: make(http.Header)}
			r.Body = []byte(`{"id":"reserved-public-id","name":"foreign-existing-name","description":"unrelated"}`)
			return r, nil
		}
		err := p.cleanup(context.Background(), 0)
		if !absent && err == nil || absent && err != nil || deletes != 0 || p.report.CleanupOwnershipVerified {
			t.Fatal("foreign/absent cleanup acquired delete authority", err)
		}
		if absent && (!p.report.AbsentBeforeCleanup || !p.report.CleanupVerified) {
			t.Fatal("absent ID lacked exact readback proof")
		}
	}
}

func TestDriveETagProbeUndispatchedReservationIsNotVisibleCall(t *testing.T) {
	p, f := newDriveETagFake()
	p.report.FileID = "reserved-public-id"
	f.exists = true
	p.save = func(driveETagReport) error { return errors.New("fake_report_write") }
	if _, err := p.call(context.Background(), "not_dispatched", http.MethodGet, p.filePath(false), nil, nil, false); err == nil {
		t.Fatal("active report failure was ignored")
	}
	if p.report.VisibleCalls != 0 || p.report.ReservedCalls != 1 || len(f.paths) != 0 {
		t.Fatal("reservation misreported as dispatched HTTP")
	}
	_ = p.cleanup(context.Background(), 0)
	if f.exists || !p.report.CleanupVerified || p.report.AttemptsUpper != p.report.ReservedCalls*8 {
		t.Fatal("cleanup/upper accounting failed")
	}
}

func TestDriveETagProbeRejectsReservedIDAliasBeforeCreateOrCleanup(t *testing.T) {
	for _, id := range []string{"root", "appDataFolder", "generateIds", "", "unsafe/id"} {
		p, _ := newDriveETagFake()
		p.do = func(context.Context, string, string, map[string]string, []byte) (*HTTPResult, error) {
			raw, _ := json.Marshal(map[string]any{"ids": []string{id}})
			return &HTTPResult{Status: 200, Proto: "HTTP/2.0", Header: make(http.Header), Body: raw}, nil
		}
		if err := p.run(context.Background()); err == nil || p.report.FileID != "" || p.report.VisibleCalls != 1 {
			t.Fatal("malformed reserved ID acquired mutation authority")
		}
		if err := p.cleanup(context.Background(), 0); err != nil || p.report.VisibleCalls != 1 {
			t.Fatal("alias ID acquired cleanup authority")
		}
	}
}

func TestDriveETagProbeRetainsRedeleteAPIErrorAndDeletesLateOwnedFileWithoutFalseProof(t *testing.T) {
	for _, late := range []bool{false, true} {
		p, f := newDriveETagFake()
		p.report.FileID = "reserved-public-id"
		f.exists = true
		f.revision = 1
		gets, deletes := 0, 0
		p.do = func(ctx context.Context, method, path string, headers map[string]string, body []byte) (*HTTPResult, error) {
			if method == http.MethodDelete {
				deletes++
				if !late && deletes == 2 {
					return &HTTPResult{Status: 403, Proto: "HTTP/2.0", Header: make(http.Header)}, nil
				}
			}
			if method == http.MethodGet {
				gets++
				if !late && gets == 2 || late && gets == 4 {
					if late {
						f.exists = true
					}
					raw, _ := json.Marshal(map[string]string{"id": p.report.FileID, "name": p.report.FileName, "description": "opaque-late"})
					return &HTTPResult{Status: 200, Proto: "HTTP/2.0", Header: make(http.Header), Body: raw}, nil
				}
			}
			return f.request(ctx, method, path, headers, body)
		}
		if err := p.cleanup(context.Background(), 0); err == nil {
			t.Fatal("cleanup failure was hidden")
		}
		if late && (f.exists || !p.report.LateOwnedDeleteAttempted || p.report.CleanupVerified) {
			t.Fatal("late owned deletion missing or falsely proved31s absence")
		}
		if !late {
			found := false
			for _, row := range p.report.Calls {
				found = found || row.Phase == "cleanup_redelete" && row.Status == 403
			}
			if !found {
				t.Fatal("redeletion403 lost")
			}
		}
	}
}
