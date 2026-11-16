# Skirk architecture assessment — November 16, 2026

The [catalog](catalog/experiments.json) contains 257 documented findings,
including 73 marked live comparable. A finding can be an API primitive,
interrupted run, deterministic fault model, or full tunnel cohort; these are
not 246 equally controlled protocol races. A universal throughput or latency
limit has **not** been demonstrated. The exact current v5 versus clean `main`
16 MiB cohorts are [reported separately](current-v5-vs-main-live-2026-11-16.md):
23 of 24 mixed arms completed. The subsequent hardened 64 MiB cohorts
[completed 8 of 8 arms](current-hardened-v5-vs-main-64m-2026-11-16.md).
The later [128 MiB download](current-v5-vs-main-128m-2026-11-16.md) completed
4/4 exact arms but v5 had 14.1% lower file rate than clean `main`; its upload
cohort stopped on the third arm's two Drive timeouts. The direction and
workload disagree, so no stable product speed win emerged.

## Comparisons that answer different questions

| Carrier and workload | Scheduled short-message p95 | File rate, decimal Mb/s | Quota and decision |
| --- | --- | --- | --- |
| Earlier frozen async package vs clean `main`, HTTP-serial 16 MiB mixed | Download 6.701 → 5.512 s (17.7% lower); upload 7.955 → 4.635 s (41.7% lower) | Download 14.416 → 13.203 (8.4% lower); upload 12.653 → 12.449 (1.6% lower) | Local estimated units about 9% higher. This is a trade, not a speed sweep. [Evidence](transport-results-round2-2026-11-15.md). |
| Exact current v5 vs clean `main`, persistent 16 MiB + 20 pipelined echoes | Final inbound-cap build: download 6.785 → 6.135 s; upload 8.469 → 7.628 s | Final build: download 20.779 → 26.744; upload 16.363 → 19.465 | Five 4/4 clean cohorts, one 3/4 interrupted by four main upload timeouts. Earlier upload cohorts disagreed; no stable product winner. [All results](current-v5-vs-main-live-2026-11-16.md). |
| Hardened v5 vs clean `main`, persistent 64 MiB + 20 pipelined echoes | Download 7.177 → 6.995 s; upload 7.757 → 6.994 s | Download 47.385 → 48.134; upload 24.716 → 34.628 | Eight/eight arms clean with exact files and text, zero reported API errors, exact cleanup. Two arms per binary/direction and a slow last main upload make the performance difference uncertain. [Full evidence](current-hardened-v5-vs-main-64m-2026-11-16.md). |
| Later current v5 vs clean `main`, persistent 128 MiB + 40 pipelined echoes | Download 7.284 → 6.963 s; upload stopped before a matched median | Download 78.785 → 67.644; upload stopped | Download 4/4 exact and clean, with 14.1% lower file rate for v5. Upload first two arms succeeded, then a current arm had two Drive timeouts, 7/40 echoes and no valid file; the preset error guard stopped the cohort. [Full evidence](current-v5-vs-main-128m-2026-11-16.md). |
| Separate exact default vs `main` 16 MiB recheck | 13.597 → 16.247 s (19.5% higher) | 14.220 → 12.418 (12.7% lower) | A negative cohort prevents calling the earlier async result stable. [Evidence](default-final-vs-main-2026-11-15.md). |
| Test-only Drive v2 priority window 8 vs window 4, four-arm same-binary pipelined 16 MiB each way + 41 echoes | Download 2.223 → 1.455 s (34.6% lower); upload 2.140 → 1.498 s (30.0% lower) | Download 24.301 → 24.406 (0.4% higher); upload 37.101 → 35.737 (3.7% lower) | Conservative whole-arm reservation 232,400 → 208,360 (10.3% lower); seven/eight physical fallbacks → zero. **Best completed Drive-only text-tail research setting for that short, undemoted workload, not a product or `main` comparison.** Many upload text frames were offered after bulk ended. [Evidence](drive-v2-priority-latency-attribution-2026-11-16.md). |
| Test-only Firestore priority plus Drive bulk vs Drive control, best of two paired pipelined cohorts | Download 2.117 → 0.414 s; upload 4.461 → 0.378 s | Download 18.154 → 35.491; upload 28.483 → 30.136 | All files/echoes verified, but each hybrid arm had a shutdown HTTP 400. Firestore's finite free write and outbound allowance disqualifies a shared 200,000-user free default. [Evidence](pipelined-live-comparisons-2026-11-16.md). |
| Test-only full Apps Script relay, best real SOCKS/TLS variant | Download 2.612 s; upload 10.169 s over 12 echoes | Download 4.116; upload 2.700 | Slower than completed Drive examples; no Drive-scale quota claim. [Evidence](apps-script-relay-full-live-2026-11-15.md). |
| Test-only immutable known-ID train, eight tiny records vs list primitive | 0.775 s list → 0.829 s known-ID (7% higher) | No full tunnel file measurement | Core discovery estimate 1,105 → 465 units (58% lower), excluding ID exchange; credible **scale hypothesis**, not a latency win. [Evidence](id-train-live-2026-11-15.md). |

