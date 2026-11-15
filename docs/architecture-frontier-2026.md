# Drive transport architecture frontier — November 2026

This record answers the architecture question with measured evidence. Work is
isolated from `main`; the public default remains the user-selected pinned
`www.google.com` front and muxv4. All live traffic below used only the isolated
personal test mailbox. Each arm had a fresh session, exact payload
verification, both-end quota accounting, and an exact-session cleanup sweep.
Raw profiles and logs remain ignored under `.skirk-runs/`.

## Architectural decision

There is **no proven global optimum**. The default still gives seconds of text
delay when a messenger keeps one connection alive past its first 64 KiB. A
bounded priority reentry plus metadata-only carrier is a promising experiment,
not a release default. Google Drive publish, visibility, discovery, and
response-body delay remain in the critical path.

Skirk carries opaque TCP bytes. It cannot reliably distinguish a chat message
from an attachment inside encrypted application traffic. It can infer a quiet,
small flow and schedule its next frames differently. TCP preserves order: if an
application puts file bytes before text on the *same* connection, Skirk cannot
deliver the later text ahead of those bytes. The mixed workloads below use a
long-lived text connection alongside a separate file connection, as many
applications do; this is an assumption that a real app trace must check.

## Workload and measurements

The newer persistent benchmark first sends and byte-verifies a 128 KiB frame
on one SOCKS TCP connection, forcing muxv4 past its permanent 64 KiB priority
budget. It then sends distinct 64-byte frames during a separate verified 16 MiB
file upload or download. The pipelined mode writes 12 text frames at intended
250 ms intervals without waiting for replies, then verifies each ordered echo.
Scheduled p95 includes delay from the intended send time. The file body is
checked by length and SHA-256. A completed arm requires every text and file
body to verify; a fast text result cannot compensate for a stalled file.

All live arms used the pinned front and the same source kit. Estimated Drive
units include both peers, warmup, and an eight-second tail; they are not Cloud
Console billing telemetry. The short arms and changing Google latency do not
establish population percentiles or a maximum throughput.

| Live workload | Baseline | Candidate | What it shows |
| --- | --- | --- | --- |
| 32 MiB **upload**, 24 new-stream 64-byte POST messages, two ABBA pairs | Scheduled text p95 median 5.613 s; 15.363 Mb/s bulk; 34,405 estimated units | Inline sealed priority bytes in Drive metadata: 3.154 s; 15.740 Mb/s; 20,605 units | All 96 messages and four file bodies verified. Inline saved a media read and about 40% of estimated units in this sample. |
| 16 MiB **upload**, six serial echoes on a previously demoted connection | Two complete controls: 29.615 and 24.589 s scheduled text p95 | Sparse reentry: one complete arm 11.466 s; the second was interrupted by two 28 s normal-upload timeouts | A permanent stream demotion is a user-visible blind spot. Sparse reentry is not established as a reliable winner. |
| 16 MiB **upload**, 12 pipelined echoes, baseline/epoch/epoch/baseline | One complete control: 7.436 s text p95, 11.449 Mb/s; opening control file stalled | Both epoch arms complete: 7.045 and 3.924 s text p95; 7.598 and 11.355 Mb/s | All 48 text echoes verified. The stalled control's exit media GETs took 13.771 and 17.487 s, with zero API errors; this run cannot establish a bulk-rate win. |
| 16 MiB **upload**, 12 pipelined echoes, epoch/epoch+metadata/epoch+metadata/epoch | Epoch median: 4.050 s text p95; 13.501 Mb/s; 20,385 units | Epoch+metadata median: 2.829 s; 10.448 Mb/s; 17,135 units | All four file bodies and 48 echoes verified. Text and units improved; file rate was lower in both metadata arms, so the combination is not a clear upload winner. |
| 16 MiB **download**, 12 pipelined echoes, baseline/epoch+metadata/epoch+metadata/baseline | Median text p95 6.857 s; bulk 12.136 Mb/s; 19,460 units | Median text p95 **3.833 s**; bulk **13.763 Mb/s**; **16,260 units** | All four file bodies and 48 echoes verified, with zero Drive API errors. The strongest complete live signal: text 44% faster, file 13% faster, estimated units 16% lower in this small sample. |
| 16 MiB **download**, same workload after epoch-cap fix, epoch+metadata/baseline/baseline/epoch+metadata | Median text p95 6.603 s; bulk 10.671 Mb/s; 20,410 units | Median text p95 **2.608 s**; bulk **16.600 Mb/s**; **15,835 units** | All 48 echoes and four files verified, zero API errors or observed receive gaps. Text p95 was 60% lower and estimated units 22% lower in this pair. |
| 16 MiB **upload**, same corrected binary, epoch+metadata/baseline/baseline/epoch+metadata | Median text p95 7.911 s; bulk 11.928 Mb/s; 16,510 units | Median text p95 **3.755 s**; bulk **14.882 Mb/s**; **16,510 units** | All 48 echoes and four files verified, zero API errors or observed receive gaps. Text p95 was 53% lower without measured unit growth in this pair. |

