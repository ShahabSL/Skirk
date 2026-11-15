package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// A latest-state/CAS fixture, not an append-only packet broker. A one-slot
// notification queue deliberately coalesces writes while retaining the doc.
type fsHybridMemoryFault struct {
	Commit        bool
	Err           error
	Gate          <-chan struct{}
	IgnoreContext bool
	Returned      *fsPriorityDocument
}
type fsHybridMemoryWrite struct {
	Side, Call int
	Expected   string
	Digest     [32]byte
}
type fsHybridMemoryCloud struct {
	mu                            sync.Mutex
	docs                          [2]fsPriorityDocument
	exists                        [2]bool
	version                       uint64
	notice                        [2]chan fsPriorityDocument
	watchErrors                   [2]chan error
	drop                          [2]bool
	writes, ownReads, peerReads   [2]int
	writeFaults                   [2]map[int]fsHybridMemoryFault
	ownReadFaults, peerReadFaults [2]map[int]error
	log                           []fsHybridMemoryWrite
}
type fsHybridMemoryCarrier struct {
	cloud  *fsHybridMemoryCloud
	side   int
	ctx    context.Context
	cancel context.CancelFunc
}

func newFSHybridMemoryCloud() *fsHybridMemoryCloud {
	c := &fsHybridMemoryCloud{version: 2, exists: [2]bool{true, true}}
	for i := 0; i < 2; i++ {
		c.notice[i] = make(chan fsPriorityDocument, 1)
		c.watchErrors[i] = make(chan error, 1)
		c.writeFaults[i] = make(map[int]fsHybridMemoryFault)
		c.ownReadFaults[i] = make(map[int]error)
		c.peerReadFaults[i] = make(map[int]error)
		c.docs[i] = fsPriorityDocument{UpdateTime: fsHybridMemoryTimestamp(uint64(i + 1))}
	}
	return c
}
func fsHybridMemoryTimestamp(version uint64) string {
	return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(version) * time.Microsecond).Format(time.RFC3339Nano)
}
func fsHybridMemoryClone(d fsPriorityDocument) fsPriorityDocument {
	d.Ciphertext = bytes.Clone(d.Ciphertext)
	return d
}
func (c *fsHybridMemoryCloud) carrier(side int) *fsHybridMemoryCarrier {
	ctx, cancel := context.WithCancel(context.Background())
	return &fsHybridMemoryCarrier{cloud: c, side: side, ctx: ctx, cancel: cancel}
}
func (c *fsHybridMemoryCloud) nextWrite(side int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes[side] + 1
}
func (c *fsHybridMemoryCloud) nextOwnRead(side int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownReads[side] + 1
}
func (c *fsHybridMemoryCloud) setWriteFault(side, call int, f fsHybridMemoryFault) {
	c.mu.Lock()
	c.writeFaults[side][call] = f
	c.mu.Unlock()
}
func (c *fsHybridMemoryCloud) setOwnReadFault(side, call int, err error) {
	c.mu.Lock()
	c.ownReadFaults[side][call] = err
	c.mu.Unlock()
}
func (c *fsHybridMemoryCloud) setDrop(side int, drop bool) {
	c.mu.Lock()
	c.drop[side] = drop
	c.mu.Unlock()
}
func (c *fsHybridMemoryCloud) counts() (writes, own, peer [2]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.ownReads, c.peerReads
}
func (c *fsHybridMemoryCloud) writeLog() []fsHybridMemoryWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fsHybridMemoryWrite(nil), c.log...)
}
func (c *fsHybridMemoryCloud) notifyLocked(side int) {
	if c.drop[side] {
		return
	}
	doc := fsHybridMemoryClone(c.docs[side])
	target := c.notice[1-side]
	select {
	case <-target:
	default:
	}
	select {
	case target <- doc:
	default:
	}
}
func (c *fsHybridMemoryCloud) deliver(side int) {
	c.mu.Lock()
	drop := c.drop[side]
	c.drop[side] = false
	c.notifyLocked(side)
	c.drop[side] = drop
	c.mu.Unlock()
}
func (c *fsHybridMemoryCloud) failWatch(receiverSide int, err error) {
	c.watchErrors[receiverSide] <- err
}
func (c *fsHybridMemoryCloud) deleteDocument(side int) {
	c.mu.Lock()
	c.exists[side] = false
	c.docs[side] = fsPriorityDocument{}
	c.mu.Unlock()
}

