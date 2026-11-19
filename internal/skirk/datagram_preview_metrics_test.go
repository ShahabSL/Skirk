package skirk

import (
	"sync"
	"testing"
)

func TestUDPPreviewMetricsPreserveTotalsAcrossCarrierReplacement(t *testing.T) {
	p := &datagramPreview{sharedUDP: true, slots: make(chan struct{}, 4)}
	tunnel := &Tunnel{udpPreview: p}
	var readers sync.WaitGroup
	done := make(chan struct{})
	readers.Go(func() {
		var previous uint64
		for {
			current := tunnel.UDPPreviewSnapshot().Packets.Offered
			if current < previous {
				t.Errorf("packet totals decreased from %d to %d", previous, current)
				return
			}
			previous = current
			select {
			case <-done:
				return
			default:
			}
		}
	})
	for range 100 {
		m := &pilotMailbox{}
		p.registerMetrics(m)
		for range 10 {
			m.offered.Add(1)
			m.originReceived.Add(1)
		}
		m.ingressDropped.Add(2)
		p.retireMetrics(m)
		p.retireMetrics(m) // Repeated cleanup must not double count.
	}
	close(done)
	readers.Wait()
	c := tunnel.UDPPreviewSnapshot().Packets
	if c.Offered != 1000 || c.OriginReceived != 1000 || c.IngressDropped != 200 || len(p.metricsMailboxes) != 0 {
		t.Fatalf("unexpected retired totals or retained mailboxes: %+v (%d)", c, len(p.metricsMailboxes))
	}
	if (&Tunnel{}).UDPPreviewSnapshot() != nil {
		t.Fatal("disabled UDP advertised metrics")
	}
}
