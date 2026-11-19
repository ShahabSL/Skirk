package skirk

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

type muxV5LostResponseStore struct {
	*MemoryStore
	writes int
	first  []byte
}

func (s *muxV5LostResponseStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	s.writes++
	if s.writes == 1 {
		s.first = bytes.Clone(data)
		if _, err := s.MemoryStore.PutObject(ctx, name, data); err != nil {
			return ObjectInfo{}, err
		}
		return ObjectInfo{}, context.DeadlineExceeded
	}
	if !bytes.Equal(s.first, data) {
		return ObjectInfo{}, errors.New("retry changed sealed bytes")
	}
	return s.MemoryStore.PutObject(ctx, name, data)
}

func TestLegacyMuxV4RestartNonceReuseProof(t *testing.T) {
	sid := [16]byte{1, 2, 3, 4, 5}
	tunnel := &Tunnel{Secret: "nonce-generation-test-secret", SessionID: sid, Transport: "muxv4"}
	first, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	first.epoch = "0000000000000001"
	second.epoch = "0000000000000002"
	if first.epoch == second.epoch {
		t.Fatal("proof requires distinct object-name generations")
	}
	firstSequence := first.lanes[0].seq.Add(1)
	secondSequence := second.lanes[0].seq.Add(1)
	firstKey, err := DeriveMuxLaneKeyV4(tunnel.Secret, sid, DirectionDown, "client", "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := DeriveMuxLaneKeyV4(tunnel.Secret, sid, DirectionDown, "client", "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	if firstSequence != secondSequence || !bytes.Equal(firstKey, secondKey) ||
		!bytes.Equal(nonce(sid, DirectionDown, firstSequence), nonce(sid, DirectionDown, secondSequence)) {
		t.Fatal("legacy restart proof no longer reproduces the same key and nonce")
	}
	firstWire, err := Seal(firstKey, sid, DirectionDown, firstSequence, []byte("first reply"), false)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, err := Seal(secondKey, sid, DirectionDown, secondSequence, []byte("other reply"), false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstWire, secondWire) {
		t.Fatal("proof requires different plaintext and ciphertext")
	}
}

func TestMuxV5RestartSeparatesAEADKeysAcrossAllLanes(t *testing.T) {
	sid := [16]byte{1, 2, 3, 4, 5}
	tunnel := &Tunnel{Secret: "nonce-generation-test-secret", SessionID: sid, Transport: "muxv5"}
	first, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newDriveMux(tunnel, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.epoch) != 32 || len(second.epoch) != 32 || first.epoch == second.epoch {
		t.Fatal("fresh mux v5 senders did not receive distinct 128-bit generations")
	}
	first.epoch = "00000000000000000000000000000001"
	second.epoch = "00000000000000000000000000000002"
	if len(first.epoch) != 32 || len(second.epoch) != 32 {
		t.Fatal("mux v5 requires 128-bit sender generations")
	}
	var laneKeys [][]byte
	for lane := range first.lanes {
		sequenceA := first.lanes[lane].seq.Add(1)
		sequenceB := second.lanes[lane].seq.Add(1)
		keyA, err := DeriveMuxLaneKeyV5(tunnel.Secret, sid, DirectionDown, "client-a", "run-a", lane, first.epoch)
		if err != nil {
			t.Fatal(err)
		}
		keyB, err := DeriveMuxLaneKeyV5(tunnel.Secret, sid, DirectionDown, "client-a", "run-a", lane, second.epoch)
		if err != nil {
			t.Fatal(err)
		}
		if sequenceA != sequenceB || !bytes.Equal(nonce(sid, DirectionDown, sequenceA), nonce(sid, DirectionDown, sequenceB)) {
			t.Fatal("test requires matching restarted sequence and nonce")
		}
		if bytes.Equal(keyA, keyB) {
			t.Fatalf("lane %d reused its key after restart", lane)
		}
		for earlier, earlierKey := range laneKeys {
			if bytes.Equal(keyA, earlierKey) {
				t.Fatalf("lanes %d and %d share an AEAD key", earlier, lane)
			}
		}
		laneKeys = append(laneKeys, keyA)
		wire, err := Seal(keyA, sid, DirectionDown, sequenceA, []byte("first generation"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenEnvelope(keyB, wire); err == nil {
			t.Fatalf("lane %d accepted another generation's ciphertext", lane)
		}
	}
	upKey, err := DeriveMuxLaneKeyV5(tunnel.Secret, sid, DirectionUp, "client-a", "run-a", 0, first.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(upKey, laneKeys[0]) {
		t.Fatal("up and down share an AEAD key")
	}
}

func TestMuxV5BatchRoundTripAndFailClosedGeneration(t *testing.T) {
	ctx := context.Background()
	sid := [16]byte{1, 2, 3, 4, 5}
	store := NewMemoryStore()
	client, err := newDriveMux(&Tunnel{Data: store, Secret: "nonce-generation-test-secret", SessionID: sid, Transport: "muxv5", ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := newDriveMux(&Tunnel{Data: store, Secret: "nonce-generation-test-secret", SessionID: sid, Transport: "muxv5"}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 7, Seq: 1, Payload: []byte("v5 authenticated payload"), Priority: true}
	batch, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(batch.name, "muxv5/") || !strings.HasPrefix(exit.recvPrefix(), "muxv5/") {
		t.Fatalf("v5 sender or receiver selected the wrong namespace: %q / %q", batch.name, exit.recvPrefix())
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, InlineData: batch.sealed})
	if !ok || meta.Transport != "muxv5" || meta.Epoch != client.epoch {
		t.Fatalf("v5 batch name did not retain its authenticated generation: %+v", meta)
	}
	if err := exit.processMuxObject(ctx, meta); err != nil {
		t.Fatalf("v5 receiver rejected valid sealed batch: %v", err)
	}
	for _, candidate := range []string{
		"", "00000000000000000000000000000000", "0000000000000000000000000000000g",
		"0000000000000000000000000000000A", "0000000000000001",
	} {
		altered := strings.Replace(batch.name, client.epoch, candidate, 1)
		if _, ok := parseMuxObjectInfo(ObjectInfo{Name: altered, InlineData: batch.sealed}); ok {
			t.Fatalf("accepted malformed mux v5 generation %q", candidate)
		}
		if _, err := DeriveMuxLaneKeyV5(client.t.Secret, sid, DirectionUp, "client-a", "run-a", 0, candidate); err == nil {
			t.Fatalf("derived a v5 key from malformed generation %q", candidate)
		}
	}
	other := "00000000000000000000000000000003"
	tamperedName := strings.Replace(batch.name, client.epoch, other, 1)
	tampered, ok := parseMuxObjectInfo(ObjectInfo{Name: tamperedName, InlineData: batch.sealed})
	if !ok {
		t.Fatal("valid but tampered generation was not parsed")
	}
	if err := exit.processMuxObject(ctx, tampered); err == nil {
		t.Fatal("receiver accepted ciphertext with a tampered generation")
	}
	if raw, err := hex.DecodeString(client.epoch); err != nil || len(raw) != 16 {
		t.Fatal("sender generation is not 128 bits")
	}
	for _, malformedID := range []string{" client-a", "client/a", "a"} {
		if _, err := DeriveMuxLaneKeyV5(client.t.Secret, sid, DirectionUp, malformedID, "run-a", 0, client.epoch); err == nil {
			t.Fatalf("derived a v5 key from malformed client ID %q", malformedID)
		}
		malformedName := strings.Replace(batch.name, "/client-a/", "/"+malformedID+"/", 1)
		if _, ok := parseMuxObjectInfo(ObjectInfo{Name: malformedName}); ok {
			t.Fatalf("parsed a v5 object with malformed client ID %q", malformedID)
		}
	}
}

func TestMuxV5RequiresMatchingPeerVersion(t *testing.T) {
	sid := [16]byte{1, 2, 3, 4, 5}
	secret := "nonce-generation-test-secret"
	for _, version := range []string{"muxv4", "muxv5"} {
		client, err := newDriveMux(&Tunnel{Secret: secret, SessionID: sid, Transport: version, ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
		if err != nil {
			t.Fatal(err)
		}
		otherVersion := "muxv4"
		if version == "muxv4" {
			otherVersion = "muxv5"
		}
		exit, err := newDriveMux(&Tunnel{Secret: secret, SessionID: sid, Transport: otherVersion}, "exit", DirectionDown, DirectionUp)
		if err != nil {
			t.Fatal(err)
		}
		frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 7, Seq: 1, Payload: []byte("mixed-version"), Priority: true}
		batch, err := client.lanes[0].prepareUploadBatchV4(context.Background(), []muxFrame{frame})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(batch.name, exit.recvPrefix()) {
			t.Fatal("mixed versions share a receiver namespace")
		}
		meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, InlineData: batch.sealed})
		if !ok {
			t.Fatal("mixed-version object failed to parse")
		}
		if err := exit.processMuxObject(context.Background(), meta); err == nil || !strings.Contains(err.Error(), "update both client and exit") {
			t.Fatalf("version mismatch did not fail with upgrade guidance: %v", err)
		}
	}
}

func TestNewTunnelDefaultsToMuxV5ButHonorsExplicitLegacy(t *testing.T) {
	cfg := &Config{Secret: "nonce-generation-test-secret", SessionID: "00112233445566778899aabbccddeeff"}
	cfg.ApplyDefaults()
	if cfg.Tunnel.Transport != "muxv5" {
		t.Fatalf("config default transport = %q, want muxv5", cfg.Tunnel.Transport)
	}
	tunnel, err := NewTunnel(NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.Transport != "muxv5" {
		t.Fatalf("new tunnel transport = %q, want muxv5", tunnel.Transport)
	}
	cfg.Tunnel.Transport = "muxv4"
	legacy, err := NewTunnel(NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Transport != "muxv4" {
		t.Fatalf("explicit legacy transport = %q", legacy.Transport)
	}
	cfg.Tunnel.Transport = ""
	bare, err := NewTunnel(NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Transport != "muxv5" {
		t.Fatalf("bare config produced unsafe transport %q", bare.Transport)
	}
}

func TestDirectDriveMuxDefaultsToMuxV5(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{Secret: "nonce-generation-test-secret", SessionID: [16]byte{1, 2, 3}}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if mux.transport != "muxv5" || len(mux.epoch) != 32 || !strings.HasPrefix(mux.recvPrefix(), "muxv5/") {
		t.Fatalf("direct tunnel selected legacy transport: %q / %q / %q", mux.transport, mux.epoch, mux.recvPrefix())
	}
}

func TestMuxV5MetadataSlotShadowUsesGenerationKey(t *testing.T) {
	ctx := context.Background()
	sid := [16]byte{1, 2, 3, 4, 5}
	secret := "nonce-generation-test-secret"
	client, err := newDriveMux(&Tunnel{Data: NewMemoryStore(), Secret: secret, SessionID: sid, Transport: "muxv5", ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := newDriveMux(&Tunnel{Secret: secret, SessionID: sid, Transport: "muxv5"}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 7, Seq: 1, Payload: []byte("slot shadow"), Priority: true}
	batch, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	link := &muxSlotLink{key: muxSlotRunKey{clientID: "client-a", runID: "run-a"}}
	record := metadataSlotRecord{SlotSeq: 1, MuxName: batch.name, MuxSealed: batch.sealed}
	if err := exit.validateSlotShadowDirection(record, link, DirectionUp); err != nil {
		t.Fatalf("v5 slot shadow could not authenticate its generation-bound mux batch: %v", err)
	}
	record.MuxName = strings.Replace(batch.name, "muxv5/", "muxv4/", 1)
	if err := exit.validateSlotShadowDirection(record, link, DirectionUp); err == nil {
		t.Fatal("v5 slot shadow accepted a legacy object name")
	}
	record.MuxName = strings.Replace(batch.name, client.epoch, "00000000000000000000000000000001", 1)
	if err := exit.validateSlotShadowDirection(record, link, DirectionUp); err == nil {
		t.Fatal("v5 slot shadow accepted a mismatched sender generation")
	}
}

func TestMuxV5MetadataSlotAndDriveClassUseVersionedNamespace(t *testing.T) {
	sid := [16]byte{1, 2, 3, 4, 5}
	generation := [16]byte{9, 8, 7, 6}
	codec, err := newMetadataSlotCodecForTransport("muxv5", "nonce-generation-test-secret", sid, DirectionUp, "client-a", "run-a", generation, metadataSlotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(codec.prefix, "muxv5/") {
		t.Fatalf("v5 metadata slot codec points to %q", codec.prefix)
	}
	name := muxObjectNameWithStreamIDsForTransport("muxv5", sid, DirectionUp, "client-a", "run-a", "00000000000000000000000000000001", 7, []uint64{7}, 0, 1, 1, 1, 1, 64, true)
	if indexedMuxClass(name) != "p0" || !isMuxNamespace(name) || !isMetadataOnlyPriorityName(name+driveMetadataOnlyNameSuffix) {
		t.Fatalf("Drive indexing or metadata priority did not recognize %q", name)
	}
	record := metadataSlotRecord{SlotSeq: 1, MuxName: name, MuxSealed: []byte{1}}
	if err := codec.validateRecord(record); err != nil {
		t.Fatalf("v5 metadata slot rejected matching namespace: %v", err)
	}
	record.MuxName = strings.Replace(name, "muxv5/", "muxv4/", 1)
	if err := codec.validateRecord(record); err == nil {
		t.Fatal("v5 metadata slot accepted a legacy mux object")
	}
}

func TestMuxV5LostUploadResponseRetriesExactCiphertext(t *testing.T) {
	ctx := context.Background()
	store := &muxV5LostResponseStore{MemoryStore: NewMemoryStore()}
	client, err := newDriveMux(&Tunnel{Data: store, Secret: "nonce-generation-test-secret", SessionID: [16]byte{1, 2, 3}, Transport: "muxv5", ClientID: "client-a", RunID: "run-a"}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	frame := muxFrame{Kind: muxFrameData, ClientID: "client-a", RunID: "run-a", StreamID: 7, Seq: 1, Payload: []byte("retry me"), Priority: true}
	batch, err := client.lanes[0].prepareUploadBatchV4(ctx, []muxFrame{frame})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.lanes[0].uploadPreparedImmutableBatchV4(ctx, batch); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first committed-but-lost response = %v", err)
	}
	if err := client.lanes[0].uploadPreparedImmutableBatchV4(ctx, batch); err != nil {
		t.Fatalf("exact retry failed: %v", err)
	}
	if store.writes != 2 || !bytes.Equal(store.first, batch.sealed) {
		t.Fatal("retry did not preserve the prepared ciphertext")
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: batch.name, InlineData: batch.sealed})
	if !ok || meta.Epoch != client.epoch {
		t.Fatal("retry changed the sender generation")
	}
}

func TestMuxV5StoragePressureCleansCurrentNamespaceFirst(t *testing.T) {
	store := &storageQuotaPrefixErrorStore{}
	mux := &driveMux{t: &Tunnel{}, transport: "muxv5"}
	mux.runStoragePressureCleanup(context.Background(), store)
	opts := store.cleanupOptions()
	if len(opts) != 3 || opts[0].Prefix != "muxv5/" || opts[1].Prefix != "muxv4/" || opts[2].Prefix != "bench-drive/" {
		t.Fatalf("storage pressure cleanup order = %+v", opts)
	}
}
