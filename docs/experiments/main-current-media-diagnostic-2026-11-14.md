# Main/current mixed-download diagnostic, November 14, 2026

The fresh pinned-front 16 MiB download ABBA at
`.skirk-runs/main-vs-current-16m-20261114-02/` used a binary built from
`main` commit `2c52ff8` and the current default binary from `a237c69`.
Each arm sent 12 new-stream small messages alongside one exact-hash file.
All four arms completed with no Drive API errors. The current binary's two
middle arms had worse small-request and file results; this diagnostic locates
the observed delay without assigning a cause.

| Arm (start, local) | Small request p95 | File goodput | Client bulk media GET median (9 calls) | Client priority GET median | Client priority list median | Client normal list median | Exit bulk PUT median (9 calls) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| main, 22:04:03 | 4.459 s | 12.684 Mb/s | 0.755 s | 0.261 s | 0.188 s | 0.187 s | 2.345 s |
| current, 22:04:32 | 7.400 s | 9.000 Mb/s | 3.265 s | 0.312 s | 0.195 s | 0.205 s | 2.359 s |
| current, 22:05:07 | 7.332 s | 8.988 Mb/s | 2.666 s | 0.326 s | 0.198 s | 0.206 s | 3.056 s |
| main, 22:05:41 | 5.019 s | 10.739 Mb/s | 1.869 s | 0.320 s | 0.189 s | 0.199 s | 2.170 s |

The delay is concentrated in bulk media GET duration: the two current arms
downloaded nine bulk objects and 16 MiB total, with similarly sized chunks,
but took longer per GET. Priority GET, priority upload, and
list medians were much closer. The mux normal receive window cap was six in
both binaries; no API error or quota backoff explains the difference.

Code inspection did not reveal a default-path change to the pinned Google
front HTTP/2 transport, `alt=media` URL, or media download headers. The
optional bulk HTTP pool is disabled in these arms, so both binaries use their
primary Drive HTTP client for media GETs. The current branch's p1 list page
size is 1000 rather than 100, but observed normal-list response times are
near 0.2 s in all arms; this run does not attribute the media slowdown to the
page-size change.

The **main/current/current/main** order controls linear drift, but a temporary
Google media-service or network slowdown centered on the two middle arms
could mimic a code regression. The first main arm's GET median was 0.755 s;
the last main arm's was already 1.869 s. Repeat with the reverse
**current/main/main/current** order before changing defaults. Set
`SKIRK_HTTP_TRACE=1` for the current binary to log safe connection-acquire,
request-write, and first-byte timings for slow Drive calls. That separates
connection wait, server response delay, and body transfer without logging
URLs, headers, payloads, or credentials. A same-binary 100-versus-1000 p1
page-size ABBA can separately test the page-size hypothesis.
