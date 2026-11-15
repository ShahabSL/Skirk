package skirk

// This entire adapter is research-only. It is not compiled into Skirk binaries.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// x/net/http2 v0.61 permits at most eight internal stream attempts per
// Transport.RoundTrip. Reserve the upper bound before any request is exposed.
const hybridH2AttemptUpper = int64(8)
const hybridCleanupAttemptReserve = 32 * hybridH2AttemptUpper
const hybridDriveCleanupUnitsReserve = int64(40000)
const hybridSealedMax = 1 << 20
const hybridResponseMax = 4 << 20
const hybridCleanupResponseMax = 16 << 10
const hybridCleanupByteReserve = 1 << 20
const hybridAckBytesMax = 4 << 20
const hybridCacheMax = 32 << 20
const hybridEntriesMax = 4096 // Retained until the bounded experiment ends.

type hybridBudget struct {
	mu                                    sync.Mutex
	Calls, BytesOut, BytesIn              int64
	AttemptReservations, OutboundReserved int64
	PendingBytes                          int64
	Denials, StoppedDenials               int64
	WriteCallbacks, WriteErrorCallbacks   int64
	MaxCalls, MaxBytes                    int64
}

func (b *hybridBudget) reserve(n, response int, cleanup bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	callLimit, byteLimit := b.MaxCalls, b.MaxBytes
	if !cleanup {
		callLimit -= hybridCleanupAttemptReserve
		byteLimit -= hybridCleanupByteReserve
	}
	if b.AttemptReservations+hybridH2AttemptUpper > callLimit || b.BytesIn+b.OutboundReserved+b.PendingBytes+int64(n)*hybridH2AttemptUpper+int64(response) > byteLimit {
		b.Denials++
		return errors.New("hybrid global call/byte guard")
	}
	b.Calls++
	b.AttemptReservations += hybridH2AttemptUpper
	b.BytesOut += int64(n)
	b.OutboundReserved += int64(n) * hybridH2AttemptUpper
	b.PendingBytes += int64(response)
	return nil
}
func (b *hybridBudget) finishResponse(reserved, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.PendingBytes -= int64(reserved)
	b.BytesIn += int64(n)
}
func (b *hybridBudget) reserveDrive(cost int, cleanup bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := b.MaxBytes
	if !cleanup {
		limit -= hybridDriveCleanupUnitsReserve
	}
	callLimit := b.MaxCalls
	if !cleanup {
		callLimit -= hybridCleanupAttemptReserve
	}
	upperCost := int64(cost) * hybridH2AttemptUpper
	if b.AttemptReservations+hybridH2AttemptUpper > callLimit || b.BytesOut+upperCost > limit {
		b.Denials++
		return errors.New("hybrid global Drive request/unit guard")
	}
	b.Calls++
	b.AttemptReservations += hybridH2AttemptUpper
	b.BytesOut += upperCost
	return nil
}
func (b *hybridBudget) snapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]int64{"calls": b.Calls, "attempt_reservations_upper": b.AttemptReservations, "request_body_bytes_upper_bound": b.OutboundReserved, "request_body_bytes": b.BytesOut, "response_body_bytes": b.BytesIn, "reserved_response_bytes": b.PendingBytes, "local_guard_denials": b.Denials, "stopped_gate_denials": b.StoppedDenials, "request_write_callbacks": b.WriteCallbacks, "request_write_error_callbacks": b.WriteErrorCallbacks}
}

func (b *hybridBudget) driveSnapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]int64{"visible_transport_dispatches": b.Calls, "attempt_reservations_upper": b.AttemptReservations, "reserved_units_upper": b.BytesOut, "local_guard_denials": b.Denials, "stopped_gate_denials": b.StoppedDenials, "request_write_callbacks": b.WriteCallbacks, "request_write_error_callbacks": b.WriteErrorCallbacks}
}

type hybridCall func(context.Context, string, string, any) (*HTTPResult, error)
type hybridREST struct {
	client  *GoogleHTTPClient
	token   string
	budget  *hybridBudget
	cleanup bool
	onGuard func()
}

