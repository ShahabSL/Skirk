// Package commentwakemodel is a deterministic research model. It does not
// perform Google requests or install a Skirk transport.
package commentwakemodel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

const (
	wakePrefix       = "skw1:"
	maxWakeWireBytes = 512
	pageSize         = 100
)

var (
	errMissing = errors.New("missing")
	errDenied  = errors.New("creator differs")
	errStale   = errors.New("page token invalidated")
	errCrash   = errors.New("injected exit crash")
)

// The fake assumes the optimistic documented permission shape: only the
// Google identity that created a comment/reply can delete it. It deliberately
// keeps tombstones after deletion, as the Drive resources describe.
type fakeDrive struct {
	comments    map[string]*fakeComment
	serial      uint64
	nonceSerial uint64
	units       int
	anchor      pointer
	pending     []pointer
	// The real API promises a timestamp, not a unique version counter.
	// Values above one model several writes sharing one projected timestamp.
	headGranularity uint64
}

type fakeComment struct {
	id      string
	creator string
	tag     string
	deleted bool
	version uint64
	replies []*fakeReply
}

type fakeReply struct {
	id      string
	creator string
	content string
	deleted bool
}

type pointer struct {
	epoch   uint64
	current string
	old     string
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{comments: make(map[string]*fakeComment)}
}

func (d *fakeDrive) nextID(prefix string) string {
	d.serial++
	return fmt.Sprintf("%s-%04d", prefix, d.serial)
}

func (d *fakeDrive) createComment(creator string) string {
	return d.createTaggedComment(creator, "")
}

func (d *fakeDrive) createTaggedComment(creator, tag string) string {
	d.units += 50
	id := d.nextID("c")
	d.comments[id] = &fakeComment{id: id, creator: creator, tag: tag, version: 1}
	return id
}

func (d *fakeDrive) findCommentsByTag(tag string) []string {
	d.units += 100
	var out []string
	for id, c := range d.comments {
		if !c.deleted && c.tag == tag {
			out = append(out, id)
		}
	}
	return out
}

func (d *fakeDrive) appendReply(commentID, creator, content string) (string, error) {
	d.units += 50
	c := d.comments[commentID]
	if c == nil || c.deleted {
		return "", errMissing
	}
	id := d.nextID("r")
	c.replies = append(c.replies, &fakeReply{id: id, creator: creator, content: content})
	c.version++
	return id, nil
}

func (d *fakeDrive) head(commentID string) (uint64, int, error) {
	d.units += 5
	c := d.comments[commentID]
	if c == nil || c.deleted {
		return 0, 0, errMissing
	}
	// fields=id,modifiedTime: response size does not depend on reply history.
	return d.projectedHead(c), len(`{"id":"c-0001","modifiedTime":"2026-11-16T00:00:00Z"}`), nil
}

func (d *fakeDrive) projectedHead(c *fakeComment) uint64 {
	if d.headGranularity > 1 {
		return (c.version - 1) / d.headGranularity
	}
	return c.version
}

func (d *fakeDrive) fullCommentBytes(commentID string) (int, error) {
	d.units += 5
	c := d.comments[commentID]
	if c == nil || c.deleted {
		return 0, errMissing
	}
	// Exact JSON is only a fake response-size model. The real response has
	// additional provider fields, so live bytes must still be measured.
	type wireReply struct {
		ID      string `json:"id"`
		Content string `json:"content,omitempty"`
		Deleted bool   `json:"deleted"`
	}
	replies := make([]wireReply, 0, len(c.replies))
	for _, r := range c.replies {
		replies = append(replies, wireReply{ID: r.id, Content: r.content, Deleted: r.deleted})
	}
	b, err := json.Marshal(struct {
		ID      string      `json:"id"`
		Replies []wireReply `json:"replies"`
	}{ID: c.id, Replies: replies})
	return len(b), err
}

