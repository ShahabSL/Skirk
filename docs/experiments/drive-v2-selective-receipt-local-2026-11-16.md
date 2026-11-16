# Drive v2 selective processing receipt prototype

Status: **local, test-only research**. The prototype changes `_test.go` code
behind an opt-in `FSH2` wire marker. Ordinary `FSH1` behavior and product mux
remain unchanged. No Google API call, live tunnel timing, billing count, or
client configuration was measured or changed here.

## Why test a new receipt

The [window-eight fairness check](drive-v2-window-fairness-2026-11-16.md)
showed that four streams can fill all 32 inline positions. If claim one is
stalled, the original cumulative processing ACK cannot release positions for
claims two through 32 even after the receiver has processed them. The
physical fallback remains correct but may have a slower path and extra Drive
work. Simply increasing the stream window cannot repair that head-of-line
receipt dependency.

This prototype sends a fixed 4,096-bit, authenticated lifetime receipt bitmap
alongside the cumulative ACK. A bit is set only after the actual mux success
callback. The sender may release each bit-marked claim's payload, global
position and per-stream position independently. Its next current snapshot
omits those processed records, which leaves gaps in sequence numbers. The
receiver accepts a gap only for an identity it previously observed and
processed. New suffix records must still be contiguous with the highest
observed sequence and have the exact cumulative byte offset. The receiver
grants new sequence/byte credit based on **unprocessed** plus already promised
work, while retaining all finite-lifetime identity tombstones. A newer bitmap
cannot clear an earlier bit or ACK a claim the sender never offered.

`FSH2` uses distinct authenticated associated data and a different marker.
The local old-peer check rejects the new wire in the old decoder and vice
versa. A real product would first need explicit versioned peer negotiation;
there is no automatic downgrade after an `FSH2` write.

## Deterministic local evidence

| Scenario | `FSH1` cumulative receipt | Opt-in `FSH2` selective receipt |
| --- | --- | --- |
| Four streams × eight claims fill 32 positions; claim 1 stalls and 2–32 finish | All 32 remain held until claim 1 finishes; fifth stream takes immutable fallback | 31 positions are released after their authenticated receipt; claim 1 stays present and fifth stream can enter inline |
| Further progress with claim 1 still stalled | Bounded queue remains full | Four waves processed 125 distinct claims while retaining claim 1; after it finished all sender positions and bytes drained |
| Lost/reordered receipt documents | Existing replay handling | A lost reply preserved original advertisements; a newer reply selectively released processed identities; the delayed old reply was rejected without mutation |
| Physical-first processed copy before its inline sequence appears | Existing exact-name/digest deduplication | The later sequence was associated with the completed identity, emitted a receipt, and was not delivered to mux again |
| Wire size under the selected 16 KiB cap | Empty 167 B; 32 tiny records 10,234 B | Empty 871 B; 32 tiny records 10,938 B: **704 B additional** in both cases |
| 2,400-byte data-frame admission before encoded cap | Four records | Four records in this particular fixture; different sizes can cross the 704-byte margin earlier |

The tests also reject an omitted unprocessed old claim, a future receipt, a
regressed receipt, a malformed bitmap, and an old wire format. Sixty-four
deterministic seeds shuffled processing order and delayed intermediate ACK
documents; every final receipt drained its exact 32 claims, and every late
document was rejected. Twenty race-enabled repetitions of the focused suite
passed, along with exact-source `go test ./...`, `go vet ./...` and
`git diff --check`.

A same-binary two-arm fake-route SOCKS/TLS run exercised `FSH1` and `FSH2`
with window eight, exact independent 16 MiB GET and POST, 40 text echoes
during bulk, a committed PATCH whose response was lost, immutable fallback,
one read/watch reconnect, joined cleanup and the same local quota guard. Both
arms passed. Descriptive fake Drive calls were **1,471/1,448** and conservative
unit reservations **192,400/191,120** for `FSH1`/`FSH2`; both had three
fallback publications. The loopback durations were 9.34/9.15 seconds. These
call/elapsed differences are scheduling noise in a local fixture, **not** a
Google latency, file-rate or quota win. This workload did not create the
stalled-first-claim saturation scenario that selective receipts target.

The fake-route store test also confirmed both modes use the same 16 KiB
admission/carrier cap and that every physical fallback retained the exact
prepared ciphertext. The live v2 fixture had previously omitted
`MaxSnapshotBytes`, leaving its queue at the shared 64 KiB default while the
actual v2 carrier rejected bodies over 16 KiB. The fixture now sets 16 KiB
for both modes. No previously completed arm reported an over-cap rejection;
its historical measurements are not reinterpreted. The live fixture accepts
an explicit `priority_selective_receipts` flag only for `drive_v2` and labels
it in the result. No live arm was run with this flag.

## Decision and remaining risks

The prototype removes the specific cumulative-receipt head-of-line effect
when later claims actually complete. It does not guarantee fairness while
32 claims remain genuinely unprocessed, and it retains immutable fallback.
The 704-byte envelope reduces room for near-capacity inline payloads. More
frequent out-of-order ACK publication may increase 50-unit Drive metadata
updates even if an existing ACK delay coalesces some callbacks; no live quota
or latency result is available. This finite in-memory epoch still lacks
distributed negotiation, rollover, crash-durable cleanup, native app wiring,
and multi-client proof. Keep `FSH2` test-only until those mechanisms and
same-binary live mixed comparisons are complete.