func (c *fsHybridMemoryCarrier) WriteOwn(ctx context.Context, expected string, wire []byte) (fsPriorityDocument, error) {
	if ctx.Err() != nil {
		return fsPriorityDocument{}, ctx.Err()
	}
	if c.ctx.Err() != nil {
		return fsPriorityDocument{}, c.ctx.Err()
	}
	cloud := c.cloud
	cloud.mu.Lock()
	cloud.writes[c.side]++
	call := cloud.writes[c.side]
	fault, hasFault := cloud.writeFaults[c.side][call]
	cloud.log = append(cloud.log, fsHybridMemoryWrite{Side: c.side, Call: call, Expected: expected, Digest: sha256.Sum256(wire)})
	cloud.mu.Unlock()
	if fault.Gate != nil {
		if fault.IgnoreContext {
			<-fault.Gate
		} else {
			select {
			case <-fault.Gate:
			case <-ctx.Done():
				return fsPriorityDocument{}, ctx.Err()
			case <-c.ctx.Done():
				return fsPriorityDocument{}, c.ctx.Err()
			}
		}
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if !cloud.exists[c.side] || cloud.docs[c.side].UpdateTime != expected {
		return fsPriorityDocument{}, &GoogleAPIError{Op: "public_fixture_cas", Status: 412, Reason: "FAILED_PRECONDITION"}
	}
	if !hasFault || fault.Commit {
		cloud.version++
		cloud.docs[c.side] = fsPriorityDocument{Ciphertext: bytes.Clone(wire), UpdateTime: fsHybridMemoryTimestamp(cloud.version)}
		cloud.notifyLocked(c.side)
	}
	doc := fsHybridMemoryClone(cloud.docs[c.side])
	if fault.Returned != nil {
		doc = fsHybridMemoryClone(*fault.Returned)
	}
	return doc, fault.Err
}
func (c *fsHybridMemoryCarrier) ReadOwn(ctx context.Context) (fsPriorityDocument, error) {
	if ctx.Err() != nil {
		return fsPriorityDocument{}, ctx.Err()
	}
	if c.ctx.Err() != nil {
		return fsPriorityDocument{}, c.ctx.Err()
	}
	c.cloud.mu.Lock()
	defer c.cloud.mu.Unlock()
	c.cloud.ownReads[c.side]++
	call := c.cloud.ownReads[c.side]
	if err := c.cloud.ownReadFaults[c.side][call]; err != nil {
		return fsPriorityDocument{}, err
	}
	if !c.cloud.exists[c.side] {
		return fsPriorityDocument{}, &GoogleAPIError{Op: "public_fixture_read", Status: 404, Reason: "NOT_FOUND"}
	}
	return fsHybridMemoryClone(c.cloud.docs[c.side]), nil
}
func (c *fsHybridMemoryCarrier) ReadPeer(ctx context.Context) (fsPriorityDocument, error) {
	if ctx.Err() != nil {
		return fsPriorityDocument{}, ctx.Err()
	}
	if c.ctx.Err() != nil {
		return fsPriorityDocument{}, c.ctx.Err()
	}
	c.cloud.mu.Lock()
	defer c.cloud.mu.Unlock()
	c.cloud.peerReads[c.side]++
	call := c.cloud.peerReads[c.side]
	if err := c.cloud.peerReadFaults[c.side][call]; err != nil {
		return fsPriorityDocument{}, err
	}
	if !c.cloud.exists[1-c.side] {
		return fsPriorityDocument{}, &GoogleAPIError{Op: "public_fixture_read", Status: 404, Reason: "NOT_FOUND"}
	}
	return fsHybridMemoryClone(c.cloud.docs[1-c.side]), nil
}
func (c *fsHybridMemoryCarrier) WatchPeer(ctx context.Context, callback func(fsPriorityDocument) error) error {
	doc, err := c.ReadPeer(ctx)
	if err != nil {
		return err
	}
	if err = callback(doc); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return c.ctx.Err()
		case err := <-c.cloud.watchErrors[c.side]:
			return err
		case doc := <-c.cloud.notice[c.side]:
			if err := callback(doc); err != nil {
				return err
			}
		}
	}
}
func (c *fsHybridMemoryCarrier) Close() error { c.cancel(); return nil }
