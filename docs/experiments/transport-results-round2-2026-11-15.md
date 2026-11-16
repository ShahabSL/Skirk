# Transport engineering results — November 15, 2026

Research is on `codex/skirk-frontier-round2-20261115`. `main` remains
`2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`. The initial frozen combined CLI is
`42e6742e93d3376a03d4354a4b172236ff1b1fc4`, SHA-256
`c6c42930544e03694fdaaaa1fd1e14e538ceb47e598250c5cc7e26e52503c9d9`.
Its build metadata records a clean tree. Pub/Sub SDK imports are test-only;
the CLI's module build information confirms they are absent from the product
binary. Later diagnostic/recovery cohorts record their own exact source and
binary hashes; they are not pooled with this executable. Nothing here is a
release or a change to the public default.

The user's product requirements are **Google.com-front compatibility and free
use**. A limited free tier is not an unlimited free service. Pub/Sub REST passed
the selected front but has only 10 GiB/month free throughput per billing
account, then published usage charges. Billing remains disabled for these
bounded probes; Pub/Sub is not a required dependency or a promoted free-product
transport. The primary work remains Drive within its free API/account limits.
The temporary Apps Script echo passed the same pinned front on both its POST
and response redirect. Its separate reliable full relay subsequently completed
all four real SOCKS/TLS mixed workloads. It remains a research implementation;
its best observed file rate is below the completed Drive examples, and it has
no same-cohort Drive control or multi-client capacity result.

## What the evidence supports

The strongest demonstrated short-workload improvements remain bounded
interactive scheduling plus encrypted metadata delivery. A new asynchronous
metadata-slot scheduler recovers some of the earlier substrate's gain in a
real persistent TCP workload. Its initial comparison is against synchronous
slots, however; it cannot be presented as a win over default mux or `main`.
The completed frozen-binary comparisons keep those controls separate. Against
clean `main`, the short mixed tests improved text latency while slightly
reducing file rate and increasing estimated units. Earlier longer and
multi-client tests did not establish a reliable joint win. The later complete
burst-recovery cohort was positive in a small sample, with an error and
estimated-unit cost.

A different priority carrier, REST Pub/Sub through the selected Google front,
also works. Its full SOCKS trial delivered twenty text responses and a 16 MiB
upload, but its download failed amid Drive upload timeouts. Fast text alone
does not make that a successful mixed transport. Reconnection also exposed
approximately 31-second one-way tails in the separate pool experiment.

There is no evidence of an absolute architectural limit or a universal winner.
The [complete ledger](../experiment-ledger-2026.md) includes older positive
results, unsuccessful protocols, incomplete runs, simulations and source-only
ideas. A [twenty-design matrix](../drive-architecture-candidates-2026-11-15.md)
records distinct API paths and the measurements that could change their rank.
It does not imply twenty finished production implementations.

## Comparable results and their limits

All rates below are decimal Mb/s. Latencies retain their original definitions:
one-way API delivery, HTTP body completion, persistent echo and scheduled
completion are different measurements. Two-arm medians and twelve-message
nearest-rank p95 values are descriptive samples, not population percentiles.

