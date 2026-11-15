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

Reserve-zero baseline admission does not protect memory for priority traffic:
fully queued normal data can delay a small priority object before scheduling.
An opt-in variant adds explicit `PriorityReserveBytes` and
`PriorityReserveObjects` on both client and broker, in both directions. Normal
objects cannot consume those bytes or immutable identity claims. The planned
full-mux variant uses 1 MiB and 16 claims within the same 64 MiB/4,096 totals;
these are research settings and do not change product defaults. Fragment
scheduling alone does not prove latency isolation or prevent a priority queue
from filling its own reserved capacity.

Caller cancellation does not discard a session-owned ambiguous publication.
Session cancellation terminates both pumps, wakes admission/processing waiters,
and joins exchanges before freeing their retained buffers. The Google carrier
reserves eight internal HTTP/2 attempts per visible dispatch, plus bounded
request and response bodies before dispatch. It never refunds the response
upper bound after an opaque partial-body error. These are conservative upper
reservations, not observations of accepted/billed Google requests or raw TLS
wire bytes. HTTP 429/5xx retries honor a bounded 1–30 second delay.

## Offline verification plan and status

The reserve-zero baseline compiled successfully. Its first run passed 19/20
tests, including all file/hash, text-overlap, and lost-reply assertions, but
failed the full test's three-second cleanup wait. The production mux normally
defers foreground cleanup for 10–15 seconds. The corrected test closes idle
HTTP and explicitly kicks the existing mux cleanup path **after** measurement
work finishes. It changes no production timing constant. The next run passed
20/20 in 2.673 seconds; the real HTTP/SOCKS test passed in 2.11 seconds with
112 UP and 127 DOWN objects accepted/deleted and zero retained payload bytes.
These are local test durations, not Google throughput or latency results.

The priority-reserve variant and its saturation tests subsequently passed in
the unified targeted run (4.711 seconds including both full-mux subcases).
The finalized scalar source `443bdbd` passed all 28 top-level relay tests and
three full-mux subcases under the race detector in 63.763 seconds; package vet
passed. Its retained ordinary test binary SHA-256 is
`a227a021b419489a126390795ee9bf8f93ea882e73e189c30b362d2655d678f0`.

The 4 MiB mux configuration first completed its files but failed the requirement
that a text reply finish before its very fast loopback upload. A separately
labeled 20 ms simulated RPC case provided a reproducible overlap window and
passed exact 16 MiB GET/POST, 17 text replies and reply-loss faults. It observed
an actual 4,194,338-byte sealed normal object and fragmented it within 768 KiB
RPC frames. Its full local test took 2.97 seconds. This is a correctness
condition, not a measured Google RTT or throughput result.

## Scalar RPC service-budget experiment

A separate test inserts 300 ms before and after each local HTTP exchange,
preserving one UP and one PULL channel, 768 KiB RPC frames, the 4 MiB mux limit,
and 1 MiB/16 priority reserves. It offers twelve ordered 64-byte messages every
250 ms on one already-open SOCKS connection while an exact 16 MiB GET runs.
The offered span was 2.750 seconds; all twelve exact echoes finished while the
file request remained active. The RTT samples in milliseconds were:

`1497.706, 1849.515, 2200.720, 2551.723, 2902.227, 3253.083,
3604.127, 3956.599, 4913.110, 5264.977, 5617.541, 5969.796`.

Median was 3428.605 ms, maximum 5969.796 ms, and first-to-last growth 4472.090 ms.
The workload used 26 UP and 43 PULL RPCs after warmup. Its largest observed
sealed normal object was 2,097,200 bytes, despite the 4 MiB configured maximum.
An initial run retained as a failed measurement condition required an actual
3 MiB object and stopped after successful delivery at 28.51 seconds. The
corrected test distinguishes configured limit from observed object size,
requires actual fragmentation, prints every sample before its condition
checks, and passed in 37.85 seconds including separately paced cleanup.

These simulated results show queue growth when one object fragment consumes
an entire 600 ms RPC at a 4-message/second offered rate. Memory headroom does
not solve the service-rate limit. A bounded multi-object priority-packing
variant was subsequently compared against this retained scalar baseline.
No timing here measures Google's service or a regional route.

## Priority packing variant

`PackPriority` is an explicit research setting, default false. It packs at most
eight complete small priority objects into at most half one RPC frame or
128 KiB, whichever is smaller. Normal fragments travel in separate RPCs, so
normal admission pressure cannot reject an otherwise admissible priority
batch. Delete-only RPCs retain precedence. UP gives a waiting normal fragment
a turn after four priority objects; PULL counts eight objects before its normal
turn. A group counts its objects rather than spending only one fairness turn.

The broker checks every grouped identity, name, total, offset, ciphertext hash,
aggregate admission bound and new object's inner authentication before any
payload publication or DOWN deletion. Wire ciphertext identity and expected
sequence are checked before consuming the previous RPC's receipt. The previous
result receipt is separate from asynchronous object validation; this design
does not claim all inner validation precedes RPC bookkeeping.

The client requires the exact set of final committed IDs before freeing any
caller's upload. Grouped DOWN replies are checked for complete priority shape,
unique names/IDs, hashes, inner authentication and whole-group capacity before
exposure. The prepared path does not run inner validation a second time after
part of a group becomes visible. Existing incomplete scalar assemblies cannot
become complete grouped objects. Unsupported scalar peers reject grouped
requests or responses instead of silently losing payload.

