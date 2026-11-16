# Deferred inbound aggregate budget (16 November 2026)

The receive worker's local handoff had a per-stream deferred tail limit of 512 frames or 8 MiB. Those tails were not charged to the 256 MiB normal receive queue once an object left that queue. With many stalled local sockets, the additional deferred memory therefore scaled with the number of streams.

This change adds a mux-wide deferred tail budget of 8,192 frames or 64 MiB. A stream that cannot reserve space closes and sends the existing best-effort reset; unrelated streams remain able to receive into their own available channels. Popping a deferred frame and closing a stream both return their reservations. The aggregate counters are protected by a small mutex and never wait for a local socket.

Offline validation: targeted tests fill the byte and frame budgets across multiple streams, exceed each cap, reclaim budget by closing a stream, verify unrelated delivery, and race concurrent close with frame receipt. The existing ordering test now checks that draining returns all reservations. These tests establish a bound on **deferred tails**, not an end-to-end throughput or latency gain. The preexisting per-stream 64-frame inbound channels, one in-flight writer frame per stream, and reassembly buffers are outside this budget; a total per-mux local-delivery memory cap would need separate accounting and a scheduling decision.