| Experiment | Control → candidate | Evidence and decision |
| --- | --- | --- |
| Earlier dual inline metadata, 32 MiB full transport | Request p95 3.249 → 2.223 s; file 36.362 → 39.788 Mb/s; estimated units 19,905 → 12,355 | A substantial earlier positive. Later larger-file stalls prevent a blanket promotion. Ledger LIVE-008/011/023. |
| Corrected interaction epoch + metadata, 16 MiB download | Scheduled persistent-text p95 6.603 → 2.608 s; file 10.671 → 16.600 Mb/s; units −22.4% | All four files and 48 echoes verified. The 100 MiB and multi-client results remain separate. [Earlier frontier](../architecture-frontier-2026.md). |
| Same corrected candidate, 16 MiB upload | Scheduled p95 7.911 → 3.755 s; file 11.928 → 14.882 Mb/s; equal estimated units | Positive small paired result; larger and five-client cases did not establish a stable joint win. |
| Fixed cumulative metadata queue beside a file | Upload one-way text p95 4.705 → 1.347 s; download 4.789 → 1.283 s; file +13%/+16% | Text was a synthetic API substrate beside a real file, not TCP messenger RTT. This motivated the new integration rather than being discarded. Ledger SLOT-007/008. |
| New asynchronous slots versus synchronous slots, 16 MiB download | Scheduled persistent-echo p95 7.448 → 4.787 s; file 13.080 → 16.465 Mb/s; units +1.4% | Full TCP bytes verified; shared interaction epochs. Offline builds/tests overlapped on the shared Mac, so local CPU was not isolated. [Exact report](metadata-slot-async-live-2026-11-15.md). |
| Same async comparison, 16 MiB upload | Scheduled p95 7.621 → 3.622 s; file 12.134 → 12.855 Mb/s; units −7.7% | Same source and CPU caveat. This is not an actual-default comparison. |
| Frozen async package versus actual same-binary default, 16 MiB download | Scheduled full-echo p95 10.105 → 4.628 s; file 11.175 → 15.748 Mb/s; units 23,135 → 23,370 | Four files, 48 echoes, four preludes and four exit proofs verified; zero API errors. No agent builds/tests overlapped; desktop background was uncontrolled. The second default arm was slower, so retain the per-arm spread rather than promising a stable 54% improvement. Eight prefixes empty immediately and after 31 s. |
| Frozen async package versus clean main, 16 MiB download | Scheduled full-echo p95 6.701 → 5.512 s; file 14.416 → 13.203 Mb/s; units 18,810 → 20,495 | Four files and 48 echoes verified, zero API errors. Text improved 17.7%, file rate fell 8.4%, units rose 9.0%. This is a tradeoff versus the actual requested baseline. |
| Frozen async package versus clean main, 16 MiB upload | Scheduled full-echo p95 7.955 → 4.635 s; file 12.653 → 12.449 Mb/s; units 18,485 → 20,120 | Four files and 48 echoes verified, zero API errors. Text improved 41.7%, file rate fell 1.6%, units rose 8.8%. |
| Negotiated metadata limit 8 → 16 KiB, 12 KiB HTTP responses | Download body p95 4.274 → 3.995 s, units 23,880 → 20,930; upload 5.882 → 3.841 s, units 27,480 → 21,155 | Eight file bodies and 96 responses verified. Upload file rates varied sharply in the controls; no universal throughput claim. [Report](metadata-16k-live-2026-11-15.md). |
| Encrypted carrier-field matrix, 2 KiB plaintext | Media create/list/verify median 1.122 s; name 0.600, description 0.553, properties 0.521 s | Two repetitions/cell; API primitive only. Metadata removes a media GET. Filename encoding showed no compelling advantage over description. [54-trial report](carrier-matrix-2026-11-15.md). |
| Pre-announced immutable ID train, corrected parallel burst | List p95 0.775 s versus train 0.829 s; core estimated units 1,105 → 465 per eight records | Lower units, slightly slower delivery. Authenticated ID-vector exchange is excluded. All exact IDs cleaned. [Report](id-train-live-2026-11-15.md). |
| Fronted Pub/Sub REST primitive | Prestarted-pull median 396 ms across four records; last two warm samples 101/72 ms | Eight AEAD records verified; one-way primitive, not app RTT. [Report](pubsub-front-2026-11-15.md). |
| Fronted Pub/Sub pull-pool experiment | Eighteen verified records; most later records 170–836 ms, restart tails 30.756–30.930 s | Worker-count medians conceal tails. Canceled pulls retaining a broker lease are an inference, not a proven cause. [All samples](pubsub-drive-hybrid-2026-11-15.md). |
| Fronted Pub/Sub priority + Drive bulk, full SOCKS | Twenty text responses verified; download-phase median 443 ms/max 1.664 s; upload-phase median 705 ms/max 1.333 s; 16 MiB upload 23.469 s | Download timed out at 180 s after 10,485,358 of 16,777,216 bytes. Seventeen Drive upload timeouts; visible transport reservations 25,570 units, below the local 40,000 active guard. An outer RoundTrip wrapper does not count hidden HTTP/2 retries or prove billed wire units. Failed full transport, despite fast text. Detailed raw report is retained in the hybrid worktree. |
| Fronted Apps Script opaque echo | Five warm 64-byte round trips: median 625 ms, range 529–914 ms; two 1 MiB echoes: 1.077/1.061 s | Twelve of twelve authenticated payloads verified through the pinned `www.google.com` front on both hops. No external fetch or TCP node was involved. This is an unloaded carrier primitive, not mixed messenger latency or sustained file throughput. [Exact report](apps-script-front-echo-live-2026-11-15.md). |
| Reliable Apps Script full SOCKS/TLS relay, 2 MiB frame scalar → packed | Download 3.498 → 4.116 Mb/s; upload 2.001 → 2.700 Mb/s; download full-echo request median 5.359 → 1.873 s; upload 14.229 → 5.811 s | All four frame/packing cells passed: eight exact 16 MiB files and 100 echoes. One observation/cell, no demotion prelude, no same-cohort Drive control. Packing increased download first-byte time at both frame sizes, and upload echo maximum was still 10.169 s in the best packed sample. [All cells, costs and cleanup](apps-script-relay-full-live-2026-11-15.md). |
| Historical throughput envelope | November single 128 MiB 74.928 Mb/s; ten × 32 MiB 78.244 Mb/s aggregate | Short, different workloads. May notes around 90 Mb/s aggregate are weaker historical evidence. These are neither the final mixed candidate's speed nor 200,000-user capacity. |
| Fronted Firestore document primitive | Warm 2 KiB create + known-ID read: 331/346 ms | Six exact AEAD states through the selected front, including 16/256 KiB cases. Discovery and a reliable queue are excluded. Fresh database removed and preceding API state restored. [Report](firestore-front-primitive-2026-11-15.md). |
| Fronted Firestore WebChannel notification, repaired observer | Five warm 2 KiB write-to-listener callbacks: median 248 ms, range 233–331 ms; cold 1.361 s | Six exact AEAD states; target removal acknowledged, child joined without forced termination, zero pending calls/events/timers. This is an API primitive, not TCP RTT. The first six-state run failed local teardown and remains separate. [Source, first failure and repair](../../experiments/firestore-listen/README.md). |
| Drive v2 metadata ETag CAS through pinned front | PATCH and PUT each accepted a current strong resource ETag and rejected impossible/old tags with 412; rapid PATCH produced four distinct tags over three back-to-back revisions | Three separate owned-file probes, exact readback and delayed cleanup. The nine-call rapid revision phase took 3.247 s, not app RTT. V3 supplied no observed write fence. Full reliable transport and cross-account behavior remain unmeasured. [Evidence](drive-v2-metadata-etag-cas-2026-11-15.md). |
| Firestore-priority/Drive-bulk first full live attempt | Exact 16 MiB down/up in 8.681/7.829 s; warm 64-byte echo 1.359 s; 20/20 download and 17/20 upload text echoes | The last three text requests were canceled when the harness's 120-active-Listen cap blocked cleanup. No complete mixed result or paired Drive control. All five residual Drive objects and the free-tier database were deleted and checked after 31 s; prior API state restored. [Failure and cleanup](firestore-hybrid-first-live-2026-11-15.md). |
| Five simultaneous clients, actual same-binary default | 80 MiB exact files + 60 echoes; aggregate 27.456 Mb/s; scheduled full-echo p95 11.342 s | Zero API errors in the completed baseline. Async candidate stopped on metadata timeouts and exposed a harness teardown bug; it has no valid aggregate or latency comparison. Exact cleanup completed, and the ownership bug is fixed. [Report](bounded-recovery-and-capacity-2026-11-15.md). |

