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

### Exact final default versus `main`

A final pinned-front comparison used the exact merged default binary, all
experimental flags off, against clean `main` (`2c52ff85`) in
main/current/current/main order. Each arm scheduled twelve new-stream small
messages alongside one SHA-verified 16 MiB download. The first three arms
verified every byte with zero Drive API errors:

| Arm | File goodput | Scheduled small p95 | Estimated units, both peers |
| --- | ---: | ---: | ---: |
| `main` 1 | 14.494 Mb/s | 14.877 s | 26,955 |
| Current 1 | 18.749 Mb/s | 12.219 s | 22,405 |
| Current 2 | 12.915 Mb/s | 15.301 s | 25,605 |
| `main` 2 | Incomplete | Incomplete | Incomplete |

The second `main` arm hit three 28-second Drive upload timeouts on the exit;
the error guard stopped the workload. The complete arms vary enough, and the
reverse-order control is missing, so this is **not** evidence that the final
default is faster or slower than `main`. Eleven residual mux objects from the
two latter sessions were deleted by exact generated-session prefix; immediate
and delayed rechecks found zero objects in all four sessions. Private raw
evidence is under `.skirk-runs/final-main-abba-16m-20261114/`.

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

A separate five-client MemoryStore upload sensitivity model varied the normal
batch cap from 4 MiB to 2 MiB with six shared large-PUT slots and an imposed
one-second-per-MiB PUT delay. In two epoch runs each, 2 MiB batches raised
modeled file goodput from 37.9–41.2 to 43.4–44.0 Mb/s, while text p95 ranges
overlapped (1.48–1.83 versus 1.49–1.73 s). All 80 MiB and 30 echoes per run
verified. The model lacks Drive's variable backend delay and metadata-only
priority, so it does not justify a production chunk-size change. Its exact
settings and four-mode results are in
`tools/persistent_bench/UPLOAD_SIM.md`.

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
The largest snapshot in this first live run held eight 64-byte frames. A later
bidirectional probe read back an exactly 8,192-byte description twice, but did
not fill the encrypted queue to that boundary. The first result is private at
`.skirk-runs/ack-slot-live-20261114.json`, and the
probe and version-skip tests are under `tools/ack_slot_probe/`. HMACs on
synthetic frames establish this probe's integrity; production would use the
existing AEAD, device ownership and replay defenses. It is a queue
feasibility result, not a full Skirk, file-mix, or 200,000-user result. The
proposed [opt-in lane design](fixed-metadata-lane-design-2026.md) specifies
bootstrap, ACK, fallback, and cleanup gates before any mux integration.

A second bounded two-slot probe sent 20 independently scheduled 64-byte
requests and generated an echo only after each request arrived. It verified
20/20 replies and authenticated ACKs in both directions in two runs. With a
forced one-second server-reader pause, scheduled request/reply p50/p95 was
**1.299/2.145 s**; without that pause it was **1.044/1.265 s**. The readers
skipped five and two Drive versions respectively without loss. Each run first
PATCHed and read back an exact 8,192-byte description; both exact slot IDs
were deleted and twice confirmed absent, with zero API errors. Conservative
complete-arm units were 3,780 and 3,950. The probe uses synthetic metadata
records and no Skirk TCP stream or bulk transfer, so these are Drive substrate
round trips, not application message latency. Source and method are in
`tools/ack_slot_probe/README.md`; raw private summaries are under
`.skirk-runs/ack-slot-roundtrip-{live,nopause-live}-20261114.json`.

Four order-reversed no-pause arms compared 100 ms and 200 ms known-ID polling.
Their pair-median scheduled RTT p95 was **1.589 versus 1.681 s**, while median
GETs were **56 versus 40** and conservative complete-arm units **4,055 versus
3,925**. The small latency difference did not exceed ordinary run variation;
the probe retains 200 ms by default. Separate simultaneous bursts verified
**100/100** request/reply exchanges across five independent slot pairs and
**200/200** across ten, with exact cleanup, no API errors, and conservative
unit totals of **17,755** and **39,565**. Combined scheduled RTT p95 was
**2.382 s** for five pairs and **1.515 s** for ten. The later, larger burst
being faster shows these are not matched capacity curves; neither tests a real
Skirk mux or supports a 200,000-user claim. Raw private summaries are under
`.skirk-runs/ack-slot-poll-sensitivity-20261114.json` and
`.skirk-runs/ack-slot-{five,ten}-pair-20261114.json`.

The [DriveStore compatibility probe](metadata-slot-compat-2026.md) then
verified the runtime adapter's distinct API path: generated-ID metadata-only
create, independent exact-ID GET, an exact 8,192-byte PATCH/readback, and
delete with two 404s plus immediate and delayed unique-prefix sweeps. It
completed with zero unexpected API errors and 375 estimated units. This
establishes API compatibility for one file, not mux delivery safety.

The next **mixed-substrate** test ran the two-slot probe beside a real,
hash-verified 16 MiB Skirk upload on the same pinned-front account. Its
matched control sent 20 pipelined text echoes on a persistent, demoted mux TCP
stream during the same file workload. The slot text is still outside the TCP
proxy, so this is a substrate comparison, not an app-level implementation.
The slot probe's reader pause was disabled for this matched latency test;
its separate forced-pause result above covers skipped-version recovery.