The same-binary comparison keeps the scalar mode's original burst
accounting, uses one shared incompressible payload, fixes 32 MiB/direction and
1 MiB/16 reserves on both modes, and changes only `PackPriority`.
Five client packing fault tests and broker atomicity,
replay and normal-progress tests accompany the new full SOCKS case.

The packing source at `ab54f98` passed the complete non-simulation relay race
suite in 27.967 seconds and package vet. That includes exact 16 MiB GET/POST
with 17 text replies and lost responses in four actual HTTP/SOCKS cases,
atomic group failures, complete-ID acknowledgements, processing deletion,
bounded cancellation, and priority/normal progress. A separate actual
end-to-end TLS origin/client test passed under race in 20.553 seconds, verifying
16 MiB GET, 16 MiB POST, 25 exact persistent HTTP echoes, one origin file
side effect in each direction, and empty payload caches. The blind forwarding
template's six Node tests passed. A later targeted race test for terminal
carrier failure aborting the application's TLS wait passed in 1.613 seconds.

## Matched simulated results

All six arms below completed every offered echo and the exact 16 MiB file.
Each pair uses one binary and one shared incompressible payload. The JSON
[evidence](apps-script-relay-packing-local-2026-11-15.json) retains all 72 echo
samples, source and binary hashes, call counts, observed object sizes, and
separate timing scopes. Earlier failed test conditions remain described above.

| Pair | Priority packing | Echo median / max | First→last echo growth | File first byte | File completion | File Mb/s |
|---|---|---:|---:|---:|---:|---:|
| First GET | Off | 3432.868 / 5974.790 ms | 4475.457 ms | Not recorded | Not recorded | Not recorded |
| First GET | On | 1526.428 / 1804.968 ms | 257.294 ms | Not recorded | Not recorded | Not recorded |
| Observed GET | Off | 3432.156 / 5972.922 ms | 4474.320 ms | 6645.582 ms | 26711.750 ms | 5.024670 |
| Observed GET | On | 1527.870 / 1805.557 ms | 259.436 ms | 15805.653 ms | 24296.635 ms | 5.524128 |
| Observed POST | Off | 4049.657 / 7209.330 ms | 5707.777 ms | 32376.658 ms | 32376.683 ms | 4.145506 |
| Observed POST | On | 3164.059 / 5078.861 ms | 3325.679 ms | 24484.676 ms | 24484.722 ms | 5.481693 |

Packing reduces GET-direction chat queue growth in these finite samples, but
the observed GET first byte became materially slower. Actual largest sealed
normal objects differed: 4,194,338 bytes without packing versus 1,048,631 with
packing in the observed GET pair. The 4 MiB setting is a maximum, not a claim
about actual object size. Coalescing and object ordering therefore confound a
simple file-performance attribution. The POST pair observed 4,194,338 bytes
in both arms; packed upload still accumulated 3.326 seconds of echo latency
growth. No blanket winner or latency isolation guarantee follows.

For POST, HTTP first response is the server-confirmed upload receipt. It is not
the time the first upload byte was sent. File completion includes receiving
and checking the exact body/hash or acknowledged upload hash. Per-subcase wall
durations include cleanup and were never substituted for file duration.

## Remaining architecture frontier

A 768 KiB normal fragment on one 600 ms service slot has an ideal ceiling of
10.486 Mb/s per direction before framing, Base64, priority work, deletion, or
provider transfer time. With permanent normal UP backlog, the current
four-priority-object burst followed by one normal RPC can serve at most
`4 / (2 × 0.6) = 3.33` priority objects/second, before delete RPCs. That is
below the four messages/second offered rate when each message becomes an
object. Multiple application messages inside one object can amortize that
cost; the bound is in objects, not application messages. PULL's eight-object
burst has a corresponding 6.67-object/second ideal service rate.

Credible next candidates include charging fairness by RPC service rather than
objects, an explicit eight-object UP burst, a separate priority channel,
larger normal frames, or independently acknowledged bulk channels. Larger
frames can block a priority packet for longer, and more channels need atomic
cross-channel admission/replay ownership and consume more execution
concurrency. Record-level authenticated slabs could also reduce the wait for
an entire large mux object before exposing its first bytes. These are design
hypotheses requiring their own fault tests and mixed measurements; no further
variant is implemented or declared optimal here.

Live research limits are separate quantities: at most 256 visible execution
POSTs per variant, each conservatively reserving eight internal HTTP/2
attempts; at most 4,096 total HTTP attempts upper and 1 GiB encoded bodies upper
on the client; at most 1,024 admitted RPCs and 128 MiB actual encoded bodies at
the owned server. ContentService GETs do not consume the execution-POST count.
The parent reserves each variant's entire POST slice in its private campaign
ledger before starting any process; four 256-POST slices reserve at most 8,192
possible URL Fetch executions. These conservative reservations do not measure
actual Google executions and do not grant access or consume quota by themselves.

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
- Reserved priority admission: fill normal budgets in both directions, then
  complete priority traffic before freeing normal payload; retain reserved
  identity claims after normal payloads become tombstones.

No Google grant, deployment, or live request is performed by these offline
tests. Live evaluation requires a separate exclusive measurement lease, bounded
free-tier budgets, and the concrete Apps Script authorization handled by the
parent task. Free-tier quotas and server requirements must be reported before
any capacity or 200,000-user claim.
