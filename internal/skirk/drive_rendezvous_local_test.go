package skirk

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is a local, test-only model of a fixed known-ID Drive rendezvous.
// It deliberately does not wire a new carrier into the product transport.
const rendezvousTestMaxSnapshotBytes = 16 << 10

var (
	errRendezvousPrecondition = errors.New("rendezvous precondition failed")
	errRendezvousLostReply    = errors.New("rendezvous response lost after commit")
	errRendezvousMissing      = errors.New("rendezvous file missing")
	errRendezvousDenied       = errors.New("rendezvous principal denied")
)

type rendezvousTestFile struct {
	ID, Name, Parent, Owner, App, Description, ETag string
}

type rendezvousTestDrive struct {
	mu          sync.Mutex
	files       map[string]rendezvousTestFile
	versions    map[string]uint64
	lostNext    map[string]bool
	ignoreFence bool
	counts      map[string]int
	units       int
	principal   string
	app         string
}

func newRendezvousTestDrive() *rendezvousTestDrive {
	return &rendezvousTestDrive{
		files: make(map[string]rendezvousTestFile), versions: make(map[string]uint64),
		lostNext: make(map[string]bool), counts: make(map[string]int),
		principal: "mailbox-owner", app: "same-oauth-client",
	}
}

func (d *rendezvousTestDrive) charge(op string, units int) {
	d.counts[op]++
	d.units += units
}

func (d *rendezvousTestDrive) authorize(principal, app string) error {
	if principal != d.principal || app != d.app {
		return errRendezvousDenied
	}
	return nil
}

func (d *rendezvousTestDrive) create(principal, app, id, name, parent, description string) (rendezvousTestFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.charge("create", 50)
	if err := d.authorize(principal, app); err != nil {
		return rendezvousTestFile{}, err
	}
	if _, exists := d.files[id]; exists {
		return rendezvousTestFile{}, errRendezvousPrecondition
	}
	d.versions[id] = 1
	f := rendezvousTestFile{ID: id, Name: name, Parent: parent, Owner: principal, App: app, Description: description, ETag: `"1"`}
	d.files[id] = f
	return f, nil
}

func (d *rendezvousTestDrive) get(principal, app, id string) (rendezvousTestFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.charge("get", 5)
	if err := d.authorize(principal, app); err != nil {
		return rendezvousTestFile{}, err
	}
	f, ok := d.files[id]
	if !ok {
		return rendezvousTestFile{}, errRendezvousMissing
	}
	return f, nil
}

func (d *rendezvousTestDrive) patch(principal, app, id, fence, description string) (rendezvousTestFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.charge("patch", 50)
	if err := d.authorize(principal, app); err != nil {
		return rendezvousTestFile{}, err
	}
	f, ok := d.files[id]
	if !ok {
		return rendezvousTestFile{}, errRendezvousMissing
	}
	if !d.ignoreFence && (fence == "" || fence != f.ETag) {
		d.counts["patch_412"]++
		return rendezvousTestFile{}, errRendezvousPrecondition
	}
	d.versions[id]++
	f.ETag = strconv.Quote(strconv.FormatUint(d.versions[id], 10))
	f.Description = description
	d.files[id] = f
	if d.lostNext[id] {
		delete(d.lostNext, id)
		d.counts["patch_lost_reply"]++
		return rendezvousTestFile{}, errRendezvousLostReply
	}
	return f, nil
}

func (d *rendezvousTestDrive) list(principal, app, prefix string) ([]rendezvousTestFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.charge("list", 100)
	if err := d.authorize(principal, app); err != nil {
		return nil, err
	}
	var out []rendezvousTestFile
	for _, f := range d.files {
		if len(f.Name) >= len(prefix) && f.Name[:len(prefix)] == prefix {
			out = append(out, f)
		}
	}
	return out, nil
}

func (d *rendezvousTestDrive) deleteOwned(principal, app string, expected rendezvousTestFile) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.charge("delete", 50)
	if err := d.authorize(principal, app); err != nil {
		return err
	}
	f, ok := d.files[expected.ID]
	if !ok {
		return nil
	}
	if f.Owner != expected.Owner || f.App != expected.App || f.Parent != expected.Parent || f.Name != expected.Name {
		return errors.New("owned rendezvous target changed identity")
	}
	delete(d.files, expected.ID)
	return nil
}

func (d *rendezvousTestDrive) snapshotMeter() (map[string]int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	counts := make(map[string]int, len(d.counts))
	for k, v := range d.counts {
		counts[k] = v
	}
	return counts, d.units
}

type rendezvousTestAnchor struct {
	Version, Generation int
	Session, CurrentID  string
	Signature           []byte
}

type rendezvousTestClaim struct {
	ID, Client, Run string
	Payload         []byte
	MAC             []byte
}

type rendezvousTestReceipt struct {
	ID        string
	ClaimMAC  []byte
	Signature []byte
}

