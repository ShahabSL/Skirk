# Skirk architecture assessment — November 16, 2026

The [catalog](catalog/experiments.json) contains 239 documented findings,
including 70 marked live comparable. A finding can be an API primitive,
interrupted run, deterministic fault model, or full tunnel cohort; these are
not 239 equally controlled protocol races. A universal throughput or latency
limit has **not** been demonstrated. The exact current v5 versus clean `main`
cohorts are [reported separately](current-v5-vs-main-live-2026-11-16.md):
15 of 16 mixed arms completed, but two complete upload orders disagreed on
the sign of both throughput and text p95. No stable product speed win emerged.

## Comparisons that answer different questions

| Carrier and workload | Scheduled short-message p95 | File rate, decimal Mb/s | Quota and decision |
| --- | --- | --- | --- |
| Earlier frozen async package vs clean `main`, HTTP-serial 16 MiB mixed | Download 6.701 → 5.512 s (17.7% lower); upload 7.955 → 4.635 s (41.7% lower) | Download 14.416 → 13.203 (8.4% lower); upload 12.653 → 12.449 (1.6% lower) | Local estimated units about 9% higher. This is a trade, not a speed sweep. [Evidence](transport-results-round2-2026-11-15.md). |
| Exact current v5 vs clean `main`, persistent 16 MiB + 20 pipelined echoes | Download completed cohort 6.610 → 6.057 s; upload orders 9.214 → 6.469 s **and** 6.772 → 6.806 s | Download 18.774 → 20.884; upload orders 11.955 → 15.264 **and** 17.558 → 16.175 | Three 4/4 clean cohorts, one 3/4 interrupted by four main upload timeouts. High variation; no stable product winner. [Evidence](current-v5-vs-main-live-2026-11-16.md). |
| Separate exact default vs `main` 16 MiB recheck | 13.597 → 16.247 s (19.5% higher) | 14.220 → 12.418 (12.7% lower) | A negative cohort prevents calling the earlier async result stable. [Evidence](default-final-vs-main-2026-11-15.md). |
| Test-only Drive v2 priority window 8 vs window 4, four-arm same-binary pipelined 16 MiB each way + 41 echoes | Download 2.223 → 1.455 s (34.6% lower); upload 2.140 → 1.498 s (30.0% lower) | Download 24.301 → 24.406 (0.4% higher); upload 37.101 → 35.737 (3.7% lower) | Conservative whole-arm reservation 232,400 → 208,360 (10.3% lower); seven/eight physical fallbacks → zero. **Best completed Drive-only text-tail research setting for that workload, not a product or `main` comparison.** [Evidence](drive-v2-priority-latency-attribution-2026-11-16.md). |
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
collision; v4 peers need a coordinated upgrade. Local fixes bound deferred
receive data and prevent a stalled destination or local reader from holding
unrelated streams. [Migration](../mux-v5-migration-2026-11-16.md) ·
[receive cap](deferred-inbound-global-budget-2026-11-16.md).

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

The next discriminating architecture experiment is a versioned immutable
known-ID priority queue with authenticated bounded ID credit exchange,
normal immutable bulk, a list fallback for cold OPEN and credit depletion,
and exact owned-ID cleanup. Compare it with current mux and the v2 research
queue under the **same** full SOCKS/TLS mixed down/up cohort, then run cold
OPEN, restart, five saturated streams and 24-hour idle quota tests. Predeclare
integrity, p95, file, quota and cleanup gates. No current data makes this a
proven winner or establishes an immovable architecture wall.
