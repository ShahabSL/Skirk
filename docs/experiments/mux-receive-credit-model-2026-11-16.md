# Mux receive credit: isolated design model

Status: **research only; no product mux change or live Google call**. This
branch starts from the deliberately red full-mux receiver fixture at
`1800b8a`. The [fixture](https://github.com/ShahabSL/Skirk/blob/codex/mux-receive-budget-20261116/internal/skirk/mux_receive_budget_full_test.go)
demonstrates that a legitimate second slow stream B resets after stream A
occupies the 64 MiB primary handoff pool. B's own 8 MiB deferred cap is the
immediate failure; the global 64 MiB deferred pool does not fill. The
[credit model](https://github.com/ShahabSL/Skirk/blob/codex/mux-flow-credit-model-20261116/tools/receive_credit_model/model_test.go) is a green
**arithmetic and lifecycle model**, not a green Skirk transport test.

## Candidate admission rule

1. Keep the current six-object, 24 MiB per-stream normal download window as
   a *soft* I/O limit. Names and `PlainBytes` are untrusted hints; they cannot
   grant memory credit. A full 4 MiB/512-frame pessimistic pre-download
   reservation would often reduce one stream to one active object.
2. After authenticated decode, atomically reserve **every** DATA and FIN
   frame's exact bytes and one frame slot for each destination stream in the
   batch. Current normal batches are single-stream; urgent coalesced batches
   can span streams. The model assigns primary slots when the 64 MiB/8192-frame global
   pool and 64-frame stream channel allow them; remaining frames require the
   8 MiB/512-frame per-stream deferred pool and 64 MiB/8192-frame global
   deferred pool. A mixed-stream object succeeds as a whole or not at all.
3. Normal reservations leave 1 MiB and 512 deferred frame slots for an
   unrelated priority object. Priority can use that reserve. FIN costs a
   frame slot even when its payload is empty, and must follow prior DATA for
   its stream. Priority to an already saturated *same* stream remains bound
   by that stream's ordered backlog.
4. On credit denial, retain only the Drive object ID/name and bounded
   scheduling metadata, drop decoded bytes, and retry when a local writer
   returns credits. Do not mark the object processed, delete it, occupy a
   worker, or consume the ordinary network-error retry count. Re-downloading
   is a quota/latency risk, so use metadata hints to avoid obviously futile
   GETs without treating those hints as safety proof.
5. Transfer claims to local handoff ownership once frames are delivered.
   Release credits only after the local writer completes the frame or the
   stream closes. Today `nextInbound` releases budget when it dequeues a
   frame, before `writeAll`; moving ownership through that write would keep
   the one possibly blocked writer frame charged. A canceled read releases
   undelivered claims. Once any frame has
   been delivered, replay/cancellation must retain the delivered effect and
   use sequence deduplication. The model rejects cancellation after a partial
   delivery; the product must either finish that batch or terminally close
   affected streams before retry. Stream close removes its queued metadata and
   releases its share of a mixed claim. The model prunes fully released
   claims in constant time per frame. The model does not implement a priority
   retry queue; bounded priority denial/retry is a product gate.

The model's denied-object queue charges at least 64 KiB per object, with
64 MiB per-stream, 256 MiB global, 8192-object and 65536 stream-reference
hard caps. The queued record contains only an ID, charge and destination
stream IDs, no decoded frames or payload. A production implementation should
apply exact fixed-size/length bounds to these IDs as well.

## Local result

| Scenario | Actual baseline mux | Candidate model |
| --- | --- | --- |
| A stalled with 47 MiB primary, then six A and six B objects already admitted | Six B objects download; B resets after its 8 MiB deferred tail fills | Six A and two B objects reserve; four B IDs wait as metadata; both streams stay open in the model |
| Primary/deferred after these claims | Primary reaches 64 MiB; B reset; global deferred is at most 9 MiB | Primary 64 MiB, deferred 7 MiB, no overcommit |
| Uncontended six-object bulk window | Existing six-object window | Six objects accepted; 18 MiB primary, zero deferred |
| Priority DATA then FIN | Full-mux fixture delivered them in order | Model preserves DATA/FIN and leaves global byte/frame headroom |
| Mixed batch, cancel/retry, close, consumption, metadata pressure | Not covered by red fixture | Focused model assertions pass |

The candidate trades speculative B download concurrency for reliable
admission under contention. In this one scenario B's initial concurrent
window drops from six objects to two; that is **not** a measured throughput
gain. The uncontended model preserves six, but a real sender may run slower
if reservation release or metadata retry wakes are delayed.

## Product gates still open

The existing mux cannot use this ledger as a small patch. Its
`queueInboundLocked` chooses primary or deferred opportunistically at frame
delivery, while downloads and streams complete concurrently. A real claim
must either control that placement or be revalidated under the same lock
before side effects. Every handoff path, including priority and pending
OPEN/dial frames, must participate so an unclaimed frame cannot steal a
reserved slot. The ledger also needs separate accounting for out-of-order
`recvPending` frames and not-yet-ready streams; the pure model assumes
contiguous ready streams. Object metadata can contain multiple streams, and
its name byte/range claims need post-decode validation.

Before product promotion, implement the admission owner/retry wake lifecycle
in the full mux and run: the previously red local receiver fixture; race
tests covering mixed batches, close during decode, cancellation after partial
delivery, duplicate object replay, exact DATA/FIN order and no worker held on
a stalled socket; then matched bulk and interactive latency measurements with
one, eight and sixteen streams under the same Drive quota. Until those gates
pass, this branch shows a feasible bound **on a model**, not a verified mux
fix or a best-speed claim.
