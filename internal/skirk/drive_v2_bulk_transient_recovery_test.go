package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fake route commits one generated-ID bulk object before returning 502.
// The sender must retry the same identity and prove its exact existing body.
type driveV2BulkTransientRoute struct {
	mu          sync.Mutex
	base        *MemoryStore
	id          string
	name        string
	digest      [32]byte
	posts       int
	proofs      int
	reserved    int64
	guardDenied bool
	violation   error
}

func (r *driveV2BulkTransientRoute) roundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Scheme != "https" || req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
		r.violation = errors.New("bulk recovery changed pinned Google front")
		return nil, r.violation
	}
	cost := int64(50)
	if req.Method == http.MethodGet {
		cost = 5
	}
	if r.reserved+cost*8 > 840 {
		r.guardDenied = true
		return nil, errors.New("bulk recovery attempt reservation exceeded")
	}
	r.reserved += cost * 8
	switch req.Method {
	case http.MethodPost:
		if req.URL.Path != "/upload/drive/v3/files" {
			break
		}
		upload, err := captureDriveUpload(req)
		if err != nil {
			return nil, err
		}
		r.posts++
		digest := sha256.Sum256(upload.data)
		if r.posts == 1 {
			r.id, r.name, r.digest = upload.id, upload.name, digest
			if _, err = r.base.PutObjectWithID(req.Context(), upload.id, upload.name, upload.data); err != nil {
				return nil, err
			}
			return stringResponse(http.StatusBadGateway, `{"error":{"code":502}}`), nil
		}
		if r.posts == 2 && upload.id == r.id && upload.name == r.name && digest == r.digest {
			stored, getErr := r.base.GetByID(req.Context(), upload.id)
			if getErr == nil && bytes.Equal(stored, upload.data) {
				return stringResponse(http.StatusConflict, `{"error":{"code":409}}`), nil
			}
		}
		r.violation = fmt.Errorf("bulk POST retry changed identity or body: post=%d same_id=%t same_name=%t same_digest=%t", r.posts, upload.id == r.id, upload.name == r.name, digest == r.digest)
		return nil, r.violation
	case http.MethodGet:
		if req.URL.Path != "/drive/v3/files/"+url.PathEscape(r.id) || !strings.Contains(req.URL.Query().Get("fields"), "sha256Checksum") {
			r.violation = fmt.Errorf("bulk proof path/fields unexpected: path=%q fields=%q", req.URL.Path, req.URL.Query().Get("fields"))
			return nil, r.violation
		}
		r.proofs++
		stored, err := r.base.GetByID(req.Context(), r.id)
		if err != nil || sha256.Sum256(stored) != r.digest {
			break
		}
		payload, _ := json.Marshal(map[string]string{
			"id": r.id, "name": r.name, "size": fmt.Sprint(len(stored)),
			"sha256Checksum": hex.EncodeToString(r.digest[:]),
		})
		return stringResponse(http.StatusOK, string(payload)), nil
	}
	r.violation = errors.New("bulk recovery made an unexpected request or changed the generated identity")
	return nil, r.violation
}

type driveV2BulkTransientStore struct {
	*MemoryStore
	mu      sync.Mutex
	drive   *DriveStore
	faultID string
}

func (s *driveV2BulkTransientStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if len(data) < 1<<20 || !strings.Contains(name, "/p1/") {
		return s.MemoryStore.PutObjectWithID(ctx, id, name, data)
	}
	s.mu.Lock()
	if s.faultID == "" {
		s.faultID = id
	}
	target := id == s.faultID
	s.mu.Unlock()
	if target {
		return s.drive.PutObjectWithID(ctx, id, name, data)
	}
	return s.MemoryStore.PutObjectWithID(ctx, id, name, data)
}

