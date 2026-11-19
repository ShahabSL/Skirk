package skirk

// UDPPreviewMetrics contains counters without destination or identity data.
type UDPPreviewMetrics struct {
	Shared                    bool                     `json:"shared"`
	ActiveCarriers            int                      `json:"active_carriers"`
	ActiveFrontends           int                      `json:"active_frontends"`
	FrontendLimit             int                      `json:"frontend_limit"`
	AdmissionLimitHits        uint64                   `json:"admission_limit_hits"`
	ExitCarrierLimit          int                      `json:"exit_carrier_limit"`
	ExitCarrierLimitHits      uint64                   `json:"exit_carrier_limit_hits"`
	RemoteExitLimitHits       uint64                   `json:"remote_exit_limit_hits,omitempty"`
	LocalFrontendLimitHits    uint64                   `json:"local_frontend_limit_hits"`
	LocalAssociationLimitHits uint64                   `json:"local_association_limit_hits"`
	ClientCarrierLimitHits    uint64                   `json:"client_carrier_limit_hits"`
	Packets                   UDPPreviewPacketCounters `json:"packets"`
}

// These process totals survive carrier replacement. Counters describe separate
// stages and may overlap; they must not be added together to infer packet loss.
type UDPPreviewPacketCounters struct {
	Offered                uint64 `json:"offered"`
	Published              uint64 `json:"published"`
	Accepted               uint64 `json:"accepted"`
	OriginSent             uint64 `json:"origin_sent"`
	OriginReceived         uint64 `json:"origin_received"`
	Expired                uint64 `json:"expired"`
	LocalQueueExpired      uint64 `json:"local_queue_expired"`
	IncomingCertainlyStale uint64 `json:"incoming_certainly_stale"`
	InvalidTiming          uint64 `json:"invalid_timing"`
	Dropped                uint64 `json:"dropped"`
	IngressDropped         uint64 `json:"ingress_dropped"`
	UploadAdmissionDropped uint64 `json:"upload_admission_dropped"`
	LocalReplyDropped      uint64 `json:"local_reply_dropped"`
	Rejected               uint64 `json:"rejected"`
	FlowLimitHits          uint64 `json:"flow_limit_hits"`
	ProviderErrors         uint64 `json:"provider_errors"`
	OriginDialErrors       uint64 `json:"origin_dial_errors"`
	OriginWriteErrors      uint64 `json:"origin_write_errors"`
	LocalWriteErrors       uint64 `json:"local_write_errors"`
}

func (c *UDPPreviewPacketCounters) addMailbox(m *pilotMailbox) {
	c.Offered += m.offered.Load()
	c.Published += m.published.Load()
	c.Accepted += m.delivered.Load()
	c.OriginSent += m.originSent.Load()
	c.OriginReceived += m.originReceived.Load()
	c.Expired += m.stale.Load()
	c.LocalQueueExpired += m.localQueueExpired.Load()
	c.IncomingCertainlyStale += m.incomingCertainlyStale.Load()
	c.InvalidTiming += m.invalidTiming.Load()
	c.Dropped += m.dropped.Load()
	c.IngressDropped += m.ingressDropped.Load()
	c.UploadAdmissionDropped += m.admissionDropped.Load()
	c.LocalReplyDropped += m.hevReplyDropped.Load()
	c.Rejected += m.rejected.Load()
	c.FlowLimitHits += m.flowCap.Load()
	c.ProviderErrors += m.errors.Load()
	c.OriginDialErrors += m.originDialErrors.Load()
	c.OriginWriteErrors += m.originWriteErrors.Load()
	c.LocalWriteErrors += m.hevWriteErrors.Load()
}

func (p *datagramPreview) registerMetrics(m *pilotMailbox) {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	if p.metricsMailboxes == nil {
		p.metricsMailboxes = make(map[*pilotMailbox]struct{})
	}
	p.metricsMailboxes[m] = struct{}{}
}

// Retire only after workers, origin readers and attached local frontends stop.
func (p *datagramPreview) retireMetrics(m *pilotMailbox) {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	if _, ok := p.metricsMailboxes[m]; ok {
		p.retiredMetrics.addMailbox(m)
		delete(p.metricsMailboxes, m)
	}
}

func (t *Tunnel) UDPPreviewSnapshot() *UDPPreviewMetrics {
	if t == nil || t.udpPreview == nil {
		return nil
	}
	p := t.udpPreview
	p.frontendMu.Lock()
	frontends := p.frontendCount
	p.frontendMu.Unlock()
	p.metricsMu.RLock()
	defer p.metricsMu.RUnlock()
	packets := p.retiredMetrics
	for m := range p.metricsMailboxes {
		packets.addMailbox(m)
	}
	exitHits := p.exitCarrierLimitHits.Load()
	frontendHits := p.frontendLimitHits.Load()
	associationHits := p.associationLimitHits.Load()
	clientHits := p.clientCarrierLimitHits.Load()
	return &UDPPreviewMetrics{
		Shared:                    p.sharedUDP,
		ActiveCarriers:            len(p.slots) + len(p.exitSlots),
		ActiveFrontends:           frontends,
		FrontendLimit:             p.frontendLimit(),
		AdmissionLimitHits:        exitHits + frontendHits + associationHits + clientHits,
		ExitCarrierLimit:          cap(p.exitCarrierSlots()),
		ExitCarrierLimitHits:      exitHits,
		RemoteExitLimitHits:       p.remoteExitLimitHits.Load(),
		LocalFrontendLimitHits:    frontendHits,
		LocalAssociationLimitHits: associationHits,
		ClientCarrierLimitHits:    clientHits,
		Packets:                   packets,
	}
}
