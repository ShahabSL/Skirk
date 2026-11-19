package skirk

import (
	"math/rand"
	"testing"
	"time"
)

func timingTestState(t *testing.T, client bool, clock *time.Time, send, receive time.Time) *pilotTimingState {
	t.Helper()
	s := newPilotTimingState()
	s.now = func() time.Time { return *clock }
	if err := s.install(1, send, receive); err != nil {
		t.Fatal(err)
	}
	if err := s.activate(1); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPilotTimingCausalOriginsIndependentOffsetsRatesAndAsymmetry(t *testing.T) {
	rng := rand.New(rand.NewSource(7261))
	base := time.Unix(1_700_000_000, 0)
	for range 5000 {
		cr := 1 + (rng.Float64()*2-1)*0.001
		er := 1 + (rng.Float64()*2-1)*0.001
		f := time.Duration(rng.Int63n(int64(2 * time.Second)))
		r := time.Duration(rng.Int63n(int64(2 * time.Second)))
		processing := time.Duration(rng.Int63n(int64(time.Second)))
		rtt := f + processing + r
		offset := time.Duration(rng.Int63n(int64(6*24*time.Hour))) - 3*24*time.Hour
		cAt := func(d time.Duration) time.Time { return base.Add(time.Duration(float64(d) * cr)) }
		eAt := func(d time.Duration) time.Time { return base.Add(offset + time.Duration(float64(d)*er)) }
		cn, en := cAt(rtt), eAt(rtt)
		c := timingTestState(t, true, &cn, cAt(rtt), cAt(0))
		e := timingTestState(t, false, &en, eAt(f), eAt(f+processing))
		for _, age := range []time.Duration{100 * time.Millisecond, pilotLifetime + time.Nanosecond, 8 * time.Second} {
			sent := rtt + time.Second
			cn, en = cAt(sent+age), eAt(sent+age)
			up := e.accept(1, cAt(sent).Sub(cAt(rtt)), cAt(sent).Sub(cAt(0)), en)
			down := c.accept(1, eAt(sent).Sub(eAt(f)), eAt(sent).Sub(eAt(f+processing)), cn)
			if age <= pilotLifetime && (!up || !down) {
				t.Fatalf("fresh packet lost up=%t down=%t", up, down)
			}
			if age > pilotLifetime+max(f, r)+200*time.Millisecond && (up || down) {
				t.Fatalf("stale accepted age=%v up=%t down=%t", age, up, down)
			}
		}
	}
}

func TestPilotTimingDelayedACKRetainsBothDirectionsAndQueuedOldSend(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base.Add(time.Second)
	c := timingTestState(t, true, &now, base.Add(400*time.Millisecond), base)
	e := timingTestState(t, false, &now, base.Add(200*time.Millisecond), base.Add(300*time.Millisecond))
	now = base.Add(1300 * time.Millisecond)
	if err := e.install(2, base.Add(1100*time.Millisecond), base.Add(1200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := c.install(2, base.Add(1300*time.Millisecond), base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.activate(2); err != nil {
		t.Fatal(err)
	}
	now = base.Add(1500 * time.Millisecond)
	if !e.accept(2, 100*time.Millisecond, 400*time.Millisecond, now) {
		t.Fatal("upstream before ACK rejected")
	}
	// ACK remains delayed longer than both six-second grace and twelve-second
	// request timeout. Exit continues to send with epoch 1; client must retain it.
	now = base.Add(15 * time.Second)
	id, sent, receiveElapsed, ok := e.stamp(now)
	if !ok || id != 1 {
		t.Fatal("unacknowledged renewal stopped old downlink")
	}
	if !c.accept(id, sent, receiveElapsed, now.Add(100*time.Millisecond)) {
		t.Fatal("old downlink receive map retired before ACK")
	}
	if err := e.activate(2); err != nil {
		t.Fatal(err)
	}
	if !e.valid(1, now.Add(200*time.Millisecond)) {
		t.Fatal("fresh old downlink queued before ACK was evicted")
	}
	if !c.accept(1, sent, receiveElapsed, now.Add(200*time.Millisecond)) {
		t.Fatal("queued old downlink lost after ACK")
	}
	newID, newSent, newReceiveElapsed, ok := e.stamp(now)
	if !ok || newID != 2 {
		t.Fatal("ACK did not switch exit send epoch")
	}
	if !c.accept(newID, newSent, newReceiveElapsed, now.Add(300*time.Millisecond)) {
		t.Fatal("new downlink rejected")
	}
	now = now.Add(pilotLifetime + time.Second)
	if e.valid(1, now) || c.accept(1, sent, receiveElapsed, now) {
		t.Fatal("old send/receive epoch outlived its grace")
	}
}

func TestPilotTimingShortSuspendUsesConservativeElapsedEnvelope(t *testing.T) {
	// time.Time's monotonic representation is opaque; test the arithmetic with
	// independently supplied observations instead of manufacturing its fields.
	mono, wall := 5900*time.Millisecond, 6100*time.Millisecond
	if pilotTimingScaleUp(max(mono, wall))-pilotTimingScaleDown(0) <= pilotLifetime {
		t.Fatal("short receiver pause rejuvenated a stale packet")
	}
	if pilotTimingClockDiscontinuous(200*time.Millisecond, 400*time.Millisecond) {
		t.Fatal("fixture did not cover subthreshold pause")
	}
	if !pilotTimingClockDiscontinuous(400*time.Millisecond, 800*time.Millisecond) {
		t.Fatal("cumulative small pauses were not detected")
	}
	base := time.Now()
	now := base.Add(time.Second)
	s := timingTestState(t, true, &now, base, base)
	var signaled int
	s.setNotify(func() { signaled++ })
	s.mu.Lock()
	changed := s.observeElapsedLocked(now, 400*time.Millisecond, 800*time.Millisecond)
	s.mu.Unlock()
	s.signal(changed)
	if _, _, _, ok := s.stamp(now); ok || signaled == 0 {
		t.Fatal("discontinuity retained old epoch")
	}
	now = now.Add(time.Millisecond)
	if err := s.install(2, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.activate(2); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := s.stamp(now); !ok {
		t.Fatal("new epoch did not recover")
	}
}

func TestPilotTimingEpochCapacityReplayExpiryAndAbandon(t *testing.T) {
	base := time.Now()
	now := base.Add(time.Second)
	s := timingTestState(t, true, &now, base, base)
	if err := s.install(1, base, base); err == nil {
		t.Fatal("replayed epoch installed")
	}
	for id := uint64(2); id < 100; id++ {
		if err := s.install(id, now, now); err != nil {
			t.Fatal(err)
		}
		s.abandon(id)
	}
	if len(s.epochs) != 1 {
		t.Fatal("abandoned maps leaked")
	}
	for _, id := range []uint64{100, 101} {
		if err := s.install(id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.install(102, now, now); err == nil {
		t.Fatal("unbounded epoch maps")
	}
	if s.accept(1, time.Duration(1<<63-1), 0, now) || s.accept(0, 0, 0, now) || s.accept(900, 0, 0, now) {
		t.Fatal("invalid packet timestamp accepted")
	}
	now = base.Add(pilotTimingMaxEpochAge + time.Second)
	if _, _, _, ok := s.stamp(now); ok {
		t.Fatal("hard-expired epoch stayed active")
	}
}

func TestPilotTimingReceivedDeadlineRetainsOnlyRemainingBudget(t *testing.T) {
	base := time.Now()
	now := base.Add(5 * time.Second)
	s := timingTestState(t, false, &now, base, base)
	sent, reason := s.received(1, 2*time.Second, 2*time.Second, now)
	if reason != pilotPacketFresh {
		t.Fatal("fresh packet rejected")
	}
	age := now.Sub(sent)
	if age < 2900*time.Millisecond || age > 3*time.Second {
		t.Fatalf("wrong conservative receive age: %v", age)
	}
	if pilotElapsedUpper(sent, now.Add(3100*time.Millisecond)) < pilotLifetime {
		t.Fatal("proxy queue granted a new six seconds")
	}
	s.invalidate()
	if s.accept(1, 2*time.Second, 2*time.Second, now) {
		t.Fatal("pending old-epoch packet survived discontinuity")
	}
}
