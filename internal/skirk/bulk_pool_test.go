package skirk

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDriveSeparateBulkMediaPoolKeepsRouteAuthQuotaAndControlPool(t *testing.T) {
	route := RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120", TimeoutSeconds: 45}
	counts := map[string]int{}
	respond := func(pool string, req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" ||
			req.Header.Get("Authorization") != "Bearer test-token" ||
			req.Header.Get("X-Goog-User-Project") != "test-quota-project" {
			return nil, fmt.Errorf("%s lost route, identity, or quota header", pool)
		}
		key := pool + ":" + req.Method + ":" + req.URL.Query().Get("alt")
		counts[key]++
		if req.Method == http.MethodPost {
			upload, err := captureDriveUpload(req)
			if err != nil {
				return nil, err
			}
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, upload.id, upload.name, fmt.Sprint(len(upload.data)))), nil
		}
		if req.URL.Query().Get("alt") == "media" {
			return stringResponse(http.StatusOK, "sealed-media"), nil
		}
		return stringResponse(http.StatusOK, `{"files":[]}`), nil
	}
	ordinary := &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return respond("control", req)
	})}}
	store := NewDriveStoreWithTokenSource(ordinary, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, route), DriveConfig{
		QuotaProject: "test-quota-project", SeparateBulkMediaPool: true,
	})
	if store.normalUploadHTTP == nil || store.normalDownloadHTTP != store.normalUploadHTTP ||
		store.normalUploadHTTP == ordinary || store.normalUploadHTTP.route != route {
		t.Fatal("bulk media pool did not share a distinct same-route HTTP client")
	}
	bulk := &GoogleHTTPClient{route: route, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return respond("bulk", req)
	})}}
	store.normalUploadHTTP, store.normalDownloadHTTP = bulk, bulk
	ctx := context.Background()
	if _, err := store.PutObjectWithID(ctx, "bulk-id", "muxv4/session/p1/normal", bytes.Repeat([]byte{1}, driveUploadHedgeMinBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObjectWithID(ctx, "priority-id", "muxv4/session/p0/priority", []byte("priority")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetNormalByID(ctx, "bulk-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, "priority-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "muxv4/session/"); err != nil {
		t.Fatal(err)
	}
	if counts["bulk:POST:"] != 1 || counts["bulk:GET:media"] != 1 ||
		counts["control:POST:"] != 1 || counts["control:GET:media"] != 1 || counts["control:GET:"] != 1 {
		t.Fatalf("unexpected pool split: %#v", counts)
	}
	if quota := store.QuotaSnapshot(); quota.Ops["upload"].Calls != 2 || quota.Ops["download"].Calls != 2 || quota.Ops["list"].Calls != 1 {
		t.Fatalf("shared quota accounting lost operations: %#v", quota.Ops)
	}
}

type muxBulkPoolTestStore struct {
	*MemoryStore
	priorityReads int
	normalReads   int
}

func (s *muxBulkPoolTestStore) GetByID(context.Context, string) ([]byte, error) {
	s.priorityReads++
	return []byte("priority"), nil
}

func (s *muxBulkPoolTestStore) GetNormalByID(context.Context, string) ([]byte, error) {
	s.normalReads++
	return []byte("normal"), nil
}

func TestMuxOnlyNormalIDDownloadUsesBulkPool(t *testing.T) {
	store := &muxBulkPoolTestStore{MemoryStore: NewMemoryStore()}
	mux, err := newDriveMux(&Tunnel{
		Data: store, Secret: strings.Repeat("a", 64), SessionID: [16]byte{1, 2, 3},
		ClientID: "client-a", RunID: "run-a", PollInterval: time.Second,
	}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		priority   bool
		plainBytes int
		want       string
	}{
		{priority: true, plainBytes: 32, want: "priority"},
		{priority: false, plainBytes: 32, want: "priority"},
		{priority: false, plainBytes: driveUploadHedgeMinBytes, want: "normal"},
	} {
		data, err := mux.downloadMuxObjectAttempt(context.Background(), muxObjectMeta{
			ID: "test-id", Name: "muxv4/test", Priority: tc.priority, PlainBytes: tc.plainBytes,
		}, nil)
		if err != nil || string(data) != tc.want {
			t.Fatalf("priority=%t: data=%q err=%v", tc.priority, data, err)
		}
	}
	if store.priorityReads != 2 || store.normalReads != 1 {
		t.Fatalf("priority=%d normal=%d", store.priorityReads, store.normalReads)
	}
}

func TestSeparateBulkMediaPoolDefaultsOff(t *testing.T) {
	cfg := &Config{Secret: strings.Repeat("a", 64)}
	cfg.ApplyDefaults()
	if cfg.Drive.SeparateBulkMediaPool {
		t.Fatal("bulk media split enabled by default")
	}
}

func TestSeparateBulkMediaPoolH3ProxyStaysOnSelectedFront(t *testing.T) {
	route := RouteConfig{Mode: "google_front_h3_pinned", Proxy: "socks5h://127.0.0.1:11093", GoogleIP: "216.239.38.120"}
	primary := NewGoogleHTTPClient(route)
	store := NewDriveStoreWithTokenSource(primary, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, route), DriveConfig{
		SeparateBulkMediaPool: true,
	})
	if store.normalUploadHTTP == nil || store.normalDownloadHTTP != store.normalUploadHTTP || store.normalUploadHTTP.route != primary.route {
		t.Fatal("bulk pool changed selected route or SOCKS proxy")
	}
	if _, ok := store.normalUploadHTTP.client.Transport.(*googleH3FallbackTransport); ok {
		t.Fatal("proxied bulk pool attempted direct QUIC")
	}
}
