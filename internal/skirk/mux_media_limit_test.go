package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newMuxMediaLimitTestMux(t *testing.T, respond func(*http.Request) (*http.Response, error)) *driveMux {
	t.Helper()
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(respond)}}
	store := NewDriveStoreWithTokenSource(client, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder"})
	mux, err := newDriveMux(&Tunnel{Data: store, Secret: strings.Repeat("a", 64), SessionID: [16]byte{1}, ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestMuxDriveMediaDownloadAllowsFullSealedFourMiBBatch(t *testing.T) {
	raw := make([]byte, muxMaxBatch)
	sealed, err := Seal(bytes.Repeat([]byte{1}, keyLen), [16]byte{1}, DirectionDown, 1, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != muxMaxSealedBatch {
		t.Fatalf("sealed full batch = %d bytes, want %d", len(sealed), muxMaxSealedBatch)
	}
	mux := newMuxMediaLimitTestMux(t, func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(sealed)), ContentLength: int64(len(sealed))}, nil
	})
	got, err := mux.downloadMuxObjectAttempt(context.Background(), muxObjectMeta{ID: "file-id", Name: "muxv5/test", PlainBytes: muxMaxBatch}, nil)
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatalf("full sealed batch download: bytes=%d error=%v", len(got), err)
	}
}

func TestMuxDriveMediaDownloadRejectsOversizePlainAndDecodedGzip(t *testing.T) {
	tooLarge := make([]byte, muxMaxSealedBatch+1)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(tooLarge); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		body     []byte
		encoding string
		priority bool
	}{
		{name: "normal_plain", body: tooLarge},
		{name: "priority_plain", body: tooLarge, priority: true},
		{name: "normal_gzip", body: compressed.Bytes(), encoding: "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			mux := newMuxMediaLimitTestMux(t, func(*http.Request) (*http.Response, error) {
				calls++
				header := make(http.Header)
				if tc.encoding != "" {
					header.Set("Content-Encoding", tc.encoding)
				}
				// A misleading length must not bypass the read limit.
				header.Set("Content-Length", "1")
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(tc.body)), ContentLength: -1}, nil
			})
			got, err := mux.downloadMuxObjectAttempt(context.Background(), muxObjectMeta{ID: "file-id", Name: "muxv5/test", Priority: tc.priority, PlainBytes: muxMaxBatch}, nil)
			if !errors.Is(err, errGoogleResponseTooLarge) || got != nil {
				t.Fatalf("oversize download: bytes=%d error=%v, want bounded error", len(got), err)
			}
			if calls != 1 {
				t.Fatalf("oversize response retried %d times, want one attempt", calls)
			}
		})
	}
}

func TestMuxDriveMediaDownloadPreservesStricterCallerLimit(t *testing.T) {
	mux := newMuxMediaLimitTestMux(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(make([]byte, 1025)))}, nil
	})
	ctx := withGoogleResponseLimit(context.Background(), 1024)
	_, err := mux.downloadMuxObjectAttempt(ctx, muxObjectMeta{ID: "file-id", Name: "muxv5/test", PlainBytes: 1025}, nil)
	if !errors.Is(err, errGoogleResponseTooLarge) {
		t.Fatalf("stricter caller limit: %v, want bounded error", err)
	}
}

func TestMuxDownloadRejectsOversizeFromStoreIgnoringHTTPContextLimit(t *testing.T) {
	store := NewMemoryStore()
	info, err := store.PutObject(context.Background(), "muxv5/test", make([]byte, muxMaxSealedBatch+1))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := newDriveMux(&Tunnel{Data: store, Secret: strings.Repeat("a", 64), SessionID: [16]byte{1}, ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mux.downloadMuxObjectAttempt(context.Background(), muxObjectMeta{ID: info.ID, Name: info.Name, PlainBytes: muxMaxBatch}, nil)
	if !errors.Is(err, errMuxObjectTooLarge) || got != nil {
		t.Fatalf("oversize non-HTTP object: bytes=%d error=%v, want bounded error", len(got), err)
	}
}
