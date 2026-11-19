package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type mediaReaderTestGet struct {
	id        int
	attempt   int
	at        time.Time
	remaining time.Duration
}

type mediaReaderTestProvider struct {
	mu        sync.Mutex
	objects   []pilotObject
	bodies    map[int][]byte
	lists     int
	active    int
	maxActive int
	gets      []mediaReaderTestGet
	attempts  map[int]int
	listDelay func(*http.Request, int) error
	delay     func(*http.Request, mediaReaderTestGet) error
	response  func(*http.Request, mediaReaderTestGet, []byte) *http.Response
}

func (f *mediaReaderTestProvider) roundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("unexpected receive request method %s", req.Method)
	}
	f.mu.Lock()
	if req.URL.Path == "/drive/v3/files" {
		f.lists++
		number := f.lists
		files := f.objects
		f.mu.Unlock()
		if f.listDelay != nil {
			if err := f.listDelay(req, number); err != nil {
				return nil, err
			}
		}
		body, err := json.Marshal(map[string]any{"files": files})
		if err != nil {
			return nil, err
		}
		return stringResponse(http.StatusOK, string(body)), nil
	}
	id, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/drive/v3/files/object-"))
	body, ok := f.bodies[id]
	if err != nil || !ok || req.URL.RawQuery != "alt=media" {
		f.mu.Unlock()
		return nil, fmt.Errorf("unexpected receive request")
	}
	f.attempts[id]++
	now := time.Now()
	deadline, _ := req.Context().Deadline()
	get := mediaReaderTestGet{id, f.attempts[id], now, deadline.Sub(now)}
	f.gets = append(f.gets, get)
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if f.delay != nil {
		if err := f.delay(req, get); err != nil {
			return nil, err
		}
	}
	if f.response != nil {
		return f.response(req, get, body), nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *mediaReaderTestProvider) snapshot() ([]mediaReaderTestGet, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gets), f.lists, f.maxActive
}

func mediaReaderTestMailbox(t *testing.T, count, plainLimit int) (*pilotMailbox, *mediaReaderTestProvider) {
	t.Helper()
	preview := publicPreviewForLifetimeTest(t)
	mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{37}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	mailbox.media, mailbox.publicIPv4 = true, true
	mailbox.mediaBytes, mailbox.batchBytes = plainLimit, pilotWidePlain
	mailbox.deliver = func(context.Context, pilotPacket) {}
	fixture := &mediaReaderTestProvider{bodies: make(map[int][]byte), attempts: make(map[int]int)}
	for seq := count; seq > 0; seq-- {
		fixture.objects = append(fixture.objects, mediaReaderTestObject(t, mailbox, fixture, seq, 24<<10, 24))
	}
	mailbox.http = &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{
		Transport: roundTripFunc(fixture.roundTrip),
	}}
	return mailbox, fixture
}

func mediaReaderTestObject(t *testing.T, mailbox *pilotMailbox, fixture *mediaReaderTestProvider, seq, plainBytes, packets int) pilotObject {
	t.Helper()
	plain := pilotMarshal(pilotMediaTestPackets(time.Now(), netip.MustParseAddrPort("8.8.8.8:3478"), plainBytes, packets))
	wire, err := Seal(mailbox.recvKey, mailbox.tunnel.SessionID, mailbox.recvDir, uint64(seq), plain, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.bodies[seq] = wire
	return pilotObject{
		ID: fmt.Sprintf("object-%d", seq), Name: fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), seq),
		Size: strconv.Itoa(len(wire)),
	}
}

// Each returned chunk spends its share of the simulated 0.5 Mbit/s link.
// A permanent body stall can return a partial chunk before waiting for cancel.
type pacedMediaTestBody struct {
	ctx   context.Context
	data  []byte
	stall bool
}

func (b *pacedMediaTestBody) Read(dst []byte) (int, error) {
	if len(b.data) == 0 {
		if !b.stall {
			return 0, io.EOF
		}
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	n := min(len(dst), len(b.data), 4<<10)
	timer := time.NewTimer(time.Duration(n*16) * time.Microsecond)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-timer.C:
		copy(dst, b.data[:n])
		b.data = b.data[n:]
		return n, nil
	}
}

func (*pacedMediaTestBody) Close() error { return nil }