A separate 100 MiB burst cohort of a **different frozen async candidate** saw
25.484 vs 19.686 Mb/s and 9.016 vs 20.545 s p95 against `main`, with about
14.4% more estimated units and six candidate RPC errors versus zero main
errors. That small result does not cancel the negative exact-default recheck.
Historical 128 MiB single-stream 74.928 Mb/s and ten × 32 MiB aggregate
78.244 Mb/s are different earlier fronted workloads, not a maximum or an
exact current-build comparison. [Earlier results](../validation-2026.md).

## Architecture decision and unresolved limits

The shipping Drive path retains immutable authenticated objects with separate
priority and bulk scheduling. It does not depend on Firestore, Pub/Sub, Apps
Script, a reusable ring, or a paid carrier. The selected route remains a user
choice: pinned `www.google.com` front by default or Direct Google. The v5
namespace and generation-bound key repair a reproduced restart nonce/key
collision; v4 peers need a coordinated upgrade. Local fixes bound queued
primary and deferred receive data and prevent a stalled destination or local
reader from holding unrelated streams. A dequeued frame per active writer
remains outside those queue budgets. [Migration](../mux-v5-migration-2026-11-16.md) ·
[deferred cap](deferred-inbound-global-budget-2026-11-16.md) ·
[inbound cap](inbound-queue-global-budget-2026-11-16.md).

Later local hardening recognizes structured Drive `RATE_LIMIT_EXCEEDED` and
`ErrorInfo` reasons, bounds a mux media response to its maximum valid sealed
batch, and admits large socket read buffers only after a full 32 KiB read,
coalescing that read before framing. Large buffers share a 64 MiB mux-wide
budget and idle streams return to 32 KiB. An initial candidate caused gap
repair in 6/6 local runs; the corrected candidate had 0/6 versus 1/6 on its
paired baseline and passed focused race tests. The subsequent exact 64 MiB
live cohorts completed 8/8 arms, but did not establish a stable speed gain.
The 32 KiB fallback and one dequeued
writer frame per stream mean total memory is still not bounded independently
of active stream count. [Local hardening evidence](mux-media-read-buffer-and-drive-rate-errors-2026-11-16.md).
The later parser also normalizes structured `STORAGE_QUOTA_EXCEEDED` and
`NOT_FOUND` reasons, so ordinary quota and missing-object handling follows
the same typed Google error path as rate-limit handling.

The attractive reusable metadata ring cannot be promoted from this evidence.
Four fresh pinned-front v2 conditional-write preflights returned HTTP 500,
before any timed arm. A deterministic late-write fault model shows an
unconditional PATCH can overwrite a reused slot after its sequence wraps;
read-side authentication detects the loss but cannot restore overwritten
bytes. Fetch-time ACK also fails a crash between fetch and application
delivery. [Capability result](fixed-rendezvous-live-2026-11-16.md) ·
[fault model](drive-ring-fault-model-2026-11-16.md). A selective-receipt
prototype repaired a local cumulative-ACK fairness case, but two live ABBA
attempts stopped on fronted 502/timeout arms, so it has no completed paired
winner. [Receipt result](drive-v2-selective-receipt-live-2026-11-16.md).
A later sustained product-versus-v2 window-eight campaign passed both pinned
Drive preflights, but its first v2 arm reached the preset 240,000-unit
rolling-minute guard with 61/80 client echoes and an unproven upload. It
stopped before any control arm, so it supplies a quota stop, **not** a speed
comparison. [Stopped campaign](matched-muxv5-drive-v2-window8-live-2026-11-16.md).

