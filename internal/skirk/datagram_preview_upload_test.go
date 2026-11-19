package skirk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
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

type pilotUploadAttempt struct {
	at          time.Time
	pool, plain int
	name        string
	completion  time.Time
	status      int
	packetIDs   []uint32
}

type pilotUploadTestProvider struct {
	mu                            sync.Mutex
	attempts                      []pilotUploadAttempt
	active, maxActive             int
	closed                        []int
	stallPool                     int
	limitMetadata                 bool
	delays                        map[uint64]pilotUploadTestDelay
	recoveryDelay                 time.Duration
	mediaDelay                    time.Duration
	metadataDelay                 time.Duration
	packetIDs                     func([]byte) []uint32
	stallAllFromSeq               uint64
	stallMediaFromSeq             uint64
	stalledOneMedia               bool
	wallFaultFrom, wallFaultUntil time.Time
}

type pilotUploadTestDelay struct {
	delay    time.Duration
	deadline bool
}

type pilotUploadTestPool struct {
	provider   *pilotUploadTestProvider
	generation int
}

func (f *pilotUploadTestProvider) snapshot() (attempts []pilotUploadAttempt, active, maxActive int, closed []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.attempts), f.active, f.maxActive, slices.Clone(f.closed)
}

func (f *pilotUploadTestProvider) client(pool int) *GoogleHTTPClient {
	return &GoogleHTTPClient{route: RouteConfig{Mode: "google_front_pinned"}, client: &http.Client{
		Transport: &pilotUploadTestPool{f, pool}, CheckRedirect: keepGoogleHTTPRedirectResponse,
	}}
}

func (p *pilotUploadTestPool) CloseIdleConnections() {
	p.provider.mu.Lock()
	defer p.provider.mu.Unlock()
	p.provider.closed = append(p.provider.closed, p.generation)
}

func (p *pilotUploadTestPool) RoundTrip(req *http.Request) (response *http.Response, requestErr error) {
	if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" || req.Method != http.MethodPost ||
		req.Header.Get("Authorization") != "Bearer owned-test-token" || req.Header.Get("X-Goog-User-Project") != "test-media-project" {
		return nil, fmt.Errorf("unexpected route, auth, or quota project")
	}
	var meta pilotObject
	var wire []byte
	switch req.URL.Path {
	case "/upload/drive/v3/files":
		if p.generation == 0 {
			return nil, fmt.Errorf("binary upload used metadata pool")
		}
		_, fields, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}
		parts := multipart.NewReader(req.Body, fields["boundary"])
		part, err := parts.NextPart()
		if err != nil {
			return nil, err
		}
		if err := json.NewDecoder(part).Decode(&meta); err != nil {
			return nil, err
		}
		part, err = parts.NextPart()
		if err != nil {
			return nil, err
		}
		wire, err = io.ReadAll(part)
		if err != nil {
			return nil, err
		}
	case "/drive/v3/files":
		if p.generation != 0 {
			return nil, fmt.Errorf("metadata upload used binary pool")
		}
		if err := json.NewDecoder(req.Body).Decode(&meta); err != nil {
			return nil, err
		}
		var err error
		wire, err = base64.RawStdEncoding.DecodeString(meta.Description)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unexpected request path")
	}
	f := p.provider
	seq, _ := strconv.ParseUint(meta.Name[strings.LastIndexByte(meta.Name, '/')+1:], 10, 64)
	f.mu.Lock()
	index := len(f.attempts)
	attempt := pilotUploadAttempt{at: time.Now(), pool: p.generation, plain: len(wire) - pilotMediaOverhead, name: meta.Name}
	if f.packetIDs != nil {
		attempt.packetIDs = f.packetIDs(wire)
	}
	f.attempts = append(f.attempts, attempt)
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	stallMedia := p.generation > 0 && f.stallMediaFromSeq != 0 && seq >= f.stallMediaFromSeq && !f.stalledOneMedia
	if stallMedia {
		f.stalledOneMedia = true
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.attempts[index].completion = time.Now()
		if response != nil {
			f.attempts[index].status = response.StatusCode
		}
		f.mu.Unlock()
	}()
	if p.generation == f.stallPool && f.stallPool > 0 {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	wallFault := !f.wallFaultFrom.IsZero() && !attempt.at.Before(f.wallFaultFrom) && attempt.at.Before(f.wallFaultUntil)
	if stallMedia || f.stallAllFromSeq != 0 && seq >= f.stallAllFromSeq || wallFault {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	script, scripted := f.delays[seq]
	if !scripted {
		delay := f.mediaDelay
		if p.generation == 0 {
			delay = f.metadataDelay
		}
		script.delay = delay
	}
	if plain := len(wire) - pilotMediaOverhead; p.generation == 2 && plain <= pilotUploadRecoveryPlain && f.recoveryDelay > 0 {
		script = pilotUploadTestDelay{delay: f.recoveryDelay}
	}
	if script.delay > 0 {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(script.delay):
			if script.deadline {
				return nil, context.DeadlineExceeded
			}
		}
	}
	if f.limitMetadata && p.generation == 0 {
		return stringResponse(http.StatusTooManyRequests, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`), nil
	}
	return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":"owned-batch","name":%q,"size":%q}`, meta.Name, strconv.Itoa(len(wire)))), nil
}