type rendezvousTestSnapshot struct {
	Version, Generation int
	Sealed              bool
	Claims              []rendezvousTestClaim
	Done                []rendezvousTestReceipt
}

type rendezvousTestInvite struct {
	AnchorID, Session, Parent, Owner, App string
	ExitPublic                            ed25519.PublicKey
	SharedKey                             []byte
}

type rendezvousTestFixture struct {
	drive      *rendezvousTestDrive
	invite     rendezvousTestInvite
	exitKey    ed25519.PrivateKey
	current    string
	generation int
	owned      map[string]rendezvousTestFile
	processed  map[string]int
	effectHash map[string][sha256.Size]byte
	mu         sync.Mutex
	maxClaims  int
}

func rendezvousTestAnchorMessage(a rendezvousTestAnchor) []byte {
	return []byte(fmt.Sprintf("skirk-rendezvous-anchor-v1|%s|%d|%s", a.Session, a.Generation, a.CurrentID))
}

func rendezvousTestReceiptMessage(session string, generation int, id string, claimMAC []byte) []byte {
	return []byte(fmt.Sprintf("skirk-rendezvous-receipt-v1|%s|%d|%s|%s", session, generation, id, hex.EncodeToString(claimMAC)))
}

func rendezvousTestClaimMessage(session string, generation int, claim rendezvousTestClaim) []byte {
	data, _ := json.Marshal(struct {
		Session, ID, Client, Run string
		Generation               int
		Payload                  []byte
	}{session, claim.ID, claim.Client, claim.Run, generation, claim.Payload})
	return data
}

func rendezvousTestSealClaim(key []byte, session string, generation int, claim rendezvousTestClaim) rendezvousTestClaim {
	h := hmac.New(sha256.New, key)
	h.Write(rendezvousTestClaimMessage(session, generation, claim))
	claim.MAC = h.Sum(nil)
	return claim
}

func rendezvousTestCheckClaim(key []byte, session string, generation int, claim rendezvousTestClaim) bool {
	want := rendezvousTestSealClaim(key, session, generation, rendezvousTestClaim{ID: claim.ID, Client: claim.Client, Run: claim.Run, Payload: claim.Payload})
	return hmac.Equal(want.MAC, claim.MAC)
}

func newRendezvousTestFixture(t *testing.T, maxClaims int) *rendezvousTestFixture {
	t.Helper()
	seed := sha256.Sum256([]byte("local-only-rendezvous-test-exit-key"))
	exitKey := ed25519.NewKeyFromSeed(seed[:])
	d := newRendezvousTestDrive()
	f := &rendezvousTestFixture{
		drive: d, exitKey: exitKey, generation: 1, current: "rdv-generation-1",
		owned: make(map[string]rendezvousTestFile), processed: make(map[string]int), effectHash: make(map[string][sha256.Size]byte), maxClaims: maxClaims,
	}
	f.invite = rendezvousTestInvite{AnchorID: "rdv-anchor", Session: "local-session", Parent: "owned-mailbox", Owner: d.principal, App: d.app,
		ExitPublic: exitKey.Public().(ed25519.PublicKey), SharedKey: []byte("local-only-rendezvous-test-shared-key")}
	snapshot := rendezvousTestSnapshot{Version: 1, Generation: 1}
	blob, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.create(d.principal, d.app, f.current, "rdv/local-session/1", f.invite.Parent, string(blob))
	if err != nil {
		t.Fatal(err)
	}
	f.owned[current.ID] = current
	if err := f.probeCAS(current.ID, 1); err != nil {
		t.Fatal(err)
	}
	a := rendezvousTestAnchor{Version: 1, Generation: 1, Session: f.invite.Session, CurrentID: f.current}
	a.Signature = ed25519.Sign(exitKey, rendezvousTestAnchorMessage(a))
	blob, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := d.create(d.principal, d.app, f.invite.AnchorID, "rdv/local-session/anchor", f.invite.Parent, string(blob))
	if err != nil {
		t.Fatal(err)
	}
	f.owned[anchor.ID] = anchor
	return f
}

func (f *rendezvousTestFixture) readAnchor() (rendezvousTestAnchor, error) {
	file, err := f.drive.get(f.invite.Owner, f.invite.App, f.invite.AnchorID)
	if err != nil {
		return rendezvousTestAnchor{}, err
	}
	if err := f.checkFile(file, "rdv/"+f.invite.Session+"/anchor"); err != nil {
		return rendezvousTestAnchor{}, err
	}
	var a rendezvousTestAnchor
	if err := json.Unmarshal([]byte(file.Description), &a); err != nil {
		return a, err
	}
	if a.Version != 1 || a.Session != f.invite.Session || a.Generation <= 0 || a.CurrentID == "" ||
		!ed25519.Verify(f.invite.ExitPublic, rendezvousTestAnchorMessage(a), a.Signature) {
		return a, errors.New("invalid rendezvous anchor")
	}
	return a, nil
}