The pipelined epoch candidate uses at most 4 KiB per requalified frame, an
eight-object burst and four-object-per-second process budget, and a per-stream
interaction window. Its local MemoryStore ABBA with equal verified 6 MiB bulk
showed text p95 medians of 2.25 s baseline and 1.03 s epoch, bulk 7.33 and
7.20 Mb/s, and about 10% more estimated units for epoch. This is a scheduling
simulation, not live Drive throughput.

The first live epoch binary permitted a new 16 KiB window after a 750 ms idle
gap even when the original six-second window had not expired. Isolated commit
`2a79afe` fixes this bypass and passes the full Go suite, vet, and focused race
tests. The two corrected 16 MiB ABBA runs above used the fixed binary. One
earlier live receive gap was repaired successfully; a completed normal upload
can still become visible to the receiver after a newer priority object. The
corrected 16 MiB arms had no observed gap, but the process budget is not shared
across devices using one account.

### Larger-file and multi-client gates

The 100 MiB file tests stopped the apparent short-workload win from becoming
a default recommendation:

| Live 100 MiB workload | Default | Corrected epoch+metadata | Verdict |
| --- | --- | --- | --- |
| Upload, no hedge | One complete control: 23.573 Mb/s, text p95 5.953 s | Two 4 MiB Google uploads wrote their bodies in about 2.3 s, then received no response before a 30 s deadline; the API-error guard stopped the arm | Incomplete candidate; no throughput claim. |
| Upload, three-second idempotent hedge | Two controls complete at 24.864/24.238 Mb/s, text p95 7.015/7.796 s | First arm stalled after 66 s with all text verified; second completed at 23.018 Mb/s, text p95 3.778 s. The stalled arm started 28 duplicate 4 MiB upload legs and used 42,710 units | Hedge did not guarantee file progress and amplified work; reject as a default. |
| Download | Control completed at 16.485 Mb/s, text p95 17.132 s | All text verified at 6.294 s p95, but the file missed the progress gate | Incomplete candidate. Both arms had very slow media reads, about 14–15 s p95. |

The 100 MiB arms confirm that a faster text path can coexist with an
unacceptable file wait. An HTTP trace separated local queueing from Google's
response: the failed unhedged uploads had an acquired reused connection and a
finished request write, then no response headers. A stalled file with zero API
errors is still a failed workload. These short failures do not prove that the
priority rule alone caused the Google delay, but they disqualify default
promotion now.

Private raw summaries and traces for the corrected binary are under
`.skirk-runs/architecture-pipeline-epoch-cap-{down-16m,up-retry-16m,up-100m,hedge-up-100m,down-100m}-20261114/`.
The local multi-client harness is isolated at commit `1359c0e`.

The isolated MemoryStore stress harness used five or ten distinct clients on
one shared mailbox, pipelined eight text frames per client, and SHA-verified
8 × 4 MiB bulk responses per client. Two runs of each mode verified every text
and 160/320 MiB bulk with zero receive gaps, transport failures, or final
objects. At five clients, text p95 was 1.993 s default versus 1.493/1.742 s
epoch, with epoch using 7–10% more estimated units and 4–5% less bulk rate.
At ten clients, text p95 was 1.894/1.997 s default versus 1.652/1.744 s
epoch; estimated units rose roughly 5–9%, and bulk-rate ranges overlapped.
Cleanup needed 37–46 s at ten clients. The simulation lacks real Drive
visibility, quota enforcement, and class-index pollution; it is a capacity
warning, not a 200,000-user proof.

