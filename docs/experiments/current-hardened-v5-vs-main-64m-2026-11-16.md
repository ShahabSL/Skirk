# Hardened muxv5 versus clean main: sustained 64 MiB mixed traffic

This is an exact-binary, same-account comparison on the approved secondary
Google Drive test mailbox. Both binaries used the pinned `www.google.com` front,
the same profile and host, serial arms, and a fresh session namespace per arm.
The comparison is **not** a universal speed limit or a 200,000-user capacity
test. No production user mailbox was used.

The current binary has SHA-256
`6f1b18f869f71d390243a5cbbd7b546481f1f07b8e3e2560169b9bc1d11ed727`
and includes the corrected adaptive socket buffer, global inbound budgets,
modern Drive rate-error handling, and media-response cap. The clean `main`
binary has SHA-256
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`.
The later commit changing the synthetic relay test fixture does not change
either measured binary.

Each arm transferred one exact-hash 64 MiB file and 20 verified persistent
text echoes scheduled every 250 ms, starting one second after bulk began and
pipelined without waiting for the previous echo. The workload used the same
Google route and client configuration in both arms; local safety guards were
125,000 estimated units per rolling minute, 150,000 per arm, and 600,000 per
cohort. These are local estimates, not Google's billed quota counters. The
download order was current/main/main/current; upload was
main/current/current/main. The benchmark and cleanup ran serially.

| Direction and metric | Clean `main` median (two arms) | Hardened v5 median (two arms) | Change |
| --- | ---: | ---: | ---: |
| Download, scheduled text p95 | 7.177 s | 6.995 s | 2.5% lower |
| Download, file throughput | 47.385 Mb/s | 48.134 Mb/s | 1.6% higher |
| Download, estimated warmup + tail units | 26,860 | 27,135 | 1.0% higher |
| Upload, scheduled text p95 | 7.757 s | 6.994 s | 9.8% lower |
| Upload, file throughput | 24.716 Mb/s | 34.628 Mb/s | 40.1% higher |
| Upload, estimated warmup + tail units | 29,810 | 26,010 | 12.7% lower |

All eight arms completed the exact file and 20/20 text echoes, with all 20
echoes overlapping active bulk in each arm, zero reported Drive API errors or
stalls, and all owned client, exit, origin, and benchmark processes stopped.
Per-arm observed results show substantial variance:

| Arm | Scheduled text p95 | File throughput | Estimated units |
| --- | ---: | ---: | ---: |
| Download current 0 | 8.120 s | 41.417 Mb/s | 27,410 |
| Download main 1 | 7.445 s | 39.308 Mb/s | 27,610 |
| Download main 2 | 6.909 s | 55.462 Mb/s | 26,110 |
| Download current 3 | 5.870 s | 54.851 Mb/s | 26,860 |
| Upload main 0 | 7.100 s | 30.117 Mb/s | 25,810 |
| Upload current 1 | 7.162 s | 30.232 Mb/s | 25,260 |
| Upload current 2 | 6.825 s | 39.024 Mb/s | 26,760 |
| Upload main 3 | 8.415 s | 19.316 Mb/s | 33,810 |

The upload aggregate is particularly sensitive to its slow final `main` arm.
These two-arm medians and the earlier upload cohorts that reversed the ranking
do **not** establish a stable product speed win. The result does establish
that the hardened source completed this longer mixed workload without an
observed integrity, cleanup, stall, or quota-error failure. The next fair
carrier comparison must retain the same sustained workload and route, test
both directions, and include cold-start and restart behavior.

Raw local evidence is under
`.skirk-runs/v5-hardened-main-64-down-20261116-01/` and
`.skirk-runs/v5-hardened-main-64-up-20261116-01/`. Both cohorts used exact
`muxv4/<session>/` and `muxv5/<session>/` cleanup only; the ledgers record the
immediate and delayed (31-second) post-delete scans. No unrelated Drive
objects were selected.