func TestUDPPreviewMediaReaderRecoversSlowBodyWithoutBlockingFreshReads(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 0, pilotMediaWidePlain)
		slow := mediaReaderTestObject(t, mailbox, fixture, 1, pilotMediaWidePlain, pilotMediaWideBatch)
		other := mediaReaderTestObject(t, mailbox, fixture, 2, pilotMediaWidePlain, pilotMediaWideBatch)
		fresh := mediaReaderTestObject(t, mailbox, fixture, 3, 24<<10, 24)
		fixture.delay = func(_ *http.Request, get mediaReaderTestGet) error {
			if get.id != 3 {
				time.Sleep(800 * time.Millisecond)
			}
			return nil
		}
		fixture.response = func(req *http.Request, get mediaReaderTestGet, wire []byte) *http.Response {
			var body io.ReadCloser = &pacedMediaTestBody{ctx: req.Context(), data: wire}
			if get.id == 3 {
				body = io.NopCloser(bytes.NewReader(wire))
			}
			return &http.Response{StatusCode: http.StatusOK, Body: body}
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		mailbox.readPage(t.Context(), []pilotObject{other, slow})
		// Both initial reads time out after positive progress. Give the
		// first widened window time to start, then admit a new object.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		gets, _, _ := fixture.snapshot()
		if len(gets) != 3 || gets[2].id != 1 || gets[2].remaining != pilotMediaRecoveryTimeout {
			t.Fatalf("only the oldest qualifying retry should widen: %+v", gets)
		}
		mailbox.read(t.Context(), fresh)
		synctest.Wait()
		mailbox.recvMu.Lock()
		freshAccepted := mailbox.replay.contains(3)
		mailbox.recvMu.Unlock()
		if !freshAccepted || mailbox.receivedBatches.Load() != 1 || len(mailbox.cleanup) != 1 {
			t.Fatal("fresh object did not pass while the widened retry was in flight")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		gets, _, _ = fixture.snapshot()
		mailbox.recvMu.Lock()
		slowAccepted := mailbox.replay.contains(1)
		mailbox.recvMu.Unlock()
		if !slowAccepted || mailbox.receivedBatches.Load() != 2 ||
			mailbox.delivered.Load() != pilotMediaWideBatch+24 || len(mailbox.cleanup) != 2 {
			t.Fatalf("exact returned body was not authenticated after widening: GETs %+v", gets)
		}
		if len(gets) != 5 || gets[3].id != 3 || gets[3].remaining != pilotMediaReadTimeout ||
			gets[4].id != 2 || !gets[4].at.After(gets[2].at) || gets[4].remaining >= pilotMediaReadTimeout {
			t.Fatalf("competing retry renewed its original retention or delayed fresh work: %+v", gets)
		}
	})
}

func TestUDPPreviewMediaReaderWidensAtMostOnceAfterGoodStatusAndBodyProgress(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, tc := range []struct {
		name     string
		status   int
		produced int
		widened  int
	}{
		{"HTTP 200 with partial body", http.StatusOK, 4 << 10, 1},
		{"HTTP 200 with no body", http.StatusOK, 0, 0},
		{"HTTP 429 with partial body", http.StatusTooManyRequests, 4 << 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
				fixture.response = func(req *http.Request, _ mediaReaderTestGet, wire []byte) *http.Response {
					return &http.Response{StatusCode: tc.status, Body: &pacedMediaTestBody{
						ctx: req.Context(), data: wire[:tc.produced], stall: true,
					}}
				}
				stop := startMediaReaderTestPoll(t, mailbox)
				defer stop()
				time.Sleep(2 * pilotLifetime)
				synctest.Wait()
				gets, _, _ := fixture.snapshot()
				count := 0
				for _, get := range gets {
					if get.remaining > pilotMediaReadTimeout {
						count++
					}
					if get.remaining > pilotMediaRecoveryTimeout ||
						get.at.Add(get.remaining).After(gets[0].at.Add(pilotLifetime)) {
						t.Fatalf("attempt exceeded one of its deadlines: %+v", gets)
					}
				}
				if count != tc.widened || len(gets) < 2 || mailbox.delivered.Load() != 0 || len(mailbox.cleanup) != 0 {
					t.Fatalf("wanted %d wider requests, got %d without authentication: %+v", tc.widened, count, gets)
				}
			})
		})
	}
}

