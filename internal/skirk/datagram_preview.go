package skirk

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pilotDatagramMagic = "SDP3"
	pilotPacketHeader  = 52 // Flow, two elapsed stamps, epoch (8 each), address+port (18), length (2).
	pilotMaxPayload    = 1452
	pilotMaxPlain      = 8 * 1024
	pilotWidePlain     = 16 * 1024
	pilotMaxBatch      = 64
	pilotMaxListFiles  = 40
	pilotMaxListBytes  = 1 << 20
	pilotLifetime      = 6 * time.Second
	pilotPublishTick   = 250 * time.Millisecond
	pilotPollTick      = 350 * time.Millisecond
	pilotMaxFlows      = 16
	pilotHEVMaxFlows   = 3
	pilotHEVIdle       = 20 * time.Second
	pilotLocalWrite    = 100 * time.Millisecond
	// HEV gives its internal loopback interface 127.0.0.1, so packets sent
	// to that address do not reach its SOCKS client. The HEV pilot frontend
	// accepts this virtual alias for the fixture's exact loopback ports.
	pilotHEVLoopbackAlias = "198.18.0.42"
)

type pilotPacket struct {
	flow           uint64
	sent           time.Time
	received       time.Time // Local receive ingress; never serialized or renewed.
	target         netip.AddrPort
	body           []byte
	timingEpoch    uint64
	elapsed        time.Duration
	receiveElapsed time.Duration
}

func pilotMarshal(packets []pilotPacket) []byte {
	body := []byte(pilotDatagramMagic)
	for _, packet := range packets {
		body = binary.BigEndian.AppendUint64(body, packet.flow)
		stamp := uint64(packet.sent.UnixMilli())
		if packet.timingEpoch != 0 {
			stamp = uint64(packet.elapsed)
		}
		body = binary.BigEndian.AppendUint64(body, stamp)
		body = binary.BigEndian.AppendUint64(body, packet.timingEpoch)
		body = binary.BigEndian.AppendUint64(body, uint64(packet.receiveElapsed))
		addr := packet.target.Addr().As16()
		body = append(body, addr[:]...)
		body = binary.BigEndian.AppendUint16(body, packet.target.Port())
		body = binary.BigEndian.AppendUint16(body, uint16(len(packet.body)))
		body = append(body, packet.body...)
	}
	return body
}

func pilotUnmarshal(raw []byte, batchBytes, maxPackets int, ipv6 bool) ([]pilotPacket, bool) {
	if len(raw) > batchBytes || !bytes.HasPrefix(raw, []byte(pilotDatagramMagic)) {
		return nil, false
	}
	var packets []pilotPacket
	for body := raw[len(pilotDatagramMagic):]; len(body) > 0; {
		if len(body) < pilotPacketHeader || len(packets) == maxPackets {
			return nil, false
		}
		flow := binary.BigEndian.Uint64(body)
		sent := int64(binary.BigEndian.Uint64(body[8:]))
		epoch := binary.BigEndian.Uint64(body[16:])
		receiveElapsed := int64(binary.BigEndian.Uint64(body[24:]))
		target := netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[32:48])).Unmap(), binary.BigEndian.Uint16(body[48:]))
		size := int(binary.BigEndian.Uint16(body[50:]))
		if flow == 0 || sent < 0 || receiveElapsed < 0 || (!ipv6 && !target.Addr().Is4()) || target.Port() == 0 ||
			size > pilotMaxPayload || size > len(body)-pilotPacketHeader {
			return nil, false
		}
		end := pilotPacketHeader + size
		localSent := time.Time{}
		if epoch == 0 {
			localSent = time.UnixMilli(sent)
		}
		packets = append(packets, pilotPacket{flow: flow, sent: localSent, target: target, body: body[pilotPacketHeader:end], timingEpoch: epoch, elapsed: time.Duration(sent), receiveElapsed: time.Duration(receiveElapsed)})
		body = body[end:]
	}
	return packets, len(packets) != 0
}

// Windows only cover records authenticated in this fixture's one-use lease.
// A gap does not prevent any other sequence from being accepted.
type pilotReplay struct{ high, seen uint64 }

func (w *pilotReplay) accept(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > w.high {
		w.high, w.seen = seq, (w.seen<<(seq-w.high))|1
		return true
	}
	offset := w.high - seq
	if offset >= 64 || w.seen&(1<<offset) != 0 {
		return false
	}
	w.seen |= 1 << offset
	return true
}

type pilotObject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Size        string `json:"size,omitempty"`
}

type pilotMailbox struct {
	tunnel           *Tunnel
	drive            *DriveStore
	http             *GoogleHTTPClient
	lease            [16]byte
	sendDir          byte
	recvDir          byte
	sendKey          []byte
	recvKey          []byte
	prefix           string
	allowed          map[netip.AddrPort]bool
	publicIPv4       bool
	publicIPv6       bool
	batchBytes       int
	media            bool
	mediaBytes       int
	separateUploads  bool
	adaptiveUploads  *pilotAdaptiveUploads
	exitProxy        string
	frontendMu       sync.Mutex
	frontends        map[*pilotMailboxFrontend]struct{}
	frontendsClosing bool
	frontendWG       sync.WaitGroup

	queue                       chan pilotPacket
	fairQueue                   *pilotFairQueue
	cleanup                     chan string
	deliver                     func(context.Context, pilotPacket)
	closeAssociation            func(uint32) error
	resetAssociations           <-chan pilotAssociationReset
	activity                    func()
	timing                      *pilotTimingState
	liveCalls                   sync.WaitGroup
	seq                         uint64 // Owned by publishLoop.
	replay                      pilotReplay
	recvMu                      sync.Mutex
	mediaReader                 pilotMediaReader
	publishMode                 pilotPublishMode
	publishWake                 chan struct{}
	mediaUploaded, mediaFetched atomic.Uint64

	offered, published, delivered, stale, dropped, admissionDropped, rejected, errors, partial atomic.Uint64
	originSent, originReceived                                                                 atomic.Uint64
	localQueueExpired, incomingCertainlyStale, invalidTiming                                   atomic.Uint64
	ingressDropped, hevReplyDropped, flowCap                                                   atomic.Uint64
	originDialErrors, originWriteErrors, hevWriteErrors                                        atomic.Uint64
	publishedBatches, receivedBatches, publishedPlainBytes, acceptedPayloadBytes               atomic.Uint64
}

