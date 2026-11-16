# Reusable Drive metadata ring: deterministic fault model

Status: **test-only local model** at
`internal/skirk/drive_ring_fault_model_test.go`. It makes no Google calls, has
no product carrier adapter, and measures no network speed or latency. It tests
the proposed two-slot reuse schedule with ideal instantaneous visibility;
real Drive propagation and retries would add uncertainty. The comparison
source is the [XDRIVE audit](xdrive-external-architecture-audit-2026-11-16.md),
not an asserted upstream incident.

## Decisive delayed-PATCH schedule

1. An old sequence-0 PATCH is sent and its response stalls. A retry of the
   same frame completes and the receiver durably accepts sequence 0.
2. Sequence 1 completes and is accepted. Sequence 2 reuses physical slot 0.
3. The old sequence-0 PATCH finally reaches the server **after** sequence 2.

With unconditional metadata updates, the late request overwrites sequence 2.
Checking its authenticated generation and sequence at read time correctly
detects a wrong frame but cannot restore the destroyed bytes. The local model
gets `errRingWrongFrame` for sequence 2. Merely delaying slot reuse by a fixed
time does not give a proof: a previously dispatched request can be delayed
longer than that time, including after client cancellation or a lost response.

An **ideal server-enforced version precondition** rejects the late request in
the same schedule and preserves sequence 2. This is a design condition, not a
claim that the selected Drive route supplies it. The independently inspected
XDRIVE v3 update path issues unconditional PATCHes, while Skirk's four fresh
pinned-`www.google.com` v2 conditional capability attempts returned HTTP 500
before any timed arm. Earlier 412 observations do not turn the current 500
into a reliable CAS primitive. [Live probe](fixed-rendezvous-live-2026-11-16.md),
[upstream source](https://github.com/XTLS/Xray-core/blob/676eaf66f8f05d4d558ee7a74b4736623d3feeb9/transport/internet/xdrive/drive.go#L378).

An immutable ID per sequence also rejects a late duplicate create in the
model, but it gives up physical slot reuse; it needs publication discovery,
bounded retention and exact cleanup. That is closer to Skirk's existing
immutable bulk path and must be compared under the same workload. An epoch
label, MAC, reader-side sequence check, HTTP connection pool, or larger ring
does not prevent a stale server-side PATCH from overwriting a reused ID.

## Watermark and fairness

Acknowledging on **fetch** allows the sender to reuse a slot before local
delivery. In the model, a crash after fetch and before delivery erases the
only volatile copy, while the sender has already overwritten the source.
Acknowledging only after durable delivery ownership keeps sequence 0
recoverable. This local state machine does not establish application-level
exactly-once effects, since a write to a TCP socket is not a durable receipt
from the application. A crash-safe processing journal and idempotent remote
effects remain separate requirements.

A safe contiguous watermark creates head-of-line pressure. With depth two,
stream A's sequence 0 is blocked, stream B's sequence 1 is accepted, and
stream C's priority sequence 2 cannot use slot 0 until A completes. Moving the
watermark on B alone would silently violate the reuse condition. Separate
fair lanes or per-stream credit could reduce this pressure, at a cost in file
setup, poll activity and coordination; the test proves the trade-off exists,
not that a particular scheduler wins. Skirk's currently measured priority
window of eight is **per-stream claim admission in a cumulative snapshot**,
not an eight-slot ring. [Measured window comparison](drive-v2-priority-latency-attribution-2026-11-16.md).

## Quota envelope, not a performance measurement

The [official Drive limit table](https://developers.google.com/workspace/drive/api/guides/limits)
(updated 2026-10-30) charges metadata GET 5 units, file create/update/delete
50, and list 100. It states 1,000,000 units/min/project,
325,000 units/min/user/project, and a 400,000,000-unit/day **free billing
threshold**. The threshold is not a hard daily request cap. Backend 429s and
egress/upload limits are additional constraints.

For two directional rings and one watermark file per direction, cold setup
requires `2 × depth + 2` file creates. These are request-cost lower bounds
before retries, probes, read-ahead, verification, cleanup and any extra class
or per-stream ring:

| Depth per direction | Cold files | Cold create units |
| ---: | ---: | ---: |
| 2 | 6 | 300 |
| 4 | 10 | 500 |
| 64 (XDRIVE default) | 130 | 6,500 |

For 32 tiny frames, one successful PATCH and one successful metadata GET each
cost at least 1,760 units. One shared watermark PATCH adds 50 (1,810 total),
whereas one PATCH per receipt adds 1,600 (3,360 total). The actual reader may
poll repeatedly between frames, and the sender may need additional GETs.
One five-unit GET per second for 200,000 continuously active users is
60,000,000 units/min and 86,400,000,000 units/day on a shared project, already
60 times its minute quota and 216 times the daily free threshold. User-owned
projects can distribute the project bucket when independently configured; they
do not change the per-user/backend or egress constraints.

## Decision from this model

Do not adopt an unconditional reusable Drive slot ring as Skirk's default.
Before a ring can compete with the existing Drive-only v2 path, it needs a
server-proven stale-writer fence, a crash/retry protocol for ambiguous writes,
durable processing ownership, a fair admission design, and a live
same-account pinned-front A/B with file hashes, text p95, goodput, all requests,
all quota reservations and exact cleanup. If a reliable fence is unavailable,
immutable per-sequence objects remain the safer candidate despite their
discovery cost. This model cannot rank their latency or throughput.

Validation: focused race-enabled tests passed. They force the late-write,
ideal-CAS, early-watermark crash, blocked unrelated stream, immutable-ID and
quota arithmetic cases. No physical Drive files or OAuth grants were touched.
