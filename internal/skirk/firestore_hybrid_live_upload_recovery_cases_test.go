package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func fsHybridLiveUploadBody(t *testing.T, id, name string, data []byte) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	meta, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]string{"id": id, "name": name})
	if _, err := meta.Write(metadata); err != nil {
		t.Fatal(err)
	}
	media, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := media.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return "multipart/related; boundary=" + writer.Boundary(), body.Bytes()
}

func TestFSHybridLiveUncommittedGeneratedUpload502CannotBecomeSuccess(t *testing.T) {
	const id = "public-guard-uncommitted-id"
	const name = "muxv4/session/p1/uncommitted"
	data := bytes.Repeat([]byte{0x52}, 1<<20)
	contentType, body := fsHybridLiveUploadBody(t, id, name, data)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	budget.MaxCalls = 33
	budget.MaxUnits = 40400
	budget.MaxMinuteUnits = 40400
	var exposed int
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		exposed++
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" || req.Method != http.MethodPost {
			t.Fatal("uncommitted probe lost pinned front or bounded request")
		}
		return fsHybridHTTP(http.StatusBadGateway, []byte(`{"error":{"code":502}}`)), nil
	})}}
	fsHybridLiveAttach(client, budget, false)
	result, err := client.RequestSingleAttempt(ctx, http.MethodPost, "www.googleapis.com", "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", map[string]string{"Content-Type": contentType}, body)
	if err != nil || result.Status != http.StatusBadGateway || exposed != 1 || ctx.Err() != nil {
		t.Fatal("bounded single-attempt 502 observation changed", err, result, exposed, ctx.Err())
	}
	if err := budget.finalizeGeneratedUploadRecovery(); err == nil {
		t.Fatal("never-committed 502 was accepted without exact content proof")
	}
	if budget.Calls != 1 || budget.Units != 400 || budget.APIError != 1 || budget.UploadTransientRaw != 1 || budget.UploadProvenIDs != 0 || budget.Denials != 0 {
		t.Fatal("uncommitted 502 raw status or quota bound lost", budget.snapshot())
	}
}

func TestFSHybridLiveGeneratedUploadProofAllowsBoundedPropagation(t *testing.T) {
	const id = "public-guard-propagating-id"
	const name = "muxv4/session/p1/propagating"
	data := bytes.Repeat([]byte{0x53}, 1<<20)
	digest := sha256.Sum256(data)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	budget.MaxCalls = 36
	budget.MaxUnits = 40520
	budget.MaxMinuteUnits = 40520
	proofs := 0
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			upload, err := captureDriveUpload(req)
			if err != nil || upload.id != id || upload.name != name || !bytes.Equal(upload.data, data) {
				return nil, fmt.Errorf("generated retry changed upload identity: %w", errFSHybridIntegrity)
			}
			return fsHybridHTTP(http.StatusConflict, []byte(`{"error":{"code":409}}`)), nil
		}
		if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files/"+id || req.URL.Query().Get("fields") != "id,name,size,sha256Checksum" {
			return nil, errFSHybridIntegrity
		}
		proofs++
		if proofs == 1 {
			return fsHybridHTTP(http.StatusNotFound, []byte(`{"error":{"code":404}}`)), nil
		}
		payload := fmt.Sprintf(`{"id":%q,"name":%q,"size":%q`, id, name, fmt.Sprint(len(data)))
		if proofs == 3 {
			payload += fmt.Sprintf(`,"sha256Checksum":%q`, hex.EncodeToString(digest[:]))
		}
		if proofs != 3 {
			return fsHybridHTTP(http.StatusOK, []byte(payload+"}")), nil
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write([]byte(payload + "}")); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		response := fsHybridHTTP(http.StatusOK, compressed.Bytes())
		response.Header.Set("Content-Encoding", "gzip")
		return response, nil
	})}}
	store := NewDriveStore(client, "public-generated-proof-token", DriveConfig{})
	fsHybridLiveGateDrive(store, budget, false)
	info, err := store.PutObjectWithID(ctx, id, name, data)
	if err != nil || info.ID != id || info.Name != name || info.Size != int64(len(data)) || ctx.Err() != nil {
		t.Fatal("bounded product checksum propagation was blocked", err, info, ctx.Err())
	}
	if err := budget.finalizeGeneratedUploadRecovery(); err != nil {
		t.Fatal("exact third-read proof was not retained", err)
	}
	if proofs != 3 || budget.Calls != 4 || budget.Units != 520 || budget.UploadConflictRaw != 1 || budget.UploadProofReadbacks != 3 || budget.UploadProvenIDs != 1 || budget.Denials != 0 || budget.APIError != 2 {
		t.Fatal("propagation path missed raw statuses or quota", budget.snapshot(), proofs)
	}
}