func (a *hybridREST) call(ctx context.Context, method, path string, body any) (*HTTPResult, error) {
	var raw []byte
	var e error
	if body != nil {
		raw, e = json.Marshal(body)
		if e != nil {
			return nil, e
		}
	}
	responseCap := hybridResponseMax
	if a.cleanup {
		responseCap = hybridCleanupResponseMax
	}
	reserved := responseCap + 1
	if e = a.budget.reserve(len(raw), reserved, a.cleanup); e != nil {
		if a.onGuard != nil {
			a.onGuard()
		}
		return nil, e
	}
	actualReceived := 0
	defer func() { a.budget.finishResponse(reserved, actualReceived) }()
	host := "pubsub.googleapis.com"
	urlHost := host
	if isGoogleFrontRoute(a.client.route.Mode) {
		urlHost = "www.google.com"
	}
	request, e := http.NewRequestWithContext(ctx, method, "https://"+urlHost+path, bytes.NewReader(raw))
	if e != nil {
		return nil, e
	}
	request.Host = host
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	started := time.Now()
	response, e := a.client.client.Do(request)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		return nil, errors.New("unexpected compressed hybrid response")
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, int64(responseCap+1)))
	ended := time.Now()
	actualReceived = len(data)
	if e != nil {
		return nil, e
	}
	if len(data) > responseCap {
		return nil, errors.New("hybrid HTTP response too large")
	}

	return &HTTPResult{Status: response.StatusCode, Body: data, Header: response.Header, Proto: response.Proto, RequestStarted: started, ResponseReceived: ended, Attempts: 1}, nil
}

type hybridMessage struct {
	AckID   string `json:"ackId"`
	Message struct {
		Data        string `json:"data"`
		PublishTime string `json:"publishTime"`
		ID          string `json:"messageId"`
	} `json:"message"`
}
type hybridEntry struct {
	info   ObjectInfo
	sealed []byte
	acks   map[string]struct{}
	done   bool
	queued bool
}
type hybridStore struct {
	base                                                BlobStore
	publish, pull, ack                                  hybridCall
	topic, subscription                                 string
	sid                                                 [16]byte
	direction, sendDirection                            byte
	clientID, runID, secret                             string
	key                                                 []byte
	startedAt                                           time.Time
	now                                                 func() time.Time
	mu                                                  sync.Mutex
	entries                                             map[string]*hybridEntry
	cacheBytes                                          int
	ackBytes                                            int
	receive                                             func(ObjectInfo) bool
	event                                               func(string)
	ctx                                                 context.Context
	cancel                                              context.CancelFunc
	wg                                                  sync.WaitGroup
	errMu                                               sync.Mutex
	err                                                 error
	published, delivered, duplicates, acked, emptyPulls atomic.Int64
}

func newHybridStore(base BlobStore, secret string, sid [16]byte, sendDirection byte, cid, rid string) (*hybridStore, error) {
	key, e := DeriveKey(secret)
	if e != nil {
		return nil, e
	}
	key = hkdfSHA256(key, sid[:], []byte("skirk-research-pubsub-hybrid-v1"+cid+"\x00"+rid), 32)
	recv := DirectionUp
	if sendDirection == DirectionUp {
		recv = DirectionDown
	}
	return &hybridStore{base: base, secret: secret, sid: sid, direction: recv, sendDirection: sendDirection, clientID: cid, runID: rid, key: key, entries: map[string]*hybridEntry{}, startedAt: time.Now().Add(-muxStartupCatchup), now: time.Now}, nil
}
func (s *hybridStore) noteEvent(kind string) {
	if s.event != nil {
		s.event(kind)
	}
}
func (s *hybridStore) setError(e error) {
	if e == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = e
	}
	s.errMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}