func newPilotMailbox(t *Tunnel, drive *DriveStore, lease [16]byte, up bool, allowed map[netip.AddrPort]bool) (*pilotMailbox, error) {
	root, err := DeriveKey(t.Secret)
	if err != nil {
		return nil, err
	}
	a, b := DirectionUp, DirectionDown
	if !up {
		a, b = b, a
	}
	key := func(direction byte) []byte {
		info := []byte("skirk-independent-datagram-interval-v3/")
		info = append(info, lease[:]...)
		info = append(info, direction)
		return hkdfSHA256(root, t.SessionID[:], info, keyLen)
	}
	return &pilotMailbox{tunnel: t, drive: drive, http: drive.http, lease: lease, prefix: "udp-pilot/" + hex.EncodeToString(lease[:]) + "/",
		sendDir: a, recvDir: b, sendKey: key(a), recvKey: key(b), allowed: allowed,
		batchBytes: pilotMaxPlain, queue: make(chan pilotPacket, pilotQueuePackets), cleanup: make(chan string, 128), publishWake: make(chan struct{}, 1)}, nil
}

func (p *pilotMailbox) dirPrefix(direction byte) string {
	return p.prefix + strconv.Itoa(int(direction)) + "/"
}

func (p *pilotMailbox) offer(flow uint64, target netip.AddrPort, payload []byte) {
	p.offered.Add(1)
	if !permitPreviewTarget(p.allowed, p.publicIPv4, p.publicIPv6, target) || len(payload) > pilotMaxPayload {
		p.rejected.Add(1)
		return
	}
	packet := pilotPacket{flow: flow, sent: time.Now(), target: target, body: bytes.Clone(payload)}
	if p.timing != nil {
		packet.sent = p.timing.nowTime()
		var ok bool
		packet.timingEpoch, packet.elapsed, packet.receiveElapsed, ok = p.timing.stamp(packet.sent)
		if !ok {
			p.recordExpiry(pilotInvalidTiming, 1)
			return
		}
	}
	if p.fairQueue != nil {
		dropped, stale := p.fairQueue.offer(packet)
		p.dropped.Add(uint64(dropped))
		p.ingressDropped.Add(uint64(dropped))
		p.recordExpiry(pilotLocalQueueExpired, stale)
		p.signalPublish()
		return
	}
	select {
	case p.queue <- packet:
	default:
		// At a saturated receiver, prefer a newer arrival over a queued
		// packet whose deadline is closer. Never block the app socket.
		select {
		case <-p.queue:
			p.dropped.Add(1)
			p.ingressDropped.Add(1)
		default:
		}
		select {
		case p.queue <- packet:
		default:
			p.dropped.Add(1)
			p.ingressDropped.Add(1)
		}
	}
	p.signalPublish()
}

// A publication owns the selected packets until its one upload finishes.
// Expiry can shrink its original binary selection into a metadata object.
type pilotPublication struct {
	packets []pilotPacket
	size    int
	binary  bool
}

func (p *pilotMailbox) prunePublication(batch *pilotPublication) {
	now := time.Now()
	if p.timing != nil {
		now = p.timing.nowTime()
	}
	batch.packets = slices.DeleteFunc(batch.packets, func(packet pilotPacket) bool {
		reason := p.outgoingExpiry(packet, now)
		if reason == pilotPacketFresh {
			return false
		}
		p.recordExpiry(reason, 1)
		batch.size -= pilotPacketHeader + len(packet.body)
		return true
	})
}

func (p *pilotMailbox) publishBatch(ctx context.Context, seq uint64, batch *pilotPublication, remainder chan<- []pilotPacket) error {
	p.prunePublication(batch)
	if len(batch.packets) == 0 {
		return errPilotTimingInvalid
	}
	newest := batch.packets[len(batch.packets)-1].sent
	if p.fairQueue != nil {
		for _, packet := range batch.packets {
			if packet.sent.After(newest) {
				newest = packet.sent
			}
		}
	}
	expires := newest.Add(pilotLifetime)
	op, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	if p.adaptiveUploads == nil {
		if err := p.drive.WaitForDriveQuota(op, "udp_pilot_upload"); err != nil {
			p.prunePublication(batch)
			if len(batch.packets) == 0 {
				return errPilotTimingInvalid
			}
			p.dropped.Add(uint64(len(batch.packets)))
			return err
		}
	}
	release := func(error, int64) {}
	if !p.separateUploads {
		// The original experiment shares v5's normal upload pool. The
		// comparison uses publishLoop's own two slots and fixed cadence,
		// without feeding datagram outcomes into v5's TCP estimator.
		admission, stop := context.WithTimeout(op, 100*time.Millisecond)
		putBack, err := p.tunnel.acquireUploadSlotBytes(admission, false)
		stop()
		if err != nil {
			p.prunePublication(batch)
			if len(batch.packets) == 0 {
				return errPilotTimingInvalid
			}
			p.dropped.Add(uint64(len(batch.packets)))
			p.admissionDropped.Add(uint64(len(batch.packets)))
			return err
		}
		release = putBack
	}
	// A shared upload permit or provider pause can outlast individual packets.
	if p.adaptiveUploads == nil || !p.separateUploads {
		p.prunePublication(batch)
		if len(batch.packets) == 0 {
			release(errPilotTimingInvalid, 0)
			return errPilotTimingInvalid
		}
	}
	// Readers recognize binary objects only above their metadata byte limit.
	// A wide binary selection can also cross the 64-packet metadata cap.
	// Return any unattempted suffix with its original timestamps and flow IDs.
	if batch.binary && batch.size <= p.batchBytes {
		batch.binary = false
		if len(batch.packets) > pilotMaxBatch {
			tail := batch.packets[pilotMaxBatch:]
			batch.packets = batch.packets[:pilotMaxBatch:pilotMaxBatch]
			for _, packet := range tail {
				batch.size -= pilotPacketHeader + len(packet.body)
			}
			select {
			case remainder <- tail:
			case <-ctx.Done():
				p.dropped.Add(uint64(len(tail) + len(batch.packets)))
				release(ctx.Err(), 0)
				return ctx.Err()
			}
		}
	}
	plain := pilotMarshal(batch.packets)
	wire, err := Seal(p.sendKey, p.tunnel.SessionID, p.sendDir, seq, plain, false)
	if err != nil {
		release(err, 0)
		p.errors.Add(1)
		return err
	}
	name := fmt.Sprintf("%s%020d", p.dirPrefix(p.sendDir), seq)
	if batch.binary {
		err = p.uploadMediaBatch(op, name, wire)
	} else {
		err = p.publishDescription(op, name, wire)
	}
	release(err, int64(len(wire)))
	if err != nil {
		if !errors.Is(err, errPilotMediaBackoff) {
			p.errors.Add(1)
		}
		p.dropped.Add(uint64(len(batch.packets)))
		return err
	}
	if batch.binary {
		p.mediaUploaded.Add(1)
	}
	p.published.Add(uint64(len(batch.packets)))
	p.publishedBatches.Add(1)
	p.publishedPlainBytes.Add(uint64(len(plain)))
	if p.activity != nil {
		p.activity()
	}
	return nil
}