The frozen async-versus-default upload ABBA was interrupted in its fourth
candidate arm by one 1,048,631-byte normal upload timeout after 28 seconds.
The first candidate and both controls completed; the last arm has no completed
mixed report. No two-candidate upload median is inferred. A late object and a
slot file needed explicit cleanup; a second 31-second sweep verified all eight
prefixes empty. This is a guard-interrupted comparison, not proof that the
transport could never recover after that individual API timeout.

Across the four final comparisons, sixteen arms were attempted and fifteen
completed, with fifteen exact 16 MiB files and 180 verified echoes. All 32
generated mux/slot prefixes were empty after the final delayed checks; 117
residual objects were removed, including one slot. All copied configs were
removed. The source kit was unchanged. These final runs had no deliberate
agent compiler/test overlap; normal desktop/UI background activity remained
uncontrolled. Clean-main and same-binary controls are not pooled together.

The longer, separately labelled read/upload recovery cohort completed three
exact 100 MiB files and 300 complete-arm echoes, plus eleven partial echoes
from its last arm. Its first candidate delivered 18.956 Mb/s with scheduled
full-echo p95 12.134 s and one retained metadata timeout. It continued after
both 64-PATCH writer retirements and delivered six messages beyond both
two-minute lifetimes through standard fallback. The two clean-main controls
delivered 19.776/15.387 Mb/s and p95 19.256/25.902 s. The final candidate hit a
metadata timeout and a mutable PATCH timeout before recovery could be
observed; it has no completed file or full-workload p95. There is no balanced
candidate median. All eight prefixes were empty immediately and after 31 s;
34 mux files and two slot files were removed.
[All arms and lifetime strata](sustained-read-upload-recovery-2026-11-15.md).

