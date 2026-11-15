# Negotiated 16 KiB metadata cap: full-proxy live comparison

This experiment isolates the metadata-only sealed-object limit: 8,192 versus
16,384 bytes. Both arms use metadata-only priority, 100-file list pages, the
same pinned `www.google.com` route, and the same binary. It does not compare
against plain `main` or alter the slot scheduler. The tested source is
`37ad19a` (`402502e` plus the gzip response-bound test); binary SHA-256 is
`7d20343d5a5b5045c8cfd9f030aaf05f923ec19a1116a2a2be3191d4c11f035b`.

Each arm creates a fresh random mailbox session on the isolated non-main test
account, runs client and exit locally, verifies one authenticated exit proof,
then schedules twelve HTTP GETs with 12 KiB random responses at 250 ms
intervals through two workers while a separate 16 MiB random file transfers.
The harness verifies response lengths and SHA-256 values. A candidate arm must
publish at least one metadata object larger than 8 KiB; a control arm must
publish none. Complete-body p95 is recorded separately from first-byte p95.

The private runtime harness strengthens the existing quota guard to 50,000
estimated units per arm and per rolling minute, 200,000 per comparison, zero
API errors/backoff, and equal 60-second cooldowns after peers stop between
arms. Exact cleanup targets only the generated mux and slot prefixes, with an
immediate dry run and a 31-second delayed check. Original kit files are not
edited; private per-arm config copies are removed.

## Download ABBA

Order is control/candidate/candidate/control. All 48 response bodies and four
16 MiB files verified, with zero Drive errors. Eight, six, nine and six small
requests respectively overlapped the file's active interval; headline numbers
include all twelve scheduled requests, including executor queue delay where
labelled.

| Arm | Cap | First-byte request p95 | Complete-body request p95 | Scheduled complete-body p95 | File goodput | Estimated units | >8 KiB metadata creates |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 control | 8 KiB | 3.208 s | 3.209 s | 13.217 s | 12.812 Mb/s | 25,005 | 0 |
| 1 candidate | 16 KiB | 5.041 s | 5.042 s | 12.963 s | 12.976 Mb/s | 20,755 | 9 |
| 2 candidate | 16 KiB | 2.947 s | 2.947 s | 9.343 s | 14.804 Mb/s | 21,105 | 13 |
| 3 control | 8 KiB | 5.340 s | 5.340 s | 14.285 s | 15.007 Mb/s | 22,755 | 0 |

Pair medians for control/candidate were complete-body request p95
**4.274/3.995 s**, scheduled complete-body p95 **13.751/11.153 s**, file
**13.910/13.890 Mb/s**, and **23,880/20,930 estimated units** per arm.
The largest successful candidate sealed metadata object was 12,548 bytes.
These two repetitions per mode show that the larger path was exercised and
used fewer estimated units in this sample; variable latency and overlap counts
prevent a general speed claim. Total process-reported units across the four
cooldown-separated arms were 89,620, excluding the wrapper's final cleanup.

The cleanup wrapper deleted sixteen residual mux objects and found all eight
exact mux/slot prefixes empty immediately and after 31 seconds, with zero
cleanup failures. No private config copy remained. Raw reports are ignored
under `.skirk-runs/metadata-16k-down16m-20261115-02/` in the review worktree.
An earlier wrapper invocation stopped before launching peers or issuing API
calls because of a local text-anchor assertion; it is not a failed live arm.

## Reverse upload ABBA

Order is candidate/control/control/candidate. All 48 response bodies and four
16 MiB POST bodies verified, with zero Drive errors. Nine, eight, twelve and
eight small requests respectively overlapped the bulk interval. The root
serialized an unrelated Cloud primitive between the two comparisons; there
was no other live traffic during either comparison.

| Arm | Cap | First-byte request p95 | Complete-body request p95 | Scheduled complete-body p95 | File goodput | Estimated units | >8 KiB metadata creates |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 candidate | 16 KiB | 4.039 s | 4.039 s | 12.190 s | 13.637 Mb/s | 21,405 | 11 |
| 1 control | 8 KiB | 4.235 s | 4.235 s | 14.390 s | 12.066 Mb/s | 25,905 | 0 |
| 2 control | 8 KiB | 7.528 s | 7.528 s | 21.650 s | 5.152 Mb/s | 29,055 | 0 |
| 3 candidate | 16 KiB | 3.643 s | 3.643 s | 11.844 s | 14.352 Mb/s | 20,905 | 10 |

Pair medians for control/candidate were complete-body request p95
**5.882/3.841 s**, scheduled complete-body p95 **18.020/12.017 s**, file
**8.609/13.995 Mb/s**, and **27,480/21,155 estimated units** per arm.
Candidate sealed objects again reached 12,548 bytes. Total process-reported
units were 97,270 across the cooldown-separated arms. The second control had
a slow 5.152 Mb/s file and 7.528-second request p95; the sample does not
establish that the larger metadata limit caused the apparent bulk-rate gain.

The cleanup wrapper deleted six residual mux objects, found all eight exact
mux/slot prefixes empty immediately and after 31 seconds, and removed every
private config copy. Raw reports are ignored under
`.skirk-runs/metadata-16k-up16m-20261115/`. The two directions together verified
all eight file bodies and 96 small responses, published 43 objects over the
old 8 KiB sealed cap, and removed 22 residual mux objects with zero cleanup
failures. Sanitized arm-level values are retained in the adjacent
[JSON evidence](metadata-16k-live-2026-11-15.json).

## Decision

Keep the negotiated larger cap opt-in. This completes a full-proxy check that
the path is exercised, byte-correct, bounded, compatible in deterministic
tests, and cleaned exactly in these eight live arms. Both pairs used fewer
estimated units with the larger cap, and their pair-median text figures were
lower. The downlink bulk rate was almost unchanged and the uplink comparison
included a slow control. More runs are needed before claiming a repeatable
latency or throughput improvement, and this cap is independent of async-slot
or alternate discovery experiments.

## Limits

This is a one-client, short mixed workload. The 16 KiB bound includes the mux
envelope and authentication bytes; it does not fit every full 16 KiB TLS record
plus framing. It does not change the separate 8 KiB dual-write inline limit.
Larger/sustained workloads, multiple clients, real messenger traffic, and
platform VPN behavior remain separate evaluations. Negotiation and local
receive caps retain compatibility; no new default is promoted by this report.