func (p *pilotMailbox) closeQueuedAssociation(id uint32) {
	if p.fairQueue != nil {
		p.dropped.Add(uint64(p.fairQueue.closeAssociation(id)))
	}
}

func (p *pilotMailbox) recordExpiry(reason pilotExpiryReason, count int) {
	if count <= 0 || reason == pilotPacketFresh {
		return
	}
	p.stale.Add(uint64(count))
	switch reason {
	case pilotLocalQueueExpired:
		p.localQueueExpired.Add(uint64(count))
	case pilotIncomingCertainlyStale:
		p.incomingCertainlyStale.Add(uint64(count))
	case pilotInvalidTiming:
		p.invalidTiming.Add(uint64(count))
	}
}

func (p *pilotMailbox) outgoingExpiry(packet pilotPacket, now time.Time) pilotExpiryReason {
	if pilotElapsedUpper(packet.sent, now) >= pilotLifetime {
		return pilotLocalQueueExpired
	}
	if p.timing != nil && !p.timing.valid(packet.timingEpoch, now) {
		return pilotInvalidTiming
	}
	return pilotPacketFresh
}

func (p *pilotMailbox) incomingExpiry(packet pilotPacket, now time.Time) pilotExpiryReason {
	if p.timing != nil {
		now = p.timing.nowTime()
	}
	if !packet.received.IsZero() && pilotElapsedUpper(packet.received, now) >= pilotLifetime {
		return pilotLocalQueueExpired
	}
	if p.timing != nil {
		_, reason := p.timing.received(packet.timingEpoch, packet.elapsed, packet.receiveElapsed, now)
		return reason
	}
	if pilotElapsedUpper(packet.sent, now) >= pilotLifetime {
		return pilotIncomingCertainlyStale
	}
	return pilotPacketFresh
}

func (p *pilotMailbox) collectFairBatch(batchBytes int) ([]pilotPacket, int) {
	if p.media && pilotMediaPacketLimit(batchBytes) > pilotMaxBatch {
		if packets, size := p.collectQueuedBinary(batchBytes, false); len(packets) > 0 {
			return packets, size
		}
	}
	now := time.Now()
	if p.timing != nil {
		now = p.timing.nowTime()
	}
	return p.collectFairBatchUsing(p.fairQueue.take, batchBytes, now, p.recordExpiry, false)
}

func (p *pilotMailbox) collectFairBatchUsing(take func(time.Time, func(pilotPacket) bool) (pilotPacket, bool, int), batchBytes int, now time.Time, expired func(pilotExpiryReason, int), allowMetadataPrefix bool) ([]pilotPacket, int) {
	size := len(pilotDatagramMagic)
	var packets []pilotPacket
	for maxPackets := pilotMediaPacketLimit(batchBytes); len(packets) < maxPackets; {
		packet, ok, stale := take(now, func(packet pilotPacket) bool {
			if p.outgoingExpiry(packet, now) != pilotPacketFresh {
				return true
			}
			nextSize := size + pilotPacketHeader + len(packet.body)
			// A binary trial can cross the metadata packet count before it
			// crosses the metadata byte limit. Only a valid binary trial commits.
			return nextSize <= batchBytes && (allowMetadataPrefix || len(packets) < pilotMaxBatch || nextSize > p.batchBytes)
		})
		expired(pilotLocalQueueExpired, stale)
		if !ok {
			break
		}
		if reason := p.outgoingExpiry(packet, now); reason != pilotPacketFresh {
			expired(reason, 1)
			continue
		}
		packets = append(packets, packet)
		size += pilotPacketHeader + len(packet.body)
	}
	return packets, size
}

func (p *pilotMailbox) publishDescription(ctx context.Context, name string, wire []byte) error {
	body, err := json.Marshal(map[string]any{"name": name, "parents": []string{p.drive.folderID},
		"mimeType": "application/octet-stream", "description": base64.RawStdEncoding.EncodeToString(wire)})
	if err != nil {
		return err
	}
	req, stopRequest := context.WithTimeout(ctx, 2*time.Second)
	defer stopRequest()
	req = withGoogleResponseLimitAtMost(req, 16<<10)
	path := "/drive/v3/files?fields=id,name"
	headers := map[string]string{"Content-Type": "application/json"}
	create := func() (*HTTPResult, error) {
		return p.requestPilotBatchOnce(req, p.http, http.MethodPost, path, headers, body)
	}
	result, err := create()
	// The legacy sender retries once after a rejected token. Apply the
	// same quota check on retry to release a shared upload slot promptly.
	if err == nil && result.Status == http.StatusUnauthorized && p.adaptiveUploads == nil && p.drive.tokenSource != nil {
		result, err = create()
	}
	if err == nil {
		err = require2xx(result, "datagram pilot create")
	}
	if err != nil {
		return err
	}
	var object pilotObject
	if json.Unmarshal(result.Body, &object) != nil || object.ID == "" || object.Name != name {
		return errors.New("Drive returned unexpected UDP description batch metadata")
	}
	return nil
}

