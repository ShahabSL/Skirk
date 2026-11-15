package skirk

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type largeMetadataTestStore struct {
	*metadataPriorityTestStore
	limit int
}

func (s *largeMetadataTestStore) MetadataOnlyPriorityLimit() int { return s.limit }
func (s *largeMetadataTestStore) PutMetadataOnlyObject(_ context.Context, name string, b []byte) (ObjectInfo, error) {
	if len(b) == 0 || len(b) > s.limit {
		return ObjectInfo{}, fmt.Errorf("oversized sealed bytes")
	}
	s.metadataWrites++
	return ObjectInfo{Name: name, ID: "large-metadata"}, nil
}
func largeMetadataMux(t *testing.T, role string, limit int) *driveMux {
	t.Helper()
	s := &largeMetadataTestStore{metadataPriorityTestStore: &metadataPriorityTestStore{MemoryStore: NewMemoryStore(), enabled: true}, limit: limit}
	m := metadataPriorityTestMux(t, role, s, true)
	m.t.MetadataOnlyMaxBytes = limit
	return m
}
func largeCapabilityExchange(t *testing.T, client, exit *driveMux, key muxStreamKey) {
	t.Helper()
	ctx := context.Background()
	if e := client.startMetadataCapability(key); e != nil {
		t.Fatal(e)
	}
	open := muxFrame{Kind: muxFrameOpen, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: encodeMuxOpenPayload("example.test:443", nil)}
	frames, _ := client.injectMetadataCapabilities([]muxFrame{open})
	if len(frames) != 2 {
		t.Fatal("capability missing")
	}
	exit.handleMetadataCapability(frames[1])
	reply := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("small reply"), Priority: true}
	batch, e := exit.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{reply})
	if e != nil {
		t.Fatal(e)
	}
	for _, frame := range batch.frames {
		if frame.Kind == muxFrameMetadataCapability {
			client.handleMetadataCapability(frame)
		}
	}
}
func TestLargeMetadataNegotiatesAndAuthenticates12KiB(t *testing.T) {
	client, exit := largeMetadataMux(t, "client", 16384), largeMetadataMux(t, "exit", 16384)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 110}
	largeCapabilityExchange(t, client, exit, key)
	frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: bytes.Repeat([]byte{0xa3}, 12*1024), Priority: true}
	batch, e := client.lanes[0].prepareUploadBatchV4(context.Background(), []muxFrame{frame})
	if e != nil || !batch.metadataOnly || len(batch.sealed) <= 8192 || len(batch.sealed) > 16384 {
		t.Fatalf("large metadata was not used: %v, len=%d metadata=%v", e, len(batch.sealed), batch.metadataOnly)
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, ID: "large", InlineData: batch.sealed})
	if !ok {
		t.Fatal("large parse failed")
	}
	if e = exit.processMuxObject(context.Background(), meta); e != nil {
		t.Fatal(e)
	}
	copyMeta := meta
	copyMeta.InlineData = append([]byte(nil), batch.sealed...)
	copyMeta.InlineData[len(copyMeta.InlineData)-1] ^= 1
	if e = exit.processMuxObject(context.Background(), copyMeta); e == nil {
		t.Fatal("accepted corrupted large AEAD")
	}
}
func TestLargeMetadataMixedPeerNegotiatesMinimum(t *testing.T) {
	for _, limits := range [][2]int{{8192, 16384}, {16384, 8192}} {
		t.Run(fmt.Sprint(limits), func(t *testing.T) {
			client, exit := largeMetadataMux(t, "client", limits[0]), largeMetadataMux(t, "exit", limits[1])
			key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 111}
			largeCapabilityExchange(t, client, exit, key)
			frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: bytes.Repeat([]byte{1}, 12*1024), Priority: true}
			for _, m := range []*driveMux{client, exit} {
				if m.metadataBatchLimit([]muxFrame{frame}) != 8192 {
					t.Fatal("did not negotiate8k")
				}
				batch, e := m.lanes[0].prepareUploadBatchV4(context.Background(), []muxFrame{frame})
				if e != nil || batch.metadataOnly {
					t.Fatal("sent12kmetadata to8kpeer")
				}
			}
		})
	}
	client := largeMetadataMux(t, "client", 16384)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 112}
	_ = client.startMetadataCapability(key)
	frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("small"), Priority: true}
	batch, e := client.lanes[0].prepareUploadBatchV4(context.Background(), []muxFrame{frame})
	if e != nil || batch.metadataOnly {
		t.Fatal("old-peer missing echo caused metadata file")
	}
}
func TestLargeMetadataRejectsCapabilityEscalationAndUnknownVersion(t *testing.T) {
	client := largeMetadataMux(t, "client", 16384)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 113}
	_ = client.startMetadataCapability(key)
	state := client.metadataCaps[key]
	for _, limit := range []int{0, 8193, 16385, 65535} {
		client.handleMetadataCapability(muxFrame{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: muxCapabilityV2Payload(muxMetadataCapabilityEchoV2, state.nonce, limit)})
		if client.metadataCaps[key].confirmed {
			t.Fatal("accepted invalid capability limit")
		}
	}
	payload := muxCapabilityV2Payload(muxMetadataCapabilityEchoV2, state.nonce, 16384)
	payload[1] = 3
	client.handleMetadataCapability(muxFrame{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: payload})
	if client.metadataCaps[key].confirmed {
		t.Fatal("accepted unknown version")
	}
	payload = muxCapabilityV2Payload(muxMetadataCapabilityEchoV2, [16]byte{99}, 16384)
	client.handleMetadataCapability(muxFrame{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: payload})
	if client.metadataCaps[key].confirmed {
		t.Fatal("accepted wrong nonce")
	}
}
func TestLargeMetadataDecodeAndConfigBounds(t *testing.T) {
	name := "muxv4/test/p0/test.mo1"
	description := driveMetadataOnlyMarker + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 12000))
	legacy := NewDriveStore(nil, "token", DriveConfig{MetadataOnlyPriority: true})
	large := NewDriveStore(nil, "token", DriveConfig{MetadataOnlyPriority: true, MetadataOnlyMaxBytes: 16384})
	if len(legacy.decodeInlineData(name, description, 0)) != 0 || len(large.decodeInlineData(name, description, 0)) != 12000 {
		t.Fatal("decode cap not profile-bound")
	}
	tooBig := driveMetadataOnlyMarker + base64.StdEncoding.EncodeToString(make([]byte, 16385))
	if large.decodeInlineData(name, tooBig, 0) != nil {
		t.Fatal("decoded oversized bytes")
	}
	if _, e := large.PutMetadataOnlyObject(context.Background(), name, make([]byte, 16385)); e == nil {
		t.Fatal("oversized create reached network")
	}
	for _, n := range []int{-1, 8193, 16385} {
		cfg := &Config{Secret: strings.Repeat("a", 64), Drive: DriveConfig{MetadataOnlyMaxBytes: n}}
		if e := cfg.Validate(); e == nil || !strings.Contains(e.Error(), "metadata_only_max_bytes") {
			t.Fatalf("accepted invalid config %d: %v", n, e)
		}
	}
}
func TestLargeMetadataFreshListClampsPageAndResponseBytes(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(fmt.Sprint(oversize), func(t *testing.T) {
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Query().Get("pageSize") != "100" {
					t.Error("large descriptions did not clamp listpage")
				}
				r := stringResponse(200, `{"files":[]}`)
				if oversize {
					r.Body = io.NopCloser(io.LimitReader(zeroReader{}, driveLargeMetadataListMaxBytes+64))
				}
				return r, nil
			})}}
			store := NewDriveStore(httpClient, "token", DriveConfig{MetadataOnlyPriority: true, MetadataOnlyMaxBytes: 16384, NormalListPageSize: 1000})
			_, e := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/test/", "/p1/"}, time.Time{}, "", 1)
			if oversize && e == nil {
				t.Fatal("accepted oversized response")
			}
			if !oversize && e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestLargeMetadataFreshListBoundsInflatedGzipResponse(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			const prefix, suffix = `{"files":[],"padding":"`, `"}`
			body := append([]byte(prefix), bytes.Repeat([]byte{'a'}, driveLargeMetadataListMaxBytes-len(prefix)-len(suffix)+extra)...)
			body = append(body, suffix...)
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if compressed.Len() >= driveLargeMetadataListMaxBytes {
				t.Fatal("fixture does not exercise decompression expansion")
			}
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				response := stringResponse(http.StatusOK, "")
				response.Header.Set("Content-Encoding", "gzip")
				response.Body = io.NopCloser(bytes.NewReader(compressed.Bytes()))
				return response, nil
			})}}
			store := NewDriveStore(httpClient, "token", DriveConfig{MetadataOnlyPriority: true, MetadataOnlyMaxBytes: 16384})
			_, err := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/test/", "/p0/"}, time.Time{}, "", 1)
			if extra == 0 && err != nil {
				t.Fatalf("rejected gzip body at the inflated limit: %v", err)
			}
			if extra == 1 && (err == nil || !strings.Contains(err.Error(), "exceeded configured byte limit")) {
				t.Fatalf("inflated gzip body beyond the limit was not rejected: %v", err)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestLargeMetadataBatchHonorsEveryPeersLimit(t *testing.T) {
	m := largeMetadataMux(t, "client", 16384)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 120}
	other := key
	other.StreamID++
	m.metadataCaps = map[muxStreamKey]muxMetadataCapability{key: {confirmed: true, version: 2, limit: 16384}, other: {confirmed: true, version: 1, limit: 8192}}
	frames := []muxFrame{{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: make([]byte, 5000), Priority: true}, {Kind: muxFrameData, ClientID: other.ClientID, RunID: other.RunID, StreamID: other.StreamID, Seq: 1, Payload: make([]byte, 5000), Priority: true}}
	batch, e := m.lanes[0].prepareUploadBatchV4(context.Background(), frames)
	if e != nil || batch.metadataOnly || m.metadataBatchLimit(frames) != 8192 {
		t.Fatal("mixed batch escaped smaller negotiated cap")
	}
}

func TestLargeMetadataConcurrentEchoAndPreparation(t *testing.T) {
	m := largeMetadataMux(t, "client", 16384)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 122}
	_ = m.startMetadataCapability(key)
	nonce := m.metadataCaps[key].nonce
	frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: make([]byte, 12000), Priority: true}
	echo := muxFrame{ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: muxCapabilityV2Payload(muxMetadataCapabilityEchoV2, nonce, 16384)}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.handleMetadataCapability(echo)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, e := m.lanes[0].prepareUploadBatchV4(context.Background(), []muxFrame{frame}); e != nil {
				t.Error(e)
				return
			}
		}
	}()
	wg.Wait()
}
