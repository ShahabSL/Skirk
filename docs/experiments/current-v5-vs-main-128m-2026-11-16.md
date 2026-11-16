# Current muxv5 versus clean main: 128 MiB mixed traffic

This serial live comparison used the approved secondary Google Drive mailbox,
the pinned `www.google.com` front in both binaries, one Mac and local origin,
and a fresh session per arm. Each scheduled arm requested one exact-hash
128 MiB file and 40 verified persistent text echoes at 250 ms intervals,
starting one second after bulk and pipelined without waiting for earlier
echoes. The cohort guarded each rolling minute at 125,000 locally estimated
Drive units, each arm at 150,000, and each cohort at 600,000. The counters are
local method-weight estimates, not Google's billing meter.

The current binary SHA-256 was
`9639c94686119164de174cc0134d22e4b60bd16d4d7a979bd3bb578e1730cce1`
(root source through `a675e8e`); clean `main` was
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`.
The later bounded seen/closed-cache fix at `eb583a5` was **not** in the
measured binary. That fix changes threshold and stale-object behavior, so
these observations are not a claim about its Google performance.

## Download: complete four-arm result

Order: current, main, main, current. All four arms completed the exact file,
40/40 text echoes, and all 40 echoes overlapped active bulk. Each reported
zero Drive API errors and stalls, and every owned process stopped.

| Metric, median of two arms per binary | Clean `main` | Current v5 | Change |
| --- | ---: | ---: | ---: |
| Scheduled text p95 | 7.284 s | 6.963 s | 4.4% lower |
| Exact file throughput | 78.785 Mb/s | 67.644 Mb/s | 14.1% lower |
| Local warmup + tail estimated units | 39,835 | 40,785 | 2.4% higher |

| Arm | Text p95 | File Mb/s | Estimated units |
| --- | ---: | ---: | ---: |
| Current 0 | 7.431 s | 64.688 | 41,060 |
| Main 1 | 8.403 s | 76.379 | 39,710 |
| Main 2 | 6.166 s | 81.190 | 39,960 |
| Current 3 | 6.494 s | 70.601 | 40,510 |

The current source gave slightly lower text p95 but materially lower file
rate in this cohort. This reverses the small file-rate advantage in the
earlier [64 MiB comparison](current-hardened-v5-vs-main-64m-2026-11-16.md).
Two arms per binary and a single account/host remain too few to establish a
general speed winner or a maximum throughput ceiling.

## Upload: stopped by the declared error guard

Planned order: main, current, current, main. The first two arms finished the
exact 128 MiB file and 40/40 overlapping text echoes, with zero recorded API
errors. Their individual readings were main 49.461 Mb/s and 6.546 s text p95,
current 49.481 Mb/s and 6.925 s text p95. These are **single arms**, not a
paired result.

The second current arm then recorded two client-side Drive **upload**
`timeout` errors, both counted in the upload operation class at its 30-second
attempt deadline.
Only 7/40 scheduled text echoes succeeded, and the file did not complete or
pass its hash check. The harness raised its preset local API-error/backoff
guard and did not launch the final main arm. The local origin logged a broken
pipe while responding to an incomplete POST; it is a consequence observed
during this stopped arm, not a classified Google cause. The exit recorded
zero API errors in that arm. No upload median, speed advantage, or 128 MiB
robustness pass follows from this incomplete cohort.

The three started upload arms all recorded stopped owned processes. Exact
`muxv4/<session>/` and `muxv5/<session>/` cleanup found 46 owned objects in
the first two arms, none in the stopped third, deleted those exact objects,
and found all six checked namespaces empty immediately and 31 seconds later.
The four download namespaces likewise had exact cleanup and empty delayed
checks. No unrelated Drive prefix was selected.

Raw private observations and cleanup ledgers are in
`.skirk-runs/v5-final-main-128-down-20261116-01/` and
`.skirk-runs/v5-final-main-128-up-20261116-01/`. The stopped arm and the
reversed earlier upload rankings are part of the result, not outliers to
discard. The next product speed study would need more serial arms across
independent times and networks, plus a separately reviewed timeout-recovery
mode; it must retain exact payload and quota guards.