func (f *rendezvousTestFixture) checkFile(file rendezvousTestFile, name string) error {
	if file.ID == "" || file.Name != name || file.Parent != f.invite.Parent || file.Owner != f.invite.Owner || file.App != f.invite.App ||
		len(file.ETag) < 3 || file.ETag[0] != '"' || file.ETag[len(file.ETag)-1] != '"' || strings.HasPrefix(file.ETag, "W/") {
		return errors.New("rendezvous file identity mismatch")
	}
	return nil
}

func (f *rendezvousTestFixture) probeCAS(id string, generation int) error {
	// A generation must be probed before its ID is advertised in an anchor.
	file, err := f.drive.get(f.invite.Owner, f.invite.App, id)
	if err != nil {
		return err
	}
	if err := f.checkFile(file, fmt.Sprintf("rdv/%s/%d", f.invite.Session, generation)); err != nil {
		return err
	}
	probeBody := `{"probe":"not-application-state"}`
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, id, `"impossible-probe-fence"`, probeBody); !errors.Is(err, errRendezvousPrecondition) {
		return errors.New("Drive ignored impossible rendezvous ETag")
	}
	unchanged, err := f.drive.get(f.invite.Owner, f.invite.App, id)
	if err != nil || unchanged.Description != file.Description || unchanged.ETag != file.ETag {
		return errors.New("impossible ETag changed rendezvous file")
	}
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, id, file.ETag, probeBody); err != nil {
		return fmt.Errorf("valid ETag probe: %w", err)
	}
	changed, err := f.drive.get(f.invite.Owner, f.invite.App, id)
	if err != nil || changed.Description != probeBody || changed.ETag == file.ETag {
		return errors.New("valid ETag probe was not visible exactly")
	}
	if err := f.checkFile(changed, file.Name); err != nil {
		return err
	}
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, id, file.ETag, file.Description); !errors.Is(err, errRendezvousPrecondition) {
		return errors.New("Drive accepted stale rendezvous ETag")
	}
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, id, changed.ETag, file.Description); err != nil {
		return fmt.Errorf("reset ETag probe: %w", err)
	}
	reset, err := f.drive.get(f.invite.Owner, f.invite.App, id)
	if err != nil || reset.Description != file.Description || reset.ETag == changed.ETag {
		return errors.New("ETag probe reset was not visible exactly")
	}
	return nil
}

func (f *rendezvousTestFixture) readSnapshot(id string, generation int) (rendezvousTestFile, rendezvousTestSnapshot, error) {
	file, err := f.drive.get(f.invite.Owner, f.invite.App, id)
	if err != nil {
		return file, rendezvousTestSnapshot{}, err
	}
	if err := f.checkFile(file, fmt.Sprintf("rdv/%s/%d", f.invite.Session, generation)); err != nil {
		return file, rendezvousTestSnapshot{}, err
	}
	if len(file.Description) > rendezvousTestMaxSnapshotBytes {
		return file, rendezvousTestSnapshot{}, errors.New("rendezvous snapshot over cap")
	}
	var snapshot rendezvousTestSnapshot
	if err := json.Unmarshal([]byte(file.Description), &snapshot); err != nil {
		return file, snapshot, err
	}
	if snapshot.Version != 1 || snapshot.Generation != generation {
		return file, snapshot, errors.New("rendezvous generation mismatch")
	}
	if len(snapshot.Claims) > f.maxClaims {
		return file, snapshot, errors.New("rendezvous claim count over cap")
	}
	seen := make(map[string]bool, len(snapshot.Claims)+len(snapshot.Done))
	for _, claim := range snapshot.Claims {
		if claim.ID == "" || seen[claim.ID] || !rendezvousTestCheckClaim(f.invite.SharedKey, f.invite.Session, generation, claim) {
			return file, snapshot, errors.New("invalid rendezvous claim")
		}
		seen[claim.ID] = true
	}
	for _, receipt := range snapshot.Done {
		if receipt.ID == "" || seen[receipt.ID] || len(receipt.ClaimMAC) != sha256.Size ||
			!ed25519.Verify(f.invite.ExitPublic, rendezvousTestReceiptMessage(f.invite.Session, generation, receipt.ID, receipt.ClaimMAC), receipt.Signature) {
			return file, snapshot, errors.New("invalid rendezvous receipt")
		}
		seen[receipt.ID] = true
	}
	return file, snapshot, nil
}

func rendezvousTestHasOwnClaim(snapshot rendezvousTestSnapshot, own rendezvousTestClaim) (bool, error) {
	for _, claim := range snapshot.Claims {
		if claim.ID == own.ID {
			if claim.Client != own.Client || claim.Run != own.Run || !bytes.Equal(claim.Payload, own.Payload) || !hmac.Equal(claim.MAC, own.MAC) {
				return false, errors.New("rendezvous OPEN identity collision")
			}
			return true, nil
		}
	}
	for _, receipt := range snapshot.Done {
		if receipt.ID == own.ID {
			if !hmac.Equal(receipt.ClaimMAC, own.MAC) {
				return false, errors.New("rendezvous receipt identity collision")
			}
			return true, nil
		}
	}
	return false, nil
}

