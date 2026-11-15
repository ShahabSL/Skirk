# Asynchronous metadata slot ownership, November 15, 2026

This experiment isolates the time a mux lane waits for a slot processing ACK.
Both modes use authenticated slot delivery and the same interaction-epoch
scheduler. Only the candidate enables `drive.metadata_slot_async_v1`. The
sender hands bounded upload ownership to a task, ACKs successful Drive
publication separately, and retains the exact sealed immutable fallback while
waiting for peer processing. It remains off by default.

The exact frozen binary was built at `05ae14a` with SHA-256
`6002b6ccd29cf38a4e06a023bcf82595671f92730b1041bca85461d7d8b06482`.
Both modes used that binary, the isolated test account, the pinned
`www.google.com` front, a fresh session each arm, one SHA-verified 16 MiB
file, and one persistent TCP text stream. A byte-verified 128 KiB prelude
demoted that stream before twelve 64-byte echoes were pipelined every 250 ms.
Text began one second after bulk started, clearing the shared scheduler's
quiet threshold. Download used sync/async/async/sync order; upload reversed
it. The fixed eight-second tail and both-peer quota estimates are included.
The file no-progress allowance was 60 seconds. Other live experiments were
serialized on this test account. The Mac was shared with offline sub-agent
development and tests; CPU load was not recorded or fully isolated, which is
another reason to treat the timing differences as small-sample observations.

## All arm results

| Direction | Arm | Text request p95 | Scheduled text p95 | File goodput | Estimated units | API errors |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Download | Sync 1 | 8.662 s | 8.667 s | 11.514 Mb/s | 24,095 | 0 |
| Download | Async 1 | 6.569 s | 6.576 s | 13.977 Mb/s | 24,545 | 0 |
| Download | Async 2 | 2.996 s | 2.998 s | 18.953 Mb/s | 22,420 | 0 |
| Download | Sync 2 | 6.220 s | 6.229 s | 14.646 Mb/s | 22,205 | 0 |
| Upload | Async 1 | 4.102 s | 4.102 s | 13.870 Mb/s | 21,085 | 0 |
| Upload | Sync 1 | 7.477 s | 7.485 s | 11.801 Mb/s | 25,205 | 0 |
| Upload | Sync 2 | 7.750 s | 7.756 s | 12.466 Mb/s | 22,880 | 0 |
| Upload | Async 2 | 3.132 s | 3.142 s | 11.840 Mb/s | 23,310 | 0 |

All eight files, 96 echoes, eight exact preludes and eight authenticated exit
proofs passed. Every arm logged real slot receiver processing and sender ACK
activity; every candidate logged asynchronous handoff and publication ACK.
These are full Skirk TCP message results, unlike the earlier slot-substrate
probe that delivered text outside the proxy.

| Median of two arms/mode | Synchronous delivery | Asynchronous ownership | Descriptive change |
| --- | ---: | ---: | ---: |
| Download request p95 | 7.441 s | 4.783 s | −35.7% |
| Download scheduled p95 | 7.448 s | 4.787 s | −35.7% |
| Download file | 13.080 Mb/s | 16.465 Mb/s | +25.9% |
| Download estimated units | 23,150 | 23,482.5 | +1.4% |
| Upload request p95 | 7.614 s | 3.617 s | −52.5% |
| Upload scheduled p95 | 7.621 s | 3.622 s | −52.5% |
| Upload file | 12.134 Mb/s | 12.855 Mb/s | +5.9% |
| Upload estimated units | 24,042.5 | 22,197.5 | −7.7% |

The small samples favor asynchronous ownership in both directions. They do
not compare against default muxv4, and do not establish a sustained capacity,
100 MiB behavior, a stable population p95 or a universal speed improvement.

## Publication, processing and fallback

| Candidate arm | Client handoff / publication / processing ACK / fallback | Exit handoff / publication / processing ACK / fallback |
| --- | ---: | ---: |
| Download 1 | 9 / 9 / 5 / 4 | 3 / 3 / 0 / 3 |
| Download 2 | 10 / 10 / 7 / 3 | 6 / 6 / 0 / 6 |
| Upload 1 | 11 / 11 / 11 / 0 | 3 / 3 / 3 / 0 |
| Upload 2 | 11 / 11 / 7 / 4 | 9 / 9 / 6 / 3 |

The candidate still sends immutable fallback after a 1.5-second processing
deadline even when PATCH publication succeeded. The download exit therefore
fell back for every handed-off record, although the client logged slot
receiver processing. This is evidence of coordination overhead worth isolating
with a distinct primary-slot experiment; it does not imply that bytes were
lost or duplicated. Existing digest and frame-sequence checks suppress late
physical copies. Publication ACK still means only that Drive accepted the
encrypted snapshot; processing ACK is not destination delivery or durable TCP
resumption.

## Cleanup and provenance

Download cleanup deleted 44 residual mux objects; upload deleted 34. All
**16 exact mux/slot prefixes** were empty immediately and after separate
31-second delayed checks, with no failed or truncated sweeps. Temporary arm
configs were removed and processes stopped. Manual sweeps occur after the
table's fixed metrics tail, so the table is not the full eventual-cleanup
quota cost and does not prove long-running automatic cleanup keeps pace.
The private reports, per-arm logs, metrics and cleanup ledgers are under
ignored `.skirk-runs/slot-async-{download,upload}-16m-20261115-01/` in the
isolated worktree.

This binary predates two subsequent correctness repairs identified by the
read-only Claude audit: stale authenticated snapshots must be replay-filtered
before regressed ACK validation, and a stopped slot writer must immediately
stop advertising itself as sendable. The frozen experiment was not modified
between arms. Later fixes need their own exact-code smoke before promotion.