func (d *fakeDrive) listPage(commentID string, page int, version uint64) ([]fakeReply, int, error) {
	d.units += 100
	c := d.comments[commentID]
	if c == nil || c.deleted {
		return nil, 0, errMissing
	}
	if d.projectedHead(c) != version {
		return nil, 0, errStale
	}
	var live []fakeReply
	for _, r := range c.replies {
		if !r.deleted {
			live = append(live, *r)
		}
	}
	start := page * pageSize
	if start >= len(live) {
		return nil, 0, nil
	}
	end := min(start+pageSize, len(live))
	if end == len(live) {
		return live[start:end], 0, nil
	}
	return live[start:end], page + 1, nil
}

func (d *fakeDrive) deleteReply(commentID, replyID, caller string) error {
	d.units += 50
	c := d.comments[commentID]
	if c == nil || c.deleted {
		return errMissing
	}
	for _, r := range c.replies {
		if r.id == replyID {
			if r.creator != caller {
				return errDenied
			}
			if !r.deleted {
				r.deleted = true
				r.content = ""
				c.version++
			}
			return nil
		}
	}
	return errMissing
}

func (d *fakeDrive) deleteComment(id, caller string) error {
	d.units += 50
	c := d.comments[id]
	if c == nil {
		return errMissing
	}
	if c.creator != caller {
		return errDenied
	}
	c.deleted = true
	c.version++
	return nil
}

// A Drive file's single mutable pointer is deliberately unfenced. Dispatch
// and arrival are separate so a prior timed-out PATCH can arrive late.
func (d *fakeDrive) dispatchPointer(p pointer) int {
	d.units += 50
	d.pending = append(d.pending, p)
	return len(d.pending) - 1
}

func (d *fakeDrive) arrivePointer(i int) {
	d.anchor = d.pending[i]
}

type packet struct {
	Kind       string `json:"k"`
	DeviceID   string `json:"d"`
	AttemptID  string `json:"a"`
	UpID       string `json:"u,omitempty"`
	DownID     string `json:"v,omitempty"`
	Generation string `json:"g,omitempty"`
	Pad        string `json:"p,omitempty"`
}

func seal(key [32]byte, iv [12]byte, p packet) (string, error) {
	// This is a fixture envelope. Fresh independent IVs are injected by tests;
	// production would need a persisted CSPRNG/nonce discipline.
	p.Pad = strings.Repeat("x", 80)
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	b, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(b)
	if err != nil {
		return "", err
	}
	ct := aead.Seal(iv[:], iv[:], plain, []byte(wakePrefix))
	wire := wakePrefix + base64.RawURLEncoding.EncodeToString(ct)
	if len(wire) < 200 || len(wire) > maxWakeWireBytes {
		return "", fmt.Errorf("wake wire size %d", len(wire))
	}
	return wire, nil
}

func open(key [32]byte, wire string) (packet, error) {
	if !strings.HasPrefix(wire, wakePrefix) || len(wire) < 200 || len(wire) > maxWakeWireBytes {
		return packet{}, errors.New("wrong envelope")
	}
	ct, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(wire, wakePrefix))
	if err != nil || len(ct) < 12+16 {
		return packet{}, errors.New("bad ciphertext")
	}
	b, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(b)
	plain, err := aead.Open(nil, ct[:12], ct[12:], []byte(wakePrefix))
	if err != nil {
		return packet{}, err
	}
	var p packet
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return packet{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return packet{}, errors.New("trailing wake data")
	}
	if p.Kind != "wake" && p.Kind != "ack" {
		return packet{}, errors.New("unexpected packet kind")
	}
	return p, nil
}

func fixtureIV(n uint64) [12]byte {
	var iv [12]byte
	binary.BigEndian.PutUint64(iv[4:], n)
	return iv
}

func runtimeID(kitSeed, localEntropy []byte) string {
	h := sha256.Sum256(append(append([]byte("skirk-device-v1"), kitSeed...), localEntropy...))
	return fmt.Sprintf("%x", h[:8])
}

type registration struct {
	deviceID  string
	attemptID string
	upID      string
	downID    string
}

type durableJournal struct {
	byAttempt map[string]registration
}