The separately labelled burst-recovery cohort completed four exact 100 MiB
downloads and 400 scheduled echoes in candidate/main/main/candidate order.
The candidate's two-arm descriptive medians were 25.484 versus 19.686 Mb/s
file goodput and 9.016 versus 20.545 seconds scheduled full-echo p95. Its
estimated API units were 14.40% higher and its first arm recorded six real
RPC errors, compared with zero in both controls. Both candidate arms continued
delivering messages after finite metadata-slot retirement. The controls varied
widely; two arms per side do not establish a general winner. The preceding
strict and v1/v2 incomplete or guard-stopped cohorts remain separate.
[Complete burst report](sustained-end-to-end-burst-recovery-2026-11-15.md).

A guarded Drive v3 metadata write-fence probe returned no ETag response header
for either projected fields or explicit `fields=*`. An impossible `If-Match`
on PATCH returned 200 and changed the exact owned ciphertext. This tested v3
path therefore supplied no observed server CAS; valid/stale provider-tag
phases could not run. Both test-file attempts were deleted and verified absent
immediately and after 31 seconds. The first attempt stopped on a corrected
fixture projection error and is not counted as a CAS negative. Separate v2
PATCH, PUT and rapid-revision probes then observed provider-enforced strong
ETag write fencing on fresh owned files. A feature-detected full transport is
still required before comparing application latency or replacing the default.
[ETag report](drive-metadata-etag-cas-2026-11-15.md).

The Firestore-priority/Drive-bulk research prototype passed an offline real
TLS/SOCKS/mux integration: independent random 16 MiB GET and POST, 40
distinct overlapping text echoes, lost-watch fallback, reconnect, processing
receipts, both OPEN arrival orders, and joined exact cleanup. Its fake carrier
timings are not Google measurements. The first bounded live hybrid arm carried
both complete files but failed its last three text echoes at a local watch
request cap; the paired campaign stopped and exact resource cleanup was proved.
The owner-token/IAM setup and finite Firestore free quotas keep it experimental;
a completed controlled live hybrid versus same-binary Drive comparison is still
required before a performance ranking.
[Design and proof](../../experiments/firestore-hybrid/README.md),
[first live failure](firestore-hybrid-first-live-2026-11-15.md).

The prior exact-default versus clean-main recheck also remains visible:
request p95 6.015 → 5.690 s, scheduled p95 13.597 → 16.247 s and file
14.220 → 12.418 Mb/s. Earlier complete pairs ranked differently. That
[result](default-final-vs-main-2026-11-15.md) did not establish a stable speed
win for the earlier default branch.

## Designs implemented or rejected this round

The asynchronous slot sender transfers ownership to a bounded independent
scheduler rather than occupying a lane worker while waiting for a processing
ACK. It preserves prepared ciphertext identity across slot and immutable
fallback, distinguishes publication from processing, and retains per-stream
and process queue limits. The audit also fixed stale authenticated revisions
acknowledging newer data and a stopped writer still advertising send capacity.
The primary-slot variant removed the post-publication timer, but its first live
candidate stalled; that incomplete comparison is
[retained](metadata-slot-primary-live-2026-11-15.md), with no invented median.

The larger metadata carrier negotiates 8 or 16 KiB sealed limits, uses the
minimum common peer capability, preserves old-peer media fallback, and bounds
plain and gzip-expanded list bodies. It does not claim a complete 16 KiB TLS
record fits after transport/authentication overhead.

The new [state-aware upload recovery](state-aware-upload-hedge-2026-11-15.md)
arms only after a fully written request with no response byte. It first probes
exact ID/name/size/SHA-256 commit proof. Only an unproved stalled upload may use
one same-ID spare per store, with a rolling multipart-byte budget and joined
losers. The final header-check-to-launch race remains documented. It is off by
default; live results must distinguish this recovery policy from the ordinary
helper's retry policy.

Its [live comparison](state-aware-upload-hedge-live-2026-11-15.md) completed all
four download arms, then stopped the upload ABBA after its second arm: an
ordinary control's 736-byte priority upload reached its ten-second deadline.
Five of six attempted mixed arms completed. Completed candidate arms triggered
zero commit probes, spares or duplicate reservations, so they establish no
recovery benefit. The short priority request was outside this bulk-only hedge's
scope. The download candidate's descriptive p95 and file rate were worse; keep
the option experimental rather than treating no-trigger samples as a win.

