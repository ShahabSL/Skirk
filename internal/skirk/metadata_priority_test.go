package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type metadataPriorityTestStore struct {
	*MemoryStore
	enabled        bool
	metadataWrites int
	mediaWrites    int
	mediaReads     int
}

func (s *metadataPriorityTestStore) MetadataOnlyPriorityEnabled() bool { return s.enabled }

func (s *metadataPriorityTestStore) PutMetadataOnlyObject(_ context.Context, name string, sealed []byte) (ObjectInfo, error) {
	if !s.enabled || !isMetadataOnlyPriorityName(name) || len(sealed) == 0 || len(sealed) > driveInlineMaxBytes {
		return ObjectInfo{}, fmt.Errorf("invalid metadata-only test upload")
	}
	s.metadataWrites++
	return ObjectInfo{Name: name, ID: fmt.Sprintf("metadata-%d", s.metadataWrites)}, nil
}

func (s *metadataPriorityTestStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	s.mediaWrites++
	return s.MemoryStore.PutObject(ctx, name, data)
}

func (s *metadataPriorityTestStore) Get(ctx context.Context, name string) ([]byte, error) {
	s.mediaReads++
	return s.MemoryStore.Get(ctx, name)
}

func (s *metadataPriorityTestStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	s.mediaReads++
	return s.MemoryStore.GetByID(ctx, id)
}

func metadataPriorityTestMux(t *testing.T, role string, store BlobStore, enabled bool) *driveMux {
	t.Helper()
	send, receive := DirectionUp, DirectionDown
	if role == "exit" {
		send, receive = DirectionDown, DirectionUp
	}
	mux, err := newDriveMux(&Tunnel{
		Data: store, Secret: strings.Repeat("a", 64), SessionID: [16]byte{1, 2, 3},
		ClientID: "client-a", RunID: "run-a", MetadataOnlyPriority: enabled,
		PollInterval: time.Second,
	}, role, send, receive)
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestMetadataOnlyPriorityHandshakeAndAuthenticatedRead(t *testing.T) {
	ctx := context.Background()
	store := &metadataPriorityTestStore{MemoryStore: NewMemoryStore(), enabled: true}
	client := metadataPriorityTestMux(t, "client", store, true)
	exit := metadataPriorityTestMux(t, "exit", store, true)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 17}
	if err := client.startMetadataCapability(key); err != nil {
		t.Fatal(err)
	}
	open := muxFrame{Kind: muxFrameOpen, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: encodeMuxOpenPayload("example.test:443", nil)}
	openBatch, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{open})
	if err != nil {
		t.Fatal(err)
	}
	if openBatch.metadataOnly || !openBatch.priority || len(openBatch.frames) != 2 || openBatch.frames[1].Kind != muxFrameMetadataCapability {
		t.Fatalf("OPEN did not carry a legacy-media capability frame: %+v", openBatch)
	}
	// The request came from the sealed OPEN batch. The exit can now use a
	// metadata-only first reply because the client advertised reader support.
	exit.handleMetadataCapability(openBatch.frames[1])
	reply := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("reply"), Priority: true}
	replyBatch, err := exit.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{reply})
	if err != nil {
		t.Fatal(err)
	}
	if !replyBatch.metadataOnly || !replyBatch.priority || !isMetadataOnlyPriorityName(replyBatch.name) || len(replyBatch.frames) != 2 {
		t.Fatalf("first reply failed to piggyback capability echo: %+v", replyBatch)
	}
	if err := exit.lanes[0].uploadPreparedBatchV4(ctx, replyBatch); err != nil {
		t.Fatal(err)
	}
	if store.metadataWrites != 1 || store.mediaWrites != 0 {
		t.Fatalf("writes metadata=%d media=%d", store.metadataWrites, store.mediaWrites)
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: replyBatch.name, ID: "metadata-1", Size: 0, InlineData: replyBatch.sealed})
	if !ok || !meta.MetadataOnly {
		t.Fatal("metadata-only object name was not recognized")
	}
	if err := client.processMuxObject(ctx, meta); err != nil {
		t.Fatal(err)
	}
	if store.mediaReads != 0 {
		t.Fatal("metadata-only reply made a media GET")
	}
	clientData := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 2, Payload: []byte("next"), Priority: true}
	afterEcho, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{clientData})
	if err != nil || !afterEcho.metadataOnly {
		t.Fatalf("client did not enable metadata after matching echo: batch=%+v err=%v", afterEcho, err)
	}
	other := clientData
	other.StreamID = 18
	mixed, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{clientData, other})
	if err != nil || mixed.metadataOnly {
		t.Fatalf("mixed confirmed/unconfirmed batch used metadata: batch=%+v err=%v", mixed, err)
	}
}