func newJournal() *durableJournal {
	return &durableJournal{byAttempt: make(map[string]registration)}
}

func (j *durableJournal) register(p packet) error {
	if p.Kind != "wake" || p.DeviceID == "" || p.AttemptID == "" || p.UpID == "" || p.DownID == "" || p.UpID == p.DownID {
		return errors.New("invalid registration")
	}
	k := p.DeviceID + ":" + p.AttemptID
	want := registration{p.DeviceID, p.AttemptID, p.UpID, p.DownID}
	if got, ok := j.byAttempt[k]; ok && got != want {
		return errors.New("nonce replay with different channel IDs")
	}
	j.byAttempt[k] = want
	return nil
}

type fakeExit struct {
	drive      *fakeDrive
	journal    *durableJournal
	key        [32]byte
	commentID  string
	identity   string
	generation string
	// Called just before a registration would be persisted.
	beforeRegister func() error
	// Called after a registration is durable and before any ACK is posted.
	afterRegister func() error
}

// scan retries a changing multi-page result from page zero. It never deletes
// while paging; an unmatched, permanently changing result fails visibly.
func (e *fakeExit) scan(afterFirstPage func()) ([]fakeReply, error) {
	for attempt := 0; attempt < 3; attempt++ {
		version, _, err := e.drive.head(e.commentID)
		if err != nil {
			return nil, err
		}
		var all []fakeReply
		for page := 0; ; {
			items, next, err := e.drive.listPage(e.commentID, page, version)
			if err == errStale {
				break
			}
			if err != nil {
				return nil, err
			}
			all = append(all, items...)
			if page == 0 && afterFirstPage != nil {
				afterFirstPage()
			}
			if next == 0 {
				endVersion, _, err := e.drive.head(e.commentID)
				if err != nil {
					return nil, err
				}
				if endVersion == version {
					return all, nil
				}
				break
			}
			page = next
		}
	}
	return nil, errStale
}

// process models the registration safety boundary. ACK is only published
// after a durable journal insert. A duplicate WAKE gets at most one logical
// registration; duplicate raw replies and ACKs may still occur.
func (e *fakeExit) process() error {
	items, err := e.scan(nil)
	if err != nil {
		return err
	}
	acked := make(map[string]bool)
	for _, r := range items {
		p, err := open(e.key, r.content)
		if err == nil && p.Kind == "ack" && p.Generation == e.generation {
			acked[p.DeviceID+":"+p.AttemptID] = true
		}
	}
	for _, r := range items {
		p, err := open(e.key, r.content)
		if err != nil || p.Kind != "wake" {
			continue
		}
		if e.beforeRegister != nil {
			if err := e.beforeRegister(); err != nil {
				return err
			}
		}
		if err := e.journal.register(p); err != nil {
			return err
		}
		if e.afterRegister != nil {
			if err := e.afterRegister(); err != nil {
				return err
			}
		}
		k := p.DeviceID + ":" + p.AttemptID
		if !acked[k] {
			// The fake's global serial makes IVs unique across exit restarts.
			// A real implementation must use a CSPRNG or persistent counter.
			e.drive.nonceSerial++
			ack, err := seal(e.key, fixtureIV(1_000_000+e.drive.nonceSerial), packet{
				Kind: "ack", DeviceID: p.DeviceID, AttemptID: p.AttemptID, Generation: e.generation,
			})
			if err != nil {
				return err
			}
			if _, err := e.drive.appendReply(e.commentID, e.identity, ack); err != nil {
				return err
			}
			acked[k] = true
		}
		// Cleanup is allowed only for a same-identity client/exit. If this
		// fails, ACK remains valid but history will need another strategy.
		if err := e.drive.deleteReply(e.commentID, r.id, e.identity); err != nil {
			return err
		}
	}
	return nil
}

func countLive(d *fakeDrive, commentID string) int {
	n := 0
	for _, r := range d.comments[commentID].replies {
		if !r.deleted {
			n++
		}
	}
	return n
}

