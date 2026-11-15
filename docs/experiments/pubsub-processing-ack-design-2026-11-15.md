# Bounded processing-ACK retransmission candidate (design only)

Status: unimplemented, unmeasured. The [retained pool run](pubsub-drive-hybrid-2026-11-15.md)
showed roughly 31-second first-message tails following receiver restart. Canceled
outstanding Pull RPCs retaining a delivery until the broker ACK lease expires is
a hypothesis, not yet proven. Neither that primitive nor the failed first full
hybrid defines a latency wall. The identical Drive control and a deliberate
cancel/reconnect ACK-deadline discriminator come first.

## Proposed state machine

Keep exactly the same prepared mux name and inner sealed batch for retries.
Derive its identity as SHA-256(name, separator, sealed bytes). The sender retains
that immutable record after Pub/Sub accepts publication until an authenticated
peer **processing ACK** arrives. This ACK means the mux authenticated/decoded and
accepted the batch, including any bounded stream reassembly queue; it does not
mean the destination socket consumed or durably stored every byte. Publication,
processing acceptance, broker ACK and TCP destination delivery stay distinct.

Add an explicitly negotiated version-2 outer record with distinct DATA and ACK
kinds. Outer AEAD binds session/client/run/direction/kind; ACK additionally binds
the original DATA direction and exact 32-byte identity. Use domain-separated
DATA/ACK keys and fresh outer nonces. Inner ciphertext is unchanged across
retransmission, preserving the mux's sequence and dedupe identity. Unknown or
unnegotiated peers continue the ordinary Drive carrier; an ambiguous publication
never silently switches carriers.

For the research adapter, a bounded observer can read `mux.processed[name]`
under `seenMu` after `markSeenProcessed` succeeds. `queued`, `seen`, publication
or the presence of an OPEN claim are insufficient evidence. Polling that local
state every 25–50 ms adds no network request. A product implementation should
instead provide a small explicit post-processing callback. Terminally invalid
objects must not receive a success processing ACK. For an explicit terminal
reject, send a separately typed authenticated NACK and fail the affected stream.

Once processing acceptance is observed, retain the nonexpiring processed claim,
release sealed cache bytes, send the peer ACK on the opposite topic and ACK all
known broker deliveries. Later identical DATA redelivery is never reenqueued:
merge/ACK its new broker ACK ID and repeat the peer processing ACK. ACK messages
are themselves broker-ACKed immediately after validation; **ACKs never request
another peer ACK**, preventing loops. A lost peer ACK therefore causes only a
bounded duplicate DATA/ACK exchange. ACKs can coalesce at most 32 identities for
5–15 ms, with a separate reserved sender worker so bulk data cannot block them.

The sender's retry timer is independent of broker lease expiry. Begin with a
conservative 2-second RTO for a cold subscription, then use measured ACK RTT plus
variance, with a proposed 750 ms floor and 5-second ceiling. These starting
values are hypotheses requiring measurement. Retransmit the same prepared
identity at most three additional times, using one bounded worker, randomized
small jitter and the shared global call/byte budget. A first delivery stranded
on a canceled pull can then be bypassed by a new copy while receiver dedupe
prevents repeating an OPEN or application bytes. This gives a bounded recovery
attempt; it cannot guarantee a network latency bound during outages.

## Bounds and lifecycle

- Sender retention: at most 64 priority records / 8 MiB globally, 16 per lane and
  four per stream. Capacity applies backpressure; it never evicts unacknowledged
  records. Publication can complete the existing mux publication ACK while the
  adapter retains bounded ownership for background recovery.
- Retransmit allowance: at most three duplicates per identity, one retry worker,
  a per-peer duplicate-byte bucket (initial experiment: 8 MiB/minute) and the
  existing absolute call/byte ceilings. These are tunable experiment guards,
  not Google quota claims. Report original and duplicate bytes separately.
- Receiver: retain processed identities through the whole bounded run and at
  least configured broker retention plus lease allowance in a production design.
  The current 4,096-claim capacity fails closed; never clear OPEN dedupe merely
  because the mux's two-minute closed map expires.
- An ACK retires only an existing exact pending identity for the authenticated
  peer/session/direction. Unknown ACKs cannot free arbitrary credits or advance
  a cumulative sequence. If a batching ACK is used, each identity is checked.
- Stop cancels and joins retry, ACK, observer and pull workers before resource
  deletion. A shared owner holds each retained record until exactly one of peer
  processing ACK, authenticated terminal reject, retry exhaustion or cancellation.
- Across process restart, existing 30-second OPEN catch-up still needs durable
  anti-replay/session-generation treatment. This candidate does not claim
  exactly-once external TCP effects or durable delivery.

## Required falsification before a live experiment

Deterministically strand the first DATA on a simulated canceled pull, deliver the
retry immediately, and assert one destination dial/initial-byte write. Independently
lose the first peer ACK, duplicate/reorder both DATA and ACK, delay processing
past several retry ticks, race cancellation against ACK and publication, fill
all byte/record capacities, and inject foreign-session/direction/corrupted ACKs.
Verify no ACK loops, no success ACK on decode failure, unchanged inner bytes,
exactly one ownership release, bounded duplicate API calls and zero workers after
stop. Expire the mux closed-stream map while retaining the adapter tombstone and
redeliver OPEN. A peer that never ACKs must exhaust a finite retry budget and
close explicitly rather than wait indefinitely or overwrite an unprocessed slot.

Then compare unchanged broker ACK deadlines (30 seconds) with this peer-ACK
candidate after a deliberate idle/cancel transition, retaining every cold and
warm sample. Compare a separate 10-second broker lease / 3-second extension
variant and a longer Pull/HTTP timeout only as distinct conditions. Finally run
same-binary mixed SOCKS text plus exact 16 MiB down/up against the ordinary Drive
control. A faster primitive without complete bulk and cleanup correctness is
insufficient to promote the architecture.