The Pub/Sub hybrid uses actual mux/SOCKS paths, authenticated identity-bound
records, processing-based broker ACK, bounded queues and nonexpiring in-process
processed tombstones. The new OPEN replay test expires the mux's closed marker
and still observes exactly one destination dial/write. It does not establish
durable exactly-once effects after process restart. Standard gRPC
StreamingPull was also tried: the selected front returned HTTP 404 before any
message. That is [one retained feasibility failure](pubsub-streaming-probe-2026-11-15.md),
not proof that every streaming route is impossible.

An independent [source audit of MasterHttpRelayVPN-RUST](master-http-relay-source-audit-2026-11-15.md)
found a real opaque Apps Script/TCP-node path, event-driven long polling and
adaptive batching. Its own client warns that a lost batch response can lose
already-drained bytes. We retain the architectural ideas without copying that
reliability flaw. The owned echo is now measured separately in the table above.
Its deployment was archived, its project moved to recoverable Trash, and a final
secret-authenticated POST through the same front returned 404 before private
settings were removed.

The test-only reliable relay uses separate upload and pull sequences, exact
cached replies after ambiguous exchanges, immutable object identity and
processing-based payload deletion. A request receipt can free an RPC reply;
it cannot discard ciphertext before mux processing. Bounded lifetime
tombstones prevent an old OPEN from dialing again after the mux's own closed
marker expires. A consumed-session marker prevents restarting an in-memory
epoch after a crash. These are finite-session properties, not durable
exactly-once destination effects across arbitrary restarts.

Real loopback HTTP/SOCKS tests completed exact incompressible 16 MiB GET and
POST bodies with concurrent text and injected lost committed responses. A
separate scalar simulation with 600 ms per carrier exchange exposed a queue
bottleneck: twelve texts offered every 250 ms grew from about 1.5 to 6.0 s
RTT during a file transfer. Those numbers are simulated, not Google latency.
Three paired scalar/batched simulations are retained in the
[reliable-relay report](apps-script-reliable-relay-client-2026-11-15.md) and
[sample data](apps-script-relay-packing-local-2026-11-15.json). Batching reduced
queue growth, but did not eliminate it; download coalescing also changed the
actual object sizes in one pair. Both remain outside the product binary and
default transport. The subsequently approved full live relay completed all
four cells with exact file hashes, successful origin request counts and all
echoes. All 96 mixed echoes completed during their file transfer. These are
small-priority-flow tests; the Drive ABBA's 128 KiB demotion prelude was not
present. Comparisons between those protocols must retain that difference.

The four live variants consumed 510 Script POSTs and 1,016 visible dispatches;
1,012 returned HTTP/2 responses. Each frozen executable recorded one closing
transport-error counter whose final trace showed owned cancellation. All four
raw counters remain unchanged. The deployment was archived, the project moved
to recoverable Trash, its exact temporary external-request grant revoked, and
the owned exit tunnel, connector token and DNS record removed. A separate
fronted inactive check returned 404; twenty copied private research files were
removed. No temporary public relay remains.

