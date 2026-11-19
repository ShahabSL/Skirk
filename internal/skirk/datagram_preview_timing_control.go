package skirk

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	pilotControlClose byte = iota + 1
	pilotControlPrepare
	pilotControlReply
	pilotControlAck
	pilotControlUncertain
	pilotControlRefresh
	pilotControlResetAssociation
	pilotControlAssociationBatch = 64
	pilotControlMaxBody          = 4 * pilotControlAssociationBatch
)

type pilotControlFrame struct {
	kind  byte
	epoch uint64
	body  []byte
}

type pilotAssociationReset struct {
	id      uint32
	written func()
}

func pilotControlEncode(key []byte, frame pilotControlFrame) ([]byte, error) {
	if len(frame.body) > pilotControlMaxBody {
		return nil, errors.New("UDP timing control frame too large")
	}
	wire := make([]byte, 11, 11+len(frame.body)+sha256.Size)
	wire[0] = frame.kind
	binary.BigEndian.PutUint64(wire[1:9], frame.epoch)
	binary.BigEndian.PutUint16(wire[9:11], uint16(len(frame.body)))
	wire = append(wire, frame.body...)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(wire)
	return append(wire, mac.Sum(nil)...), nil
}

func pilotControlRead(reader io.Reader, key []byte) (pilotControlFrame, error) {
	var header [11]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return pilotControlFrame{}, err
	}
	n := int(binary.BigEndian.Uint16(header[9:]))
	if n > pilotControlMaxBody {
		return pilotControlFrame{}, errors.New("UDP timing control frame too large")
	}
	body := make([]byte, n+sha256.Size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return pilotControlFrame{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(header[:])
	_, _ = mac.Write(body[:n])
	if !hmac.Equal(body[n:], mac.Sum(nil)) {
		return pilotControlFrame{}, errors.New("UDP timing control authentication failed")
	}
	return pilotControlFrame{header[0], binary.BigEndian.Uint64(header[1:]), body[:n]}, nil
}

type pilotTimingControl struct {
	net.Conn
	timing           *pilotTimingState
	key              []byte
	client           bool
	closeAssociation func(uint32) error
	resets           <-chan pilotAssociationReset
	writeMu          sync.Mutex
	refresh          chan struct{}
	done             chan struct{}
	mu               sync.Mutex
	err              error
	resetAssociation func(uint32) // Registered before the client opens its first association.
	logger           *log.Logger
}

// The existing challenge installs epoch 1. This wrapper owns subsequent typed
// control traffic; the shared association owner retains ownership of CLOSE IDs.
func newPilotTimingControl(ctx context.Context, conn net.Conn, tunnel *Tunnel, lease [16]byte, timing *pilotTimingState, client bool, closeAssociation func(uint32) error, resets <-chan pilotAssociationReset, onFailure func()) (*pilotTimingControl, error) {
	root, err := DeriveKey(tunnel.Secret)
	if err != nil {
		return nil, err
	}
	info := append([]byte("skirk-udp-interval-control-v3/"), lease[:]...)
	c := &pilotTimingControl{Conn: conn, timing: timing, key: hkdfSHA256(root, tunnel.SessionID[:], info, sha256.Size), client: client,
		closeAssociation: closeAssociation, resets: resets, refresh: make(chan struct{}, 1), done: make(chan struct{}), logger: tunnel.Logger}
	timing.setNotify(func() {
		select {
		case c.refresh <- struct{}{}:
		default:
		}
	})
	go func() {
		err := c.run(ctx)
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		_ = conn.Close()
		close(c.done)
		if onFailure != nil {
			onFailure()
		}
	}()
	return c, nil
}

func (c *pilotTimingControl) wait() error { <-c.done; c.mu.Lock(); defer c.mu.Unlock(); return c.err }

func (c *pilotTimingControl) setAssociationResetHandler(handler func(uint32)) {
	c.mu.Lock()
	c.resetAssociation = handler
	c.mu.Unlock()
}

func validPilotAssociationFrame(frame pilotControlFrame) bool {
	if frame.epoch != 0 || len(frame.body) == 0 || len(frame.body)%4 != 0 {
		return false
	}
	for ids := frame.body; len(ids) != 0; ids = ids[4:] {
		if binary.BigEndian.Uint32(ids) == 0 {
			return false
		}
	}
	return true
}

func (c *pilotTimingControl) Write(data []byte) (int, error) {
	if !c.client || len(data) == 0 || len(data)%4 != 0 || len(data) > pilotControlMaxBody {
		return 0, errors.New("invalid UDP association close frame")
	}
	if err := c.send(pilotControlFrame{kind: pilotControlClose, body: data}); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (c *pilotTimingControl) send(frame pilotControlFrame) error {
	wire, err := pilotControlEncode(c.key, frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(udpPreviewHandshake)); err != nil {
		return err
	}
	return writeAll(c.Conn, wire)
}

func (c *pilotTimingControl) logEpoch(epoch uint64, elapsed time.Duration) {
	if c.logger == nil {
		return
	}
	role := "exit"
	if c.client {
		role = "client"
	}
	c.logger.Printf("udp timing role=%s epoch=%d control_ms=%d rate_bound_ppm=%d", role, epoch, elapsed.Milliseconds(), pilotTimingRatePPM)
}

func (c *pilotTimingControl) run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Conn.Close() })
	defer stop()
	type received struct {
		frame pilotControlFrame
		at    time.Time
		err   error
	}
	incoming := make(chan received, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			frame, err := pilotControlRead(c.Conn, c.key)
			item := received{frame, time.Now(), err}
			select {
			case incoming <- item:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = c.Conn.Close(); <-readerDone }()
	ticker := time.NewTicker(pilotTimingRenewInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(udpPreviewHandshake)
	deadline.Stop()
	defer deadline.Stop()
	var pending uint64
	next := uint64(1)
	var started time.Time
	refreshSent := false
	begin := func() error {
		if pending != 0 {
			return nil
		}
		next++
		if next == 0 {
			return errors.New("UDP timing epoch exhausted")
		}
		pending, started = next, c.timing.begin()
		deadline.Reset(udpPreviewHandshake)
		return c.send(pilotControlFrame{kind: pilotControlPrepare, epoch: pending})
	}
	requestRefresh := func() error {
		if refreshSent {
			return nil
		}
		if _, _, _, ok := c.timing.stamp(c.timing.nowTime()); ok {
			return nil
		}
		if err := c.send(pilotControlFrame{kind: pilotControlRefresh}); err != nil {
			return err
		}
		refreshSent = true
		deadline.Reset(udpPreviewHandshake)
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.client {
				if err := begin(); err != nil {
					return err
				}
			}
		case <-deadline.C:
			if c.client {
				c.timing.abandon(pending)
				pending = 0
				if err := begin(); err != nil {
					return err
				}
			} else {
				refreshSent = false
				if err := requestRefresh(); err != nil {
					return err
				}
			}
		case <-c.refresh:
			if c.client {
				if err := begin(); err != nil {
					return err
				}
			} else if err := requestRefresh(); err != nil {
				return err
			}
		case reset, ok := <-c.resets:
			if !ok {
				return errors.New("UDP association reset queue closed")
			}
			var batch [pilotControlAssociationBatch]pilotAssociationReset
			batch[0] = reset
			n := 1 + min(len(c.resets), len(batch)-1)
			for i := 1; i < n; i++ {
				batch[i] = <-c.resets
			}
			var frame [pilotControlMaxBody]byte
			body := frame[:0]
			for _, reset := range batch[:n] {
				body = binary.BigEndian.AppendUint32(body, reset.id)
			}
			if err := c.send(pilotControlFrame{kind: pilotControlResetAssociation, body: body}); err != nil {
				return err
			}
			for _, reset := range batch[:n] {
				if reset.written != nil {
					reset.written()
				}
			}
		case item := <-incoming:
			if item.err != nil {
				return item.err
			}
			f := item.frame
			if f.kind != pilotControlClose && f.kind != pilotControlResetAssociation && len(f.body) != 0 {
				return errors.New("unexpected UDP timing payload")
			}
			switch f.kind {
			case pilotControlClose:
				if c.client || c.closeAssociation == nil || !validPilotAssociationFrame(f) {
					return errors.New("unexpected UDP association close frame")
				}
				for b := f.body; len(b) != 0; b = b[4:] {
					if err := c.closeAssociation(binary.BigEndian.Uint32(b)); err != nil {
						return err
					}
				}
			case pilotControlResetAssociation:
				c.mu.Lock()
				reset := c.resetAssociation
				c.mu.Unlock()
				if !c.client || reset == nil || !validPilotAssociationFrame(f) {
					return errors.New("invalid UDP association reset frame")
				}
				for ids := f.body; len(ids) != 0; ids = ids[4:] {
					reset(binary.BigEndian.Uint32(ids))
				}
			case pilotControlPrepare:
				if c.client || f.epoch == 0 {
					return errors.New("unexpected UDP timing request")
				}
				if f.epoch <= next {
					continue
				}
				c.timing.abandon(pending)
				next, pending = f.epoch, f.epoch
				// Send origin is after PREPARE; receive origin precedes REPLY.
				started = item.at
				if err := c.timing.install(pending, started, c.timing.nowTime()); err != nil {
					if err := c.send(pilotControlFrame{kind: pilotControlUncertain, epoch: pending}); err != nil {
						return err
					}
					pending = 0
					continue
				}
				if err := c.send(pilotControlFrame{kind: pilotControlReply, epoch: pending}); err != nil {
					return err
				}
				// A delayed ACK remains usable until a newer PREPARE or hard expiry.
			case pilotControlReply:
				if !c.client {
					return errors.New("unexpected UDP timing reply")
				}
				if f.epoch <= next && f.epoch != pending {
					continue
				}
				if pending == 0 || f.epoch != pending {
					return errors.New("unexpected UDP timing epoch")
				}
				if err := c.timing.install(pending, item.at, started); err != nil {
					c.timing.abandon(pending)
					pending = 0
					deadline.Reset(time.Second)
					continue
				}
				if err := c.timing.activate(pending); err != nil {
					c.timing.abandon(pending)
					pending = 0
					deadline.Reset(time.Second)
					continue
				}
				deadline.Stop()
				if err := c.send(pilotControlFrame{kind: pilotControlAck, epoch: pending}); err != nil {
					return err
				}
				c.logEpoch(pending, pilotElapsedUpper(started, item.at))
				pending = 0
			case pilotControlAck:
				if c.client {
					return errors.New("unexpected UDP timing ACK")
				}
				if f.epoch <= next && f.epoch != pending {
					continue
				}
				if pending == 0 || f.epoch != pending {
					return errors.New("unexpected UDP timing epoch")
				}
				if err := c.timing.activate(pending); err != nil {
					pending = 0
					refreshSent = false
					if err := requestRefresh(); err != nil {
						return err
					}
					continue
				}
				c.logEpoch(pending, pilotElapsedUpper(started, item.at))
				pending = 0
				refreshSent = false
				deadline.Stop()
			case pilotControlUncertain:
				if !c.client {
					return errors.New("unexpected UDP timing refusal")
				}
				if f.epoch <= next && f.epoch != pending {
					continue
				}
				if pending == 0 || f.epoch != pending {
					return errors.New("unexpected UDP timing epoch")
				}
				c.timing.abandon(pending)
				pending = 0
				deadline.Reset(time.Second)
			case pilotControlRefresh:
				if !c.client || f.epoch != 0 {
					return errors.New("unexpected UDP timing refresh")
				}
				if err := begin(); err != nil {
					return err
				}
			default:
				return errors.New("unknown UDP timing control frame")
			}
		}
	}
}