func (p *pilotMailbox) publishLoop(ctx context.Context) {
	tick := time.NewTicker(pilotPublishTick)
	defer tick.Stop()
	creditTimer := time.NewTimer(time.Hour)
	creditTimer.Stop()
	defer creditTimer.Stop()
	var creditReady <-chan time.Time
	var wake <-chan struct{}
	if p.publishMode != pilotPublishFixed {
		wake = p.publishWake
	}
	credits := newPilotPublishCredits(time.Now())
	const slots = 2
	completed := make(chan pilotUploadCompletion, slots)
	// Only a selected wide batch that shrinks below the metadata limit can
	// return a suffix. Service those previously admitted packets on the next
	// ordinary tick before selecting new packets from the fair queue.
	remainders := make(chan []pilotPacket, slots)
	var deferred [][]pilotPacket
	inFlight := 0
	var media pilotUploadSchedule
	adaptive := p.adaptiveUploads
	if adaptive != nil {
		media.inFlight = make(map[uint64]time.Time, slots)
		media.allowEarlyProbe = adaptive.tryEarlyRecovery
		adaptive.mediaHealthThresholdMS.Store(uint64(media.healthThreshold().Milliseconds()))
	}
	complete := func(done pilotUploadCompletion) {
		inFlight--
		if adaptive != nil && done.binary {
			if media.complete(done) {
				adaptive.resetMediaHTTP()
			}
			adaptive.mediaHealthThresholdMS.Store(uint64(media.healthThreshold().Milliseconds()))
		}
	}
	var buffered []pilotPacket
	var groupedSinceTick bool
	for {
		ticked := false
		select {
		case <-ctx.Done():
			return
		case tail := <-remainders:
			deferred = append(deferred, tail)
		case done := <-completed:
			complete(done)
			if p.publishMode == pilotPublishFixed {
				continue
			}
		case <-wake:
		case <-creditReady:
		case <-tick.C:
			ticked = true
		}
		if ctx.Err() != nil {
			return
		}
		// After sending a group, let a subsequent partial group grow for one
		// extra ordinary tick. Consume that opportunity even when the queue is
		// idle or an upload is blocked, so it cannot delay a later burst.
		coalesce := ticked && groupedSinceTick
		if ticked {
			groupedSinceTick = false
		}
		creditTimer.Stop()
		creditReady = nil
		// Reap before selecting a cap: an expired old binary worker cannot
		// admit another binary into its draining pool, including on a wake.
		for n := len(completed); n > 0; n-- {
			complete(<-completed)
		}
		for n := len(remainders); n > 0; n-- {
			deferred = append(deferred, <-remainders)
		}
		if len(deferred) != 0 && !ticked {
			continue
		}
		hasWork := len(buffered) != 0 || len(p.queue) != 0
		if p.fairQueue != nil {
			hasWork = p.fairQueue.len() != 0
		}
		if !hasWork && len(deferred) == 0 {
			continue
		}
		if inFlight == slots {
			if adaptive != nil && ticked {
				adaptive.slotWaitTicks.Add(1)
			}
			continue
		}
		batchBytes := p.batchBytes
		var fallback pilotUploadFallback
		if p.media {
			batchBytes = p.mediaPlainLimit()
			if adaptive != nil {
				if p.drive.QuotaBackoffSnapshot().Active {
					if ticked {
						adaptive.quotaHoldTicks.Add(1)
					}
					continue
				}
				batchBytes, fallback = media.limit(time.Now(), p.batchBytes, batchBytes)
			}
		}
		if p.publishMode != pilotPublishFixed {
			if wait := credits.delay(time.Now()); wait > 0 {
				creditTimer.Reset(wait)
				creditReady = creditTimer.C
				continue
			}
		}
		if len(deferred) == 0 && coalesce && p.fairQueue != nil && p.fairQueue.canCoalesce(batchBytes) {
			continue
		}
		size := len(pilotDatagramMagic)
		var packets []pilotPacket
		if len(deferred) != 0 {
			packets = deferred[0]
			deferred[0] = nil
			deferred = deferred[1:]
			for _, packet := range packets {
				size += pilotPacketHeader + len(packet.body)
			}
		} else if p.publishMode == pilotPublishFullBinary && !ticked {
			packets, size = p.collectFullFairBatch(batchBytes)
		} else if p.fairQueue != nil {
			packets, size = p.collectFairBatch(batchBytes)
		} else {
			for maxPackets := pilotMediaPacketLimit(batchBytes); len(packets) < maxPackets; {
				if len(buffered) == 0 {
					select {
					case item := <-p.queue:
						buffered = append(buffered, item)
					default:
					}
				}
				if len(buffered) == 0 {
					break
				}
				packet := buffered[0]
				if reason := p.outgoingExpiry(packet, time.Now()); reason != pilotPacketFresh {
					buffered = buffered[:0]
					p.recordExpiry(reason, 1)
					continue
				}
				nextSize := size + pilotPacketHeader + len(packet.body)
				// Only binary batches may exceed the metadata packet cap.
				if nextSize > batchBytes || len(packets) >= pilotMaxBatch && nextSize <= p.batchBytes {
					break
				}
				buffered = buffered[:0]
				packets = append(packets, packet)
				size = nextSize
			}
		}
		if len(packets) == 0 {
			continue
		}
		binary := p.media && size > p.batchBytes
		p.seq++
		seq := p.seq
		start := time.Now()
		if p.publishMode != pilotPublishFixed {
			groupedSinceTick = len(packets) > 1
			credits.take(start)
		}
		var stats *pilotUploadStats
		if adaptive != nil {
			if binary {
				if fallback == pilotUploadMediaFirstProbe {
					media.earlyProbeSeq = seq
					adaptive.firstRecoveryProbeDispatches.Add(1)
				}
				if len(media.inFlight) != 0 {
					adaptive.mediaOverlapBatches.Add(1)
				}
				media.inFlight[seq] = start
				stats = &adaptive.media
			} else {
				stats = &adaptive.metadata
				switch fallback {
				case pilotUploadMediaCooldown, pilotUploadMediaFirstProbe:
					adaptive.mediaCooldownBatches.Add(1)
				case pilotUploadMediaSlow:
					adaptive.mediaBusyBatches.Add(1)
					adaptive.mediaSlowBatches.Add(1)
				case pilotUploadMediaDrain:
					adaptive.mediaBusyBatches.Add(1)
					adaptive.mediaDrainBatches.Add(1)
				case pilotUploadMediaProbe:
					adaptive.mediaBusyBatches.Add(1)
					adaptive.mediaProbeBatches.Add(1)
				}
			}
		}
		inFlight++
		p.liveCalls.Go(func() {
			batch := pilotPublication{packets: packets, size: size, binary: binary}
			err := p.publishBatch(ctx, seq, &batch, remainders)
			end := time.Now()
			if stats != nil && !errors.Is(err, errPilotTimingInvalid) {
				if binary && !batch.binary {
					stats = &adaptive.metadata
				}
				stats.record(batch.size, len(batch.packets), end.Sub(start), err)
			}
			completed <- pilotUploadCompletion{seq: seq, binary: binary, demoted: binary && !batch.binary, err: err, finished: end}
		})
		p.signalPublish()
	}
}

