# Drive v2 window-eight saturation and fairness check

Status: deterministic local fixture only, on the test-only Drive v2 carrier.
No live Google calls or new speed measurements were made in this check. The
[four-arm live comparison](drive-v2-priority-latency-attribution-2026-11-16.md)
remains the latency evidence.

## What was exercised

The test creates four independent mux streams and admits eight small priority
claims from each, filling the 32-record global inline queue under the
16 KiB encoded metadata limit. A fifth stream then tries to enter. For a
control, four streams with window four admit 16 claims, leaving room for the
fifth stream's claim. The window-eight state exchanges the actual sealed
snapshot with its peer; the peer processes claims 2–32 before claim 1, sends
an ACK, then processes claim 1 and sends another ACK. A separate store fixture
uses the route-aware Drive v2 fake, puts the fifth priority object through the
physical fallback, writes a normal bulk object, cancels and joins the owner,
and checks its exact physical cleanup identity.

| Condition | Window 4 | Window 8 |
| --- | --- | --- |
| Four streams at their per-stream cap | 16 of 32 slots occupied | 32 of 32 slots occupied |
| Fifth stream admitted inline immediately | Yes | No; immutable fallback |
| One stalled first claim; claims 2–32 processed | Not exercised | Cumulative ACK remains zero, so no inline slots are released |
| First claim then processed | Not exercised | ACK advances to 32; all 32 slots and their payloads are released |
| Physical fallback and normal bulk object while saturated | Not exercised | Both published and read back exactly |
| Cancel/Close with outstanding inline records | Not exercised | Owners joined; fallback's exact cleanup ID retained |

The tests are `TestDriveV2PriorityWindowEightSaturatedStreamsAndCumulativeACK`
and `TestDriveV2PriorityWindowEightSaturatedQueuePreservesPhysicalAndBulkPaths`
in `internal/skirk/drive_v2_priority_window_cases_test.go`. The focused
four-test window suite passed 30 consecutive repetitions under `go test -race`
on this Mac; this is a local race/invariant check, not a field capacity result.

## Engineering decision

Window eight does not lose or duplicate these claims, and a full queue does
not block the independent bulk or immutable priority path. It **does** trade
inline admission fairness for the measured text-tail improvement: four early
streams can occupy the full queue, and cumulative ACK makes unrelated slots
wait for the oldest unprocessed claim. The fifth stream can still progress by
physical fallback, but that route has different latency and Drive costs. No
five-stream live latency or many-client capacity figure follows from this
fixture.

An adaptive per-stream cap is not automatically better. Dropping back to four
as soon as a second stream appears would recreate the limit responsible for
the live text fallbacks in a text-plus-bulk workload. Reserving some global
slots can protect later arrivals only until those slots also fill; it cannot
resolve a stalled cumulative ACK or guarantee fairness to an unbounded number
of future streams. A production candidate would need an explicit per-flow
scheduler/short bounded wait policy, plus measured p95/p99 and fallback quota
under concurrent streams. Selective processing receipts could remove the
cross-stream head-of-line effect, but require a versioned protocol change and
replay/integrity proof. Until those experiments exist, retain window eight as
a bounded research setting rather than a product default.