func TestDriveV2BulkGeneratedIDTransient502RecoversThroughMux(t *testing.T) {
	backend := newDriveV2PriorityFake()
	up := driveV2PriorityFixture(t, backend, true, func(cfg *driveV2PriorityConfig) { cfg.MaxQuotaUnitsUpper = 150000 })
	down := driveV2PriorityFixture(t, backend, false, func(cfg *driveV2PriorityConfig) { cfg.MaxQuotaUnitsUpper = 150000 })
	for _, carrier := range []*driveV2PriorityCarrier{up, down} {
		if err := carrier.ProbeOwnCAS(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	base := NewMemoryStore()
	route := &driveV2BulkTransientRoute{base: base}
	httpClient := &GoogleHTTPClient{
		route:  RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"},
		client: &http.Client{Transport: roundTripFunc(route.roundTrip)},
	}
	bulkDrive := NewDriveStore(httpClient, "public-bulk-recovery-token", DriveConfig{})
	store := &driveV2BulkTransientStore{MemoryStore: base, drive: bulkDrive}
	cfg := fsHybridStoreConfig{
		Secret: "public-bulk-recovery-fixture", Session: [16]byte{1}, Epoch: [16]byte{2},
		Direction: DirectionUp, ClientID: "client", RunID: "run", FallbackAfter: time.Second,
		ACKDelay: time.Millisecond, DiscoveryInterval: time.Millisecond,
		OperationTimeout: time.Second, Duration: 20 * time.Second,
		MaxSnapshotBytes: driveV2PriorityCipherMax, InlinePerStream: 8,
	}
	client, err := newFSHybridPriorityStore(context.Background(), store, up, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg.Direction = DirectionDown
	exit, err := newFSHybridPriorityStore(context.Background(), store, down, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := exit.Close(); err != nil {
			t.Error(err)
		}
	})
	ready, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	for _, peer := range []*fsHybridPriorityStore{client, exit} {
		if err := peer.WaitReady(ready); err != nil {
			t.Fatal("priority bootstrap", err)
		}
	}
	f := fsHybridFullMuxFixtureWithStores(t, client, exit, base, true)
	before := func() {
		backend.mu.Lock()
		backend.frozenReadID = up.cfg.OwnID
		backend.mu.Unlock()
	}
	afterGET := func() {
		backend.mu.Lock()
		backend.frozenReadID = ""
		backend.mu.Unlock()
		select {
		case down.wake <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		route.mu.Lock()
		t.Logf("bulk recovery diagnostic posts=%d proofs=%d reserved=%d denied=%t violation=%v", route.posts, route.proofs, route.reserved, route.guardDenied, route.violation)
		route.mu.Unlock()
		t.Logf("bulk recovery peers client=%v exit=%v", client.Error(), exit.Error())
	})
	check := func() {
		route.mu.Lock()
		posts, proofs, reserved, denied, violation := route.posts, route.proofs, route.reserved, route.guardDenied, route.violation
		route.mu.Unlock()
		store.mu.Lock()
		faultID := store.faultID
		store.mu.Unlock()
		if faultID == "" || posts != 2 || proofs != 1 || reserved != 840 || denied || violation != nil {
			t.Fatalf("bounded generated-ID recovery: selected=%t posts=%d proofs=%d reserved=%d denied=%t violation=%v", faultID != "", posts, proofs, reserved, denied, violation)
		}
		quota := bulkDrive.QuotaSnapshot()
		if quota.Calls != 3 || quota.Units != 105 || quota.Errors != 2 {
			t.Fatalf("recovered 502 quota accounting: calls=%d units=%d errors=%d", quota.Calls, quota.Units, quota.Errors)
		}
		for _, carrier := range []*driveV2PriorityCarrier{up, down} {
			s := carrier.Snapshot()
			if s.Joined != 1 || s.GuardDenied != 0 || s.QuotaUnitsUpper > carrier.cfg.MaxQuotaUnitsUpper {
				t.Fatal("priority carrier ownership/quota after recovery", s)
			}
		}
	}
	runFSHybridTLSMixedWorkload(t, f, before, afterGET, check)
}

func driveGeneratedIDProofTestStore(transport roundTripFunc) *DriveStore {
	route := RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}
	client := &GoogleHTTPClient{route: route, client: &http.Client{Transport: transport}}
	return NewDriveStore(client, "public-generated-id-proof-token", DriveConfig{})
}

func TestDriveGeneratedIDConflictRejectsSameSizeWrongContent(t *testing.T) {
	const id = "public-generated-id-wrong"
	const name = "muxv4/session/p1/wrong-content"
	data := bytes.Repeat([]byte{0x4a}, 1<<20)
	wrong := bytes.Repeat([]byte{0x4b}, len(data))
	wrongDigest := sha256.Sum256(wrong)
	var posts, proofs int
	store := driveGeneratedIDProofTestStore(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			return nil, errors.New("generated-ID proof changed pinned Google route")
		}
		switch req.Method {
		case http.MethodPost:
			upload, err := captureDriveUpload(req)
			if err != nil || upload.id != id || upload.name != name || !bytes.Equal(upload.data, data) {
				return nil, errors.New("wrong-content fixture upload changed identity")
			}
			posts++
			return stringResponse(http.StatusConflict, `{"error":{"code":409}}`), nil
		case http.MethodGet:
			if req.URL.Path != "/drive/v3/files/"+id || !strings.Contains(req.URL.Query().Get("fields"), "sha256Checksum") {
				return nil, errors.New("wrong-content fixture did not request checksum")
			}
			proofs++
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, id, name, fmt.Sprint(len(data)), hex.EncodeToString(wrongDigest[:]))), nil
		}
		return nil, errors.New("wrong-content fixture unexpected method")
	})
	_, err := store.PutObjectWithID(context.Background(), id, name, data)
	if err == nil || !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("same-size wrong content was accepted: %v", err)
	}
	q := store.QuotaSnapshot()
	if posts != 1 || proofs != 1 || q.Calls != 2 || q.Units != 55 {
		t.Fatalf("wrong-content proof accounting posts=%d proofs=%d calls=%d units=%d", posts, proofs, q.Calls, q.Units)
	}
}

