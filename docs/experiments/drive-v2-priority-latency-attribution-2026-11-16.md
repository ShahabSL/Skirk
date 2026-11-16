# Drive v2 priority latency attribution plan

Status: isolated test-only instrumentation; no new Google timing or product
behavior. The completed v2/control live pairs remain in the
[pipelined comparison](pipelined-live-comparisons-2026-11-16.md).

The live v2 arms used 100 ms active metadata polls and reported text p95 near
two seconds. A sender's `fallback_attempts` includes records that were too
large or otherwise ineligible for the inline priority queue, so that counter
cannot explain the forty text frames. More frequent polling would spend quota
without locating the actual delay.

The optional trace added in this isolated branch records a finite sequence of
events from both peers in the **same** test process. Every event has a
per-run HMAC of the Drive object name, direction, priority sequence, phase,
and monotonic elapsed milliseconds. Admission events also retain at most four
numeric mux stream IDs and the total stream count to permit correlation with
the private mux log. No name, sealed body, OAuth token, file ID, route address,
or HMAC key is exported. The trace holds at most 1,024 events, copies snapshots,
and reports its dropped count; a run with drops cannot claim complete
per-claim attribution. It is absent from ordinary product binaries.

The phases are `queued_inline`, `immutable_only`, `patch_started`,
`patch_completed`, `peer_metadata_observed`, `receiver_processed`,
`sender_processing_ack`, `fallback_started`, `fallback_completed`,
`fallback_error`, and `peer_physical_observed`. `patch_completed` includes a
byte-identical lost-response readback that proved commitment. Pair events by
HMAC claim and direction; never equate a two-second fallback timer with the
text p95 merely because the values look alike. A full path may be observed in
a different order if a peer sees the authenticated PATCH before its HTTP
response or if physical fallback wins. Stream IDs identify multiplexed TCP
streams, not individual TLS frames; the private workload/mux log must establish
which stream carried the text series before attributing its tail.

## One bounded live attribution arm

If a live follow-up is run, freeze this source and executable hash, retain the
existing 16 MiB down/up plus one warm and forty pipelined 64-byte frames, the
approved secondary account, fresh owned v2 files, pinned `www.google.com`
front, and the existing 280,000 conservative units/minute admission with
cleanup reserve. Run **one** v2 arm to inspect the trace first. Require exact
file hashes, all forty-one replies, zero unexpected Drive errors or guard
denials, joined writers, exact object absence immediately and after 31 seconds,
and `dropped=0` before any per-claim latency analysis. The experimental five
minute epoch and single-host time base limit interpretation. Existing A/B
cohorts are not reclassified by this new instrumentation.

Only if the trace shows a repeatable avoidable phase should a separate frozen
binary compare one controlled change in candidate/control/control/candidate
order. For example, compare 1 s versus 2 s fallback deadlines **without**
changing poll cadence, API guard, run length, payloads, or cleanup. Repeated
fallback may consume more Drive calls and bytes; the experiment stops on its
original local guard. Record all per-arm text p95, file goodput, phase timings,
fallback identity counts, expected 412s, errors, minute reservations and exact
cleanup. A failure or two small positive arms do not establish a universal
latency optimum or 200,000-user capacity.

## Single-arm attribution result — separate November 16 campaign F

The root ran one bounded live arm from exact source `ef080d8`, test binary
SHA-256 `35af8a37b885779c3bb072fafbce4ca55fe7c6b7295ac4d494229b7a892e2bc3`.
The private `.skirk-runs/drive-v2-trace-20261116f/` ledger is complete: 1/1
arm passed, both seeded 16 MiB files and all 41 text echoes matched, the
origin saw one GET/POST and no bad bodies, both writers joined, and both exact
owned Drive prefixes were empty immediately and after 31 seconds. Borrowed
tokens were removed. The local guard saw 651 Drive calls, a conservative
244,160-unit whole-arm upper and 199,480-unit rolling-minute upper; there
were no guard denials, timeouts, transport errors or unexpected provider
statuses. Four conditional 412s and 32 cleanup 404s were expected and remain
counted in `api_errors`. No Firestore resource was used.

This arm's file rates were 9.489 Mb/s down and 31.778 Mb/s up. The twenty-frame
scheduled text p95 was 2.290 s during download and 2.481 s during upload;
the warm 64-byte frame took 1.344 s. It is a single diagnostic arm with a
different instrumented binary from the two earlier v2/control pairs, not a
new paired performance comparison.

The finite trace recorded 385 events, `dropped=0`: 56 inline claims and ten
`immutable_only` claims. All ten fallback starts belonged to those ten
physical-only claims; **zero inline claims started fallback**. Fifty-five of
56 inline claims had a sender-side processing ACK before shutdown. Across
inline claims, the per-claim phase intervals were:

| Interval | Count | Median | Nearest-rank p95 |
| --- | ---: | ---: | ---: |
| Inline enqueue → PATCH start | 56 | 144 ms | 311 ms |
| PATCH start → PATCH completion/readback | 56 | 318 ms | 453 ms |
| Inline enqueue → peer metadata observation | 56 | 559 ms | 935 ms |
| Peer observation → receiver mux processing | 56 | 0.1 ms | 0.2 ms |
| Receiver processing → sender processing ACK | 55 | 490 ms | 810 ms |
| Inline enqueue → sender processing ACK | 55 | 1,037 ms | 1,590 ms |
| Physical-only admission → receiver processing | 10 | 1,580 ms | 2,119 ms |