func newAdaptiveTestMailbox(t *testing.T, provider *pilotUploadTestProvider) *pilotMailbox {
	t.Helper()
	preview := publicPreviewForLifetimeTest(t)
	client := provider.client(0)
	drive := NewDriveStore(client, "owned-test-token", DriveConfig{FolderID: "isolated-drive-folder", QuotaProject: "test-media-project"})
	mailbox, err := newPilotMailbox(preview.tunnel, drive, [16]byte{1}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	mailbox.publicIPv4, mailbox.separateUploads, mailbox.media, mailbox.batchBytes = true, true, true, pilotWidePlain
	generation := 0
	mailbox.adaptiveUploads = newPilotAdaptiveUploads(func(previous *GoogleHTTPClient) *GoogleHTTPClient {
		generation++
		return provider.client(generation)
	})
	return mailbox
}

func offerPilotUploadTestPackets(mailbox *pilotMailbox, count, size int) {
	target := netip.MustParseAddrPort("8.8.8.8:3478")
	for i := 0; i < count; i++ {
		mailbox.offer(1, target, bytes.Repeat([]byte{byte(i)}, size))
	}
}

func TestUDPPreviewAdaptiveUploadsContinueMetadataAndRecover(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{stallPool: 1}
		mailbox := newAdaptiveTestMailbox(t, provider)
		adaptive := mailbox.adaptiveUploads
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		advance := func(d time.Duration) { time.Sleep(d); synctest.Wait() }
		const fullBatchPackets = (pilotMediaMaxPlain - len(pilotDatagramMagic)) / (pilotPacketHeader + pilotMaxPayload)
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick + time.Millisecond)
		attempts, active, _, _ := provider.snapshot()
		if len(attempts) != 1 || attempts[0].pool != 1 || attempts[0].plain <= pilotWidePlain || active != 1 {
			t.Fatalf("first binary batch not pending: attempts=%+v active=%d", attempts, active)
		}
		offerPilotUploadTestPackets(mailbox, 12, 160)
		advance(pilotPublishTick)
		attempts, active, maxActive, _ := provider.snapshot()
		if len(attempts) != 2 || attempts[1].pool != 0 || attempts[1].plain != len(pilotDatagramMagic)+12*(pilotPacketHeader+160) ||
			mailbox.published.Load() != 12 || active != 1 || maxActive != 2 {
			t.Fatalf("audio batch did not publish while binary was pending: attempts=%+v delivered=%d active=%d max=%d",
				attempts, mailbox.published.Load(), active, maxActive)
		}
		advance(4 * pilotPublishTick)
		offerPilotUploadTestPackets(mailbox, 16, 1200)
		advance(pilotPublishTick)
		attempts, _, _, _ = provider.snapshot()
		if len(attempts) != 3 || attempts[2].pool != 0 || attempts[2].plain > pilotWidePlain || len(mailbox.queue) == 0 || adaptive.mediaSlowBatches.Load() != 1 {
			t.Fatalf("large backlog did not split at metadata cap: attempts=%+v queued=%d", attempts, len(mailbox.queue))
		}
		advance(3 * pilotPublishTick)
		_, _, _, closed := provider.snapshot()
		if adaptive.media.deadlines.Load() != 1 || adaptive.media.deadlineDroppedPackets.Load() != uint64(fullBatchPackets) ||
			adaptive.mediaPoolResets.Load() != 1 || len(closed) != 1 || closed[0] != 1 {
			t.Fatalf("binary deadline was not recorded and isolated: deadlines=%d packets=%d resets=%d closed=%v",
				adaptive.media.deadlines.Load(), adaptive.media.deadlineDroppedPackets.Load(), adaptive.mediaPoolResets.Load(), closed)
		}
		offerPilotUploadTestPackets(mailbox, 16, 1200)
		advance(pilotPublishTick)
		attempts, _, _, _ = provider.snapshot()
		if n := len(attempts); attempts[n-1].pool != 0 || adaptive.mediaCooldownBatches.Load() != 1 {
			t.Fatalf("expected metadata during cooldown: attempts=%+v cooldown=%d", attempts, adaptive.mediaCooldownBatches.Load())
		}
		advance(pilotUploadCooldownMin)
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick)
		attempts, _, _, _ = provider.snapshot()
		if n := len(attempts); attempts[n-1].pool != 2 || attempts[n-1].plain <= pilotWidePlain ||
			attempts[n-1].plain > pilotUploadRecoveryPlain || mailbox.mediaUploaded.Load() != 1 {
			t.Fatalf("recovery did not use smaller batch in a fresh pool: attempts=%+v media_success=%d", attempts, mailbox.mediaUploaded.Load())
		}
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick)
		attempts, _, _, _ = provider.snapshot()
		if n := len(attempts); attempts[n-1].pool != 2 || attempts[n-1].plain <= pilotUploadRecoveryPlain ||
			attempts[n-1].plain > pilotMediaMaxPlain || mailbox.mediaUploaded.Load() != 2 {
			t.Fatalf("successful recovery failed to restore full cap: attempts=%+v media_success=%d", attempts, mailbox.mediaUploaded.Load())
		}
		seen := make(map[string]bool)
		for i, attempt := range attempts {
			if attempt.name == "" || seen[attempt.name] || i > 0 && attempt.at.Sub(attempts[i-1].at) < pilotPublishTick {
				t.Fatalf("publication retried a sequence or exceeded cadence: attempts=%+v", attempts)
			}
			seen[attempt.name] = true
		}
	})
}

