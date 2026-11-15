# Exact final-code versus clean-main mixed download, November 15, 2026

This bounded pinned-`www.google.com` front comparison used the unchanged
`tools/skirk_live_ab.py` harness with `main,current,current,main` order. The
clean-main binary was built from `2c52ff85` (SHA-256
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`);
the current default muxv4 binary was built from `6b7b894`, whose code tip is
`18ba457` (SHA-256
`3aec9f7e6709f32c38140b7eb126633452eebdbe7bed4e2fc227d1be0e998046`).
All experimental transport flags were off, and the current binary used its
1,000-object normal-list default. Each arm used a fresh session, one exact
SHA-verified 16 MiB download and twelve new-stream POST/ack messages scheduled
250 ms apart with two benchmark workers. `--observe` was on for both binaries;
the exit and client ran on the same Mac with the same OAuth grant. Main lacks
the newer `probe-exit` command, so the comparison used successful HTTP
requests through each exit as the common reachability proof.

| Arm | Request message p95 | Scheduled message p95 | File goodput | Estimated Drive units, both peers |
| --- | ---: | ---: | ---: | ---: |
| First main | 7.204 s | 13.952 s | 13.148 Mb/s | 23,405 |
| First current | 5.687 s | 16.245 s | 10.898 Mb/s | 22,555 |
| Second current | 5.693 s | 16.249 s | 13.937 Mb/s | 21,955 |
| Last main | 4.826 s | 13.243 s | 15.292 Mb/s | 19,405 |
| Main two-arm median | **6.015 s** | **13.597 s** | **14.220 Mb/s** | **21,405** |
| Current two-arm median | **5.690 s** | **16.247 s** | **12.418 Mb/s** | **22,255** |

All four files matched their expected SHA-256, all 48 messages completed,
and both peers recorded zero Drive API errors or active backoff. The current
request-p95 median was about 5% lower than main in this pair, reversing both
earlier completed 16 MiB comparisons. Its scheduled-p95 median was about 19%
higher and file goodput about 13% lower here. Scheduled delay includes the
benchmark's two-worker queue under a four-messages-per-second offered schedule:
its small-request queue reached about 10–13 seconds p95 in individual arms.
That queue is relevant to overload but cannot be read as one isolated Skirk
message's network delay. Four short, sequential arms do not establish a
stable latency or throughput ranking.

The previous reverse-order run's last current arm recorded seven HTTP 200
requests with first-byte waits over one second, including a 3.901-second
list and 3.831-second media GET. Connection acquisition and request write
were below 0.11 ms for those calls. This locates that arm's tail after the
request was written; it does not distinguish Google service delay from
HTTP/2 multiplexing effects. A static default-path audit found that the
pinned HTTP/2 transport and media GET headers are unchanged, and normal
media GETs still use the primary client when the separate pool is off. The
new normal-list page size, 1,000 versus main's 100, is a plausible
query-shape difference, but the earlier same-binary sparse page-size ABBA
favored 1,000 on its request-p95 median and did not isolate it as a cause.
No default was changed on the strength of these samples.

Private raw data are in
`.skirk-runs/default-perf-final-vs-main-16m-20261115-01/`. The exact-session
cleanup deleted 16 residual mux objects; all eight generated `muxv4/` and
`slotv1/` prefixes were empty immediately and after 30 seconds, with zero
failed or truncated sweeps. The four workload arms consumed an estimated
87,320 Drive units in total, before the separate exact-cleanup calls.
