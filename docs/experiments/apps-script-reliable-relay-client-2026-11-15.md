# Reliable relay client research, 2026-11-15

This is a test-only alternative carrier. It does not change a shipped Skirk
transport, its Google Drive grant, or the user's route setting. No live result
or performance improvement is established by this document.

The research client reaches an owned relay only through an Apps Script web app
using the existing pinned `www.google.com` HTTP transport. The research exchange
requires `google_front_pinned` and a parsed pinned IP. It explicitly follows at
most six redirects between `script.google.com` and
`script.googleusercontent.com`; it rejects a redirect to the owned relay or any
other host. The product's Direct Google option remains separate and unchanged.

## Delivery and processing ownership

Two session-owned pumps serialize independent UP and PULL sequence spaces. Each
request authenticates its channel, sequence, previous-response receipt, and
fresh session epoch inside AEAD. A transport retry reuses the exact sealed
request. A response must match the channel and sequence before any local state
advances. The server retains an identical sealed response for an ambiguous
retry; an older RPC below its receipt watermark is rejected.

UP object identity binds its name and complete ciphertext hash. A final
`Committed` ID means the complete object passed hash validation, any configured
inner mux validation, and publication. Partial fragment acceptance has a
different meaning and cannot complete the caller's upload. Object identity
claims survive payload deletion on both sides so a later RPC carrying the same
OPEN cannot publish it again. This protects a running research session; it is
not an exactly-once guarantee across process restarts or arbitrary application
retries. A fresh process must use a fresh epoch.

PULL receipts release only cached RPC responses. DOWN ciphertext remains owned
by the receiver until the mux's processing/deferred-cleanup path calls
`DeleteID`. That deletion travels on UP and releases the server payload only
after the complete object has been sent. A lost Delete reply can temporarily
make the client retain more old data than the server; the PULL pump therefore
waits on local capacity without receipting its new response while UP recovers
the exact lost reply.

## Bounded progress

Default fragment size is 768 KiB, configurable from 128 KiB through 4 MiB.
Complete sealed objects are at most 8 MiB. Future live mux configurations must
leave room for envelope overhead below that object bound. The local full-mux
test uses 256 KiB chunks.

Each direction defaults to 64 MiB retained payload and 4,096 immutable identity
claims. Claims are not silently evicted. Exhausting the identity ledger stops
new admissions; this is a bounded experiment lifetime, not an always-running
product design. Serialization buffers, one in-flight fragment per channel,
and returned caller copies are additional bounded working memory.

The UP scheduler rotates between fragments, selects priority traffic first,
and gives a waiting normal fragment a turn after four priority fragments.
Deletes take precedence in their own payload-free request. The broker returns
a cached, no-mutation `Busy` response when a new UP object cannot reserve its
full size, so a full UP buffer cannot monopolize the channel needed to delete
DOWN. The client advances its RPC sequence, rotates the same immutable object,
and retries with bounded pacing.

This does not reserve memory for priority admission: fully queued normal data
can still delay a small priority object before scheduling. A priority-headroom
variant needs consistent admission limits and saturation measurements on both
sides. Fragment scheduling alone does not prove latency isolation.

Caller cancellation does not discard a session-owned ambiguous publication.
Session cancellation terminates both pumps, wakes admission/processing waiters,
and joins exchanges before freeing their retained buffers. The Google carrier
reserves eight internal HTTP/2 attempts per visible dispatch, plus bounded
request and response bodies before dispatch. It never refunds the response
upper bound after an opaque partial-body error. These are conservative upper
reservations, not observations of accepted/billed Google requests or raw TLS
wire bytes. HTTP 429/5xx retries honor a bounded 1–30 second delay.

## Offline verification plan and status

Source has been written for the following deterministic tests. It has **not yet
been compiled or executed** because the parent experiment holds a CPU-quiet
live measurement window. Replace this status only after actual execution.

- Lost committed UP reply: exact retry, one publication, deletion followed by a
  fresh-RPC duplicate, and rejection of a receipted old RPC.
- Lost PULL reply: contiguous reassembly and unchanged server ciphertext until
  explicit mux deletion.
- Lost Delete reply at the full cache bound: new DOWN waits and then resumes
  after the independent UP pump recovers the deletion response.
- Full duplex payload pressure: Busy UP permits DOWN deletion, then resumes UP
  after exit processing frees capacity.
- Admission cancellation and Close: no second allocation above the bound,
  pending callers released, and both exchange workers joined.
- Fragment gaps, changed overlaps, impossible bounds, and unsolicited commit
  IDs rejected locally.
- Actual sealed mux OPEN: destination dial and initial bytes occur once, then
  remain once after payload deletion, forced mux closed-claim expiry, and a
  duplicate delivered at a new authenticated RPC sequence.
- Actual local SOCKS and production mux: exact 16 MiB GET and POST with hashes,
  17 exact text replies, and committed-UP/PULL reply loss. These are loopback
  correctness tests, not Google speed or regional availability measurements.
- Carrier route/redirect bounds, foreign redirects, auth reflection, and
  pre-dispatch budget exhaustion rejected without escaping the selected front.

No Google grant, deployment, or live request is performed by these offline
tests. Live evaluation requires a separate exclusive measurement lease, bounded
free-tier budgets, and the concrete Apps Script authorization handled by the
parent task. Free-tier quotas and server requirements must be reported before
any capacity or 200,000-user claim.
