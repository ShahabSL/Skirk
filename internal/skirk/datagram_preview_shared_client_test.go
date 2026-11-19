package skirk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUDPPreviewSharedClientKeepsPendingHandshakeForOtherWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opened := make(chan context.Context, 1)
		proceed := make(chan struct{})
		s := &pilotSharedClient{ctx: ctx, open: func(ctx context.Context) (*pilotClientLease, error) {
			opened <- ctx
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-proceed:
				lease, remote := sharedClientTestLease(ctx)
				context.AfterFunc(ctx, func() { _ = remote.Close() })
				return lease, nil
			}
		}}
		firstCtx, stopFirst := context.WithCancel(ctx)
		first := asyncSharedClientAcquire(t, s, firstCtx)
		opening := <-opened
		second := asyncSharedClientAcquire(t, s, ctx)
		synctest.Wait()
		stopFirst()
		if got := <-first; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled first waiter returned %v", got.err)
		}
		if opening.Err() != nil {
			t.Fatal("first local close canceled the other waiter’s authentication")
		}
		close(proceed)
		got := <-second
		if got.err != nil || got.id != 2 || got.lease == nil {
			t.Fatalf("second waiter did not acquire the same generation: id=%d error=%v", got.id, got.err)
		}
		got.release()
		synctest.Wait()
		if s.current != nil {
			t.Fatal("final local close retained its carrier")
		}
	})
}

func TestUDPPreviewSharedClientFailureWakesWaitersAndReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		failed := errors.New("fixture authentication failure")
		failNow := make(chan struct{})
		var calls atomic.Int32
		s := &pilotSharedClient{ctx: ctx, open: func(ctx context.Context) (*pilotClientLease, error) {
			if calls.Add(1) == 1 {
				<-failNow
				return nil, failed
			}
			lease, remote := sharedClientTestLease(ctx)
			context.AfterFunc(ctx, func() { _ = remote.Close() })
			return lease, nil
		}}
		var waiting []<-chan sharedClientResult
		for range 8 {
			waiting = append(waiting, asyncSharedClientAcquire(t, s, ctx))
		}
		synctest.Wait()
		close(failNow)
		for _, waiter := range waiting {
			if got := <-waiter; got.err != failed {
				t.Fatalf("shared authentication error did not reach a waiter: %v", got.err)
			}
		}
		got := <-asyncSharedClientAcquire(t, s, ctx)
		if got.err != nil || got.id != 1 || got.lease == nil || calls.Load() != 2 {
			t.Fatalf("failed shared generation did not reopen: id=%d error=%v opens=%d", got.id, got.err, calls.Load())
		}
		got.release()
	})
}

func TestUDPPreviewSharedClientExpiredLeaseClosesAllLocalControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		expirations := make(chan context.CancelFunc, 2)
		s := &pilotSharedClient{ctx: ctx, open: func(ctx context.Context) (*pilotClientLease, error) {
			ctx, expire := context.WithCancel(ctx)
			expirations <- expire
			lease, remote := sharedClientTestLease(ctx)
			context.AfterFunc(ctx, func() { _ = remote.Close() })
			return lease, nil
		}}
		var controls []net.Conn
		var releases []func()
		var first *pilotClientLease
		for range 6 {
			local, peer := net.Pipe()
			defer local.Close()
			defer peer.Close()
			lease, _, release, _, err := s.acquire(ctx, local)
			if err != nil || first != nil && lease != first {
				t.Fatalf("active sockets did not share a carrier: %v", err)
			}
			first = lease
			controls = append(controls, peer)
			releases = append(releases, release)
		}
		(<-expirations)()
		synctest.Wait()
		for _, peer := range controls {
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("dead carrier did not close a local socket: %v", err)
			}
		}
		next := <-asyncSharedClientAcquire(t, s, ctx)
		if next.err != nil || next.id != 1 || next.lease == first {
			t.Fatalf("expired generation did not reopen: id=%d error=%v", next.id, next.err)
		}
		for _, release := range releases {
			release()
		}
		if next.lease.ctx.Err() != nil {
			t.Fatal("old local teardown reached the new carrier")
		}
		next.release()
	})
}

func TestUDPPreviewSharedClientBoundsAdmissionAndWritesCloseIDs(t *testing.T) {
	for _, limit := range []int{16, 32, 64} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			checkSharedClientBounds(t, limit)
		})
	}
}

