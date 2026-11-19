package skirk

import (
	"context"
	"errors"
	"math"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewSharedExitDoesNotInferUnseenIDsAreClosed(t *testing.T) {
	peer, target := stableUDPTestPeer(t)
	mailbox := stableUDPTestMailbox(target)
	s := sharedExitTestManager(t, mailbox)
	// An older still-live local association may have lost all of its first
	// packets while these explicitly closed IDs were seen on control.
	for id := uint32(2); id <= pilotSharedRetirementWindow+20; id++ {
		if err := s.closeAssociation(id); err != nil {
			t.Fatal(err)
		}
	}
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, body: []byte("first arrival")})
	if mailbox.rejected.Load() != 0 || sharedExitTestGroupCount(s) != 1 {
		t.Fatal("dropped a live but previously unseen association")
	}
	stableUDPRead(t, peer, "first arrival")
	s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(2, 1), target: target, body: []byte("explicitly closed")})
	if mailbox.rejected.Load() != 1 || sharedExitTestGroupCount(s) != 1 {
		t.Fatal("an explicitly closed association was resurrected")
	}
}

func TestUDPPreviewSharedExitLargeIDCannotPoisonUnrelatedAssociations(t *testing.T) {
	for _, send := range []bool{false, true} {
		name := "control"
		if send {
			name = "data"
		}
		t.Run(name, func(t *testing.T) {
			peer, target := stableUDPTestPeer(t)
			mailbox := stableUDPTestMailbox(target)
			s := sharedExitTestManager(t, mailbox)
			if send {
				s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(math.MaxUint32, 1), target: target, body: []byte("high")})
				stableUDPRead(t, peer, "high")
			} else if err := s.closeAssociation(math.MaxUint32); err != nil {
				t.Fatal(err)
			}
			s.send(t.Context(), pilotPacket{flow: sharedExitTestFlow(1, 1), target: target, body: []byte("unrelated")})
			if mailbox.rejected.Load() != 0 {
				t.Fatal("one high association ID rejected a different valid association")
			}
			stableUDPRead(t, peer, "unrelated")
		})
	}
}

func TestUDPPreviewSharedExitExactRetirementIsBounded(t *testing.T) {
	_, target := stableUDPTestPeer(t)
	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)
	s := newPilotSharedUDPExit(ctx, stableUDPTestMailbox(target), 16, func(err error) { failed <- err })
	t.Cleanup(func() { cancel(); <-s.done })
	for id := uint32(1); id < 2*pilotSharedRetirementWindow; id += 2 {
		if err := s.closeAssociation(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.closeAssociation(2 * pilotSharedRetirementWindow); err != nil {
		t.Fatalf("full history should still merge an adjacent close: %v", err)
	}
	if err := s.closeAssociation(2*pilotSharedRetirementWindow + 2); !errors.Is(err, errSharedUDPRetirementLimit) {
		t.Fatalf("fragmented history overflow did not reset the carrier: %v", err)
	}
	if err := <-failed; !errors.Is(err, errSharedUDPRetirementLimit) {
		t.Fatalf("retirement failure was not reported to the carrier owner: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.retired) != pilotSharedRetirementWindow || s.ctx.Err() == nil {
		t.Fatal("retirement did not stop at its exact bounded budget")
	}
}

func TestUDPPreviewSharedClientWaitsForDyingGeneration(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "control_write_failure"
		if full {
			name = "expired_at_capacity"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				finishing := make(chan string, 1)
				finish := make(chan struct{})
				finishReleased := false
				defer func() {
					if !finishReleased {
						close(finish)
					}
				}()
				var expire context.CancelFunc
				var opens, refused int
				s := &pilotSharedClient{ctx: ctx, maxAssociations: 2, onLimit: func() { refused++ }}
				s.open = func(parent context.Context) (*pilotClientLease, error) {
					opens++
					leaseCtx, stop := context.WithCancel(parent)
					lease, remote := sharedClientTestLease(leaseCtx)
					cleanup := lease.finish
					lease.finish = func(reason string) {
						if opens == 1 {
							finishing <- reason
							<-finish
						}
						stop()
						_ = remote.Close()
						cleanup(reason)
					}
					if opens == 1 {
						expire = stop
						if !full {
							lease.control = sharedFailWriteDeadline{lease.control}
						}
					}
					return lease, nil
				}
				first := <-asyncSharedClientAcquire(t, s, ctx)
				second := <-asyncSharedClientAcquire(t, s, ctx)
				if first.err != nil || second.err != nil {
					t.Fatal("could not acquire fixture associations")
				}
				defer first.release()
				defer second.release()
				if full {
					expire()
				} else {
					second.release()
				}
				<-finishing
				waiting := asyncSharedClientAcquire(t, s, ctx)
				synctest.Wait()
				select {
				case result := <-waiting:
					if result.release != nil {
						result.release()
					}
					t.Fatalf("returned before retiring the previous carrier: error=%v", result.err)
				default:
				}
				if refused != 0 {
					t.Errorf("reported %d association limit hits for a dying carrier", refused)
				}
				close(finish)
				finishReleased = true
				next := <-waiting
				if next.err != nil || next.id != 1 || next.lease == first.lease {
					t.Fatalf("failed to re-open after teardown: id=%d error=%v", next.id, next.err)
				}
				next.release()
			})
		})
	}
}