func TestFSHybridLiveGeneratedUploadRejectsWrongDigest(t *testing.T) {
	const id = "public-guard-wrong-digest"
	const name = "muxv4/session/p1/wrong-digest"
	data := bytes.Repeat([]byte{0x54}, 1<<20)
	wrong := sha256.Sum256(bytes.Repeat([]byte{0x55}, len(data)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return fsHybridHTTP(http.StatusConflict, []byte(`{"error":{"code":409}}`)), nil
		}
		if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files/"+id {
			return nil, errFSHybridIntegrity
		}
		payload := fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, id, name, fmt.Sprint(len(data)), hex.EncodeToString(wrong[:]))
		return fsHybridHTTP(http.StatusOK, []byte(payload)), nil
	})}}
	store := NewDriveStore(client, "public-generated-proof-token", DriveConfig{})
	fsHybridLiveGateDrive(store, budget, false)
	_, err := store.PutObjectWithID(ctx, id, name, data)
	if err == nil || !strings.Contains(err.Error(), "content differs") || ctx.Err() == nil {
		t.Fatal("wrong content did not fail closed", err, ctx.Err())
	}
	if err := budget.finalizeGeneratedUploadRecovery(); err == nil || budget.UploadProvenIDs != 0 || budget.Denials != 1 {
		t.Fatal("guard falsely proved wrong content", err, budget.snapshot())
	}
}

func TestFSHybridLiveGeneratedUploadExceptionNeverCoversOtherMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	gate := &fsHybridLiveGate{budget: budget, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return fsHybridHTTP(http.StatusBadGateway, []byte(`{"error":{"code":502}}`)), nil
	})}
	req := fsHybridLiveFakeRequest("www.googleapis.com")
	req.URL.Path = "/drive/v3/files"
	response, err := gate.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if ctx.Err() == nil || budget.UploadTransientRaw != 0 || budget.APIError != 1 {
		t.Fatal("unrelated list 502 was misclassified as recovered bulk", budget.snapshot())
	}
}

func TestFSHybridLiveGeneratedUploadStatusLimitStopsWithoutProof(t *testing.T) {
	const id = "public-guard-eight-status-id"
	const name = "muxv4/session/p1/status-limit"
	data := bytes.Repeat([]byte{0x56}, 1<<20)
	contentType, body := fsHybridLiveUploadBody(t, id, name, data)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return fsHybridHTTP(http.StatusBadGateway, []byte(`{"error":{"code":502}}`)), nil
	})}}
	fsHybridLiveAttach(client, budget, false)
	for i := int64(0); i < fsHybridLiveTransientLimit; i++ {
		result, err := client.RequestSingleAttempt(ctx, http.MethodPost, "www.googleapis.com", "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", map[string]string{"Content-Type": contentType}, body)
		if err != nil || result.Status != http.StatusBadGateway || ctx.Err() != nil {
			t.Fatal("provisional status blocked before fixed limit", i, err, ctx.Err())
		}
	}
	result, err := client.RequestSingleAttempt(ctx, http.MethodPost, "www.googleapis.com", "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size", map[string]string{"Content-Type": contentType}, body)
	if result == nil || result.Status != http.StatusBadGateway || err != nil || ctx.Err() == nil || budget.Denials != 1 || budget.UploadTransientRaw != fsHybridLiveTransientLimit {
		t.Fatal("ninth status did not stop bounded arm", result, err, budget.snapshot())
	}
}

func TestFSHybridLiveGeneratedUploadRetryMustKeepExactBody(t *testing.T) {
	const id = "public-guard-retry-id"
	const name = "muxv4/session/p1/retry-body"
	first := bytes.Repeat([]byte{0x57}, 1<<20)
	second := bytes.Repeat([]byte{0x58}, len(first))
	contentTypeA, bodyA := fsHybridLiveUploadBody(t, id, name, first)
	contentTypeB, bodyB := fsHybridLiveUploadBody(t, id, name, second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := fsHybridLiveNewBudget("drive", cancel)
	budget.AllowGeneratedUploadRecovery = true
	calls := 0
	client := &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fsHybridHTTP(http.StatusBadGateway, []byte(`{"error":{"code":502}}`)), nil
		}
		return fsHybridHTTP(http.StatusConflict, []byte(`{"error":{"code":409}}`)), nil
	})}}
	fsHybridLiveAttach(client, budget, false)
	path := "/upload/drive/v3/files?uploadType=multipart&fields=id,name,size"
	if _, err := client.RequestSingleAttempt(ctx, http.MethodPost, "www.googleapis.com", path, map[string]string{"Content-Type": contentTypeA}, bodyA); err != nil || ctx.Err() != nil {
		t.Fatal("first provisional status did not remain bounded", err, ctx.Err())
	}
	_, _ = client.RequestSingleAttempt(ctx, http.MethodPost, "www.googleapis.com", path, map[string]string{"Content-Type": contentTypeB}, bodyB)
	if calls != 2 || ctx.Err() == nil || budget.Denials != 1 || budget.UploadTransientRaw != 1 || budget.UploadConflictRaw != 0 {
		t.Fatal("same ID with changed media was not rejected", budget.snapshot(), calls)
	}
}