func checkSharedClientBounds(t *testing.T, limit int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var upstream net.Conn
		var refusals atomic.Int32
		s := &pilotSharedClient{
			ctx: ctx, maxAssociations: limit, open: func(ctx context.Context) (*pilotClientLease, error) {
				lease, remote := sharedClientTestLease(ctx)
				upstream = remote
				return lease, nil
			}, onLimit: func() { refusals.Add(1) },
		}
		var links []sharedClientResult
		for i := range limit {
			link := <-asyncSharedClientAcquire(t, s, ctx)
			if link.err != nil || link.id != uint32(i+1) {
				t.Fatalf("association %d received id %d and error %v", i, link.id, link.err)
			}
			links = append(links, link)
		}
		defer upstream.Close()
		if got := <-asyncSharedClientAcquire(t, s, ctx); got.err == nil || refusals.Load() != 1 {
			t.Fatalf("more than %d local associations reached the shared lease", limit)
		}
		for _, i := range []int{0, 3, 5} {
			// No remote reader yet. Closing a local association cannot wait
			// for the lease control stream's pipe or provider.
			links[i].release()
		}
		for _, want := range []uint32{1, 4, 6} {
			var frame [4]byte
			if _, err := io.ReadFull(upstream, frame[:]); err != nil || binary.BigEndian.Uint32(frame[:]) != want {
				t.Fatalf("shared close stream did not emit id %d: %v", want, err)
			}
		}
		synctest.Wait()
		if got := <-asyncSharedClientAcquire(t, s, ctx); got.err != nil || got.id != uint32(limit+1) || got.lease != links[1].lease {
			t.Fatalf("churn reused an ID or opened a second carrier: id=%d error=%v", got.id, got.err)
		} else {
			got.release()
		}
		for _, link := range links {
			link.release()
		}
		synctest.Wait()
		if s.current != nil {
			t.Fatal("queued close frames blocked final teardown")
		}
	})
}

func TestUDPPreviewSharedClientKeepsCloseSlotsUntilWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 64
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var upstream net.Conn
		s := &pilotSharedClient{
			ctx: ctx, maxAssociations: limit, open: func(ctx context.Context) (*pilotClientLease, error) {
				lease, remote := sharedClientTestLease(ctx)
				upstream = remote
				context.AfterFunc(ctx, func() { _ = remote.Close() })
				return lease, nil
			},
		}
		var links []sharedClientResult
		for range limit {
			link := <-asyncSharedClientAcquire(t, s, ctx)
			if link.err != nil {
				t.Fatal(link.err)
			}
			links = append(links, link)
		}
		defer upstream.Close()

		for _, link := range links[1:] {
			link.release()
		}
		// The writer can dequeue but cannot finish a write until the peer
		// reads. Repeated churn must not borrow any of those slots.
		synctest.Wait()
		for range 2 * limit {
			if result := <-asyncSharedClientAcquire(t, s, ctx); result.err == nil {
				result.release()
				t.Fatal("admitted a new association before its prior CLOSE was written")
			}
		}
		if links[0].lease.ctx.Err() != nil {
			t.Fatal("a full carrier closed its established flow")
		}
		for _, want := range links[1:] {
			var id [4]byte
			if _, err := io.ReadFull(upstream, id[:]); err != nil || binary.BigEndian.Uint32(id[:]) != want.id {
				t.Fatalf("missing or out-of-order CLOSE for %d: %v", want.id, err)
			}
		}
		synctest.Wait()
		replacement := <-asyncSharedClientAcquire(t, s, ctx)
		if replacement.err != nil || replacement.lease != links[0].lease || replacement.id != limit+1 {
			t.Fatalf("draining CLOSEs did not free slots in the same carrier: %+v", replacement)
		}
		replacement.release()
		links[0].release()
		synctest.Wait()
		if s.current != nil {
			t.Fatal("CLOSE ownership prevented final teardown")
		}
	})
}