func rendezvousTestReceiptsFit(snapshot rendezvousTestSnapshot) (bool, error) {
	// The client cannot sign exit receipts, but their Ed25519 signature length is
	// fixed. Reserve the complete post-processing snapshot before publishing.
	settled := snapshot
	settled.Done = append([]rendezvousTestReceipt(nil), snapshot.Done...)
	for _, claim := range snapshot.Claims {
		settled.Done = append(settled.Done, rendezvousTestReceipt{
			ID: claim.ID, ClaimMAC: claim.MAC, Signature: make([]byte, ed25519.SignatureSize),
		})
	}
	settled.Claims = nil
	encoded, err := json.Marshal(settled)
	return len(encoded) <= rendezvousTestMaxSnapshotBytes, err
}

func rendezvousTestEffectHash(claim rendezvousTestClaim) [sha256.Size]byte {
	identity, _ := json.Marshal(struct {
		ID, Client, Run string
		Payload         []byte
	}{claim.ID, claim.Client, claim.Run, claim.Payload})
	return sha256.Sum256(identity)
}

func (f *rendezvousTestFixture) recordEffect(claim rendezvousTestClaim) error {
	digest := rendezvousTestEffectHash(claim)
	f.mu.Lock()
	defer f.mu.Unlock()
	if previous, exists := f.effectHash[claim.ID]; exists {
		if previous != digest {
			return errors.New("cross-generation OPEN identity collision")
		}
		return nil
	}
	f.effectHash[claim.ID] = digest
	f.processed[claim.ID] = 1
	return nil
}

func (f *rendezvousTestFixture) appendOpen(ctx context.Context, claim rendezvousTestClaim, beforeFirstPatch func()) (string, error) {
	if claim.ID == "" || claim.Client == "" || claim.Run == "" {
		return "", errors.New("missing OPEN identity")
	}
	var hooked bool
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		anchor, err := f.readAnchor()
		if err != nil {
			return "", err
		}
		file, snapshot, err := f.readSnapshot(anchor.CurrentID, anchor.Generation)
		if err != nil {
			return "", err
		}
		if snapshot.Sealed {
			continue
		}
		preparedClaim := rendezvousTestSealClaim(f.invite.SharedKey, f.invite.Session, anchor.Generation, claim)
		found, err := rendezvousTestHasOwnClaim(snapshot, preparedClaim)
		if err != nil {
			return "", err
		}
		if found {
			return "inline", nil
		}
		if len(snapshot.Claims) >= f.maxClaims {
			return f.immutableFallback(claim)
		}
		snapshot.Claims = append(snapshot.Claims, preparedClaim)
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return "", err
		}
		if len(encoded) > rendezvousTestMaxSnapshotBytes {
			return f.immutableFallback(claim)
		}
		receiptsFit, err := rendezvousTestReceiptsFit(snapshot)
		if err != nil {
			return "", err
		}
		if !receiptsFit {
			return f.immutableFallback(claim)
		}
		if beforeFirstPatch != nil && !hooked {
			hooked = true
			beforeFirstPatch()
		}
		_, err = f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(encoded))
		if err == nil {
			return "inline", nil
		}
		if errors.Is(err, errRendezvousLostReply) {
			current, currentSnapshot, readErr := f.readSnapshot(file.ID, anchor.Generation)
			if readErr != nil {
				return "", readErr
			}
			found, matchErr := rendezvousTestHasOwnClaim(currentSnapshot, preparedClaim)
			if matchErr != nil {
				return "", matchErr
			}
			if found {
				return "inline", nil
			}
			if current.Description == file.Description {
				_, retryErr := f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(encoded))
				if retryErr == nil {
					return "inline", nil
				}
				if retryErr != nil && !errors.Is(retryErr, errRendezvousPrecondition) {
					return "", retryErr
				}
			}
			continue
		}
		if errors.Is(err, errRendezvousPrecondition) {
			continue
		}
		return "", err
	}
	return f.immutableFallback(claim)
}

func (f *rendezvousTestFixture) immutableFallback(claim rendezvousTestClaim) (string, error) {
	// The fake uses a prepared unique ID, mirroring a generated Drive ID.
	claim = rendezvousTestSealClaim(f.invite.SharedKey, f.invite.Session, 0, claim)
	encoded, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	id := "rdv-fallback-" + claim.ID
	file, err := f.drive.create(f.invite.Owner, f.invite.App, id, "rdv/"+f.invite.Session+"/fallback/"+claim.ID, f.invite.Parent, string(encoded))
	if errors.Is(err, errRendezvousPrecondition) {
		file, err = f.drive.get(f.invite.Owner, f.invite.App, id)
		if err == nil && (file.Name != "rdv/"+f.invite.Session+"/fallback/"+claim.ID || file.Description != string(encoded)) {
			err = errors.New("fallback identity collision")
		}
	}
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.owned[file.ID] = file
	f.mu.Unlock()
	return "fallback", nil
}