func (s *hybridStore) error() error { s.errMu.Lock(); defer s.errMu.Unlock(); return s.err }
func hybridID(name string, sealed []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(sealed)
	return "pubsub:" + hex.EncodeToString(h.Sum(nil))
}
func (s *hybridStore) seal(name string, sealed []byte) ([]byte, error) {
	if len(name) == 0 || len(name) > 4096 || len(sealed) == 0 || len(sealed) > hybridSealedMax {
		return nil, errors.New("hybrid record bounds")
	}
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name})
	if !ok || !meta.Priority || meta.ClientID != s.clientID || meta.RunID != s.runID || !strings.HasPrefix(name, muxDirPrefix(s.sid, s.sendDirection, s.clientID, s.runID)) {
		return nil, errors.New("hybrid outgoing identity")
	}
	raw := make([]byte, 2+len(name)+len(sealed))
	binary.BigEndian.PutUint16(raw, uint16(len(name)))
	copy(raw[2:], name)
	copy(raw[2+len(name):], sealed)
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return nil, e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return append(nonce, aead.Seal(nil, nonce, raw, append([]byte("skirk-hybrid-v1"), s.sendDirection))...), nil
}
func (s *hybridStore) open(data []byte) (string, []byte, error) {
	if len(data) > hybridSealedMax+8192 {
		return "", nil, errors.New("hybrid outer bounds")
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return "", nil, e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return "", nil, e
	}
	if len(data) < aead.NonceSize()+aead.Overhead()+2 {
		return "", nil, errors.New("hybrid truncated record")
	}
	raw, e := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], append([]byte("skirk-hybrid-v1"), s.direction))
	if e != nil {
		return "", nil, errors.New("hybrid outer authentication")
	}
	n := int(binary.BigEndian.Uint16(raw))
	if n < 1 || n > 4096 || 2+n >= len(raw) || len(raw)-2-n > hybridSealedMax {
		return "", nil, errors.New("hybrid inner bounds")
	}
	name := string(raw[2 : 2+n])
	sealed := raw[2+n:]
	meta, ok := parseMuxObjectInfo(ObjectInfo{Name: name})
	if !ok || !meta.Priority || meta.MetadataOnly || meta.ClientID != s.clientID || meta.RunID != s.runID || !strings.HasPrefix(name, muxDirPrefix(s.sid, s.direction, s.clientID, s.runID)) {
		return "", nil, errors.New("hybrid incoming identity")
	}
	key, e := DeriveMuxLaneKeyV4(s.secret, s.sid, s.direction, meta.ClientID, meta.RunID, meta.Lane)
	if e != nil {
		return "", nil, e
	}
	env, plain, e := OpenEnvelope(key, sealed)
	if e != nil {
		return "", nil, errors.New("hybrid inner authentication")
	}
	if env.SessionID != s.sid || env.Direction != s.direction || env.Sequence != meta.Seq {
		return "", nil, errors.New("hybrid mux envelope identity")
	}
	frames, e := decodeMuxBatch(plain)
	if e != nil || len(frames) == 0 {
		return "", nil, errors.New("hybrid mux batch invalid")
	}
	return name, bytes.Clone(sealed), nil
}
func (s *hybridStore) Put(ctx context.Context, name string, data []byte) error {
	_, e := s.PutObject(ctx, name, data)
	return e
}
func (s *hybridStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if !strings.Contains(name, "/p0/") {
		if b, ok := s.base.(ObjectPutStore); ok {
			return b.PutObject(ctx, name, data)
		}
		return ObjectInfo{Name: name, Size: int64(len(data))}, s.base.Put(ctx, name, data)
	}
	outer, e := s.seal(name, data)
	if e != nil {
		return ObjectInfo{}, e
	}
	r, e := s.publish(ctx, "POST", "/v1/"+s.topic+":publish", map[string]any{"messages": []any{map[string]any{"data": base64.StdEncoding.EncodeToString(outer)}}})
	if e = pubsubStreamRequire(r, e); e != nil {
		return ObjectInfo{}, e
	}
	var response struct {
		IDs []string `json:"messageIds"`
	}
	if json.Unmarshal(r.Body, &response) != nil || len(response.IDs) != 1 || response.IDs[0] == "" {
		return ObjectInfo{}, errors.New("hybrid ambiguous publish response")
	}
	s.published.Add(1)
	return ObjectInfo{Name: name, ID: hybridID(name, data), Size: int64(len(data))}, nil
}
func (s *hybridStore) PutObjectWithID(ctx context.Context, id, name string, data []byte) (ObjectInfo, error) {
	if strings.Contains(name, "/p0/") {
		return ObjectInfo{}, errors.New("priority reserved Drive ID unsupported")
	}
	if b, ok := s.base.(ObjectPutIDStore); ok {
		return b.PutObjectWithID(ctx, id, name, data)
	}
	return s.PutObject(ctx, name, data)
}
func (s *hybridStore) GenerateObjectIDs(ctx context.Context, n int) ([]string, error) {
	if b, ok := s.base.(ObjectIDReserveStore); ok {
		return b.GenerateObjectIDs(ctx, n)
	}
	return nil, errors.New("normal ID reserve unavailable")
}
func (s *hybridStore) GetByID(ctx context.Context, id string) ([]byte, error) {
	if strings.HasPrefix(id, "pubsub:") {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.entries[id]
		if e == nil || e.done {
			return nil, errors.New("hybrid cached payload unavailable")
		}
		return bytes.Clone(e.sealed), nil
	}
	if b, ok := s.base.(ObjectIDStore); ok {
		return b.GetByID(ctx, id)
	}
	return nil, errors.New("base ID lookup unavailable")
}
func (s *hybridStore) GetNormalByID(ctx context.Context, id string) ([]byte, error) {
	if b, ok := s.base.(NormalObjectIDStore); ok {
		return b.GetNormalByID(ctx, id)
	}
	return s.GetByID(ctx, id)
}
func (s *hybridStore) Get(ctx context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	for _, e := range s.entries {
		if e.info.Name == name && !e.done {
			b := bytes.Clone(e.sealed)
			s.mu.Unlock()
			return b, nil
		}
	}
	s.mu.Unlock()
	if strings.Contains(name, "/p0/") {
		return nil, errors.New("hybrid missing priority name")
	}
	return s.base.Get(ctx, name)
}
func (s *hybridStore) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	id := ""
	for k, e := range s.entries {
		if e.info.Name == name {
			id = k
			break
		}
	}
	s.mu.Unlock()
	if id != "" {
		return s.DeleteID(ctx, id)
	}
	if strings.Contains(name, "/p0/") {
		return errors.New("hybrid missing priority name")
	}
	return s.base.Delete(ctx, name)
}
func (s *hybridStore) DeleteID(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, "pubsub:") {
		if b, ok := s.base.(ObjectIDStore); ok {
			return b.DeleteID(ctx, id)
		}
		return errors.New("base ID delete unavailable")
	}
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		s.mu.Unlock()
		return errors.New("hybrid delete unknown ID")
	}
	if !entry.done {
		s.cacheBytes -= len(entry.sealed)
		entry.sealed = nil
		entry.done = true
	}
	ids := make([]string, 0, len(entry.acks))
	for ack := range entry.acks {
		ids = append(ids, ack)
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	r, e := s.ack(ctx, "POST", "/v1/"+s.subscription+":acknowledge", map[string]any{"ackIds": ids})
	if e = pubsubStreamRequire(r, e); e != nil {
		return e
	}
	s.mu.Lock()
	for _, ack := range ids {
		if _, exists := entry.acks[ack]; exists {
			s.ackBytes -= len(ack)
			delete(entry.acks, ack)
		}
	}
	s.mu.Unlock()
	s.acked.Add(int64(len(ids)))
	return nil
}
func (s *hybridStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	result, e := s.ListFreshContainsPageStatus(ctx, []string{prefix}, time.Time{}, "", 4)
	return result.Objects, e
}
func (s *hybridStore) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if len(contains) == 0 {
		return ObjectListInfo{}, errors.New("hybrid empty discovery predicate")
	}
	// No p0 Drive fallback is emitted, even after ambiguous Pub/Sub publication.
	for _, part := range contains {
		if part == "/p0/" {
			return ObjectListInfo{}, nil
		}
	}
	if b, ok := s.base.(FreshListContainsPageStatusStore); ok {
		return b.ListFreshContainsPageStatus(ctx, append(append([]string{}, contains...), "/p1/"), since, pageToken, maxPages)
	}
	objects, e := s.base.List(ctx, contains[0])
	var matched []ObjectInfo
	for _, o := range objects {
		yes := strings.Contains(o.Name, "/p1/")
		for _, part := range contains {
			yes = yes && strings.Contains(o.Name, part)
		}
		if yes {
			matched = append(matched, o)
		}
	}
	return ObjectListInfo{Objects: matched, Pages: 1}, e
}
func (s *hybridStore) GetMuxObjectMetadataByID(ctx context.Context, id string) (ObjectInfo, bool, error) {
	if strings.HasPrefix(id, "pubsub:") {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.entries[id]
		if e == nil || e.done {
			return ObjectInfo{}, false, nil
		}
		return e.info, true, nil
	}
	if b, ok := s.base.(MuxObjectMetadataStore); ok {
		return b.GetMuxObjectMetadataByID(ctx, id)
	}
	return ObjectInfo{}, false, errors.New("base metadata unavailable")
}
func (s *hybridStore) ingest(ctx context.Context, m hybridMessage) error {
	if len(m.AckID) == 0 || len(m.AckID) > 4096 || len(m.Message.Data) > base64.StdEncoding.EncodedLen(hybridSealedMax+8192) {
		return errors.New("hybrid broker message bounds")
	}
	published, e := time.Parse(time.RFC3339Nano, m.Message.PublishTime)
	if e != nil || published.After(s.now().Add(driveClockTolerance)) {
		return errors.New("hybrid original publish time invalid")
	}
	data, e := base64.StdEncoding.DecodeString(m.Message.Data)
	if e != nil {
		return errors.New("hybrid broker base64 invalid")
	}
	name, sealed, e := s.open(data)
	if e != nil {
		return e
	}
	id := hybridID(name, sealed)
	info := ObjectInfo{Name: name, ID: id, Size: int64(len(sealed)), Updated: published.UTC().Format(time.RFC3339Nano)}
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		if len(s.entries) >= hybridEntriesMax || s.cacheBytes+len(sealed) > hybridCacheMax {
			s.mu.Unlock()
			return errors.New("hybrid cache capacity")
		}
		entry = &hybridEntry{info: info, sealed: sealed, acks: map[string]struct{}{}}
		s.entries[id] = entry
		s.cacheBytes += len(sealed)
	} else {
		s.duplicates.Add(1)
		// A retried publish may receive a newer broker timestamp. Preserve the
		// first authenticated broker time: redelivery cannot refresh replay age.
		info = entry.info
	}
	if _, exists := entry.acks[m.AckID]; !exists && (len(entry.acks) >= 64 || s.ackBytes+len(m.AckID) > hybridAckBytesMax) {
		s.mu.Unlock()
		return errors.New("hybrid redelivery ACK capacity")
	}
	if _, exists := entry.acks[m.AckID]; !exists {
		s.ackBytes += len(m.AckID)
	}
	entry.acks[m.AckID] = struct{}{}
	done := entry.done
	s.mu.Unlock()
	if done {
		return s.DeleteID(ctx, id)
	}
	originalPublished, _ := time.Parse(time.RFC3339Nano, info.Updated)
	if !controlIsFresh(info, s.startedAt) || originalPublished.Before(s.now().Add(-600*time.Second-driveClockTolerance)) {
		return s.DeleteID(ctx, id)
	} // Authenticated stale discard, never replay OPEN.
	s.dispatch(id)
	return nil
}
func (s *hybridStore) dispatch(id string) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil || entry.done || entry.queued || s.receive == nil {
		s.mu.Unlock()
		return
	}
	info := entry.info
	s.mu.Unlock()
	if s.receive(info) {
		s.mu.Lock()
		entry.queued = true
		s.mu.Unlock()
		s.delivered.Add(1)
	}
}
func (s *hybridStore) start(ctx context.Context, pool int) error {
	if pool < 1 || pool > 4 || s.pull == nil || s.ack == nil || s.publish == nil || s.receive == nil {
		return errors.New("invalid hybrid workers")
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	for i := 0; i < pool; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for s.ctx.Err() == nil {
				op, cancel := context.WithTimeout(s.ctx, 25*time.Second)
				r, e := s.pull(op, "POST", "/v1/"+s.subscription+":pull", map[string]any{"maxMessages": 8})
				cancel()
				if e = pubsubStreamRequire(r, e); e != nil {
					if s.ctx.Err() != nil {
						s.noteEvent("pull_cancelled")
						return
					}
					if errors.Is(e, context.DeadlineExceeded) {
						s.noteEvent("pull_deadline")
						continue
					}
					s.noteEvent("pull_error")
					s.setError(e)
					return
				}
				var response struct {
					Messages []hybridMessage `json:"receivedMessages"`
				}
				if (len(bytes.TrimSpace(r.Body)) > 0 && json.Unmarshal(r.Body, &response) != nil) || len(response.Messages) > 8 {
					s.noteEvent("pull_invalid_response")
					s.setError(errors.New("hybrid bounded pull response invalid"))
					return
				}
				for _, m := range response.Messages {
					if e = s.ingest(s.ctx, m); e != nil {
						if s.ctx.Err() == nil {
							s.noteEvent("admission_error")
							s.setError(e)
						}
						return
					}
				}
				if len(response.Messages) == 0 {
					s.emptyPulls.Add(1)
					s.noteEvent("pull_empty")
					select {
					case <-s.ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		retry := time.NewTicker(100 * time.Millisecond)
		defer retry.Stop()
		leases := time.NewTicker(10 * time.Second)
		defer leases.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-retry.C:
				s.mu.Lock()
				ids := make([]string, 0)
				for id, e := range s.entries {
					if !e.done && !e.queued {
						ids = append(ids, id)
					}
				}
				s.mu.Unlock()
				for _, id := range ids {
					s.dispatch(id)
				}
			case <-leases.C:
				s.mu.Lock()
				var ids []string
				for _, e := range s.entries {
					if !e.done {
						for id := range e.acks {
							ids = append(ids, id)
						}
					}
				}
				s.mu.Unlock()
				for len(ids) > 0 {
					n := len(ids)
					if n > 256 {
						n = 256
					}
					r, e := s.ack(s.ctx, "POST", "/v1/"+s.subscription+":modifyAckDeadline", map[string]any{"ackIds": ids[:n], "ackDeadlineSeconds": 30})
					if e = pubsubStreamRequire(r, e); e != nil {
						if s.ctx.Err() == nil {
							s.noteEvent("lease_error")
							s.setError(e)
						}
						return
					}
					ids = ids[n:]
				}
			}
		}
	}()
	return nil
}
func (s *hybridStore) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}