func TestMetadataOnlyPriorityMixedVersionAndBadCapability(t *testing.T) {
	ctx := context.Background()
	store := &metadataPriorityTestStore{MemoryStore: NewMemoryStore(), enabled: true}
	client := metadataPriorityTestMux(t, "client", store, true)
	exit := metadataPriorityTestMux(t, "exit", store, true)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 19}
	if err := client.startMetadataCapability(key); err != nil {
		t.Fatal(err)
	}
	frame := muxFrame{Kind: muxFrameData, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Seq: 1, Payload: []byte("small"), Priority: true}
	checkLegacy := func(label string) {
		t.Helper()
		batch, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{frame})
		if err != nil || batch.metadataOnly {
			t.Fatalf("%s: unconfirmed client used metadata: %+v %v", label, batch, err)
		}
	}
	checkLegacy("no echo from old exit")
	exit.handleMetadataCapability(muxFrame{Kind: muxFrameMetadataCapability, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: []byte{99}})
	reply, err := exit.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{frame})
	if err != nil || reply.metadataOnly || len(reply.frames) != 1 {
		t.Fatalf("old client/unsupported request enabled exit metadata: %+v %v", reply, err)
	}
	badEcho := muxCapabilityPayload(muxMetadataCapabilityEcho, [16]byte{1, 2, 3})
	client.handleMetadataCapability(muxFrame{Kind: muxFrameMetadataCapability, ClientID: key.ClientID, RunID: key.RunID, StreamID: key.StreamID, Payload: badEcho})
	checkLegacy("mismatched echo")
	client.t.MetadataOnlyPriority = false
	checkLegacy("config off")
	if err := client.startMetadataCapability(muxStreamKey{ClientID: key.ClientID, RunID: key.RunID, StreamID: 20}); err != nil {
		t.Fatal(err)
	}
	client.t.MetadataOnlyPriority = true
	store.enabled = false
	checkLegacy("store unsupported")
}

func TestMetadataOnlyPriorityCorruptionNeverFallsBackToMedia(t *testing.T) {
	store := &metadataPriorityTestStore{MemoryStore: NewMemoryStore(), enabled: true}
	client := metadataPriorityTestMux(t, "client", store, true)
	key := muxStreamKey{ClientID: "client-a", RunID: "run-a", StreamID: 21}
	name := muxObjectNameWithStreamIDs(client.t.SessionID, DirectionDown, key.ClientID, key.RunID, "epoch", key.StreamID, []uint64{key.StreamID}, 0, 1, 1, 0, 0, 100, true) + driveMetadataOnlyNameSuffix
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"missing", nil},
		{"tampered ciphertext", bytes.Repeat([]byte{0x7f}, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name, ID: "metadata-id", Size: 0, InlineData: tc.data})
			if !ok {
				t.Fatal("metadata-only object was not parsed")
			}
			if err := client.processMuxObject(context.Background(), meta); err == nil {
				t.Fatal("corrupt metadata-only object was accepted")
			}
			if store.mediaReads != 0 {
				t.Fatal("corrupt metadata-only object fell back to zero-byte media")
			}
		})
	}
}

func TestDriveMetadataOnlyCreateAndSizeZeroList(t *testing.T) {
	sealed := bytes.Repeat([]byte{0x42}, driveInlineMaxBytes)
	name := "muxv4/session/up/client/run/epoch/p0/s0000000000000001/l00/0000000000000001.f1.b100.mo1"
	description := driveMetadataOnlyMarker + base64.StdEncoding.EncodeToString(sealed)
	var creates int
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodPost:
			creates++
			if req.URL.Path != "/drive/v3/files" || strings.Contains(req.URL.String(), "uploadType") || !strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
				t.Errorf("metadata create used wrong endpoint or content type: %s %s", req.Method, req.URL)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(body, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["name"] != name || metadata["description"] != description || metadata["mimeType"] != "application/octet-stream" {
				t.Fatal("metadata-only create body mismatch")
			}
			parents, _ := metadata["parents"].([]any)
			if len(parents) != 1 || parents[0] != "appDataFolder" {
				t.Fatal("missing appDataFolder parent")
			}
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"metadata-id","name":%q,"size":"0"}`, name)), nil
		case http.MethodGet:
			if !strings.Contains(req.URL.Query().Get("fields"), "description") {
				t.Fatal("size-zero list omitted description")
			}
			body, _ := json.Marshal(map[string]any{"files": []map[string]any{{"id": "metadata-id", "name": name, "size": "0", "description": description}}})
			return stringResponse(http.StatusOK, string(body)), nil
		}
		return stringResponse(http.StatusMethodNotAllowed, ""), nil
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{Space: "appDataFolder", MetadataOnlyPriority: true})
	if _, err := store.PutMetadataOnlyObject(context.Background(), name, sealed); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("creates=%d, want one", creates)
	}
	listed, err := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/", "/p0/"}, time.Time{}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Objects) != 1 || listed.Objects[0].Size != 0 || !bytes.Equal(listed.Objects[0].InlineData, sealed) {
		t.Fatal("size-zero list did not preserve sealed description")
	}
	for _, bad := range []string{"", driveMetadataOnlyMarker + "bad!", driveMetadataOnlyMarker + base64.StdEncoding.EncodeToString(append(sealed, 1))} {
		if got := store.decodeInlineData(name, bad, 0); got != nil {
			t.Fatal("accepted malformed or oversized description")
		}
	}
	if got := store.decodeInlineData(name, description, 1); got != nil {
		t.Fatal("accepted metadata-only media size")
	}
	if _, err := store.PutMetadataOnlyObject(context.Background(), name, append(sealed, 1)); err == nil {
		t.Fatal("accepted >8 KiB metadata-only upload")
	}
	if creates != 1 {
		t.Fatal("invalid upload reached Drive")
	}
}