func (f *rendezvousTestFixture) consumeGeneration(id string, generation int) error {
	for attempt := 0; attempt < 32; attempt++ {
		file, snapshot, err := f.readSnapshot(id, generation)
		if err != nil {
			return err
		}
		if len(snapshot.Claims) == 0 {
			return nil
		}
		// A peer with the mailbox credential may have written a snapshot that
		// bypassed the client admission check. Do not perform an exit effect that
		// cannot be acknowledged within the generation's hard size cap.
		receiptsFit, err := rendezvousTestReceiptsFit(snapshot)
		if err != nil {
			return err
		}
		if !receiptsFit {
			return errors.New("receipt tombstones require rollover")
		}
		for _, claim := range snapshot.Claims {
			if err := f.recordEffect(claim); err != nil {
				return err
			}
			snapshot.Done = append(snapshot.Done, rendezvousTestReceipt{ID: claim.ID, ClaimMAC: bytes.Clone(claim.MAC),
				Signature: ed25519.Sign(f.exitKey, rendezvousTestReceiptMessage(f.invite.Session, generation, claim.ID, claim.MAC))})
		}
		snapshot.Claims = nil
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if len(encoded) > rendezvousTestMaxSnapshotBytes {
			return errors.New("receipt tombstones require rollover")
		}
		_, err = f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(encoded))
		if err == nil {
			return nil
		}
		if errors.Is(err, errRendezvousLostReply) {
			current, currentSnapshot, readErr := f.readSnapshot(file.ID, generation)
			if readErr != nil {
				return readErr
			}
			if current.Description == string(encoded) && len(currentSnapshot.Claims) == 0 {
				return nil
			}
		}
		if errors.Is(err, errRendezvousPrecondition) || errors.Is(err, errRendezvousLostReply) {
			continue
		}
		return err
	}
	return errors.New("receipt CAS retries exhausted")
}

func (f *rendezvousTestFixture) consumeFallback() error {
	files, err := f.drive.list(f.invite.Owner, f.invite.App, "rdv/"+f.invite.Session+"/fallback/")
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := f.checkFile(file, file.Name); err != nil {
			return err
		}
		var claim rendezvousTestClaim
		if err := json.Unmarshal([]byte(file.Description), &claim); err != nil {
			return err
		}
		if file.Name != "rdv/"+f.invite.Session+"/fallback/"+claim.ID || !rendezvousTestCheckClaim(f.invite.SharedKey, f.invite.Session, 0, claim) {
			return errors.New("invalid immutable OPEN fallback")
		}
		if err := f.recordEffect(claim); err != nil {
			return err
		}
	}
	return nil
}

func (f *rendezvousTestFixture) rollover() error {
	f.mu.Lock()
	oldID, oldGeneration := f.current, f.generation
	f.mu.Unlock()
	for attempt := 0; attempt < 32; attempt++ {
		file, snapshot, err := f.readSnapshot(oldID, oldGeneration)
		if err != nil {
			return err
		}
		if snapshot.Sealed {
			break
		}
		snapshot.Sealed = true
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if len(encoded) > rendezvousTestMaxSnapshotBytes {
			return errors.New("sealed rendezvous snapshot over cap")
		}
		_, err = f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(encoded))
		if err == nil {
			break
		}
		if !errors.Is(err, errRendezvousPrecondition) {
			return err
		}
		if attempt == 31 {
			return errors.New("cannot seal old rendezvous")
		}
	}
	nextGeneration := oldGeneration + 1
	nextID := fmt.Sprintf("rdv-generation-%d", nextGeneration)
	initial, _ := json.Marshal(rendezvousTestSnapshot{Version: 1, Generation: nextGeneration})
	newFile, err := f.drive.create(f.invite.Owner, f.invite.App, nextID, fmt.Sprintf("rdv/%s/%d", f.invite.Session, nextGeneration), f.invite.Parent, string(initial))
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.owned[nextID] = newFile
	f.mu.Unlock()
	if err := f.probeCAS(nextID, nextGeneration); err != nil {
		return err
	}
	anchor, err := f.drive.get(f.invite.Owner, f.invite.App, f.invite.AnchorID)
	if err != nil {
		return err
	}
	a := rendezvousTestAnchor{Version: 1, Generation: nextGeneration, Session: f.invite.Session, CurrentID: nextID}
	a.Signature = ed25519.Sign(f.exitKey, rendezvousTestAnchorMessage(a))
	encoded, _ := json.Marshal(a)
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, anchor.ID, anchor.ETag, string(encoded)); err != nil {
		return err
	}
	f.mu.Lock()
	f.current, f.generation = nextID, nextGeneration
	f.mu.Unlock()
	return nil
}