func (p *pilotMailbox) read(ctx context.Context, object pilotObject) {
	p.readPage(ctx, []pilotObject{object})
}

func (p *pilotMailbox) readPage(ctx context.Context, objects []pilotObject) {
	type descriptionObject struct {
		object pilotObject
		seq    uint64
	}
	var media []pilotMediaRead
	var descriptions []descriptionObject
	for _, object := range slices.Backward(objects) {
		seq := p.unreadSequence(object)
		if seq == 0 {
			continue
		}
		if !p.media || object.Size == "" || object.Size == "0" {
			descriptions = append(descriptions, descriptionObject{object, seq})
			continue
		}
		size, err := strconv.Atoi(object.Size)
		if err != nil || size <= p.batchBytes+pilotMediaOverhead ||
			!p.validMediaSize(size) || !validPilotMediaID(object.ID) || object.Description != "" {
			p.rejected.Add(1)
			continue
		}
		media = append(media, pilotMediaRead{key: pilotMediaKey{seq: seq, id: object.ID}, size: size})
	}
	if p.media {
		p.admitMediaReads(ctx, media)
	}
	// Concurrent creates can become visible in a different order from their
	// publisher sequence. Order descriptions already in this page. Media GETs
	// keep their independent completion path and no absent sequence is awaited.
	slices.SortStableFunc(descriptions, func(a, b descriptionObject) int {
		return cmp.Compare(a.seq, b.seq)
	})
	for _, d := range descriptions {
		if p.unreadSequence(d.object) != 0 {
			p.readDescription(ctx, d.object, d.seq)
		}
	}
}

func (p *pilotMailbox) unreadSequence(object pilotObject) uint64 {
	prefix := p.dirPrefix(p.recvDir)
	if !strings.HasPrefix(object.Name, prefix) || object.ID == "" {
		return 0
	}
	suffix := strings.TrimPrefix(object.Name, prefix)
	seq, err := strconv.ParseUint(suffix, 10, 64)
	if err != nil || len(suffix) != 20 || seq == 0 || seq > maxSequence {
		return 0
	}
	p.recvMu.Lock()
	seen := p.replay.contains(seq)
	p.recvMu.Unlock()
	if seen {
		return 0
	}
	return seq
}

func (p *pilotMailbox) readDescription(ctx context.Context, object pilotObject, seq uint64) {
	if len(object.Description) > base64.RawStdEncoding.EncodedLen(p.batchBytes+headerLen+16) {
		p.rejected.Add(1)
		return
	}
	wire, err := base64.RawStdEncoding.DecodeString(object.Description)
	if err != nil {
		p.rejected.Add(1)
		return
	}
	p.acceptBatch(ctx, object.ID, seq, wire, p.batchBytes, pilotMaxBatch)
}

func (w *pilotReplay) contains(seq uint64) bool {
	if seq > w.high {
		return false
	}
	offset := w.high - seq
	return offset >= 64 || w.seen&(1<<offset) != 0
}

func (p *pilotMailbox) acceptBatch(ctx context.Context, id string, seq uint64, wire []byte, batchBytes, maxPackets int) {
	envelope, plain, err := OpenEnvelope(p.recvKey, wire)
	if err != nil || envelope.SessionID != p.tunnel.SessionID || envelope.Direction != p.recvDir ||
		envelope.Sequence != seq || envelope.Flags != FlagData {
		p.rejected.Add(1)
		return
	}
	packets, ok := pilotUnmarshal(plain, batchBytes, maxPackets, p.publicIPv6)
	if !ok {
		p.rejected.Add(1)
		return
	}
	p.recvMu.Lock()
	fresh := p.replay.accept(seq)
	p.recvMu.Unlock()
	if !fresh {
		return
	}
	p.receivedBatches.Add(1)
	for _, packet := range packets {
		now := time.Now()
		if p.timing != nil {
			now = p.timing.nowTime()
		}
		age := now.Sub(packet.sent)
		reason := pilotPacketFresh
		if age > pilotLifetime {
			reason = pilotIncomingCertainlyStale
		} else if age < -250*time.Millisecond {
			reason = pilotInvalidTiming
		}
		if p.timing != nil {
			packet.sent, reason = p.timing.received(packet.timingEpoch, packet.elapsed, packet.receiveElapsed, now)
		}
		if reason != pilotPacketFresh {
			p.recordExpiry(reason, 1)
		} else if permitPreviewTarget(p.allowed, p.publicIPv4, p.publicIPv6, packet.target) {
			packet.received = now
			p.delivered.Add(1)
			p.acceptedPayloadBytes.Add(uint64(len(packet.body)))
			if p.activity != nil {
				p.activity()
			}
			p.deliver(ctx, packet)
		} else {
			p.rejected.Add(1)
		}
	}
	select {
	case p.cleanup <- id: // Only authenticated data in this one-use scope.
	default:
	}
}

func (p *pilotMailbox) poll(ctx context.Context) {
	tick := time.NewTicker(pilotPollTick)
	defer tick.Stop()
	prefixes := []string{p.dirPrefix(p.recvDir)}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		page, err := p.listDiscovery(ctx, prefixes)
		if err != nil {
			if ctx.Err() == nil {
				p.errors.Add(1)
			}
			continue
		}
		p.readDiscovery(ctx, page)
	}
}

func (p *pilotMailbox) deleteLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-p.cleanup:
			// These authenticated immutable IDs aren't metadata-slot IDs.
			// Keep the dequeued ID through a known hold. The exit's orphan
			// janitor covers failed requests and a full cleanup queue.
			_, _ = p.drive.requestWithHTTPTimeout(ctx, p.http, http.MethodDelete, "/drive/v3/files/"+url.PathEscape(id),
				nil, nil, false, 1500*time.Millisecond)
		}
	}
}

