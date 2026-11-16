package skirk

// Owned, secondary-account research fixture. None of this file is linked into
// the product. The ordinary Drive store remains the immutable bulk/fallback
// path; two fresh Drive v2 files carry only bounded priority snapshots.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type driveV2LiveTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Side int    `json:"side"`
}

type driveV2LiveEndpoint struct {
	target  driveV2LiveTarget
	cleanup *DriveStore
	client  *GoogleHTTPClient
	token   string
	project string
}

func (r *fsHybridLiveRun) driveV2Request(ctx context.Context, endpoint driveV2LiveEndpoint, method, path string, body []byte) (*HTTPResult, error) {
	if ctx == nil || ctx.Err() != nil || endpoint.client == nil || endpoint.token == "" {
		return nil, errFSHybridBounds
	}
	headers := map[string]string{"Authorization": "Bearer " + endpoint.token, "Accept-Encoding": "identity"}
	if len(body) != 0 {
		headers["Content-Type"] = "application/json"
	}
	if endpoint.project != "" {
		headers["X-Goog-User-Project"] = endpoint.project
	}
	return endpoint.client.RequestSingleAttempt(withGoogleResponseLimit(ctx, 32<<10), method, "www.googleapis.com", path, headers, body)
}

func (r *fsHybridLiveRun) setupDriveV2(client, exit *DriveStore, cc, ec *Config, sid [16]byte, epoch [16]byte) error {
	if len(r.drives) != 2 || len(r.cleanupDrives) != 2 || cc.Drive.FolderID == "" || cc.Drive.FolderID == "appDataFolder" || cc.Drive.FolderID != ec.Drive.FolderID {
		return errFSHybridBounds
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	stem := "skirk-v2-priority-" + hex.EncodeToString(nonce)
	targets := []driveV2LiveTarget{{Name: stem + "-up", Side: 0}, {Name: stem + "-down", Side: 1}}
	roles := []*DriveStore{client, exit}
	configs := []*Config{cc, ec}
	tokenNames := []string{r.settings.ClientTokenFile, r.settings.ExitTokenFile}
	for side := range targets {
		token, err := fsHybridLiveToken(r.dir, tokenNames[side])
		if err != nil {
			return err
		}
		api := NewGoogleHTTPClient(configs[side].Route)
		r.activeGates = append(r.activeGates, fsHybridLiveAttach(api, r.driveBudget, false))
		endpoint := driveV2LiveEndpoint{cleanup: r.cleanupDrives[side], client: api, token: token, project: configs[side].Drive.QuotaProject}
		response, err := r.driveV2Request(r.ctx, endpoint, http.MethodGet, "/drive/v2/files/generateIds?fields=ids&maxResults=1", nil)
		var generated struct {
			IDs []string `json:"ids"`
		}
		if err != nil || response == nil || response.Proto != "HTTP/2.0" || response.Status != 200 || json.Unmarshal(response.Body, &generated) != nil || len(generated.IDs) != 1 || len(generated.IDs[0]) < 16 || len(generated.IDs[0]) > 128 || !isSafeObjectSegment(generated.IDs[0]) {
			r.journal.write(map[string]any{"phase": "v2_id_reservation_failed", "side": side})
			return errFSHybridBounds
		}
		targets[side].ID = generated.IDs[0]
		endpoint.target = targets[side]
		r.v2Cleanup = append(r.v2Cleanup, endpoint)
	}
	if targets[0].ID == targets[1].ID {
		return errFSHybridIntegrity
	}
	// No create can start until both exact generated IDs and names are durable.
	if err := fsHybridLiveExclusiveJSON(filepath.Join(r.dir, "drive-v2-owned-targets.json"), targets); err != nil {
		return err
	}
	r.journal.write(map[string]any{"phase": "v2_owned_targets_durable", "count": len(targets)})
	r.v2Metadata = targets
	for side, endpoint := range r.v2Cleanup {
		body, _ := json.Marshal(map[string]any{
			"id": endpoint.target.ID, "title": endpoint.target.Name,
			"description": "", "mimeType": "application/octet-stream",
			"parents": []map[string]string{{"id": cc.Drive.FolderID}},
		})
		response, err := r.driveV2Request(r.ctx, endpoint, http.MethodPost, "/drive/v2/files?fields=id,title,description,version,etag", body)
		if err != nil || response == nil || response.Proto != "HTTP/2.0" || response.Status != 200 && response.Status != 201 {
			r.journal.write(map[string]any{"phase": "v2_file_create_failed", "side": side})
			return errFSHybridBounds
		}
		var metadata struct{ ID, Title, Description, ETag, Version string }
		dec := json.NewDecoder(bytes.NewReader(response.Body))
		dec.DisallowUnknownFields()
		if dec.Decode(&metadata) != nil || dec.Decode(new(any)) != io.EOF || metadata.ID != endpoint.target.ID || metadata.Title != endpoint.target.Name || metadata.Description != "" || !(fsPriorityDocument{FenceKind: fsPriorityDriveV2Fence, OpaqueFence: metadata.ETag}).valid() || strings.HasPrefix(metadata.ETag, "W/") || response.Header.Get("ETag") != "" && response.Header.Get("ETag") != metadata.ETag {
			r.journal.write(map[string]any{"phase": "v2_file_create_identity_failed", "side": side})
			return errFSHybridIntegrity
		}
		info, found, err := roles[side].GetMuxObjectMetadataByID(r.ctx, endpoint.target.ID)
		if err != nil || !found || info.ID != endpoint.target.ID || info.Name != endpoint.target.Name {
			r.journal.write(map[string]any{"phase": "v2_file_parent_validation_failed", "side": side})
			return errFSHybridIntegrity
		}
		r.journal.write(map[string]any{"phase": "v2_owned_file_created", "side": side})
	}
	for side, endpoint := range r.v2Cleanup {
		peer := r.v2Cleanup[1-side].target
		dir := filepath.Join(r.dir, "role-v2-"+[]string{"client", "exit"}[side])
		if err := os.Mkdir(dir, 0700); err != nil {
			return errFSHybridBounds
		}
		carrier, err := newDriveV2PriorityCarrier(r.ctx, driveV2PriorityConfig{
			PrivateDir: dir, OwnID: endpoint.target.ID, PeerID: peer.ID,
			OwnName: endpoint.target.Name, PeerName: peer.Name,
			QuotaProject: endpoint.project, Route: configs[side].Route,
			Token: func(ctx context.Context) (string, error) { return endpoint.token, ctx.Err() },
			// Both carrier pools are wrapped by the SAME per-arm Drive gate.
			// This hook checks only cancellation; charging it would double count.
			BeforeDispatch: func(ctx context.Context, _, _ string, _ int) error { return ctx.Err() },
			PollInterval:   100 * time.Millisecond, IdleInterval: time.Second,
			ActiveHold: 2 * time.Second, Duration: fsHybridLiveDuration,
			MaxCalls: 2048, MaxQuotaUnitsUpper: 150000, MaxRequestBytesUpper: 64 << 20,
		})
		if err != nil {
			return err
		}
		r.v2Carriers = append(r.v2Carriers, carrier)
		r.activeGates = append(r.activeGates, fsHybridLiveAttach(carrier.getClient, r.driveBudget, false), fsHybridLiveAttach(carrier.patchClient, r.driveBudget, false))
	}
	// Both capacity/fence probes finish before either application store starts.
	for side, carrier := range r.v2Carriers {
		if err := carrier.ProbeOwnCAS(r.ctx); err != nil {
			r.journal.write(map[string]any{"phase": "v2_cas_probe_failed", "side": side})
			return err
		}
		r.journal.write(map[string]any{"phase": "v2_cas_probe_passed", "side": side})
	}
	for _, carrier := range r.v2Carriers {
		peer, err := carrier.ReadPeer(r.ctx)
		if err != nil || len(peer.Ciphertext) != 0 {
			return errFSHybridIntegrity
		}
	}
	for side, carrier := range r.v2Carriers {
		direction, base := DirectionUp, client
		if side == 1 {
			direction, base = DirectionDown, exit
		}
		store, err := newFSHybridPriorityStore(r.ctx, base, carrier, fsHybridStoreConfig{
			Secret: r.settings.Secret, Session: sid, Epoch: epoch,
			Direction: direction, ClientID: r.settings.ClientID, RunID: r.settings.RunID,
			FallbackAfter: 2 * time.Second, OperationTimeout: 10 * time.Second,
			ACKDelay: 10 * time.Millisecond, Duration: fsHybridLiveDuration,
			DiscoveryInterval: time.Second,
		})
		if err != nil {
			return err
		}
		r.stores = append(r.stores, store)
		r.launch(func() {
			select {
			case <-r.ctx.Done():
			case <-store.Done():
				r.cancel()
			}
		})
	}
	for _, store := range r.stores {
		if err := store.WaitReady(r.ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *fsHybridLiveRun) driveV2NamedObjects(ctx context.Context, endpoint driveV2LiveEndpoint) ([]ObjectInfo, error) {
	page, err := endpoint.cleanup.ListFreshContainsPageStatus(ctx, []string{endpoint.target.Name}, time.Time{}, "", 4)
	if err != nil || page.Truncated || page.Incomplete {
		return nil, errFSHybridBounds
	}
	var owned []ObjectInfo
	for _, object := range page.Objects {
		if object.Name == endpoint.target.Name {
			owned = append(owned, object)
		}
	}
	return owned, nil
}

func (r *fsHybridLiveRun) deleteDriveV2Metadata(ctx context.Context) error {
	for _, endpoint := range r.v2Cleanup {
		objects, err := r.driveV2NamedObjects(ctx, endpoint)
		if err != nil {
			return err
		}
		seen := map[string]bool{endpoint.target.ID: true}
		for _, object := range objects {
			if object.ID == "" || !isSafeObjectSegment(object.ID) {
				return errFSHybridIntegrity
			}
			seen[object.ID] = true
		}
		for id := range seen {
			info, found, err := endpoint.cleanup.GetMuxObjectMetadataByID(ctx, id)
			if isDriveNotFound(err) {
				continue
			}
			if err != nil || !found || info.Name != endpoint.target.Name {
				return errFSHybridIntegrity
			}
			if err := endpoint.cleanup.DeleteID(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *fsHybridLiveRun) verifyDriveV2Absent(ctx context.Context) error {
	for _, endpoint := range r.v2Cleanup {
		objects, err := r.driveV2NamedObjects(ctx, endpoint)
		if err != nil || len(objects) != 0 {
			return errFSHybridIntegrity
		}
		_, _, err = endpoint.cleanup.GetMuxObjectMetadataByID(ctx, endpoint.target.ID)
		if !isDriveNotFound(err) {
			return errFSHybridIntegrity
		}
	}
	return nil
}

func TestLivePinnedDriveV2Priority(t *testing.T) {
	if os.Getenv("SKIRK_DRIVE_V2_LIVE") != "1" {
		t.Skip("explicit owned pinned Drive v2 priority live entry")
	}
	if os.Getenv("SKIRK_FIRESTORE_HYBRID_ACCOUNT") != "isolated-secondary" {
		t.Fatal("explicit isolated secondary account assertion required")
	}
	dir := os.Getenv("SKIRK_FIRESTORE_HYBRID_PRIVATE_DIR")
	settings, err := fsHybridLoadLiveSettings(dir)
	if err != nil || settings.Mode != "drive_v2" {
		t.Fatal("private Drive v2 live settings precondition failed")
	}
	runPinnedHybridLiveArm(t, dir, settings)
}