func (f *rendezvousTestFixture) cleanup() error {
	f.mu.Lock()
	owned := make([]rendezvousTestFile, 0, len(f.owned))
	for _, file := range f.owned {
		owned = append(owned, file)
	}
	f.mu.Unlock()
	for _, file := range owned {
		if err := f.drive.deleteOwned(f.invite.Owner, f.invite.App, file); err != nil {
			return err
		}
		if _, err := f.drive.get(f.invite.Owner, f.invite.App, file.ID); !errors.Is(err, errRendezvousMissing) {
			return errors.New("exact rendezvous target survived cleanup")
		}
	}
	return nil
}

func TestRendezvousFixedIDConcurrentOpenMatrix(t *testing.T) {
	for _, count := range []int{1, 2, 4, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := newRendezvousTestFixture(t, 32)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			start := make(chan struct{})
			atFirstPatch := make(chan struct{}, count)
			releaseFirstPatch := make(chan struct{})
			errCh := make(chan error, count)
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					claim := rendezvousTestClaim{ID: fmt.Sprintf("open-%02d", i), Client: fmt.Sprintf("client-%02d", i), Run: "run-1", Payload: []byte("opaque-open")}
					mode, err := f.appendOpen(ctx, claim, func() {
						atFirstPatch <- struct{}{}
						<-releaseFirstPatch
					})
					if err == nil && mode != "inline" {
						err = fmt.Errorf("mode = %s, want inline", mode)
					}
					errCh <- err
				}(i)
			}
			close(start)
			for i := 0; i < count; i++ {
				select {
				case <-atFirstPatch:
				case <-ctx.Done():
					t.Fatal("clients did not reach simultaneous CAS attempt")
				}
			}
			close(releaseFirstPatch)
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.consumeGeneration(f.current, f.generation); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := f.readSnapshot(f.current, f.generation)
			if err != nil || len(snapshot.Claims) != 0 || len(snapshot.Done) != count {
				t.Fatalf("snapshot after receipts: pending=%d done=%d error=%v", len(snapshot.Claims), len(snapshot.Done), err)
			}
			for i := 0; i < count; i++ {
				if got := f.processed[fmt.Sprintf("open-%02d", i)]; got != 1 {
					t.Fatalf("OPEN %d effects = %d, want one", i, got)
				}
			}
			if err := f.cleanup(); err != nil {
				t.Fatal(err)
			}
			calls, units := f.drive.snapshotMeter()
			if calls["patch_412"] < 2+count-1 {
				t.Fatalf("simultaneous CAS conflicts = %d, want at least %d including two probe rejections", calls["patch_412"], 2+count-1)
			}
			t.Logf("clients=%d calls=%v local_upper_units=%d", count, calls, units)
		})
	}
}