func (p *pilotMailbox) evidence() map[string]any {
	return map[string]any{"offered": p.offered.Load(), "published": p.published.Load(), "accepted": p.delivered.Load(),
		"expired": p.stale.Load(), "dropped": p.dropped.Load(), "upload_admission_dropped": p.admissionDropped.Load(), "rejected": p.rejected.Load(),
		"local_queue_expired": p.localQueueExpired.Load(), "incoming_certainly_stale": p.incomingCertainlyStale.Load(), "invalid_timing": p.invalidTiming.Load(),
		"provider_or_rpc_errors": p.errors.Load(), "partial_lists": p.partial.Load(), "quota": p.drive.QuotaSnapshot()}
}

type pilotClientFlow struct {
	target netip.AddrPort
	reply  func(pilotPacket)
}

type pilotExitFlow struct {
	target netip.AddrPort
	socket *net.UDPConn
}

type pilotFlowTable struct {
	mu             sync.Mutex
	wg             sync.WaitGroup
	next           atomic.Uint64
	client         map[uint64]pilotClientFlow
	exit           map[uint64]pilotExitFlow
	sharedMaxFlows int
}

func (f *pilotFlowTable) returnToClient(_ context.Context, packet pilotPacket) {
	f.mu.Lock()
	flow, ok := f.client[packet.flow]
	f.mu.Unlock()
	if ok && flow.target == packet.target {
		flow.reply(packet)
	}
}

func (f *pilotFlowTable) registerClientFlow(target netip.AddrPort, reply func(pilotPacket)) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.client) >= pilotMaxFlows {
		return 0
	}
	id := f.next.Add(1)
	f.client[id] = pilotClientFlow{target: target, reply: reply}
	return id
}

func (f *pilotFlowTable) registerSharedClientFlow(id uint64, target netip.AddrPort, reply func(pilotPacket)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.client[id]; exists || len(f.client) >= f.sharedMaxFlows {
		return false
	}
	f.client[id] = pilotClientFlow{target: target, reply: reply}
	return true
}

func (f *pilotFlowTable) removeClientFlows(flows map[netip.AddrPort]uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range flows {
		delete(f.client, id)
	}
}

func (f *pilotFlowTable) sendToOrigin(ctx context.Context, mailbox *pilotMailbox, packet pilotPacket) {
	if !permitPreviewTarget(mailbox.allowed, mailbox.publicIPv4, false, packet.target) {
		return
	}
	f.mu.Lock()
	flow, ok := f.exit[packet.flow]
	if !ok && len(f.exit) < pilotMaxFlows {
		socket, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(packet.target))
		if err == nil {
			flow, ok = pilotExitFlow{packet.target, socket}, true
			f.exit[packet.flow] = flow
			f.wg.Go(func() {
				stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
				defer stop()
				defer socket.Close()
				defer func() {
					f.mu.Lock()
					delete(f.exit, packet.flow)
					f.mu.Unlock()
				}()
				buffer := make([]byte, pilotMaxPayload+1)
				for {
					n, err := socket.Read(buffer)
					if err != nil {
						return
					}
					if mailbox.publicIPv4 {
						mailbox.originReceived.Add(1)
						if n <= pilotMaxPayload {
							_ = socket.SetReadDeadline(time.Now().Add(pilotHEVIdle))
						}
					}
					mailbox.offer(packet.flow, packet.target, buffer[:n])
				}
			})
		} else {
			mailbox.originDialErrors.Add(1)
		}
	} else if !ok {
		mailbox.flowCap.Add(1)
	}
	f.mu.Unlock()
	if ok && flow.target == packet.target {
		_ = flow.socket.SetReadDeadline(time.Now().Add(pilotHEVIdle))
		_ = flow.socket.SetWriteDeadline(time.Now().Add(pilotLocalWrite))
		n, err := flow.socket.Write(packet.body)
		if mailbox.publicIPv4 {
			if err == nil && n == len(packet.body) {
				mailbox.originSent.Add(1)
			} else {
				mailbox.originWriteErrors.Add(1)
			}
		}
	}
}

func previewUDPAllowedPort(port uint16) bool {
	return port != 0 && port != 53 && port != 853
}

// IsGlobalUnicast also accepts private and several reserved IPv4 ranges.
// The preview does not send datagrams to any of these special-use networks.
var previewReservedIPv4 = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

var previewGlobalIPv6 = netip.MustParsePrefix("2000::/3")

var previewReservedIPv6 = [...]netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments and transition mechanisms.
	netip.MustParsePrefix("2001:db8::/32"), // Documentation.
	netip.MustParsePrefix("2002::/16"),     // 6to4 embeds an IPv4 destination.
	netip.MustParsePrefix("3ffe::/16"),     // Former 6bone allocation.
	netip.MustParsePrefix("3fff::/20"),     // Documentation.
}

func previewUDPOwnedLoopback(ip netip.Addr, ipv6 bool) bool {
	return ip == netip.AddrFrom4([4]byte{127, 0, 0, 1}) || ipv6 && ip == netip.IPv6Loopback()
}