func TestUDPPreviewSharedClientBothFrontendsIsolateAndReopen(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	origin, target := stableUDPTestPeer(t)
	type seenPacket struct {
		body   string
		source netip.AddrPort
	}
	seen := make(chan seenPacket, 16)
	go func() {
		buf := make([]byte, 512)
		for {
			n, source, err := origin.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			seen <- seenPacket{string(buf[:n]), source}
			_, _ = origin.WriteToUDPAddrPort(buf[:n], source)
		}
	}()
	meta := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
	client, exit, socksAddress := startUDPPreviewTestPeers(t, ctx, "matching shared peers", target, NewMemoryStore(), meta)
	type association struct {
		control net.Conn
		udp     *net.UDPConn
	}
	open := func(command byte) association {
		control, udp := openPreviewAssociation(t, socksAddress, command)
		t.Cleanup(func() {
			_ = control.Close()
			if udp != nil {
				_ = udp.Close()
			}
		})
		return association{control, udp}
	}
	checkEcho := func(a association, want string) {
		t.Helper()
		_ = a.control.SetReadDeadline(time.Now().Add(5 * time.Second))
		if a.udp != nil {
			_ = a.udp.SetReadDeadline(time.Now().Add(5 * time.Second))
		}
		got, err := readPreviewPacket(a.control, a.udp)
		if err != nil || string(got.Payload) != want || got.Port != int(target.Port()) {
			t.Fatalf("wrong association reply: %q versus %q, error=%v", got.Payload, want, err)
		}
	}
	checkOrigin := func(want string) netip.AddrPort {
		t.Helper()
		select {
		case got := <-seen:
			if got.body != want {
				t.Fatalf("origin did not see %q: got %q", want, got.body)
			}
			return got.source
		case <-ctx.Done():
			t.Fatal("owned origin did not receive a packet")
			return netip.AddrPort{}
		}
	}
	const count = 8
	var links [count]association
	var labels [count]string
	for i := range count {
		command := byte(socksCommandUDPAssociate)
		if i%2 != 0 {
			command = socksCommandUDPInTCP
		}
		links[i] = open(command)
		labels[i] = fmt.Sprintf("fixture %d", i)
		writePreviewPacket(t, links[i].control, links[i].udp, target.Addr().String(), int(target.Port()), []byte(labels[i]))
	}
	for i, link := range links {
		checkEcho(link, labels[i])
	}
	sources := make(map[string]netip.AddrPort)
	unique := make(map[netip.AddrPort]bool)
	for range count {
		event := <-seen
		sources[event.body] = event.source
		unique[event.source] = true
	}
	if len(sources) != count || len(unique) != count || len(client.udpPreview.slots) != 1 || len(exit.udpPreview.slots) != 1 {
		t.Fatalf("eight associations did not share one carrier with unique source ports: sources=%d client_leases=%d exit_leases=%d", len(unique), len(client.udpPreview.slots), len(exit.udpPreview.slots))
	}
	_ = links[0].control.Close()
	_ = links[1].control.Close()
	writePreviewPacket(t, links[2].control, links[2].udp, target.Addr().String(), int(target.Port()), []byte("survivor"))
	checkEcho(links[2], "survivor")
	if got := checkOrigin("survivor"); got != sources[labels[2]] {
		t.Fatal("closing neighbors changed the surviving source endpoint")
	}
	churn := open(socksCommandUDPInTCP)
	writePreviewPacket(t, churn.control, churn.udp, target.Addr().String(), int(target.Port()), []byte("replacement"))
	checkEcho(churn, "replacement")
	if got := checkOrigin("replacement"); got == sources[labels[2]] {
		t.Fatal("replacement used a still active association's source endpoint")
	}
	for _, link := range links {
		_ = link.control.Close()
	}
	_ = churn.control.Close()
	for deadline := time.Now().Add(5 * time.Second); len(client.udpPreview.slots) != 0 || len(exit.udpPreview.slots) != 0; {
		if time.Now().After(deadline) {
			t.Fatal("final local close did not release both carriers")
		}
		time.Sleep(time.Millisecond)
	}
	reopened := open(socksCommandUDPAssociate)
	writePreviewPacket(t, reopened.control, reopened.udp, target.Addr().String(), int(target.Port()), []byte("new generation"))
	checkEcho(reopened, "new generation")
	checkOrigin("new generation")
}

type sharedClientResult struct {
	lease   *pilotClientLease
	id      uint32
	release func()
	err     error
}

func asyncSharedClientAcquire(t *testing.T, client *pilotSharedClient, ctx context.Context) <-chan sharedClientResult {
	t.Helper()
	local, peer := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = peer.Close() })
	result := make(chan sharedClientResult, 1)
	go func() {
		lease, id, release, _, err := client.acquire(ctx, local)
		result <- sharedClientResult{lease, id, release, err}
	}()
	return result
}

func sharedClientTestLease(ctx context.Context) (*pilotClientLease, net.Conn) {
	local, remote := net.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = local.Close() })
	lease := &pilotClientLease{ctx: ctx, control: local, finish: func(string) { stop(); _ = local.Close() }}
	return lease, remote
}