func TestRendezvousFixedIDLostPatchReadback(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	f.drive.mu.Lock()
	f.drive.lostNext[f.current] = true
	f.drive.mu.Unlock()
	claim := rendezvousTestClaim{ID: "lost-reply", Client: "client", Run: "run", Payload: []byte("opaque-open")}
	mode, err := f.appendOpen(context.Background(), claim, nil)
	if err != nil || mode != "inline" {
		t.Fatalf("append after lost response: mode=%s error=%v", mode, err)
	}
	_, snapshot, err := f.readSnapshot(f.current, f.generation)
	if err != nil || len(snapshot.Claims) != 1 || snapshot.Claims[0].ID != claim.ID {
		t.Fatalf("lost-response readback did not retain exact claim: %v", err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDRolloverRace(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	ready := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := f.appendOpen(context.Background(), rendezvousTestClaim{ID: "racing-open", Client: "client", Run: "run", Payload: []byte("opaque-open")}, func() {
			close(ready)
			<-release
		})
		result <- err
	}()
	<-ready
	if err := f.rollover(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	_, old, err := f.readSnapshot("rdv-generation-1", 1)
	if err != nil || !old.Sealed || len(old.Claims) != 0 {
		t.Fatalf("old generation was not sealed cleanly: %v", err)
	}
	if f.processed["racing-open"] != 1 {
		t.Fatal("racing OPEN missing after rollover")
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDImmutableFallbackAndExactCleanup(t *testing.T) {
	f := newRendezvousTestFixture(t, 1)
	sentinel, err := f.drive.create(f.invite.Owner, f.invite.App, "unrelated-sentinel", "unrelated/file", f.invite.Parent, "leave-me")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		mode, err := f.appendOpen(context.Background(), rendezvousTestClaim{ID: fmt.Sprintf("claim-%d", i), Client: "client", Run: "run", Payload: []byte("opaque-open")}, nil)
		if err != nil || mode != []string{"inline", "fallback"}[i] {
			t.Fatalf("claim %d mode=%s error=%v", i, mode, err)
		}
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if err := f.consumeFallback(); err != nil {
		t.Fatal(err)
	}
	if f.processed["claim-0"] != 1 || f.processed["claim-1"] != 1 {
		t.Fatalf("wrong effects: %#v", f.processed)
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
	still, err := f.drive.get(f.invite.Owner, f.invite.App, sentinel.ID)
	if err != nil || still.Description != "leave-me" {
		t.Fatalf("unrelated sentinel changed or deleted: %v", err)
	}
	_, units := f.drive.snapshotMeter()
	if units <= 0 {
		t.Fatal("meter did not count calls")
	}
}

func rendezvousTestReceiptBoundary(t *testing.T, f *rendezvousTestFixture, claim rendezvousTestClaim) (rendezvousTestSnapshot, rendezvousTestClaim) {
	t.Helper()
	prepared := rendezvousTestSealClaim(f.invite.SharedKey, f.invite.Session, f.generation, claim)
	completed := rendezvousTestReceipt{ID: prepared.ID, ClaimMAC: bytes.Clone(prepared.MAC),
		Signature: ed25519.Sign(f.exitKey, rendezvousTestReceiptMessage(f.invite.Session, f.generation, prepared.ID, prepared.MAC))}
	snapshot := rendezvousTestSnapshot{Version: 1, Generation: f.generation}
	for i := 0; i < 128; i++ {
		trial := snapshot
		trial.Done = append([]rendezvousTestReceipt(nil), snapshot.Done...)
		id := fmt.Sprintf("past-%03d-", i)
		trial.Done = append(trial.Done, rendezvousTestReceipt{ID: id, ClaimMAC: bytes.Repeat([]byte{1}, sha256.Size),
			Signature: bytes.Repeat([]byte{1}, ed25519.SignatureSize)})
		pending := trial
		pending.Claims = []rendezvousTestClaim{prepared}
		settled := trial
		settled.Done = append(append([]rendezvousTestReceipt(nil), trial.Done...), completed)
		pendingJSON, _ := json.Marshal(pending)
		settledJSON, _ := json.Marshal(settled)
		padding := max(0, rendezvousTestMaxSnapshotBytes-len(settledJSON)+1)
		if padding < 256 && len(pendingJSON)+padding <= rendezvousTestMaxSnapshotBytes {
			id += strings.Repeat("x", padding)
			trial.Done[len(trial.Done)-1].ID = id
			trial.Done[len(trial.Done)-1].Signature = ed25519.Sign(f.exitKey, rendezvousTestReceiptMessage(
				f.invite.Session, f.generation, id, trial.Done[len(trial.Done)-1].ClaimMAC))
			t.Logf("receipt boundary: prior_done=%d pending_bytes=%d settled_bytes=%d cap=%d",
				len(trial.Done), len(pendingJSON)+padding, len(settledJSON)+padding, rendezvousTestMaxSnapshotBytes)
			return trial, prepared
		}
		regularID := fmt.Sprintf("past-%03d", i)
		snapshot.Done = append(snapshot.Done, rendezvousTestReceipt{ID: regularID, ClaimMAC: bytes.Repeat([]byte{1}, sha256.Size),
			Signature: ed25519.Sign(f.exitKey, rendezvousTestReceiptMessage(f.invite.Session, f.generation, regularID, bytes.Repeat([]byte{1}, sha256.Size)))})
	}
	t.Fatal("could not construct a valid snapshot whose receipt exceeds the cap")
	return rendezvousTestSnapshot{}, rendezvousTestClaim{}
}

func TestRendezvousFixedIDReservesReceiptCapacityBeforeEffect(t *testing.T) {
	f := newRendezvousTestFixture(t, 128)
	claim := rendezvousTestClaim{ID: "receipt-boundary", Client: "client", Run: "run", Payload: []byte("open")}
	snapshot, prepared := rendezvousTestReceiptBoundary(t, f, claim)
	file, _, err := f.readSnapshot(f.current, f.generation)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := json.Marshal(snapshot)
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(seed)); err != nil {
		t.Fatal(err)
	}
	mode, err := f.appendOpen(context.Background(), claim, nil)
	if err != nil || mode != "fallback" {
		t.Fatalf("full receipt snapshot must route OPEN to immutable fallback: mode=%s error=%v", mode, err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if f.processed[prepared.ID] != 0 {
		t.Fatal("rendezvous exit produced an effect without a receipt")
	}
	if err := f.consumeFallback(); err != nil || f.processed[prepared.ID] != 1 {
		t.Fatalf("fallback OPEN did not produce one effect: count=%d error=%v", f.processed[prepared.ID], err)
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDRejectsUnreceiptableClaimBeforeEffect(t *testing.T) {
	f := newRendezvousTestFixture(t, 128)
	claim := rendezvousTestClaim{ID: "receipt-boundary", Client: "client", Run: "run", Payload: []byte("open")}
	snapshot, prepared := rendezvousTestReceiptBoundary(t, f, claim)
	snapshot.Claims = []rendezvousTestClaim{prepared}
	file, _, err := f.readSnapshot(f.current, f.generation)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := json.Marshal(snapshot)
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(seed)); err != nil {
		t.Fatal(err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err == nil {
		t.Fatal("exit accepted an OPEN whose signed receipt cannot fit")
	}
	if f.processed[claim.ID] != 0 {
		t.Fatal("exit produced an effect before confirming receipt capacity")
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDRejectsWrongPrincipalAndForgedReceipt(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	if _, err := f.drive.get("different-account", f.invite.App, f.current); !errors.Is(err, errRendezvousDenied) {
		t.Fatalf("cross-account read = %v, want denied", err)
	}
	file, snapshot, err := f.readSnapshot(f.current, f.generation)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Done = append(snapshot.Done, rendezvousTestReceipt{ID: "forged", Signature: bytes.Repeat([]byte{0}, ed25519.SignatureSize)})
	encoded, _ := json.Marshal(snapshot)
	if _, err := f.drive.patch(f.invite.Owner, f.invite.App, file.ID, file.ETag, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.readSnapshot(f.current, f.generation); err == nil {
		t.Fatal("forged exit receipt accepted")
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDRejectsIgnoredCAS(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	f.drive.mu.Lock()
	f.drive.ignoreFence = true
	f.drive.mu.Unlock()
	if err := f.probeCAS(f.current, f.generation); err == nil {
		t.Fatal("ignored If-Match was accepted as a safe rendezvous")
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDRejectsClaimAndReceiptCollisions(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	original := rendezvousTestClaim{ID: "same-id", Client: "client", Run: "run", Payload: []byte("first")}
	if _, err := f.appendOpen(context.Background(), original, nil); err != nil {
		t.Fatal(err)
	}
	conflicting := rendezvousTestClaim{ID: "same-id", Client: "client", Run: "run", Payload: []byte("second")}
	if _, err := f.appendOpen(context.Background(), conflicting, nil); err == nil {
		t.Fatal("same claim ID with different body was accepted")
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.appendOpen(context.Background(), conflicting, nil); err == nil {
		t.Fatal("signed receipt for different body was accepted")
	}
	if _, err := f.appendOpen(context.Background(), original, nil); err != nil {
		t.Fatalf("exact idempotent retry after receipt failed: %v", err)
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestRendezvousFixedIDImmutableListControlMeter(t *testing.T) {
	for _, count := range []int{1, 2, 4, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			d := newRendezvousTestDrive()
			key := []byte("local-only-rendezvous-test-shared-key")
			created := make([]rendezvousTestFile, 0, count)
			for i := 0; i < count; i++ {
				claim := rendezvousTestSealClaim(key, "local-session", 0, rendezvousTestClaim{
					ID: fmt.Sprintf("control-%02d", i), Client: fmt.Sprintf("client-%02d", i), Run: "run", Payload: []byte("opaque-open"),
				})
				encoded, err := json.Marshal(claim)
				if err != nil {
					t.Fatal(err)
				}
				file, err := d.create(d.principal, d.app, "control-"+claim.ID, "rdv/local-session/fallback/"+claim.ID, "owned-mailbox", string(encoded))
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, file)
			}
			found, err := d.list(d.principal, d.app, "rdv/local-session/fallback/")
			if err != nil || len(found) != count {
				t.Fatalf("immutable list found %d/%d: %v", len(found), count, err)
			}
			seen := make(map[string]bool)
			for _, file := range found {
				var claim rendezvousTestClaim
				if err := json.Unmarshal([]byte(file.Description), &claim); err != nil {
					t.Fatal(err)
				}
				if seen[claim.ID] || file.Name != "rdv/local-session/fallback/"+claim.ID || !rendezvousTestCheckClaim(key, "local-session", 0, claim) {
					t.Fatal("invalid or repeated immutable control OPEN")
				}
				seen[claim.ID] = true
			}
			for _, file := range created {
				if err := d.deleteOwned(d.principal, d.app, file); err != nil {
					t.Fatal(err)
				}
				if _, err := d.get(d.principal, d.app, file.ID); !errors.Is(err, errRendezvousMissing) {
					t.Fatalf("control file survived cleanup: %v", err)
				}
			}
			calls, units := d.snapshotMeter()
			t.Logf("immutable clients=%d calls=%v local_upper_units=%d", count, calls, units)
		})
	}
}

func TestRendezvousFixedIDCrossGenerationDedupAndCollision(t *testing.T) {
	f := newRendezvousTestFixture(t, 32)
	original := rendezvousTestClaim{ID: "same-across-epoch", Client: "client", Run: "run", Payload: []byte("original")}
	if _, err := f.appendOpen(context.Background(), original, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if err := f.rollover(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.appendOpen(context.Background(), original, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.consumeGeneration(f.current, f.generation); err != nil {
		t.Fatal(err)
	}
	if f.processed[original.ID] != 1 {
		t.Fatal("replayed OPEN produced another exit effect")
	}
	conflict := rendezvousTestClaim{ID: original.ID, Client: original.Client, Run: original.Run, Payload: []byte("different")}
	if _, err := f.immutableFallback(conflict); err != nil {
		t.Fatal(err)
	}
	if err := f.consumeFallback(); err == nil {
		t.Fatal("cross-generation identity collision was accepted")
	}
	if err := f.cleanup(); err != nil {
		t.Fatal(err)
	}
}