func permitPreviewTarget(loopback map[netip.AddrPort]bool, publicIPv4, publicIPv6 bool, target netip.AddrPort) bool {
	ip := target.Addr()
	if !previewUDPAllowedPort(target.Port()) || ip.Zone() != "" || ip.Is4In6() {
		return false
	}
	if ip.IsLoopback() {
		return previewUDPOwnedLoopback(ip, publicIPv6) && loopback[target]
	}
	if ip.Is6() {
		if !publicIPv6 || !previewGlobalIPv6.Contains(ip) {
			return false
		}
		for _, prefix := range previewReservedIPv6 {
			if prefix.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !ip.Is4() || !publicIPv4 || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, prefix := range previewReservedIPv4 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func pilotAllowedTarget(allowed map[netip.AddrPort]bool, publicIPv4, publicIPv6 bool, packet socksUDPPacket) (netip.AddrPort, bool) {
	ip, err := netip.ParseAddr(packet.Host)
	if err != nil || packet.Port < 1 || packet.Port > 65535 || len(packet.Payload) > pilotMaxPayload {
		return netip.AddrPort{}, false
	}
	target := netip.AddrPortFrom(ip.Unmap(), uint16(packet.Port))
	return target, permitPreviewTarget(allowed, publicIPv4, publicIPv6, target)
}

type pilotClientRelay struct {
	allowed        map[netip.AddrPort]bool
	publicIPv4     bool
	publicIPv6     bool
	batchBytes     int
	mediaBytes     int
	dns            bool
	remoteDNS      func(context.Context, []byte, bool) ([]byte, error)
	namedUDP       bool
	namedTargets   map[string]previewNamedAdmission
	mailbox        *pilotMailbox
	flows          *pilotFlowTable
	open           func() (*pilotMailbox, *pilotFlowTable, func(), error)
	finish         func()
	shared         *pilotSharedClient
	association    uint32
	nextFlow       uint32
	activity       func()
	localActivity  func()
	localCancel    context.CancelFunc
	localIdle      time.Duration
	detachFrontend func()
}

func (r *pilotClientRelay) answerDNS(ctx context.Context, query []byte) ([]byte, error) {
	if r.remoteDNS != nil {
		return r.remoteDNS(ctx, query, true)
	}
	return answerDNSQuery(query)
}

func (r *pilotClientRelay) connect(ctx context.Context, control net.Conn) bool {
	if r.mailbox != nil {
		return true
	}
	if r.shared != nil {
		association := control
		if r.localCancel != nil {
			association = pilotFrontendControl{Conn: control, cancel: r.localCancel}
		}
		lease, id, finish, activity, err := r.shared.acquire(ctx, association)
		if err != nil {
			return false
		}
		r.mailbox, r.flows, r.finish, r.association = lease.mailbox, lease.flows, finish, id
		r.activity = activity
		return r.attachFrontend(control)
	}
	mailbox, flows, finish, err := r.open()
	if err != nil {
		return false
	}
	r.mailbox, r.flows, r.finish = mailbox, flows, finish
	return r.attachFrontend(control)
}

func (r *pilotClientRelay) attachFrontend(control net.Conn) bool {
	detach, ok := r.mailbox.attachFrontend(func() {
		if r.localCancel != nil {
			r.localCancel()
		}
		_ = control.Close()
	})
	if !ok {
		finish := r.finish
		r.mailbox, r.flows = nil, nil
		r.association = 0
		r.activity = nil
		r.finish = nil
		finish()
		return false
	}
	r.detachFrontend = detach
	return true
}

func (r *pilotClientRelay) offerDatagram(id uint64, dest netip.AddrPort, payload []byte) {
	r.mailbox.offer(id, dest, payload)
	if r.localActivity != nil {
		r.localActivity()
	}
	if r.activity != nil {
		r.activity()
	}
}

func (r *pilotClientRelay) registerFlow(target netip.AddrPort, reply func(pilotPacket)) uint64 {
	if r.association == 0 {
		return r.flows.registerClientFlow(target, reply)
	}
	if r.nextFlow >= pilotMaxFlows {
		return 0
	}
	r.nextFlow++
	id := uint64(r.association)<<32 | uint64(r.nextFlow)
	if !r.flows.registerSharedClientFlow(id, target, reply) {
		return 0
	}
	return id
}

func (r *pilotClientRelay) close(ids map[netip.AddrPort]uint64) {
	if r.flows != nil {
		r.flows.removeClientFlows(ids)
	}
	if r.association != 0 {
		r.mailbox.closeQueuedAssociation(r.association)
	}
	if r.detachFrontend != nil {
		r.detachFrontend()
	}
	if r.finish != nil {
		r.finish()
	}
}

func pilotAssoc(ctx context.Context, relay *pilotClientRelay, target string, control net.Conn) {
	remote, err := netip.ParseAddrPort(control.RemoteAddr().String())
	request, parseErr := netip.ParseAddrPort(target)
	bind := pilotUDPRelayBind(remote.Addr(), control.LocalAddr())
	if err != nil || parseErr != nil || !bind.IsValid() ||
		(!request.Addr().IsUnspecified() && request.Addr().Unmap() != remote.Addr().Unmap()) {
		_ = socksReply(control, 0x02)
		return
	}
	socket, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(bind, 0)))
	if err != nil {
		_ = socksReply(control, 0x01)
		return
	}
	defer socket.Close()
	ctx, cancel := context.WithCancel(ctx)
	relay.localCancel = cancel
	defer cancel()
	idle := relay.localIdle
	if idle <= 0 {
		idle = udpPreviewStableIdle
	}
	ctx, stopIdle, touch := pilotFrontendIdleContext(ctx, idle)
	defer stopIdle()
	relay.localActivity = touch
	stop := context.AfterFunc(ctx, func() { _ = socket.Close(); _ = control.Close() })
	defer stop()
	addr := socket.LocalAddr().(*net.UDPAddr)
	if err := socksReplyUDP(control, 0, addr.IP, addr.Port); err != nil {
		return
	}
	_ = control.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, control)
		cancel()
		_ = socket.Close()
	}()
	flows := make(map[netip.AddrPort]uint64)
	defer func() {
		relay.close(flows)
		_ = control.Close()
		<-done
	}()
	var source netip.AddrPort
	// Winsock reports truncated UDP reads as errors. Read the complete local
	// SOCKS datagram before applying the DNS or carrier payload limit.
	buf := make([]byte, 64<<10)
	for {
		n, peer, err := socket.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if peer.Addr().Unmap() != remote.Addr().Unmap() ||
			request.Port() != 0 && peer.Port() != request.Port() || source.IsValid() && peer != source {
			continue
		}
		packet, err := parseSOCKSUDPDatagram(buf[:n])
		if err != nil {
			continue
		}
		if relay.dns && packet.Port == 53 {
			if len(packet.Payload) > previewDNSMaxQuery {
				continue
			}
			if response, err := relay.answerDNS(ctx, packet.Payload); err == nil {
				source = peer
				_ = socket.SetWriteDeadline(time.Now().Add(pilotLocalWrite))
				if _, err := socket.WriteToUDPAddrPort(buildSOCKSUDPDatagram(packet.Host, packet.Port, response), peer); err == nil {
					touch()
				}
			}
			continue
		}
		dest, allowed := relay.allowedTarget(ctx, packet)
		if !allowed {
			continue
		}
		if !relay.connect(ctx, control) {
			return
		}
		source = peer
		id, ok := flows[dest]
		if !ok {
			id = relay.registerFlow(dest, func(reply pilotPacket) {
				body := buildSOCKSUDPDatagram(reply.target.Addr().String(), int(reply.target.Port()), reply.body)
				_ = socket.SetWriteDeadline(time.Now().Add(pilotLocalWrite))
				if n, err := socket.WriteToUDPAddrPort(body, peer); err == nil && n == len(body) {
					touch()
					if relay.activity != nil {
						relay.activity()
					}
				}
			})
			if id == 0 {
				relay.mailbox.flowCap.Add(1)
				continue
			}
			flows[dest] = id
		}
		relay.offerDatagram(id, dest, packet.Payload)
	}
}