func startMediaReaderTestPoll(t *testing.T, mailbox *pilotMailbox) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	mailbox.mediaReader.wake = make(chan struct{}, 1)
	var owners sync.WaitGroup
	owners.Go(func() { mailbox.poll(ctx) })
	owners.Go(func() { mailbox.mediaReadWakeLoop(ctx) })
	synctest.Wait()
	return func() {
		cancel()
		owners.Wait()
		mailbox.liveCalls.Wait()
	}
}

func TestUDPPreviewMediaReaderRetryWakesWithoutSuccessfulList(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
		fixture.listDelay = func(req *http.Request, number int) error {
			if number == 1 {
				return nil
			}
			<-req.Context().Done()
			return req.Context().Err()
		}
		fixture.delay = func(_ *http.Request, get mediaReaderTestGet) error {
			if get.attempt == 1 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		time.Sleep(2*pilotPollTick + 10*time.Millisecond)
		synctest.Wait()
		gets, lists, maxActive := fixture.snapshot()
		if lists != 2 || len(gets) != 2 || maxActive != 1 || mailbox.receivedBatches.Load() != 1 {
			t.Fatalf("known retry waited for the failed list: gets=%d lists=%d peak=%d authenticated=%d",
				len(gets), lists, maxActive, mailbox.receivedBatches.Load())
		}
		if gets[1].at.Sub(gets[0].at) != pilotPollTick {
			t.Fatalf("retry time %v, want %v", gets[1].at.Sub(gets[0].at), pilotPollTick)
		}
	})
}

func TestUDPPreviewMediaReaderRefillsAfterTwoStalledIDs(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 4, pilotMediaWidePlain)
		fixture.delay = func(req *http.Request, get mediaReaderTestGet) error {
			if get.id <= 2 {
				<-req.Context().Done()
				return req.Context().Err()
			}
			return nil
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		time.Sleep(pilotPollTick + 1600*time.Millisecond + 50*time.Millisecond)
		synctest.Wait()
		gets, _, maxActive := fixture.snapshot()
		ids := make([]int, 0, len(gets))
		for _, get := range gets {
			ids = append(ids, get.id)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []int{1, 2, 3, 4}) || maxActive != 2 || mailbox.receivedBatches.Load() != 2 {
			t.Fatalf("cached fresh work did not refill before the next list: GET ids %v, peak %d, authenticated %d", ids, maxActive, mailbox.receivedBatches.Load())
		}
		if len(mailbox.cleanup) != 2 {
			t.Fatal("cleanup must contain only authenticated objects")
		}
	})
}

func TestUDPPreviewMediaReaderHealthyNoDuplicateGets(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, plainLimit := range []int{pilotMediaMaxPlain, pilotMediaWidePlain} {
		t.Run(strconv.Itoa(plainLimit), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mailbox, fixture := mediaReaderTestMailbox(t, 8, plainLimit)
				fixture.delay = func(_ *http.Request, _ mediaReaderTestGet) error {
					time.Sleep(50 * time.Millisecond)
					return nil
				}
				stop := startMediaReaderTestPoll(t, mailbox)
				defer stop()
				time.Sleep(pilotPollTick + 250*time.Millisecond)
				synctest.Wait()
				gets, lists, maxActive := fixture.snapshot()
				if len(gets) != 8 || lists != 1 || maxActive != 2 || mailbox.receivedBatches.Load() != 8 {
					t.Fatalf("one page should finish in four read waves: gets=%d lists=%d peak=%d delivered=%d",
						len(gets), lists, maxActive, mailbox.receivedBatches.Load())
				}
				for i := 0; i < len(gets); i += 2 {
					if min(gets[i].id, gets[i+1].id) != i+1 || max(gets[i].id, gets[i+1].id) != i+2 {
						t.Fatalf("read wave %d lost sequence order: %+v", i/2, gets[i:i+2])
					}
				}
				time.Sleep(3 * pilotPollTick)
				synctest.Wait()
				gets, lists, _ = fixture.snapshot()
				if len(gets) != 8 || lists < 2 || len(mailbox.cleanup) != 8 {
					t.Fatalf("relists refetched already authenticated objects: gets=%d lists=%d cleanup=%d", len(gets), lists, len(mailbox.cleanup))
				}
			})
		})
	}
}