func TestUDPPreviewAdaptiveUploadsUseBothHealthySlots(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{delays: map[uint64]pilotUploadTestDelay{
			1: {delay: 600 * time.Millisecond},
			2: {delay: 400 * time.Millisecond},
		}}
		mailbox := newAdaptiveTestMailbox(t, provider)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		advance := func(d time.Duration) { time.Sleep(d); synctest.Wait() }
		const fullBatchPackets = (pilotMediaMaxPlain - len(pilotDatagramMagic)) / (pilotPacketHeader + pilotMaxPayload)
		for range 2 {
			offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
			advance(pilotPublishTick + time.Millisecond)
		}
		attempts, active, maxActive, _ := provider.snapshot()
		if len(attempts) != 2 || attempts[0].pool != 1 || attempts[1].pool != 1 || active != 2 || maxActive != 2 ||
			attempts[0].plain <= pilotUploadRecoveryPlain || attempts[1].plain <= pilotUploadRecoveryPlain {
			t.Fatalf("first two healthy ticks did not use both binary slots: attempts=%+v active=%d max=%d", attempts, active, maxActive)
		}
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick)
		attempts, active, _, _ = provider.snapshot()
		if len(attempts) != 2 || active != 2 || mailbox.adaptiveUploads.slotWaitTicks.Load() != 1 || len(mailbox.queue) != fullBatchPackets {
			t.Fatalf("third upload bypassed slot or discarded queued packets: attempts=%+v active=%d queued=%d", attempts, active, len(mailbox.queue))
		}
		advance(pilotPublishTick)
		attempts, _, maxActive, closed := provider.snapshot()
		if len(attempts) != 3 || attempts[2].pool != 1 || attempts[2].plain <= pilotUploadRecoveryPlain || maxActive != 2 || len(closed) != 0 ||
			mailbox.mediaUploaded.Load() != 3 || mailbox.adaptiveUploads.mediaOverlapBatches.Load() != 1 || mailbox.adaptiveUploads.mediaSlowBatches.Load() != 0 {
			t.Fatalf("healthy throughput did not resume in original pool: attempts=%+v active_max=%d closed=%v media=%d", attempts, maxActive, closed, mailbox.mediaUploaded.Load())
		}
	})
}