Google currently documents 20,000 URL Fetch calls/day per consumer account,
30 simultaneous executions/user and six minutes/execution. These limits are
per user, not multiplied by publishing more deployments. A fixed 15-second
idle pull would alone consume up to 5,760 fetches/day per continuously
connected client, before response overhead; adaptive idle polling must be
measured rather than assuming an unlimited central relay. Each fresh live
campaign separately reserves its actual POST allowance and conservative
HTTP replay upper bounds before launch. No billing is enabled.
[Official quotas](https://developers.google.com/apps-script/guides/services/quotas).

## Independent reasoning and practical product boundaries

Claude performed three substantive read-only architecture passes: a
constraint-led twenty-design exploration, an evidence-aware comparison and a
targeted slot integration review. Two returned pure Fable 5.2 model metadata;
the evidence-aware pass fell back to Opus and is labeled accordingly. The
constraint-led pass disclosed an automatically attached repository handbook,
so it was not perfectly blinded. Full answers, corrections and provenance are
[preserved](../claude-architecture-review-2026-11-15.md). A separately rejected
external request was not retried to bypass that service's safeguards. Codex
agents wrote the implementations and performed measurements.

Separate client-UI/lifecycle requests did not produce a completed external
audit. Two attempts returned the account usage limit without inference; one
retry was early because of an incorrect Chicago/UTC conversion. A corrected
post-reset request ran for 22 minutes and reported 128,000 thinking tokens,
then reached the account limit before returning a final answer. All three
attempts and reset times are retained in the provenance. Codex client reviews
and fixes are not attributed to a completed Claude UI audit.

Google front remains the default, with explicit Direct Google choice. Skirk
carries opaque ordered TCP: it cannot identify text inside encrypted app data
or move text ahead of an attachment already queued on the same TCP connection.
The tested scheduler infers small/quiet traffic and bounds its priority budget;
it does not promise semantic chat detection or eliminate congestion.

The existing client improvements—profile recovery, Google access status,
authenticated Test exit, renewal instructions, route choice and platform
accessibility—remain on this branch. Publishing an OAuth app removes Google's
Testing-specific seven-day refresh-token expiry, not every possible revocation.
A normal shared-app user needs no domain; personal OAuth remains an advanced
per-operator setup. Product profiles contain no Pub/Sub owner credential.

The new client lifecycle audit fixed Android failure ownership when switching
profiles and desktop stale asynchronous snapshots after user actions. Android
and iOS file imports now run bounded background reads with picker/target
ownership, cooperative cancellation and stale-result fences. The Android
emulator verified A-failed → B-Connect → A-renewal presentation without a Google
request; it did not validate a live grant. Five controlled desktop promise
tests, three Android Gradle/JUnit regressions and six Swift tests with ten
parameterized cases passed. Full Android, desktop and iOS Simulator builds
also passed. Five additional Android emulator instrumentation cases exercised
the real `ContentResolver`, remote cancellation IPC and proxy file descriptor
through a controlled provider in a separate process. They passed cancellation,
retained permits, stale-result and descriptor-release assertions; this does
not cover every external provider or a physical phone.
[Device evidence](../audits/android-document-reader-device-2026-11-15.md),
[client audit](../audits/client-state-ownership-2026-11-15.md),
[iOS import audit](../audits/ios-file-import-2026-11-15.md).

Two hundred thousand installations and two hundred thousand simultaneous active
users are different targets. Ten clients is the largest measured live
concurrency here. Current project/account limits, workload duty cycle and
cleanup cost must enter a capacity model; no installation count or one fast
primitive establishes concurrent capacity. Pub/Sub also adds resource IAM,
provisioning and published pricing, separate from Drive's `drive.file` grant.

The dated GitHub snapshot records 793 stars, 94 forks and 68,861 aggregate
release-asset downloads across seventeen published releases. The asset sum
includes checksums and other assets; it is neither unique installations nor
concurrent users. [Snapshot](github-project-snapshot-2026-11-15.json).

## Validation and cleanup

The integrated runtime passed full Go race tests, 116 Python harness tests and
`SKIRK_FULL_PREFLIGHT=1 make preflight`, including desktop frontend and Android
debug builds. The iOS Simulator application/Go engine rebuild passed. The
subsequent hybrid journal-only change has its own targeted race/vet evidence;
the frozen-source cross-platform Smoke run is
[43737455855](https://github.com/ShahabSL/Skirk/actions/runs/43737455855).
All fourteen jobs passed on the first attempt for frozen source `42e6742`.
Later client and diagnostic changes have separate local verification; that
older CI result is not evidence for an untested later head.

The capacity harness now persists the workload outcome before teardown,
journals exact process identity and attempts every owned child even after a
signal error. Darwin process creation time and argv are verified without
environment access; no broad process-group kill or privilege escalation is
used. Seventeen native macOS helper cases and integration tests passed. The
original lost outcome and 1,655-unit overrun remain visible in the failed
capacity record. [Teardown audit](../audits/owned-benchmark-process-teardown-2026-11-15.md).

Live experiments use only authorized isolated accounts, fresh namespaces and
explicit request/unit/byte bounds. All completed new probes record exact
cleanup and delayed rechecks; the hybrid verifies deletion of both topics and
both subscriptions immediately and after 31 seconds, removes its borrowed
token and recovery ledger, and records Drive cleanup independently. Foreground
measurement tails exclude later manual cleanup calls unless explicitly stated.

Previous native Android, Windows, macOS and Linux smoke evidence is indexed in
the ledger. Physical iPhone packet forwarding and notarized macOS distribution
remain unverified; an iPhone Simulator UI/build pass does not substitute for
the former. Neither those limitations nor small paired speed wins should be
hidden by describing the entire application as finished or optimal.
