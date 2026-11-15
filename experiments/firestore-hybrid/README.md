# Firestore priority / Drive bulk research design

Status: an offline, test-only prototype is implemented and component-tested.
No live Google hybrid transport has run and no new Google resources or
credentials have been provisioned for it. Primitive
Firestore write/read and six-state WebChannel results do not establish this
transport's correctness or performance.

The prototype carries all small priority mux objects, including OPEN and RST,
through two fresh Firestore documents, one single-writer document per direction.
Drive remains the bulk transport and the bounded priority fallback. A watch
event is a notification of current state, never a durable packet. Every document
contains a cumulative authenticated queue of immutable mux name/ciphertext
identities retained until the optional mux processing-receipt callback
acknowledges bounded ownership. Physical deletion is a separate deferred action.

This is research authorization only: the secondary researcher's owner token is
kept in a private test directory and cannot enter product profiles. The existing
`drive.file` grant does not authorize Firestore. Production least-privilege
authentication, onboarding and abuse isolation remain unresolved. No billing,
IAM, rules, or free quotas are changed.

## Carrier contract

The Go store depends on a small interface so fault tests do not require Google:

```go
type fsPriorityDocument struct {
    Ciphertext []byte
    UpdateTime string
}
type fsPriorityCarrier interface {
    WriteOwn(context.Context, string, []byte) (fsPriorityDocument, error)
    ReadOwn(context.Context) (fsPriorityDocument, error)
    ReadPeer(context.Context) (fsPriorityDocument, error)
    WatchPeer(context.Context, func(fsPriorityDocument) error) error
    Close() error
}
```

`WriteOwn` requires the current server-provided updateTime as a precondition.
The writer has at most one prepared ciphertext and one in-flight write. A lost
write response is followed by an exact own-document readback. It can retry only
the identical prepared ciphertext with the identical precondition; a different
snapshot cannot be published until the earlier result is resolved. An unknown
state stops that generation's writer and hands pending identities to bounded
Drive fallback. Conditional writes cannot recreate a document after deletion.
Initial document creation belongs to the external fixture, before measurement;
an ambiguous creation requires cleanup of the whole fresh owned database.

The live carrier's browser SDK has only an authenticated loopback Go bridge.
Every forwarded API request and every REST write/read uses a distinct bounded
small-state Google HTTP pool with the selected pinned `www.google.com` front.
There is no native SDK network primitive or direct-origin fallback. The Drive
pool is separate. Dispatch/replay, bytes, responses, writes, watch events and
cleanup reserves are counted independently.

## Snapshot and ownership

An AEAD envelope binds a fresh process epoch, session, client/run namespace,
direction and monotonic document revision. Records include their sequence,
immutable object name, sealed mux payload and monotonic byte offset. A snapshot
also carries cumulative peer-processing ACK and receive-credit watermarks.

The initial prototype will bound snapshots to 64 KiB, individual inline sealed
objects to 16 KiB, queue slots to 32, per-stream pending records to four, retained
payload memory to 8 MiB per direction and lifetime identity claims to 4,096.
Physical priority fallback objects are bounded to 4 MiB plus 64 KiB of envelope
headroom, matching the research fixture's maximum four-MiB mux chunk. Larger
objects are rejected rather than silently bypassing the replay claims.
These are experimental bounds, not claimed optimal values. Priority objects
above the inline bound but within the physical bound go directly to Drive. A
full queue or exhausted receive credit cannot
block bulk traffic; it takes the bounded same-identity Drive fallback instead.

Receive credit is a monotonic sequence/byte watermark, not a repeatedly reusable
free-slot count. Preparing a credit advertisement reserves that grant before the
write can become visible. The peer's cumulative offered positions debit the
watermark even if multiple watch notifications coalesce. Admission checks the
entire authenticated snapshot before publishing any new receive entry.

The receiver stores one lifetime name/hash claim shared by its virtual Firestore
entry and any physical Drive fallback. A different ciphertext under the same
name, one ciphertext renamed into another name, or one physical ID reassigned
to another identity is an integrity failure. The optional `NoteMuxProcessed`
callback runs after successful mux processing and `markSeenProcessed`, before
physical deletion. It records no receipt on failed/canceled processing and does
no network I/O. Receipt errors never re-execute already accepted bytes. This
means bounded mux ownership, not a durable destination/application receipt.
Out-of-order processing advances the cumulative ACK only
through the contiguous completed prefix. Payload bytes can be released after
processing, but tombstones remain until the fresh bounded epoch ends; replay
after mux's closed-stream TTL cannot redial an OPEN. Hitting the lifetime claim
cap closes the finite research session without evicting claims. Restart requires
a fresh epoch and mux RunID; rotating just the document while old fallback files
remain in the same namespace is forbidden. There is no cross-restart durable
exactly-once claim. Outstanding advertised receive credit reserves up to 32
future identity claims, so physical-only admission can close conservatively
before 4,096 actual received objects. Reserved claims are not silently stolen.

The sender returns after publication, while a bounded owned fallback worker
waits for processing ACK. A missing notification or disconnected watch cannot
leave the application object dependent on an unobserved last event. Reconnect
reads the latest cumulative state; a bounded processing timeout publishes the
original name and exact sealed bytes through Drive. Physical fallback processing
must update the same receive claim, including when it precedes the first watch
notification. Every publication, retry and fallback is counted.