type sharedFailWriteDeadline struct{ net.Conn }

func (sharedFailWriteDeadline) SetWriteDeadline(time.Time) error {
	return errors.New("fixture control write failure")
}

func TestUDPPreviewSharedExitStopsRetryingUnavailableProxy(t *testing.T) {
	_, target := stableUDPTestPeer(t)
	fixture, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	go func() {
		for {
			conn, err := fixture.Accept()
			if err != nil {
				return
			}
			// Refuse the transport before any valid SOCKS response. This is
			// transient from the client's point of view.
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	mailbox := stableUDPTestMailbox(target)
	mailbox.exitProxy = "socks5://" + fixture.Addr().String()
	failures := make(chan error, 1)
	s := newPilotSharedUDPExit(ctx, mailbox, 16, func(err error) { failures <- err })
	t.Cleanup(func() { cancel(); <-s.done })
	for attempt := uint64(1); attempt <= pilotSharedSocketAttempts; attempt++ {
		s.send(ctx, pilotPacket{flow: sharedExitTestFlow(1, 1), sent: time.Now(), target: target, body: []byte("test")})
		stableUDPWait(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			group := s.groups[1]
			return s.ctx.Err() != nil || group != nil && !group.opening && mailbox.originDialErrors.Load() >= attempt
		})
		if s.ctx.Err() != nil {
			if err := <-failures; attempt != pilotSharedSocketAttempts || !errors.Is(err, errSharedUDPSetupExhausted) {
				t.Fatalf("carrier failed with %v at unexpected retry count %d", err, attempt)
			}
			return
		}
		s.mu.Lock()
		if group := s.groups[1]; group != nil {
			group.retryAfter = time.Time{}
		}
		s.mu.Unlock()
	}
	t.Fatal("dead proxy continues retrying indefinitely on a live carrier")
}

func TestUDPPreviewFairQueueExpiresBeforeEvictingFreshPacket(t *testing.T) {
	target := netip.MustParseAddrPort("127.0.0.1:12000")
	mailbox := stableUDPTestMailbox(target)
	mailbox.fairQueue = newPilotFairQueue(3, 3*pilotFairQuantum)
	mailbox.fairQueue.offer(pilotPacket{flow: sharedExitTestFlow(2, 1), sent: time.Now().Add(-pilotLifetime), target: target, body: []byte("old")})
	mailbox.offer(sharedExitTestFlow(1, 1), target, []byte("fresh one"))
	mailbox.offer(sharedExitTestFlow(1, 2), target, []byte("fresh two"))
	mailbox.offer(sharedExitTestFlow(3, 1), target, []byte("incoming"))
	if mailbox.ingressDropped.Load() != 0 || mailbox.stale.Load() != 1 || mailbox.fairQueue.len() != 3 {
		t.Fatalf("avoided expiry by dropping fresh traffic: evicted=%d expired=%d queued=%d",
			mailbox.ingressDropped.Load(), mailbox.stale.Load(), mailbox.fairQueue.len())
	}
}