func TestCopiedProfileRequiresFreshRuntimeIdentityAndSealedWake(t *testing.T) {
	kit := []byte("same copied profile bytes")
	a := runtimeID(kit, []byte("installation A entropy"))
	b := runtimeID(kit, []byte("installation B entropy"))
	if a == b {
		t.Fatal("copied profiles collided")
	}
	if runtimeID(kit, nil) != runtimeID(kit, nil) {
		t.Fatal("determinism broke")
	}
	// If a device ID is derived solely from the copied kit, both installs
	// collide. Runtime entropy is therefore a protocol requirement.
	if a == runtimeID(kit, nil) || b == runtimeID(kit, nil) {
		t.Fatal("runtime identity did not separate from copied kit")
	}
	key := sha256.Sum256([]byte("fixture key"))
	wake := packet{Kind: "wake", DeviceID: a, AttemptID: "attempt-1", UpID: "up-1", DownID: "down-1"}
	wire, err := seal(key, fixtureIV(1), wake)
	if err != nil || len(wire) < 200 || len(wire) > 512 {
		t.Fatalf("sealed wake size %d: %v", len(wire), err)
	}
	if strings.Contains(wire, "up-1") || strings.Contains(wire, "attempt-1") {
		t.Fatal("registration appeared in Drive-visible plaintext")
	}
	got, err := open(key, wire)
	if err != nil || got.DeviceID != a || got.UpID != wake.UpID {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	tampered := []byte(wire)
	tampered[len(tampered)-2] ^= 1
	if _, err := open(key, string(tampered)); err == nil {
		t.Fatal("tampered wake authenticated")
	}
}

func TestAmbiguousAppendAckLossAndExitCrash(t *testing.T) {
	d := newFakeDrive()
	commentID := d.createComment("shared-user")
	key := sha256.Sum256([]byte("fixture key"))
	wake := packet{Kind: "wake", DeviceID: "device-1", AttemptID: "attempt-1", UpID: "up-1", DownID: "down-1"}
	wire, _ := seal(key, fixtureIV(1), wake)
	// The provider commits append, then the caller loses the response.
	if _, err := d.appendReply(commentID, "shared-user", wire); err != nil {
		t.Fatal(err)
	}
	j := newJournal()
	e := &fakeExit{drive: d, journal: j, key: key, commentID: commentID, identity: "shared-user", generation: "exit-1"}
	e.beforeRegister = func() error { return errCrash }
	if err := e.process(); err != errCrash || len(j.byAttempt) != 0 || countLive(d, commentID) != 1 {
		t.Fatalf("pre-journal crash boundary: err=%v journal=%d live=%d", err, len(j.byAttempt), countLive(d, commentID))
	}
	e.beforeRegister = nil
	e.afterRegister = func() error { return errCrash }
	if err := e.process(); err != errCrash || len(j.byAttempt) != 1 || countLive(d, commentID) != 1 {
		t.Fatalf("crash boundary: err=%v journal=%d live=%d", err, len(j.byAttempt), countLive(d, commentID))
	}
	// A client that did not receive its append response retries the exact
	// attempt with a fresh AEAD IV. Two raw replies remain one registration.
	dup, _ := seal(key, fixtureIV(2), wake)
	if _, err := d.appendReply(commentID, "shared-user", dup); err != nil {
		t.Fatal(err)
	}
	e.afterRegister = nil
	if err := e.process(); err != nil {
		t.Fatal(err)
	}
	if len(j.byAttempt) != 1 || countLive(d, commentID) != 1 {
		t.Fatalf("dedupe/cleanup: journal=%d live=%d", len(j.byAttempt), countLive(d, commentID))
	}
	// The ACK response to the exit is ambiguous, but the ACK reply is
	// already present. A restart must not re-register or create another ACK.
	restarted := &fakeExit{drive: d, journal: j, key: key, commentID: commentID, identity: "shared-user", generation: "exit-1"}
	if err := restarted.process(); err != nil || countLive(d, commentID) != 1 || len(j.byAttempt) != 1 {
		t.Fatalf("ACK loss/restart: err=%v journal=%d live=%d", err, len(j.byAttempt), countLive(d, commentID))
	}
	ack := d.comments[commentID].replies[len(d.comments[commentID].replies)-1]
	if p, err := open(key, ack.content); err != nil || p.Kind != "ack" || p.Generation != "exit-1" {
		t.Fatalf("invalid ACK: %+v, %v", p, err)
	}
	if err := d.deleteReply(commentID, ack.id, "shared-user"); err != nil {
		t.Fatal(err)
	}
	if countLive(d, commentID) != 0 {
		t.Fatal("ACK cleanup left live reply")
	}
	// A lost client-side ACK must be repaired by re-appending the identical
	// attempt from its local pending journal, never by making a new attempt.
	if _, err := d.appendReply(commentID, "shared-user", wire); err != nil {
		t.Fatal(err)
	}
	if err := restarted.process(); err != nil || len(j.byAttempt) != 1 || countLive(d, commentID) != 1 {
		t.Fatalf("ACK repair: err=%v journal=%d live=%d", err, len(j.byAttempt), countLive(d, commentID))
	}
	// After an exit restart, an ACK from the old generation is stale for
	// opening an active channel; the client must retry the same WAKE.
	newExit := &fakeExit{drive: d, journal: j, key: key, commentID: commentID, identity: "shared-user", generation: "exit-2"}
	if _, err := d.appendReply(commentID, "shared-user", dup); err != nil {
		t.Fatal(err)
	}
	if err := newExit.process(); err != nil || len(j.byAttempt) != 1 {
		t.Fatalf("generation refresh: err=%v journal=%d", err, len(j.byAttempt))
	}
	foundNewGeneration := false
	for _, r := range d.comments[commentID].replies {
		if r.deleted {
			continue
		}
		p, err := open(key, r.content)
		foundNewGeneration = foundNewGeneration || err == nil && p.Kind == "ack" && p.Generation == "exit-2"
	}
	if !foundNewGeneration {
		t.Fatal("new exit generation did not acknowledge retry")
	}
}

func TestUncommittedLostAppendUsesTheSamePendingAttempt(t *testing.T) {
	d := newFakeDrive()
	id := d.createComment("shared-user")
	key := sha256.Sum256([]byte("fixture key"))
	j := newJournal()
	e := &fakeExit{drive: d, journal: j, key: key, commentID: id, identity: "shared-user", generation: "exit-1"}
	// The first request fails before the provider commits anything.
	if err := e.process(); err != nil || len(j.byAttempt) != 0 {
		t.Fatalf("uncommitted append registered: %v, %d", err, len(j.byAttempt))
	}
	wake := packet{Kind: "wake", DeviceID: "device-1", AttemptID: "attempt-1", UpID: "up-1", DownID: "down-1"}
	wire, _ := seal(key, fixtureIV(42), wake)
	if _, err := d.appendReply(id, "shared-user", wire); err != nil {
		t.Fatal(err)
	}
	if err := e.process(); err != nil || len(j.byAttempt) != 1 {
		t.Fatalf("retry failed: %v, %d", err, len(j.byAttempt))
	}
}

func TestDeleteTombstonesAndPermissionBoundary(t *testing.T) {
	d := newFakeDrive()
	id := d.createComment("exit-user")
	replyID, _ := d.appendReply(id, "client-user", "opaque")
	if err := d.deleteReply(id, replyID, "exit-user"); err != errDenied {
		t.Fatalf("cross-account reply deletion: %v", err)
	}
	if err := d.deleteReply(id, replyID, "client-user"); err != nil {
		t.Fatal(err)
	}
	if len(d.comments[id].replies) != 1 || !d.comments[id].replies[0].deleted {
		t.Fatal("Drive tombstone modeled incorrectly")
	}
	initialFullBytes, err := d.fullCommentBytes(id)
	if err != nil {
		t.Fatal(err)
	}
	v, fixedBytes, err := d.head(id)
	if err != nil || fixedBytes > 100 {
		t.Fatalf("constant-size head: version=%d bytes=%d err=%v", v, fixedBytes, err)
	}
	items, _, err := d.listPage(id, 0, v)
	if err != nil || len(items) != 0 {
		t.Fatalf("deleted reply visible in default list: %d, %v", len(items), err)
	}
	// The underlying history still grows. API-visible live-reply bounds do
	// not prove Drive's internal tombstone and comment limits are unlimited.
	for n := 0; n < 500; n++ {
		rid, _ := d.appendReply(id, "client-user", "opaque")
		if err := d.deleteReply(id, rid, "client-user"); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.comments[id].replies) != 501 || countLive(d, id) != 0 {
		t.Fatal("history-growth model failed")
	}
	_, laterBytes, _ := d.head(id)
	if laterBytes != fixedBytes {
		t.Fatalf("idle read grew with tombstones: %d -> %d", fixedBytes, laterBytes)
	}
	laterFullBytes, err := d.fullCommentBytes(id)
	if err != nil || laterFullBytes <= initialFullBytes+10_000 {
		t.Fatalf("full comments.get did not grow with tombstones: %d -> %d, %v", initialFullBytes, laterFullBytes, err)
	}
}

func TestPaginationMutationForcesRestartOrVisibleFailure(t *testing.T) {
	d := newFakeDrive()
	id := d.createComment("shared-user")
	for n := 0; n < 101; n++ {
		_, _ = d.appendReply(id, "shared-user", "invalid encrypted reply")
	}
	e := &fakeExit{drive: d, commentID: id}
	mutated := false
	all, err := e.scan(func() {
		if !mutated {
			_, _ = d.appendReply(id, "shared-user", "late")
			mutated = true
		}
	})
	if err != nil || len(all) != 102 {
		t.Fatalf("rescan lost concurrent append: %d, %v", len(all), err)
	}
	// A deliberately permanently changing provider cannot be silently
	// treated as a complete snapshot. The live harness must cap this case.
	if _, err := e.scan(func() { _, _ = d.appendReply(id, "shared-user", "more") }); err != errStale {
		t.Fatalf("permanent churn reported complete: %v", err)
	}
	if err := d.deleteComment(id, "shared-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.scan(nil); err != errMissing {
		t.Fatalf("deleted comment not visible: %v", err)
	}
}

func TestProjectedModifiedTimeIsNotAProvableChangeCounter(t *testing.T) {
	d := newFakeDrive()
	d.headGranularity = 4
	id := d.createComment("shared-user")
	before, _, _ := d.head(id)
	if _, err := d.appendReply(id, "shared-user", "opaque WAKE"); err != nil {
		t.Fatal(err)
	}
	after, _, _ := d.head(id)
	if before != after || countLive(d, id) != 1 {
		t.Fatalf("coalesced timestamp scenario absent: %d -> %d, live=%d", before, after, countLive(d, id))
	}
	// A watcher comparing only modifiedTime would skip the list forever
	// after this one WAKE. A periodic reconciliation or stronger provider
	// signal is necessary unless Google documents unique change versions.
	items, _, err := d.listPage(id, 0, after)
	if err != nil || len(items) != 1 {
		t.Fatalf("forced reconciliation did not recover wake: %d, %v", len(items), err)
	}
}

func TestEightCopiedProfilesShareOneBootstrapWithoutIdentityCollision(t *testing.T) {
	d := newFakeDrive()
	id := d.createComment("shared-user")
	key := sha256.Sum256([]byte("fixture key"))
	j := newJournal()
	const copies = 8
	for n := 0; n < copies; n++ {
		device := runtimeID([]byte("copied kit"), []byte(fmt.Sprintf("local-%d", n)))
		wire, err := seal(key, fixtureIV(uint64(n+1)), packet{
			Kind: "wake", DeviceID: device, AttemptID: "attempt-1",
			UpID: fmt.Sprintf("up-%d", n), DownID: fmt.Sprintf("down-%d", n),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.appendReply(id, "shared-user", wire); err != nil {
			t.Fatal(err)
		}
	}
	e := &fakeExit{drive: d, journal: j, key: key, commentID: id, identity: "shared-user", generation: "exit-1"}
	if err := e.process(); err != nil {
		t.Fatal(err)
	}
	if len(j.byAttempt) != copies || countLive(d, id) != copies {
		t.Fatalf("eight-client registration/ACK mismatch: %d, %d", len(j.byAttempt), countLive(d, id))
	}
	// The fake is a correctness model only; it does not overturn the
	// measured 8-writer p95 regression from the real comment probe.
}

func TestRotationPointerRollbackAndExactCleanup(t *testing.T) {
	d := newFakeDrive()
	a := d.createTaggedComment("shared-user", "owned-rotation")
	b := d.createTaggedComment("shared-user", "owned-rotation")
	c := d.createTaggedComment("shared-user", "owned-rotation")
	sentinel := d.createTaggedComment("shared-user", "unrelated")
	d.anchor = pointer{epoch: 1, current: a}
	old := d.dispatchPointer(pointer{epoch: 2, current: b, old: a})
	newer := d.dispatchPointer(pointer{epoch: 3, current: c, old: b})
	d.arrivePointer(newer)
	if d.anchor.current != c {
		t.Fatal("new pointer not published")
	}
	// A timed-out earlier PATCH has no documented server-side fence. If it
	// arrives late, a cold client reads a valid but stale pointer.
	d.arrivePointer(old)
	if d.anchor.epoch != 2 || d.anchor.current != b {
		t.Fatal("model did not reproduce stale-pointer rollback")
	}
	if err := d.deleteComment(a, "shared-user"); err != nil {
		t.Fatal(err)
	}
	if err := d.deleteComment(b, "shared-user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.head(d.anchor.current); err != errMissing {
		t.Fatalf("cold client did not hit dead pointer: %v", err)
	}
	// The successor remains live, yet a cold client has no safe way to
	// discover it from this rolled-back single pointer. Exact cleanup must
	// either remove the whole fixture or first prove an alternative root.
	if _, _, err := d.head(c); err != nil {
		t.Fatalf("successor disappeared unexpectedly: %v", err)
	}
	// Reconcile an orphan whose create response was lost through a scoped
	// tag list, then verify exact IDs. An unrelated comment is never removed.
	for _, orphan := range d.findCommentsByTag("owned-rotation") {
		if err := d.deleteComment(orphan, "shared-user"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{a, b, c} {
		if _, _, err := d.head(id); err != errMissing {
			t.Fatalf("comment %s survived exact cleanup: %v", id, err)
		}
	}
	if _, _, err := d.head(sentinel); err != nil {
		t.Fatalf("unrelated sentinel was removed: %v", err)
	}
}

func TestQuotaEnvelope(t *testing.T) {
	const perMinute = 1_000_000
	const dailyFree = 400_000_000
	const mailboxes = 200_000
	const getUnits = 5
	perMailboxMinute := 60 * getUnits
	perMailboxDay := 24 * 60 * perMailboxMinute
	if perMailboxMinute != 300 || perMailboxDay != 432_000 {
		t.Fatal("idle arithmetic changed")
	}
	if perMinute/perMailboxMinute != 3333 || dailyFree/perMailboxDay != 925 {
		t.Fatal("shared-project capacity arithmetic changed")
	}
	if mailboxes*perMailboxMinute != 60_000_000 || mailboxes*perMailboxDay != 86_400_000_000 {
		t.Fatal("200k-account arithmetic changed")
	}
	// One cold wake lower bound: client reply create; idle exit head GET;
	// two consistency head GETs around one active reply list; ACK reply
	// create; client reply list; and one WAKE and ACK deletion each.
	// Excludes retries and all active-channel setup.
	minimumCold := 50 + 5 + 5 + 100 + 5 + 50 + 50 + 100 + 50
	if minimumCold != 415 {
		t.Fatal(strconv.Itoa(minimumCold))
	}
}