| 16 MiB mixed upload, two order-reversed complete pairs | Mux text control | Fixed metadata slots |
| --- | ---: | ---: |
| Verified text / files | 40/40 echoes; 2/2 files | 40/40 frames and ACKs; 2/2 files |
| One-way scheduled text p95, pair 1 / pair 2 | **4.273 / 5.136 s** | **1.312 / 1.382 s** |
| File goodput, pair 1 / pair 2 | **12.914 / 10.508 Mb/s** | **13.987 / 12.556 Mb/s** |
| Estimated units including cleanup, pair 1 / pair 2 | **18,860 / 19,110** | **≤16,265 / ≤15,810** |
| Drive API errors / final session or slot objects | 0 / 0 | 0 / 0 |

The pair-median one-way text p95 fell from **4.705 s to 1.347 s** (71% lower)
and file goodput rose from **11.711 to 13.272 Mb/s** (13% higher) in these
short samples. The slot figures include conservative setup, PATCH, GET, ACK,
and cleanup accounting; their upper bound was about 15% below the control's
estimated units. The live slot readers skipped eight and six intermediate
Drive versions and still delivered all frames. An earlier control-first
attempt stopped on a 30-second 4 MiB Drive upload timeout before its text/file
workload completed; exact cleanup succeeded. The two complete pairs do not
establish a sustained or integrated win, and setup timing differs slightly:
the slot probe creates its files after the bulk worker starts. Raw private
evidence is under `.skirk-runs/slot-mix-upload-{001,002,003}/`.

The same mixed-substrate harness then ran **two order-reversed 16 MiB
downloads** with all 40 text frames and ACKs per mode and both file bodies
verified:

| 16 MiB mixed download, two complete pairs | Mux text control | Fixed metadata slots |
| --- | ---: | ---: |
| One-way scheduled text p95, pair 1 / pair 2 | **4.016 / 5.563 s** | **1.172 / 1.394 s** |
| File goodput, pair 1 / pair 2 | **14.003 / 13.370 Mb/s** | **16.452 / 15.262 Mb/s** |
| Estimated units including cleanup, pair 1 / pair 2 | **20,810 / 20,410** | **≤15,790 / ≤15,935** |
| Drive API errors / final session or slot objects | 0 / 0 | 0 / 0 |

Pair-median one-way text p95 was **1.283 s** for slots versus **4.789 s** for
the mux control (73% lower). Pair-median file goodput was **15.857 versus
13.687 Mb/s** (16% higher), and the conservative slot-unit bound was 23%
lower. The slot reader skipped seven and eight intermediate Drive versions
with no missing frame. Delayed exact-session sweeps found zero objects from
all four download arms. These short substrate results are the strongest
reason to build an opt-in integrated prototype, but they do not measure app
TCP behavior, bootstrap cost, sustained capacity, or a 100 MiB mixed file.
Raw private evidence is under `.skirk-runs/slot-mix-download-{001,002}/`.

### Opt-in runtime shadow gate

The next slice wires two generated-ID metadata files and an authenticated
capability handshake into the mux, behind `drive.metadata_slot_v1` (default
off). It mirrors bounded p0 DATA/FIN ciphertext into the files, but **only the
existing immutable object delivers application bytes**. This tests lifecycle,
compatibility, quota interference, and cleanup; it does not test faster app
delivery. [Shadow design and live record](metadata-slot-shadow.md) describe
its limits and safety gates.

The first pinned-front 16 MiB download control completed with 12/12 small
messages, 11.125 Mb/s file goodput, 23,855 estimated units, and zero API
errors. The first shadow arm completed with 12/12 messages, 14.953 Mb/s,
26,445 units, and 21 authenticated shadow observations across two active
peers. The second shadow arm verified 12/12 messages and 17 observations,
but its file stalled before the first byte and failed the progress gate;
its 25,025 units are for an incomplete workload and cannot be compared as
an efficiency result. Exit Drive uploads of 1 MiB objects took 16–26 seconds
in that failed arm. The control/shadow/shadow/control sequence stopped at
this failure, leaving no complete order-balanced comparison or causal
attribution. Generated session cleanup verified zero remaining slot or mux
objects. The shadow lane is not a promotion candidate from this run.

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
narrower: the [bounded Android VPN integrity smoke](android-vpn-smoke-2026-11-14.md)
verified an HTTPS page plus exact 8 MiB uploads and downloads on a Pixel
emulator. iOS Simulator Connect stopped before the packet extension; macOS ran local proxy
mode, and Windows was not run natively. None of those app builds exercised the
new opt-in scheduler or fixed-slot probes through a platform UI. Physical
iPhone packet forwarding, sustained Android browsing/video with concurrent bulk traffic,
Windows runtime, and signed macOS distribution remain unverified.
The iOS UI now gives typed recovery steps for its own `invalid_grant`, while
labeling a local Network Extension connection “VPN active” without claiming
that the remote exit is reachable.

## Promotion gate

Keep the proven default while testing the corrected epoch and metadata-only
combination against both upload and download. A release decision needs
repeatable larger mixed transfers, 5–10 clients and quota pressure, cleanup
under backlog, sustained multi-client operation, restart/reconnect, browser and video behavior, Android VPN on a
real device, an iPhone packet tunnel, and native Windows execution. The
selected Direct or Google-front route must remain the user's choice, with
`www.google.com` pinned front as the default.
The emergency storage-pressure sweep now retains `muxv4/` and opt-in
`slotv1/` objects until the 24-hour janitor age, so an 11-minute slow live
transfer is not deleted solely to free space. A separate bounded late-reply
scan after an aborted stream passed local tests but remains isolated: it adds
100–200 Drive units per unclean idle episode and still misses observed
25–30-second backend stalls.