The peer observed twelve inline claims before the sender's PATCH response
completed; this is why subtracting `patch_completed` from peer observation can
be negative. The dominant stream's private mux close counters were about
3.7 KiB sent and 6.1 KiB received, identifying the persistent text TCP
stream; the other priority-bearing stream carried about 16.8 MiB each way.
The text stream had 52 inline claims and all ten physical-only claims. At
**every** physical-only admission, exactly four earlier inline claims on that
same stream remained unacknowledged, and there were only four globally. The
corresponding priority uploads in the private mux log were 169–2,555 bytes,
below the 16 KiB inline payload cap. In the code's admission order, the
per-stream-four check precedes peer-credit and encoded-size checks, while
the global queue holds 32; these observations identify that window as the
reason the ten small text-stream claims took immediate physical fallback.

The sender's processing ACK frees inline per-stream credit; it does not block
all later priority sends. When four claims are outstanding, `PutObject`
selects physical fallback immediately, and mux publication completion does
not wait for the processing ACK. ACK latency can therefore change the carrier
chosen for later text data. The 2.290/2.481 s values are complete TLS-frame
round trips involving both directions and scheduling; a mux claim may carry
multiple frames. This trace does not map any individual frame to a specific
fallback or prove that increasing the window improves p95. It does show a
discriminating next experiment: retain the same poll cadence, encoded 16 KiB
cap, global queue, fallback deadline and quota guard while comparing the
per-stream inline window of four versus eight with a frozen same-binary pair.

## Four-arm window comparison — separate November 16 campaign G

The root then ran a bounded 8/4/4/8 comparison from exact test-only source
`5c1039b0cf3e7fa4da7ba07dc8ea07bb3116c19a` and one binary SHA-256
`11e4b9ad55d577b17d70e7876bd0174d8aa593526ec0a759515a3b71980a8769`.
The private `.skirk-runs/drive-v2-window-20261116g/` ledger says 4/4 arms
completed. Each used fresh owned Drive v2 files, pinned `www.google.com`,
the same 100 ms active poll setting, 16 KiB encoded metadata cap, global
queue of 32, 4,096-claim lifetime cap, fallback behavior and 280,000-unit
rolling-minute guard. The only selected candidate setting was
`priority_stream_window: 8`; control used 4. Every arm matched the exact
16 MiB file in both directions and all 41 text echoes, had no origin bad
bodies, joined both writers, verified Drive cleanup, and recorded zero guard
denials, timeouts, transport errors, unexpected provider errors and trace
drops. The four conditional 412s per arm are expected. Cleanup 404s are
also expected and remain counted in `api_errors`.

The text columns are nearest-rank p95 of twenty 64-byte TLS frames in each
bulk direction, using scheduled-to-completion time. File throughput is
16 MiB × 8 / file elapsed seconds, in decimal Mb/s. `Upper` is the local
conservative Drive quota reservation, not billed or observed bytes.

| Arm, in run order | Window | Warm frame | Down text p95 | Up text p95 | File down | File up |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| First | 8 | 1,677 ms | 1,552 ms | 1,552 ms | 23.91 Mb/s | 36.10 Mb/s |
| Second | 4 | 1,433 ms | 2,547 ms | 2,240 ms | 25.03 Mb/s | 38.26 Mb/s |
| Third | 4 | 1,036 ms | 1,899 ms | 2,039 ms | 23.57 Mb/s | 35.95 Mb/s |
| Fourth | 8 | 1,180 ms | 1,358 ms | 1,444 ms | 24.90 Mb/s | 35.37 Mb/s |

| Arm | Visible Drive calls | Whole-arm upper units | Rolling-minute upper units | Published physical fallbacks | API errors (412 + cleanup 404) | Cleanup |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| First, window 8 | 564 | 209,000 | 206,320 | 0 | 24 (4 + 20) | Verified |
| Second, window 4 | 643 | 228,120 | 223,720 | 7 | 38 (4 + 34) | Verified |
| Third, window 4 | 620 | 236,680 | 225,720 | 8 | 45 (4 + 41) | Verified |
| Fourth, window 8 | 590 | 207,720 | 204,200 | 0 | 26 (4 + 22) | Verified |

With two arms per setting, the descriptive median is the midpoint of the two
observations. Relative to window 4, window 8 had down text p95
1,455 versus 2,223 ms (**34.6% lower**) and up text p95 1,498 versus
2,140 ms (**30.0% lower**). Median file rates were 24.41 versus 24.30 Mb/s
down (**0.4% higher**) and 35.74 versus 37.10 Mb/s up (**3.7% lower**).
The warm-frame median was 1,429 versus 1,234 ms (**15.7% higher**). Median
visible calls were 577 versus 631.5 (**8.6% lower**); whole-arm upper
units 208,360 versus 232,400 (**10.3% lower**); rolling-minute upper
205,260 versus 224,720 (**8.7% lower**). Window 8 had zero physical
fallbacks in both arms versus seven and eight with window 4. The measured
tail improvement and disappearance of fallback agree with the earlier
four-outstanding-claims attribution, but this run still does not map a
specific TLS frame to its claim or isolate every scheduling contribution.

This is a four-arm result on one secondary account, one network path, one
payload schedule and a mostly persistent text stream alongside bulk transfer.
It does not establish a universal latency optimum, fairness under many
simultaneous active streams, or capacity for 200,000 users. The local tests
prove that a second stream can enter the global queue while one has eight
outstanding claims and that the 16 KiB cap still forces immutable fallback;
they do not replace a multi-client live fairness and quota study. Within the
measured workload, window 8 is the better text-tail candidate, with no
measured file-download penalty and a small file-upload reduction.