The expensive product gap is cold/idle discovery. The exit still lists at
roughly one call per second when idle. At Google's current 100-unit
`files.list` weight, that is about 6,000 units/min or **8.64 million/day per
continuously idle exit** before useful traffic. Replacing that with a 5-unit
known-ID metadata GET would reduce the read portion twentyfold, but the
receiver first needs a crash-safe authenticated ID announcement and bounded
rotation. Existing primitive ID-train evidence did not include that exchange
and was slightly slower. [Gap review](fronted-carrier-gap-review-2026-11-16.md) ·
[Drive limits](https://developers.google.com/workspace/drive/api/guides/limits).

Google documents 1 million Drive units/min/project and a 400 million/day
free threshold for new projects. A shared project with 200,000 active users
doing merely two 5-unit metadata GETs every 30 seconds would need 4 million
units/min and 5.76 billion/day. Per-user owned quota projects distribute the
project bucket for one tested pinned-front method, but do not prove that
200,000 projects, exit accounts, fronted routing, or concurrent clients work.
Ten simultaneous clients is the largest successful recorded live stress,
not a capacity ceiling. [Capacity model](capacity-and-production-gate-2026-11-16.md).

A local [full-mux known-ID prototype](id-train-full-mux-local-2026-11-16.md)
then passed real muxv5/SOCKS5/verified TLS with exact local files and
concurrent text, but exposed a decisive quota distinction: it reads tiny
records as **file content at 200 units** after the metadata probe, so it
cannot inherit the primitive's five-unit metadata-only economics. It also
needs dynamic-client bootstrap listing, bounded head state, durable rotation,
and carrier-owned cleanup. It remains on a separate research branch.

An isolated [metadata-only full-mux prototype](metadata-known-id-mux-local-2026-11-16.md)
then delivered tiny priority batches through exact-ID metadata reads without
the 200-unit media GET, with exact local SOCKS/TLS files and text. Its
dynamic-client boot list at the same one-second cold-open cadence costs about
**7,200 units/min while idle**, versus 6,000 for current v5. Slowing boot
lists saves units but raises modeled cold-open p95; cross-app `drive.file`
visibility and durable recovery remain unproved. This is a working local
primitive with a **negative product quota gate**, not a live speed win.

An [isolated fairness-reserve candidate](inbound-fairness-reserve-test-only-2026-11-16.md)
proved that a saturated pair of receive budgets can reset a new draining
stream: the unchanged product test failed and the reserve candidate passed.
The reserve can still fill and may reset legitimate large streams earlier, so
it remains test-only.

The existing pre-OPEN pending-frame guard had a separate isolation bug: when
its global 256 MiB budget filled, it discarded and reset **all** pending
streams. The product fix now rejects the stream that attempted to exceed the
budget and releases only that stream's pending frames. A deterministic
regression preserves two unrelated streams; focused race tests passed. This
does not fix the independent primary/deferred receive saturation case above
or establish a fixed memory bound regardless of active stream count.

The [seen/closed cache audit](mux-bounded-state-audit-2026-11-16.md)
reproduced another threshold failure: one 200,001st seen name erased the
whole 200,000-name history, and a 100,001st closed stream erased all live
tombstones. The product fix incrementally evicts the oldest seen identity,
keeps a hard 100,000 closed-key cap, and temporarily rejects new OPENs for
two minutes if extreme churn forces a live tombstone eviction. This makes
the saturation cost explicit. It does not make v5 restart replay-safe or
guarantee an old object cannot recur after finite cache retention.

A fake-Drive test also [reproduced v5 replay](exit-generation-fence-v6-2026-11-16.md):
an exit restart can reprocess a retained authenticated OPEN and repeat its
initial destination bytes. The opt-in muxv6 exit-generation fence passed one
pinned-front crash/re-upload scenario, then a local test found that a copied
OPEN with a changed physical `.bN` name could replay **within the same exit
generation** after finite tombstone expiry. A bounded generation-lifetime
OPEN ledger now blocks that local replay and a 60-second authenticated cache
refresh handles a lost restart RST for later streams; focused race and full
preflight passed. The corrected source has **not** been rerun live. Cold
streams pay a control exchange, quiet established streams still need a
liveness path, and all platforms need coordinated migration. A four-arm v5/v6
download comparison of the earlier flawed v6 source is not a product ranking;
v6 is not a shipping default.

A [known-comment WAKE fault model](comment-wake-rendezvous-fault-model-2026-11-16.md)
tested a five-unit exact-ID `comments.get` head read and an on-change
`replies.list` scan as a copied-profile bootstrap. The projected head is
constant-size, but Drive does not promise a distinct `modifiedTime` for every
reply: two changes with the same timestamp can hide a WAKE until another
change or a full reconciliation. A stale update can also roll back an
unfenced rotation pointer. At one head poll per second, idle reads alone cost
300 units/min per mailbox, or 60 million/min for 200,000 centrally hosted
mailboxes; periodic reconciliation adds quota and wake delay. This remains a
local research model, not a live full tunnel or product path.

The next discovery design must remove or amortize dynamic bootstrap without
giving up cold-open latency or crash safety. Kit-time generated rendezvous IDs
with explicit per-device pairing, sparse known-ID polling and dense list
fallback are hypotheses to test. Compare both directions under identical
full SOCKS/TLS sustained bulk, cold OPEN, restart, five saturated streams and
24-hour idle quota before changing the product. No current data makes a
universal winner or establishes an immovable architecture wall.
