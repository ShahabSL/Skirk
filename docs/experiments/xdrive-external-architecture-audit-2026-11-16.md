# Xray-core XDRIVE comparison: transferable ideas and missing proof

Status: independent Sol ultra and Astra ultra **source audits**, not a Skirk
live performance experiment. Both read Skirk's 228-entry catalog and the
released Xray-core XDRIVE implementation; they made no code or Cloud changes.
The inspected upstream revisions were
[`676eaf66`](https://github.com/XTLS/Xray-core/tree/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive)
and
[`5d17b872`](https://github.com/XTLS/Xray-core/tree/5d17b872edde3024a5b906d9cd6c7a3ad4973d81/transport/internet/xdrive).
This is an external comparison, not an endorsement or a same-workload winner.

## What is structurally different

The [original eight-slot PR](https://github.com/XTLS/Xray-core/pull/7217)
evolved into a **64-slot-per-direction** Drive default, with two watermark
files: 130 objects at cold session setup. Two prewarmed spare rings can add
260 further objects. Sequence `n` uses slot `n % depth`; a reader watermark
allows reuse. The reader speculates ahead and overlaps head GETs. Small
segments go in metadata descriptions (14 KiB raw payload plus a header,
then Base64); large segments update media and metadata together. [Defaults](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/params.go#L71),
[ring](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/slots.go).

Skirk's measured window eight is **eight in-flight priority claims per stream
in a 32-record cumulative metadata snapshot**, beside immutable Drive bulk.
It is not an eight-slot ring. The best completed Drive-only mixed comparison
reported scheduled small-text p95 1.455 s during download and 1.498 s during
upload, with file rates 24.406/35.737 Mb/s in that short workload. XDRIVE's
reported approximately 0.9–0.96 s one-way and 1.5 s warm RTT used different
workloads, setup accounting, and client topology, so they are not a head-to-head
speed comparison. [Skirk evidence](drive-v2-priority-latency-attribution-2026-11-16.md),
[XDRIVE claims](https://github.com/XTLS/Xray-core/pull/7352).

XDRIVE's transport retries and connection lifecycle are useful to study.
Its configurable HTTP/2 pool uses session affinity, and it reserves request
positions for head/watermark work. Skirk already has parallel download pools
and a separate priority path, but has not measured physical connection
identity and acquisition waits factorially on the best v2 workload.
[Upstream client](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/client.go#L454).

## Correctness conditions before borrowing a ring

Upstream ring PATCHes are unconditional Drive v3 updates. A delayed old PATCH
could overwrite the same physical slot after its sequence is reused. Sequence
checks can detect wrong content when read; they cannot prevent a newer object
from being overwritten. This is a **source-derived fault hypothesis**, not an
observed upstream incident. A Skirk ring must hold per-slot publication
ownership until every old attempt has terminated, fence reuse against stale
writes, authenticate generation/sequence, and recover a lost response without
replaying a side effect. The decisive local schedule is: with depth two, delay
the first seq0 write, let its retry complete and ACK, publish seq1 and seq2,
then release the old seq0 write before consuming seq2. [Upstream mutation](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/drive.go#L378),
[reuse predicate](https://github.com/XTLS/Xray-core/blob/5d17b872edde3024a5b906d9cd6c7a3ad4973d81/transport/internet/xdrive/slots.go#L320).

Fetching into memory is not application processing. A reader watermark must
not free a slot whose bytes could still be lost on a crash or a blocked local
socket. Exact ownership journals and delayed absence checks also matter:
Skirk's cleanup requirement is stronger than a periodic inactive-prefix
sweep. [Upstream reader](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/slots.go#L660),
[collector](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/xdrive.go#L468).

The audits also found Skirk-local scheduler risks: blocked local readers may
occupy shared receive workers, a dial permit may remain held while a successful
connection waits for local delivery, and abandoned priority waiters may leave
refilled tokens unused. These are being reproduced and repaired separately;
they are not XDRIVE performance evidence.

## Ranked next experiments

1. **Fix and fault-test local fairness first.** Two stalled streams must not
   block an unrelated authenticated text stream or new OPEN. Preserve bounded
   memory and process receipts only after safe delivery ownership.
2. **Small fenced ring, not a copied 64-slot default.** Model two/four reusable
   slots per class with a generation, CAS or stronger write fence, independent
   publication ownership, bounded credit, and lazy setup. Compare against
   immutable list and frozen v2 window eight; include the delayed-write schedule
   above, rollover, cancellation, exact cleanup and all setup/idle calls.
3. **Carrier-aware batching and fair admission.** Mix 64 B, 2 KiB, 8 KiB and
   12 KiB text across eight streams while bulk moves. Avoid coupling an eligible
   tiny claim to an oversized or saturated stream. Compare physical fallback,
   small-text p95, file goodput, quota and late-stream fairness.
4. **Targeted GET speculation and physical H2 pool measurement.** Permit one
   tail-triggered duplicate metadata GET only during outstanding priority work;
   cancel/join the loser and count both calls. Separately compare one/two/four
   physical H2 connections with logical workers fixed. Do not infer a speed win
   from an HTTP/3 primitive; prior H3 full-tunnel arms were incomplete.

The direct, pinned `www.google.com` route and Drive-scale free quota are
separate gates for every candidate. At 200,000 continuously active users, even
one five-unit GET each second costs 60 million units/minute and 86.4 billion
units/day on a shared project, far beyond Google's 1 million/minute and
400-million/day free threshold. A larger ring does not change that arithmetic.
User-owned projects distribute project buckets only when ownership and
authorization are real; backend, per-user, upload and egress limits remain.
[Drive limits](https://developers.google.com/workspace/drive/api/guides/limits).