The live five-client upload harness then ran five distinct clients against one
fresh session and exit, with 16 MiB per client and six pipelined 64-byte text
echoes each. A 128 KiB verified prelude had already demoted every text stream.
Each arm required all 80 MiB of file bodies, all 30 echoes, simultaneous file
transfers, zero origin hash failures, and exact-session cleanup. Two
order-reversed pairs used the same binary and pinned front, with a 65-second
quota cooldown between arms. A 60-second low-speed window was necessary for this buffered POST: with
the initial 25-second window, one file in each mode was canceled at about 32
seconds, although curl had sent its full body to the local SOCKS proxy and
Drive uploads were still in flight. This was a genuine 25-second no-response
UX failure, not evidence of a mux deadlock or a candidate-only failure.

| Live five-client upload | Default | Corrected epoch+metadata |
| --- | ---: | ---: |
| Verified file/text transfers, both pairs | 10 × 16 MiB; 60/60 echoes | 10 × 16 MiB; 60/60 echoes |
| Overlapping text scheduled p95, pair 1 / pair 2 | **8.214 / 7.320 s** | **5.211 / 4.984 s** |
| Aggregate file goodput, pair 1 / pair 2 | **26.421 / 23.618 Mb/s** | **22.293 / 18.594 Mb/s** |
| Estimated Drive units, both peers, pair 1 / pair 2 | **73,380 / 78,630** | **66,730 / 72,280** |
| Drive API errors / final session objects | 0 / 0 | 0 / 0 |

Across these two complete pairs, candidate pair-median text p95 was **5.098 s**
versus **7.767 s** default (34% lower), and estimated units were **69,505**
versus **76,005** (9% lower). Pair-median aggregate file goodput was
**20.444 Mb/s** versus **25.020 Mb/s** default (18% lower). This repeated
tradeoff prevents a default promotion. The observations do not isolate the
priority scheduler from the metadata-only switch or changing Google response
times. Both comparisons used below 79,000 estimated units per arm; their
maximum observed 60-second rolling counts were 75,080 and 78,630, below the
100,000-unit test guard. The project/user published limits are much higher,
but this short shared-account run cannot establish sustained capacity or how
many people the service supports. Delayed exact-session sweeps found zero
objects from all four arms. Private details are under
`.skirk-runs/architecture-five-client-longstall{,-reverse}-20261114/`.

## Drive class discovery pilot

Google documents `name contains` as **prefix-only** for file names, whereas
the current query also asks for an interior `/p0/` or `/p1/` marker. The
isolated pilot created eight p0 and 100 newer p1 production-shaped objects,
tagged with a public `properties.skirkClass` value in the existing create
request. A broad control query verified every exact ID. The legacy p1 query
returned all 108 objects in two pages; a property p1 query returned the exact
100 p1 objects in one page. Local name validation would discard the eight
false matches, but their page still costs a list call and can crowd a bounded
scan. Both p0 queries found the exact eight p0 objects in one page in this
sample. Three post-load timing samples are too few to claim that property
search is faster.