func TestUDPPreviewMediaReaderRelistDoesNotRenewRetryLifetime(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
		fixture.delay = func(_ *http.Request, _ mediaReaderTestGet) error {
			return io.ErrUnexpectedEOF
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		time.Sleep(2 * pilotLifetime)
		synctest.Wait()
		gets, lists, maxActive := fixture.snapshot()
		if len(gets) != 5 || lists < 10 || maxActive != 1 || len(mailbox.cleanup) != 0 {
			t.Fatalf("unexpected retry or cleanup volume: gets=%d lists=%d peak=%d cleanup=%d",
				len(gets), lists, maxActive, len(mailbox.cleanup))
		}
		for i, get := range gets {
			if !get.at.Before(gets[0].at.Add(pilotLifetime)) || get.remaining > min(1600*time.Millisecond, pilotLifetime-get.at.Sub(gets[0].at)) {
				t.Fatalf("retry was launched outside its first-discovery deadline: %+v", get)
			}
			if i > 0 && get.at.Sub(gets[i-1].at) < pilotPollTick<<(i-1) {
				t.Fatalf("retry %d skipped its delay", i)
			}
		}
	})
}

func TestUDPPreviewMediaReaderThirdAttemptCanRecover(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
		fixture.delay = func(req *http.Request, get mediaReaderTestGet) error {
			if get.attempt <= 2 {
				<-req.Context().Done()
				return req.Context().Err()
			}
			time.Sleep(1200 * time.Millisecond)
			return nil
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		time.Sleep(pilotLifetime)
		synctest.Wait()
		gets, _, maxActive := fixture.snapshot()
		if len(gets) != 3 || maxActive != 1 || mailbox.receivedBatches.Load() != 1 || mailbox.delivered.Load() != 24 {
			t.Fatalf("third attempt within the deadline did not recover: gets=%d peak=%d authenticated=%d delivered=%d",
				len(gets), maxActive, mailbox.receivedBatches.Load(), mailbox.delivered.Load())
		}
	})
}

func TestUDPPreviewMediaReaderEarliestDeadlineWinsWhenSlotOpens(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 4, pilotMediaWidePlain)
		fixture.delay = func(_ *http.Request, get mediaReaderTestGet) error {
			switch get.id {
			case 2:
				if get.attempt == 1 {
					return io.ErrUnexpectedEOF
				}
			case 3:
				time.Sleep(300 * time.Millisecond)
			case 4:
				time.Sleep(700 * time.Millisecond)
			}
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			mailbox.liveCalls.Wait()
		}()
		// The first visible object fails and becomes retryable at 350 ms.
		// Both slots are still occupied. A delayed listing reveals sequence
		// 1 before the slot for sequence 3 opens at 400 ms.
		mailbox.readPage(ctx, fixture.objects[2:3])
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		mailbox.readPage(ctx, fixture.objects[:2])
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		mailbox.readPage(ctx, fixture.objects[3:])
		time.Sleep(210 * time.Millisecond)
		synctest.Wait()
		gets, _, peak := fixture.snapshot()
		if len(gets) != 5 || gets[3].id != 2 || gets[3].attempt != 2 || gets[4].id != 1 || peak != 2 {
			t.Fatalf("eligible retry with earliest discovery did not win the slot: GETs %+v, peak %d", gets, peak)
		}
	})
}

