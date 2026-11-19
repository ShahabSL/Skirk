package skirk

import (
	"math/rand"
	"testing"
	"time"
)

func TestPilotTimingCertainStalePolicyAsymmetric(t *testing.T) {
	const send, forward, reply = 10 * time.Second, 1200 * time.Millisecond, 1300 * time.Millisecond
	const clientReply = 2800 * time.Millisecond
	exitOrigin := time.Unix(1_700_000_000, 0)
	exitNow := exitOrigin.Add(send)
	exit := timingTestState(t, false, &exitNow, exitOrigin.Add(forward), exitOrigin.Add(reply))
	for _, tc := range []struct {
		age  time.Duration
		want pilotExpiryReason
	}{
		{5 * time.Second, pilotPacketFresh},
		{6200 * time.Millisecond, pilotPacketFresh},
		{7500 * time.Millisecond, pilotIncomingCertainlyStale},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			exitNow = exitOrigin.Add(send + tc.age)
			lower, upper, valid := pilotTimingAgeBounds(send+tc.age-forward, send+tc.age-reply, send-clientReply, send)
			if !valid || lower > tc.age || upper < tc.age || upper <= pilotLifetime {
				t.Fatalf("fixture should contain real age %s with a late upper bound, got [%s,%s] valid=%t", tc.age, lower, upper, valid)
			}
			rebased, got := exit.received(1, send-clientReply, send, exitNow)
			if got != tc.want || got == pilotPacketFresh && exitNow.Sub(rebased) != lower {
				t.Fatalf("received reason=%v, want %v, rebased=%v", got, tc.want, rebased)
			}
		})
	}
}

func TestPilotTimingAgeIntervalContainsRealAgeWithClockFaults(t *testing.T) {
	random := rand.New(rand.NewSource(1845))
	ms := func(limit int) time.Duration { return time.Duration(random.Intn(limit)) * time.Millisecond }
	type clock struct {
		ppm       int64
		faultMode int
	}
	newClock := func() clock {
		return clock{int64(random.Intn(2001)) - 1000, random.Intn(4)}
	}
	measure := func(c clock, real time.Duration) (time.Duration, time.Duration) {
		// Physical timings are on millisecond boundaries so the simulated
		// ppm rate is representable exactly at nanosecond resolution.
		good := time.Duration(int64(real) * (1_000_000 + c.ppm) / 1_000_000)
		delta := 200 * time.Millisecond
		switch c.faultMode {
		case 0: // Monotonic clock paused while wall time kept advancing.
			return max(0, good-delta), good
		case 1: // Wall-clock rollback alone.
			return max(0, good-delta), good
		case 2: // Wall clock jumped forward alone.
			return good, good + delta
		default:
			return good, good
		}
	}
	for range 1500 {
		client, exit := newClock(), newClock()
		e0, processing, backward := ms(3000), ms(1000), ms(3000)
		e1 := e0 + processing
		c1 := e1 + backward
		sent, age := c1+time.Second+ms(15000), ms(11000)
		arrival := sent + age
		for _, dir := range []struct {
			ourClock, peerClock                     clock
			ourSend, ourReceive, peerSend, peerRecv time.Duration
		}{
			{exit, client, e0, e1, c1, 0},
			{client, exit, c1, 0, e0, e1},
		} {
			localLow, _ := measure(dir.ourClock, arrival-dir.ourSend)
			_, localHigh := measure(dir.ourClock, arrival-dir.ourReceive)
			peerLow, _ := measure(dir.peerClock, sent-dir.peerSend)
			_, peerHigh := measure(dir.peerClock, sent-dir.peerRecv)
			lower, upper, ok := pilotTimingAgeBounds(localLow, localHigh, peerLow, peerHigh)
			if !ok || lower > age || upper < age || lower < 0 {
				t.Fatalf("real age %s outside [%s,%s], valid=%t", age, lower, upper, ok)
			}
		}
	}
	if _, _, ok := pilotTimingAgeBounds(time.Second, 0, time.Second, 0); ok {
		t.Fatal("contradictory age interval accepted")
	}
}

func TestPilotTimingRetainedProxyPacketRechecksOriginalIntervals(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name       string
		forward    time.Duration
		firstAge   time.Duration
		proxyDelay time.Duration
		want       pilotExpiryReason
	}{
		{"certain-stale before local timeout", time.Second, 5500 * time.Millisecond, 2 * time.Second, pilotIncomingCertainlyStale},
		{"local cap in ambiguity interval", 11 * time.Second, 2 * time.Second, 6 * time.Second, pilotLocalQueueExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const send = 20 * time.Second
			now := base.Add(send + tc.firstAge)
			s := timingTestState(t, false, &now, base.Add(tc.forward), base.Add(tc.forward))
			origin := pilotMailbox{timing: s}
			packet := pilotPacket{
				timingEpoch: 1, elapsed: send - tc.forward - time.Second, receiveElapsed: send, received: now,
			}
			rebased, reason := s.received(1, packet.elapsed, packet.receiveElapsed, now)
			if reason != pilotPacketFresh {
				t.Fatalf("proxy packet should initially be usable: %v", reason)
			}
			packet.sent = rebased
			now = now.Add(tc.proxyDelay)
			if got := origin.incomingExpiry(packet, now); got != tc.want {
				t.Fatalf("queued packet lost its original bound: got %v, want %v", got, tc.want)
			}
			s.invalidate()
			packet.received = now
			if got := origin.incomingExpiry(packet, now); got != pilotInvalidTiming {
				t.Fatalf("invalidated queued epoch remained usable: %v", got)
			}
		})
	}
}
