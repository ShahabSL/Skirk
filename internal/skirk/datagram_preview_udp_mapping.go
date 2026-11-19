package skirk

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"syscall"
	"time"
)

const udpPreviewStableIdle = 120 * time.Second

const udpPreviewReadRetryMin = 5 * time.Millisecond
const udpPreviewReadRetryMax = 100 * time.Millisecond

type pilotUDPReader interface {
	ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error)
	Close() error
}

type pilotUDPSocket interface {
	pilotUDPReader
	WriteToUDPAddrPort([]byte, netip.AddrPort) (int, error)
	SetWriteDeadline(time.Time) error
}

// pilotUDPMapping owns one exit-side source port per family for a lease.
// Locally sent packets admit a bounded set of exact remote addresses and ports
// for replies, while the source port is independent of the destination.
type pilotUDPMapping struct {
	socket   pilotUDPSocket
	socket6  pilotUDPSocket
	mailbox  *pilotMailbox
	done     chan struct{}
	activity func()
	offer    func(uint64, netip.AddrPort, []byte)

	mu         sync.Mutex
	byFlow     map[uint64]netip.AddrPort
	byEndpoint map[netip.AddrPort]uint64
}

func newPilotUDPMapping(ctx context.Context, mailbox *pilotMailbox, onFatal func(error)) (*pilotUDPMapping, error) {
	return newPilotUDPMappingWithCallbacks(ctx, mailbox, onFatal, nil, mailbox.offer)
}

func newPilotUDPMappingWithCallbacks(ctx context.Context, mailbox *pilotMailbox, onFatal func(error), activity func(), offer func(uint64, netip.AddrPort, []byte)) (*pilotUDPMapping, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mapping := &pilotUDPMapping{
		mailbox: mailbox, done: make(chan struct{}), activity: activity, offer: offer,
		byFlow: make(map[uint64]netip.AddrPort), byEndpoint: make(map[netip.AddrPort]uint64),
	}
	if mailbox.exitProxy != "" {
		relay, err := dialSOCKSUDPProxy(ctx, mailbox.exitProxy)
		if err != nil {
			mailbox.originDialErrors.Add(1)
			return nil, err
		}
		mapping.socket = relay
		if mailbox.publicIPv6 {
			mapping.socket6 = relay
		}
		mapping.startReaders(ctx, []pilotUDPReader{relay}, onFatal)
		return mapping, nil
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		mailbox.originDialErrors.Add(1)
		return nil, err
	}
	sockets := []pilotUDPReader{socket}
	mapping.socket = socket
	if mailbox.publicIPv6 {
		socket6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
		if err != nil {
			_ = socket.Close()
			mailbox.originDialErrors.Add(1)
			return nil, err
		}
		mapping.socket6 = socket6
		sockets = append(sockets, socket6)
	}
	mapping.startReaders(ctx, sockets, onFatal)
	return mapping, nil
}

func (m *pilotUDPMapping) startReaders(parent context.Context, sockets []pilotUDPReader, onFatal func(error)) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ctx, func() {
		for _, conn := range sockets {
			_ = conn.Close()
		}
	})
	var readers sync.WaitGroup
	var fail sync.Once
	for _, conn := range sockets {
		readers.Go(func() {
			defer conn.Close()
			if err := m.readReplies(ctx, conn); err != nil {
				fail.Do(func() {
					// Stop both families before notifying the lease owner.
					cancel()
					if onFatal != nil {
						onFatal(err)
					}
				})
			}
		})
	}
	go func() {
		defer close(m.done)
		defer cancel()
		defer stop()
		readers.Wait()
	}()
}

func (m *pilotUDPMapping) send(ctx context.Context, packet pilotPacket) {
	if packet.flow == 0 || len(packet.body) > pilotMaxPayload ||
		!permitPreviewTarget(m.mailbox.allowed, m.mailbox.publicIPv4, m.mailbox.publicIPv6, packet.target) {
		m.mailbox.rejected.Add(1)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if !packet.received.IsZero() {
		if reason := m.mailbox.incomingExpiry(packet, time.Now()); reason != pilotPacketFresh {
			m.mailbox.recordExpiry(reason, 1)
			return
		}
	}
	target, knownFlow := m.byFlow[packet.flow]
	flow, knownEndpoint := m.byEndpoint[packet.target]
	if (knownFlow && target != packet.target) || (knownEndpoint && flow != packet.flow) {
		m.mailbox.rejected.Add(1)
		return
	}
	if !knownFlow && len(m.byFlow) >= pilotMaxFlows {
		m.mailbox.flowCap.Add(1)
		return
	}

	// Serialize admission with the write and receive lookup. A response that
	// arrives immediately can then be matched, and a failed send admits no peer.
	socket := m.socket
	if packet.target.Addr().Is6() {
		socket = m.socket6
	}
	_ = socket.SetWriteDeadline(time.Now().Add(pilotLocalWrite))
	n, err := socket.WriteToUDPAddrPort(packet.body, packet.target)
	if err != nil || n != len(packet.body) {
		if ctx.Err() == nil {
			m.mailbox.originWriteErrors.Add(1)
		}
		return
	}
	if !knownFlow {
		m.byFlow[packet.flow] = packet.target
		m.byEndpoint[packet.target] = packet.flow
	}
	m.mailbox.originSent.Add(1)
	if m.activity != nil {
		m.activity()
	}
}

func (m *pilotUDPMapping) readReplies(ctx context.Context, socket pilotUDPReader) error {
	// Read a complete datagram before enforcing the transport MTU.
	// Windows reports a truncated datagram as a socket read error.
	buffer := make([]byte, 64<<10)
	retry := udpPreviewReadRetryMin
	for {
		n, source, err := socket.ReadFromUDPAddrPort(buffer)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if !pilotUDPRecoverableReadError(err) {
				return err
			}
			// Even persistent host/network errors must not spin or refresh
			// the lease. A successful read resets this bounded backoff.
			timer := time.NewTimer(retry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			retry = min(2*retry, udpPreviewReadRetryMax)
			continue
		}
		retry = udpPreviewReadRetryMin
		m.mu.Lock()
		flow, known := m.byEndpoint[source]
		m.mu.Unlock()
		if !known || n > pilotMaxPayload {
			m.mailbox.rejected.Add(1)
			continue
		}
		m.mailbox.originReceived.Add(1)
		if m.activity != nil {
			m.activity()
		}
		// Confirmed mailbox publication refreshes the bidirectional lease. A
		// socket read by itself does not extend its lifetime.
		m.offer(flow, source, buffer[:n])
	}
}

func pilotUDPRecoverableReadError(err error) bool {
	var code syscall.Errno
	if !errors.As(err, &code) {
		return false
	}
	if runtime.GOOS == "windows" {
		// Winsock errors differ from syscall's portable errno constants.
		switch code {
		case 10004, 10035, 10040, 10051, 10052, 10054, 10055, 10061, 10065:
			return true // Interrupted, would-block, datagram/network errors.
		default:
			return false
		}
	}
	switch code {
	case syscall.EINTR, syscall.EAGAIN, syscall.ENOBUFS, syscall.EMSGSIZE,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETRESET,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH:
		return true
	default:
		return false
	}
}