func TestDriveGeneratedIDCommittedLostResponseProvesExactContent(t *testing.T) {
	const id = "public-generated-id-lost"
	const name = "muxv4/session/p1/lost-response"
	data := bytes.Repeat([]byte{0x6c}, 1<<20)
	digest := sha256.Sum256(data)
	var posts, proofs int
	store := driveGeneratedIDProofTestStore(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			return nil, errors.New("lost-response proof changed pinned Google route")
		}
		switch req.Method {
		case http.MethodPost:
			upload, err := captureDriveUpload(req)
			if err != nil || upload.id != id || upload.name != name || !bytes.Equal(upload.data, data) {
				return nil, errors.New("lost-response retry changed generated identity or body")
			}
			posts++
			if posts == 1 {
				// The fake remote committed the exact body; the response vanished.
				return nil, io.ErrUnexpectedEOF
			}
			return stringResponse(http.StatusConflict, `{"error":{"code":409}}`), nil
		case http.MethodGet:
			if posts != 2 || req.URL.Path != "/drive/v3/files/"+id || !strings.Contains(req.URL.Query().Get("fields"), "sha256Checksum") {
				return nil, errors.New("lost-response retry omitted exact checksum proof")
			}
			proofs++
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, id, name, fmt.Sprint(len(data)), hex.EncodeToString(digest[:]))), nil
		}
		return nil, errors.New("lost-response fixture unexpected method")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := store.PutObjectWithID(ctx, id, name, data)
	if err != nil || info.ID != id || info.Name != name || info.Size != int64(len(data)) {
		t.Fatalf("committed lost response was not proven: info=%+v err=%v", info, err)
	}
	q := store.QuotaSnapshot()
	if posts != 2 || proofs != 1 || q.Calls != 3 || q.Units != 105 {
		t.Fatalf("lost-response proof accounting posts=%d proofs=%d calls=%d units=%d", posts, proofs, q.Calls, q.Units)
	}
}

func TestDriveGeneratedIDConflictWaitsForChecksumPropagation(t *testing.T) {
	const id = "public-generated-id-propagating"
	const name = "muxv4/session/p1/propagating"
	data := bytes.Repeat([]byte{0x70}, 1<<20)
	digest := sha256.Sum256(data)
	var proofs int
	store := driveGeneratedIDProofTestStore(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			return nil, errors.New("propagation proof changed pinned Google route")
		}
		if req.Method == http.MethodPost {
			return stringResponse(http.StatusConflict, `{"error":{"code":409}}`), nil
		}
		if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files/"+id || !strings.Contains(req.URL.Query().Get("fields"), "sha256Checksum") {
			return nil, errors.New("propagation fixture unexpected request")
		}
		proofs++
		if proofs == 1 {
			return stringResponse(http.StatusNotFound, `{"error":{"code":404}}`), nil
		}
		if proofs == 2 {
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, id, name, fmt.Sprint(len(data)))), nil
		}
		return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, id, name, fmt.Sprint(len(data)), hex.EncodeToString(digest[:]))), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := store.PutObjectWithID(ctx, id, name, data)
	if err != nil || info.ID != id || proofs != 3 {
		t.Fatalf("bounded checksum propagation failed: info=%+v proofs=%d err=%v", info, proofs, err)
	}
	q := store.QuotaSnapshot()
	if q.Calls != 4 || q.Units != 65 {
		t.Fatalf("propagation proof accounting calls=%d units=%d", q.Calls, q.Units)
	}
}