func TestUDPPreviewAdaptiveUploadsDrainOutOfOrderBeforeRotation(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{delays: map[uint64]pilotUploadTestDelay{
			1: {delay: 1600 * time.Millisecond},
			2: {delay: 500 * time.Millisecond, deadline: true},
		}, recoveryDelay: 600 * time.Millisecond}
		mailbox := newAdaptiveTestMailbox(t, provider)
		adaptive := mailbox.adaptiveUploads
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		advance := func(d time.Duration) { time.Sleep(d); synctest.Wait() }
		const fullBatchPackets = (pilotMediaMaxPlain - len(pilotDatagramMagic)) / (pilotPacketHeader + pilotMaxPayload)
		for range 2 {
			offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
			advance(pilotPublishTick + time.Millisecond)
		}
		advance(2 * pilotPublishTick)
		attempts, active, _, closed := provider.snapshot()
		if len(attempts) != 2 || active != 1 || len(closed) != 0 || adaptive.media.deadlines.Load() != 1 || adaptive.mediaPoolResets.Load() != 0 {
			t.Fatalf("later sequence did not time out ahead of earlier work: attempts=%+v active=%d closed=%v deadlines=%d resets=%d",
				attempts, active, closed, adaptive.media.deadlines.Load(), adaptive.mediaPoolResets.Load())
		}
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick)
		attempts, active, _, closed = provider.snapshot()
		if len(attempts) != 3 || attempts[2].pool != 0 || attempts[2].plain > pilotWidePlain || active != 1 || len(closed) != 0 || adaptive.mediaDrainBatches.Load() != 1 {
			t.Fatalf("a deadline did not reserve the free slot until the older binary returned: attempts=%+v active=%d closed=%v drains=%d",
				attempts, active, closed, adaptive.mediaDrainBatches.Load())
		}
		advance(3 * pilotPublishTick)
		attempts, _, _, closed = provider.snapshot()
		if len(closed) != 1 || closed[0] != 1 || adaptive.mediaPoolResets.Load() != 1 || adaptive.media.published.Load() != 1 {
			t.Fatalf("old success did not permit exactly one safe rotation: attempts=%+v closed=%v resets=%d published=%d",
				attempts, closed, adaptive.mediaPoolResets.Load(), adaptive.media.published.Load())
		}
		for _, attempt := range attempts[2:] {
			if attempt.pool != 0 {
				t.Fatalf("old success bypassed cooldown: attempts=%+v", attempts)
			}
		}
		advance(pilotUploadCooldownMin)
		offerPilotUploadTestPackets(mailbox, fullBatchPackets, pilotMaxPayload)
		advance(pilotPublishTick)
		attempts, active, _, _ = provider.snapshot()
		if n := len(attempts); attempts[n-1].pool != 2 || attempts[n-1].plain > pilotUploadRecoveryPlain || active != 1 {
			t.Fatalf("recovery did not start one smaller binary batch in fresh pool: attempts=%+v active=%d", attempts, active)
		}
		advance(pilotPublishTick)
		attempts, active, _, _ = provider.snapshot()
		if n := len(attempts); attempts[n-1].pool != 0 || attempts[n-1].plain > pilotWidePlain || active != 1 || adaptive.mediaProbeBatches.Load() != 1 {
			t.Fatalf("pending recovery allowed a second binary batch: attempts=%+v active=%d probes=%d", attempts, active, adaptive.mediaProbeBatches.Load())
		}
		seen := make(map[string]bool)
		for _, attempt := range attempts {
			if seen[attempt.name] {
				t.Fatalf("retried an old sequence: attempts=%+v", attempts)
			}
			seen[attempt.name] = true
		}
	})
}

func TestUDPPreviewAdaptiveUploadsHoldSharedQuotaBackoff(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotUploadTestProvider{limitMetadata: true}
		mailbox := newAdaptiveTestMailbox(t, provider)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go mailbox.publishLoop(ctx)
		synctest.Wait()
		offerPilotUploadTestPackets(mailbox, 8, 160)
		time.Sleep(pilotPublishTick + time.Millisecond)
		synctest.Wait()
		attempts, _, _, _ := provider.snapshot()
		if len(attempts) != 1 || !mailbox.drive.QuotaBackoffSnapshot().Active {
			t.Fatalf("metadata quota response did not arm backoff: attempts=%+v", attempts)
		}
		offerPilotUploadTestPackets(mailbox, 44, pilotMaxPayload)
		time.Sleep(2 * pilotPublishTick)
		synctest.Wait()
		attempts, _, _, _ = provider.snapshot()
		if len(attempts) != 1 || mailbox.adaptiveUploads.quotaHoldTicks.Load() != 2 || len(mailbox.queue) != 44 ||
			mailbox.adaptiveUploads.mediaPoolResets.Load() != 0 {
			t.Fatalf("quota backoff was bypassed or discarded fresh input: attempts=%+v held=%d queue=%d resets=%d",
				attempts, mailbox.adaptiveUploads.quotaHoldTicks.Load(), len(mailbox.queue), mailbox.adaptiveUploads.mediaPoolResets.Load())
		}
	})
}

func TestUDPPreviewAdaptiveUploadPolicyIsSenderLocal(t *testing.T) {
	base := publicPreviewForLifetimeTest(t)
	base.tunnel.Data = base.drive
	cfg := &Config{Tunnel: TunnelConfig{
		UDPPreviewPublicIPv4: true, UDPPreviewBatchBytes: pilotWidePlain, UDPPreviewAdaptiveUploads: true,
	}, Drive: DriveConfig{FolderID: base.drive.folderID}}
	if _, err := newDatagramPreview(base.tunnel, cfg); err == nil {
		t.Fatal("accepted adaptive uploads without media batches")
	}
	cfg.Tunnel.UDPPreviewMedia = true
	adaptive, err := newDatagramPreview(base.tunnel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tunnel.UDPPreviewAdaptiveUploads = false
	legacy, err := newDatagramPreview(base.tunnel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !adaptive.adaptiveUploads || legacy.adaptiveUploads || legacy.policy != adaptive.policy {
		t.Fatal("adaptive scheduling was not sender-local")
	}
}