The probe-only implementation is commit `9f2bf7c` in an isolated worktree.
Production discovery is unchanged because a property-only query would miss
untagged objects from older peers. It needs a version/capability handshake or
a bounded broad-prefix fallback, followed by a larger pagination and
visibility test. The pilot used 17,190 estimated units; all 108 objects were
deleted and missing by ID immediately and after a delay, with zero unique-prefix
residue. See Google's [search terms](https://developers.google.com/workspace/drive/api/guides/ref-search-terms)
and [custom properties](https://developers.google.com/workspace/drive/api/guides/properties).

## Scale and alternative architecture

[Google's current Drive limits](https://developers.google.com/workspace/drive/api/guides/limits)
are 1,000,000 units per minute per project and 325,000 per minute per user per
project. A list costs 100, a file create or delete 50, and a media GET 200.
Google also publishes a 1 TB daily project egress threshold before charges and
a 400-million-unit daily project billing threshold; these are not substitute
per-minute throughput guarantees. They deserve separate accounting in any
large deployment.
For 200,000 simultaneously polling users, one list per second each would be
1.2 **billion** units per minute, 1,200 times one project's limit. Even one
list per user per minute would be 20 million units per minute. Self-managed
quota projects distribute project load, but do not prove 200,000-user service
capacity, per-account stability, or acceptable setup friction.

Known-ID GETs previously added speculative reads and lost to one prefix list;
the Workspace Drive Events path adds billing, Pub/Sub, subscription renewal,
and delivery uncertainty. Google says event generation may take minutes, so
it is better treated as an idle-wakeup research path than a proven hot-text
carrier. Resumable uploads add a setup round trip for small frames, and batch
requests do not discount quota or include media bodies. A distinct Pub/Sub
StreamingPull control plane might reduce text polling delay, while keeping
Drive for bulk, but would be a different product mode: pinned-front reachability,
billing, OAuth/IAM, and sharded 200,000-user operation are untested.

A more radical small-control design is a pair of fixed Drive files, one writer
per direction. Each metadata PATCH would store a bounded cumulative encrypted
queue of unacknowledged frames in the file description; the peer would poll
its known file ID with `files.get` metadata (5 units) instead of discovering
new objects with `files.list` (100 units). The latest snapshot must retain
every unacknowledged frame, because Drive versions can advance across polls.
Peer acknowledgements would ride the opposite file. Earlier live **media**
update-ring experiments reached only 7.85–13.87 Mb/s with a 74.2-second
16 MiB upload p95; whole-file updates are not a proven bulk replacement.

A separate fixed-**metadata** primitive probe on the pinned front created one
test-owned file, performed 16 sequential 64-byte synthetic description
updates, and read each from an independent HTTP pool before starting the next.
All 16 exact values and versions were observed, with zero stale reads or API
errors; exact-ID deletion and two 404 checks succeeded. Median PATCH time was
297 ms (p95 428 ms), and median publish-to-observed time was **447 ms** (p95
**966 ms**). It used 16 PATCH and 19 GET calls, plus setup/cleanup, under a
conservative **1,945-unit** accounting bound. The private result is
`.skirk-runs/mutable-slot-live-20261114.json`. These are API primitive timings,
not messenger end-to-end latency. The sequential probe cannot detect skipped
versions or verify ACK/replay; a cumulative-log pause/restart and mixed-traffic
test are required before implementing the runtime architecture. The earlier
immutable metadata-only create/list primitive had a 508 ms median in a
different nine-file sample, so the two cannot be treated as a matched latency
comparison.

The next bounded probe used **two** fixed files, one writer each for data and
cumulative ACKs. It scheduled 20 distinct 64-byte frames every 250 ms without
waiting for each read, paused the reader for one second after frame ten, and
retained all unacknowledged frames in every new description snapshot. The
reader skipped **nine Drive versions** but verified and acknowledged all
20 frames in order. Scheduled receive p50/p95, including the forced pause,
was **0.859/1.788 s**. Fifteen data PATCHes, six ACK PATCHes, and 28 GETs
completed under a conservative **3,240-unit** accounting bound. Both files
were deleted and confirmed absent twice by ID; no recovery ledger remained.
The result is private at `.skirk-runs/ack-slot-live-20261114.json`, and the
probe and version-skip tests are under `tools/ack_slot_probe/`. HMACs on
synthetic frames establish this probe's integrity; production would use the
existing AEAD, device ownership and replay defenses. It is a queue
feasibility result, not a full Skirk, file-mix, or 200,000-user result.

At 4 Hz polling in both directions, the two known-ID GETs alone would cost
2,400 units per minute per active pair. Four one-way metadata PATCHes per
second add 12,000 units per minute before acknowledgements, retries, or other
traffic. A single shared 1,000,000-unit project cannot carry 200,000 such
active pairs; self-managed projects distribute project quota but add setup and
account-level limits. A fixed slot should therefore be considered only for
bounded interactive traffic, alongside immutable media objects for bulk.

## Platform validation on this branch

Fresh current-branch builds passed the full preflight, Android debug
sidecar/APK build, iOS Simulator build and two UI tests, macOS ARM64 Tauri
bundle, and Windows x64 GUI/sidecar cross-compile with portable-ZIP CRC and
contents checks. Logs are under
`.skirk-runs/platform-build-current-20261114/`. Runtime evidence remains
narrower: one live HTTPS request through Android VPN on a Pixel emulator, iOS
Simulator Connect stopped before the packet extension, macOS ran local proxy
mode, and Windows was not run natively. None of those app builds exercised the
new opt-in scheduler or fixed-slot probes through a platform UI. Physical
iPhone packet forwarding, sustained Android browser/video/bulk traffic,
Windows runtime, and signed macOS distribution remain unverified.

## Promotion gate

Keep the proven default while testing the corrected epoch and metadata-only
combination against both upload and download. A release decision needs
repeatable larger mixed transfers, 5–10 clients and quota pressure, cleanup
under backlog, sustained multi-client operation, restart/reconnect, browser and video behavior, Android VPN on a
real device, an iPhone packet tunnel, and native Windows execution. The
selected Direct or Google-front route must remain the user's choice, with
`www.google.com` pinned front as the default.