func TestUDPPreviewMediaReaderRetryTimerHonorsQuotaHold(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
		fixture.delay = func(_ *http.Request, get mediaReaderTestGet) error {
			if get.attempt == 1 {
				mailbox.drive.backoff.Observe("fixture", &HTTPResult{
					Status: http.StatusTooManyRequests,
					Header: http.Header{"Retry-After": {"2"}},
					Body:   []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`),
				}, nil)
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		defer stop()
		time.Sleep(pilotPollTick + 1900*time.Millisecond)
		synctest.Wait()
		gets, _, _ := fixture.snapshot()
		if len(gets) != 1 {
			t.Fatalf("read during quota hold: gets=%d", len(gets))
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		gets, _, _ = fixture.snapshot()
		if len(gets) != 2 || gets[1].at.Sub(gets[0].at) < 2*time.Second || mailbox.receivedBatches.Load() != 1 {
			t.Fatalf("retry did not respect and recover from quota hold: %+v", gets)
		}
	})
}

func TestUDPPreviewMediaReaderCapacityKeepsActiveAndRetiresEvictions(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 2*pilotMaxListFiles, pilotMediaWidePlain)
		fixture.delay = func(req *http.Request, _ mediaReaderTestGet) error {
			<-req.Context().Done()
			return req.Context().Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			mailbox.liveCalls.Wait()
		}()
		mailbox.readPage(ctx, fixture.objects[pilotMaxListFiles:])
		synctest.Wait()
		mailbox.readPage(ctx, fixture.objects[:pilotMaxListFiles])
		mailbox.recvMu.Lock()
		r := &mailbox.mediaReader
		count, evicted := len(r.reads), r.stats.evicted
		activeKept := r.sequenceInFlight(1) && r.sequenceInFlight(2)
		retired := r.isRetired(pilotMediaKey{seq: 3, id: "object-3"})
		mailbox.recvMu.Unlock()
		if count != pilotMaxListFiles || evicted != pilotMaxListFiles || !activeKept || !retired {
			t.Fatalf("invalid bounded state: candidates=%d evicted=%d active_kept=%t retired=%t", count, evicted, activeKept, retired)
		}
		mailbox.read(ctx, fixture.objects[len(fixture.objects)-3])
		mailbox.recvMu.Lock()
		_, revived := r.reads[pilotMediaKey{seq: 3, id: "object-3"}]
		mailbox.recvMu.Unlock()
		if revived {
			t.Fatal("repeated listing reset retained evicted object")
		}
	})
}

func TestUDPPreviewMediaReaderClassifiesClippedDeadline(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 1, pilotMediaWidePlain)
		fixture.listDelay = func(_ *http.Request, number int) error {
			if number == 1 {
				mailbox.drive.backoff.Observe("fixture", &HTTPResult{
					Status: http.StatusTooManyRequests,
					Header: http.Header{"Retry-After": {"5"}},
					Body:   []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`),
				}, nil)
			}
			return nil
		}
		fixture.delay = func(req *http.Request, _ mediaReaderTestGet) error {
			<-req.Context().Done()
			return req.Context().Err()
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		time.Sleep(pilotPollTick + pilotLifetime + 50*time.Millisecond)
		synctest.Wait()
		stop()
		gets, _, _ := fixture.snapshot()
		stats := mailbox.mediaReader.stats
		if len(gets) != 1 || gets[0].remaining != time.Second || stats.fetchTimeouts != 0 ||
			stats.retentionTimeouts != 1 || stats.retentionExpired != 1 || len(mailbox.cleanup) != 0 {
			t.Fatalf("clipped request recorded as fetch failure or renewed retention: gets=%+v stats=%+v", gets, stats)
		}
	})
}

func TestUDPPreviewMediaReaderShutdownDoesNotRefill(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 5, pilotMediaWidePlain)
		fixture.delay = func(req *http.Request, _ mediaReaderTestGet) error {
			<-req.Context().Done()
			return req.Context().Err()
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		time.Sleep(2 * pilotPollTick)
		synctest.Wait()
		stop()
		gets, _, maxActive := fixture.snapshot()
		if len(gets) != 2 || maxActive != 2 || len(mailbox.cleanup) != 0 {
			t.Fatalf("canceled lease started more media work: gets=%d peak=%d cleanup=%d", len(gets), maxActive, len(mailbox.cleanup))
		}
	})
}

func TestUDPPreviewMediaReaderUnauthenticatedNameCannotAdvanceReplay(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		mailbox, fixture := mediaReaderTestMailbox(t, 3, pilotMediaWidePlain)
		prefix := mailbox.dirPrefix(mailbox.recvDir)
		fixture.objects[0].Name = fmt.Sprintf("%s%020d", prefix, uint64(1)<<40)
		fixture.objects[1].Name = fixture.objects[2].Name
		fixture.delay = func(_ *http.Request, get mediaReaderTestGet) error {
			if get.id == 1 {
				// Let the high-sequence wrong envelope arrive before this one.
				time.Sleep(50 * time.Millisecond)
			}
			return nil
		}
		stop := startMediaReaderTestPoll(t, mailbox)
		time.Sleep(3 * pilotPollTick)
		synctest.Wait()
		stop()
		gets, _, _ := fixture.snapshot()
		ids := make([]int, 0, len(gets))
		for _, get := range gets {
			ids = append(ids, get.id)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []int{1, 3}) || mailbox.replay.high != 1 || mailbox.receivedBatches.Load() != 1 ||
			mailbox.rejected.Load() != 1 || len(mailbox.cleanup) != 1 || <-mailbox.cleanup != "object-1" {
			t.Fatalf("invalid or duplicate metadata acquired replay/cleanup ownership: GET ids %v, replay %d", ids, mailbox.replay.high)
		}
	})
}