Drive fallback priority discovery has a separate owned pump. The mux's p0 list
call returns cached virtual identities and a coalesced wake immediately, because
the core polls p0 and p1 sequentially. The pump retains a complete bounded page
tail until its 64-slot/two-worker prefetch queue owns every descriptor, then
advances the backend token. Fetch/authentication/normalization happens before
mux enqueue. Normal p1 discovery and its optional quota, page, adaptive and bulk
pool interfaces remain direct. A configurable bounded interval follows a
discovery cycle; there is no additional always-running Google polling timer.

Store close cancels and joins its workers, carrier and base operations; a base
that ignores cancellation keeps the join pending. It clears payloads only after
joining, retains finite tombstone metadata until GC, and fences late callbacks.
The outer fixture owns the actual mux/workload context: it must observe each
store's `Done()` signal, cancel the whole workload on an epoch failure/expiry,
and join that observer. Canceling adapter discovery alone is not proof that a
separately created mux or application stopped. Normal fixture teardown cancels
the workload before closing either store and records every retained error.
Provider deletion belongs to the external owned fixture, using exact cleanup
targets after the join, a global Drive hard guard with reserved cleanup quota,
and delayed verification. A 30-second context alone is not a quota or cleanup
proof; Store close makes no unbudgeted provider deletion calls.
`CleanupTargets` covers generated priority fallback IDs and authenticated
priority aliases only. The unchanged normal p1 path requires the outer fixture
to scan both exact fresh run prefixes as well, after every mux/store writer has
joined, then repeat the bounded absence check after the cleanup delay.

ACK changes can be piggybacked on outbound data; bounded ACK-only scheduling is
measured explicitly. Receiving an ACK or credit-only snapshot does not itself
schedule another ACK-only write. This prevents ACK-of-ACK loops. A blocked head
can delay cumulative retirement of later records; bounded fallback preserves
progress, and this remains a latency limitation to measure rather than hide.

## Offline proof before any live run

Counter-controlled tests must cover cumulative coalescing/lost notifications,
exact lost-write readback, old-state readback and conditional retry, late writes
after deletion, same-name ciphertext conflicts, stale revisions/generation,
processing ACK order, no ACK-before-processing, no ACK-of-ACK loop, receive
credit saturation/replay, queue/memory/claim bounds and cancellation joins.

Fallback tests must process the physical copy before a delayed watch OPEN,
expire mux's closed-stream cache, replay again, and prove exactly one outbound
dial and application write. They must also cover watch-first processing,
duplicate physical/virtual copies, fallback failures followed by a late valid
processing ACK, reconnect to the latest snapshot and unrelated bulk progress.

The prerequisite mixed proof is the actual SOCKS/mux path with independent
random 16 MiB GET and POST plus at least 20 concurrent small messages, exact
hash/count checks, artificial notification/write/ACK loss, bounded cleanup and
zero retained workers/payloads. Local fake-carrier timings are not Google
performance. The real pinned browser SDK/loopback bridge must be exercised
separately and then in the controlled owned live fixture, never replaced by a
native/direct path.

Current component evidence (2026-11-15; not a live transport result):

- The isolated carrier at `22d9f06` passed its full Go suite, vet, targeted
  real pinned SDK/loopback REST/reconnect/cancel cases and repeated race checks.
  Its 15 Node cases include natural child exit and 16 seconds of empty-poll
  continuity. The prototype now integrates that carrier unchanged.
- Combined deterministic store cases passed three times under the race
  detector. They exercise exact committed-write readback, byte-identical
  conditional retry, hard failures that cannot be laundered by another write,
  lost notifications with immutable physical-first fallback, initial-only empty
  bootstrap, reconnect versus authorization failure, late conditional writes
  after document deletion, and close waiting for an unresponsive writer.
- An 80-object first page plus a nonempty next-page token and one more object
  proves page-tail retention beyond the old 16-object boundary. With both
  readers blocked and all 66 admission slots occupied, the actual sequential
  mux poll still executes normal discovery; after release all 81 identities are
  dispatched once. This is a counter-controlled offline proof, not a latency
  benchmark.
- The actual full mixed SOCKS proof and OPEN replay/dial proof are still being
  built. These component results do not authorize a live hybrid performance or
  production-security claim.

## Official API references

- [Firestore REST PATCH](https://firebase.google.com/docs/firestore/reference/rest/v1/projects.databases.documents/patch)
  supports an explicit currentDocument condition and returns the updated
  document; Firestore uses datastore or cloud-platform authorization.
- [Preconditions](https://firebase.google.com/docs/firestore/reference/rest/v1/Precondition)
  require existence and the exact supplied last-update timestamp when updateTime
  is used. Server timestamp strings are kept intact, without floating-point
  conversion.
- [Listen RPC](https://firebase.google.com/docs/firestore/reference/rpc/google.firestore.v1#firestore)
  and the pinned browser SDK transport used by the preceding Listen probe supply
  the WebChannel path and add/remove target behavior.
- [Free quotas](https://firebase.google.com/docs/firestore/quotas) are finite,
  including 20,000 document writes/day and 10 GiB outbound transfer/month for the
  eligible free database. No centralized 200,000-user capacity claim follows.

Context7's official Node client precondition documentation was consulted, then
the official REST reference above for the exact query contract. This design
requires no new service dependency in any product build.