func pilotUDPRelayBind(peer netip.Addr, listener net.Addr) netip.Addr {
	peer = peer.Unmap()
	if peer.IsLoopback() {
		if peer.Is6() {
			return netip.IPv6Loopback()
		}
		return netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	if !peer.IsPrivate() || listener == nil {
		return netip.Addr{}
	}
	address, err := netip.ParseAddrPort(listener.String())
	local := address.Addr().Unmap()
	// A LAN relay is reachable only through the exact address that accepted
	// its SOCKS control connection. Never bind UDP to a wildcard address.
	if err != nil || !local.IsPrivate() || local.BitLen() != peer.BitLen() {
		return netip.Addr{}
	}
	return local
}

func pilotHEVAssoc(ctx context.Context, relay *pilotClientRelay, target string, control net.Conn) {
	remote, err := netip.ParseAddrPort(control.RemoteAddr().String())
	request, parseErr := netip.ParseAddrPort(target)
	if err != nil || !remote.Addr().IsLoopback() || parseErr != nil ||
		!request.Addr().IsUnspecified() || request.Port() != 0 {
		_ = socksReply(control, 0x02)
		return
	}
	if err := socksReply(control, 0); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	relay.localCancel = cancel
	stop := context.AfterFunc(ctx, func() { _ = control.Close() })
	replyCapacity := pilotMaxFlows
	if relay.batchBytes == pilotWidePlain {
		// Two creates can appear in the same poll. Queue both batches
		// without blocking discovery on the Android socket.
		replyCapacity = pilotMediaReadSlots * pilotMediaPacketLimit(relay.mediaBytes)
	}
	type hevReply struct {
		host     string
		port     int
		packet   pilotPacket
		mailbox  *pilotMailbox
		activity func()
	}
	replies := make(chan hevReply, replyCapacity)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case reply := <-replies:
				if reply.mailbox != nil && !reply.packet.received.IsZero() {
					if reason := reply.mailbox.incomingExpiry(reply.packet, time.Now()); reason != pilotPacketFresh {
						reply.mailbox.recordExpiry(reason, 1)
						continue
					}
				}
				frame := buildUDPInTCPFrame(reply.host, reply.port, reply.packet.body)
				_ = control.SetWriteDeadline(time.Now().Add(pilotLocalWrite))
				if _, err := control.Write(frame); err != nil {
					if reply.mailbox != nil && ctx.Err() == nil {
						reply.mailbox.hevWriteErrors.Add(1)
					}
					return
				}
				if reply.activity != nil {
					reply.activity()
				}
			}
		}
	}()
	flows := make(map[netip.AddrPort]uint64)
	maxFlows := pilotHEVMaxFlows
	if relay.publicIPv4 {
		maxFlows = pilotMaxFlows
	}
	defer func() {
		cancel()
		_ = control.Close()
		<-done
		relay.close(flows)
		stop()
	}()
	idle := relay.localIdle
	if idle <= 0 {
		idle = pilotHEVIdle
	}
	// Until a public lease takes over, only a useful DNS reply or an
	// accepted datagram renews the local frontend's lifetime.
	idleDeadline := time.Now().Add(idle)
	for {
		localDeadline := !relay.publicIPv4 || relay.mailbox == nil
		if localDeadline {
			if !time.Now().Before(idleDeadline) {
				return
			}
			_ = control.SetReadDeadline(idleDeadline)
		}
		packet, err := readUDPInTCPFrame(control)
		if err != nil {
			return
		}
		packetContext := ctx
		var finishLookup context.CancelFunc
		if localDeadline {
			packetContext, finishLookup = context.WithDeadline(ctx, idleDeadline)
		}
		if relay.dns && packet.Port == 53 {
			response, dnsErr := relay.answerDNS(packetContext, packet.Payload)
			if finishLookup != nil {
				finishLookup()
			}
			if dnsErr == nil {
				select {
				case <-ctx.Done():
					return
				case replies <- hevReply{host: packet.Host, port: packet.Port, packet: pilotPacket{body: response}}:
					if localDeadline {
						idleDeadline = time.Now().Add(idle)
					}
				default:
				}
			}
			continue
		}
		isVirtualLoopback := packet.Host == pilotHEVLoopbackAlias
		if isVirtualLoopback {
			packet.Host = "127.0.0.1"
		}
		dest, allowed := relay.allowedTarget(packetContext, packet)
		if finishLookup != nil {
			finishLookup()
		}
		if !allowed {
			if relay.mailbox != nil {
				relay.mailbox.rejected.Add(1)
			}
			continue
		}
		openingPublicLease := relay.publicIPv4 && relay.mailbox == nil
		if !relay.connect(ctx, control) {
			return
		}
		if openingPublicLease {
			// The active preview lease measures traffic in both directions and
			// closes this socket when idle or when its mux stream shuts down.
			_ = control.SetReadDeadline(time.Time{})
		}
		wireDest := dest
		if isVirtualLoopback {
			wireDest = netip.AddrPortFrom(netip.MustParseAddr(pilotHEVLoopbackAlias), dest.Port())
		}
		id, ok := flows[wireDest]
		if !ok {
			if len(flows) == maxFlows {
				relay.mailbox.rejected.Add(1)
				relay.mailbox.flowCap.Add(1)
				continue
			}
			id = relay.registerFlow(dest, func(reply pilotPacket) {
				select {
				case <-ctx.Done():
				case replies <- hevReply{host: wireDest.Addr().String(), port: int(wireDest.Port()), packet: reply, mailbox: relay.mailbox, activity: relay.activity}:
				default:
					relay.mailbox.dropped.Add(1)
					relay.mailbox.hevReplyDropped.Add(1)
				}
			})
			if id == 0 {
				relay.mailbox.rejected.Add(1)
				relay.mailbox.flowCap.Add(1)
				continue
			}
			flows[wireDest] = id
		}
		relay.offerDatagram(id, dest, packet.Payload)
		if !relay.publicIPv4 {
			idleDeadline = time.Now().Add(idle)
		}
	}
}
