# Independent audit and experiments — November 14, 2026

The subsequent [Astra audit](astra-audit-2026.md) adds deterministic lifecycle
fault tests, a stricter live harness, and a correction to the old Cloudflare
socket conclusion. This document retains the earlier measurements.

Work branch: `codex/skirk-audit-experiments-20261114`. This branch preserves
the previous uncommitted work. No Skirk main commit or push is part of this
audit. The older [validation record](validation-2026.md) remains evidence for
the earlier code; this document records the new audit separately.

## Findings and changes

| Finding | Change and evidence |
| --- | --- |
| A refreshed Drive call omitted its initial HTTP 401 from estimated quota. Network failures and canceled losing hedges also lost attempt counts; a later HTTP error could be hidden by a stale earlier network error. | Count all attempted HTTP requests, including the 401, recovered retry failures, and canceled hedges. Intentional cancellation does not increment API errors. Preserve the latest response and cancellation cause. Focused tests reproduce these failures. Counts remain conservative local estimates; a failed network attempt may not have reached Google. Retry response-byte totals still include only final bodies. |
| The final idle cleanup test used MemoryStore, while production class listing could skip normal-priority objects. | Force both priority classes during the delayed final list; exercise the production listing interface in a test. The drain remains bounded; a truncated backlog can require a future stream or the stale-file janitor. |
| Closing a stream could race with appending pending frames, leaving idle memory retained. | Synchronize closed-state checking with pending-frame mutation; test concurrent close and frame arrival under the race detector. |
| Client mux workers could outlive a failed listener bind, and shared listener lifetime depended on the first caller's context. | Give shared listeners explicit lifetime ownership so the last listener stops the mux and independent listeners cannot cancel each other. Test occupied ports, restart, and independent cancellation. |
| Android mode and LAN-sharing choices were persisted only at Connect. Editing a profile could reorder or select it unexpectedly. | Save choices when changed; preserve order and selection on edits. Verify force-stop/relaunch and profile editing with disposable emulator data. |
| Benchmark output could succeed despite failed requests, accepted unverified synthetic response bodies, and mixed warm-up quota with workload quota. Capacity tests could treat missing telemetry as zero usage. | Validate random bytes and SHA-256, return failure for request errors, split warm-up counters, use nearest-rank percentiles, and refuse stale/missing capacity telemetry. Include the final metrics update and require every scheduled small request and benchmark process to succeed. |

## Offline tuning matrix

`tools/skirk_experiment_matrix.py` ran six cases three times each, in a seeded
shuffled order. Every run used twelve concurrent tiny HTTP requests and an
8 MiB random response with exact length and SHA-256 validation. Store delays
were 150 ms upload, 60 ms list, and 100 ms download. All eighteen workloads
completed without request failures.

| Case | Median run small TTFB p95 | Median simulated bulk Mb/s | Median foreground estimated units |
| --- | ---: | ---: | ---: |
| Auto, 1000 ms | 1317.438 ms | 46.289 | 5255 |
| Fixed, 1000 ms | 1315.805 ms | 46.366 | 5255 |
| Fixed, 500 ms | 1316.766 ms | 46.362 | 5555 |
| Fixed, 250 ms | 818.488 ms | 61.206 | 6055 |
| Auto with burst | 572.605 ms | 88.795 | 6505 |
| Auto, concurrency cap 8 | 1316.540 ms | 43.318 | 5305 |

These figures are **not live Drive throughput**. The memory store lacks Drive
visibility delay, bandwidth limits, backend throttling, and production class
pagination. Small requests arrive together, so coalescing is favorable. A
median of three run p95s is not a pooled population p95. The earlier live burst
comparison lost despite the synthetic gain, so this matrix does not justify
changing the production polling default. Raw results:
`.skirk-runs/audit-matrix-20261114/`.

## New candidate: encrypted small objects in discovery metadata

Drive's [file resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
includes a description field. Its [metadata guide](https://developers.google.com/workspace/drive/api/guides/file-metadata)
allows retrieving selected metadata through `files.list`. The experiment puts
an opaque record in the description and requests it in discovery, removing a
separate media download for eligible small objects. It changes the API call
shape; Base64 alone provides no performance benefit.

The first live primitive probe alternated ordinary media and metadata-only
creation for 256, 2048, and 8192 random bytes, four repeats of each pair. All
24 records passed exact byte comparison, and every exact created file was
deleted. Both paths used the same warm HTTP client, `google_front_pinned`,
and isolated test account/mailbox. All records appeared in the first list.
The metadata path received bytes in 408–598 ms; media received bytes in
1337–3834 ms. These are one-way primitive timings, not browser or tunnel RTT.
Raw results: `.skirk-runs/audit-inline-probe.json`.

Under the current [Drive unit schedule](https://developers.google.com/workspace/drive/api/guides/limits),
one create, list, media download, and delete costs 400 estimated units. Without
the media download it costs 200. Extra polling and retries add units. This is
a per-object calculation, not a capacity multiplier for the whole service.

The implementation is explicitly opt-in through
`drive.inline_small_objects: true`, with a default of false. It duplicates
only sealed mux priority objects no larger than 8192 bytes into metadata and
keeps their ordinary media body. An older reader can still download that body.
New readers request metadata only when enabled, bound its decoded size, and
run the existing AEAD authentication before processing. A corrupt inline copy
falls back to the ordinary media once. Tests verify valid delivery without a
media Get and successful fallback after a corrupt metadata copy.

This dual-write prototype differs from the faster metadata-only creation in
the primitive probe. Larger metadata responses can
increase list bandwidth and memory use; a 100-object page of 8 KiB sealed
copies can approach 1.1 MiB before JSON overhead. Metadata is visible anywhere
the file's description is visible, so only already encrypted mux data belongs
there. The size cap is an application bound tested here, not a documented
Google maximum. Public release needs cross-platform, regional, long-running
cleanup, and quota-pressure testing.

### Live tunnel comparison

The final ABBA sequence used a fresh session for each arm, the same isolated
mailbox/account, Google front pinned on both ends, one random 32 MiB HTTP
response, and eight scheduled small requests. All four bulk responses and all
32 small requests succeeded. Each run includes identical small warm-up and
an eight-second metrics/cleanup tail. The table's units include both client
and exit, warm-up, and tail; they are local estimates rather than Console
attribution. Intentional canceled hedge attempts are included in this final
sequence. Raw reports: `.skirk-runs/audit-live-inline-isolated-abba-repeat/`.

| Order | Mode | Bulk Mb/s | Overlap small successes | Overlap TTFB p95 | Scheduled TTFB p95 including executor queue | Combined estimated units | Media downloads |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | Baseline | 37.816 | 7/7 | 3.035 s | 5.659 s | 20,455 | 41 |
| 2 | Inline | 43.131 | 8/8 | 1.776 s | 3.224 s | 12,255 | 13 |
| 3 | Inline | 36.445 | 8/8 | 2.669 s | 4.959 s | 12,455 | 13 |
| 4 | Baseline | 34.908 | 6/6 | 3.462 s | 7.484 s | 19,355 | 39 |

The median of two run p95s fell from 3.249 to 2.223 seconds (31.6%), median
estimated units from 19,905 to 12,355 (37.9%), and median bulk goodput rose
from 36.362 to 39.788 Mb/s (9.4%). There are only two repetitions per arm;
each overlap p95 is the maximum of six to eight successful samples. This is
evidence for a useful candidate, not a statistical latency guarantee or a
200,000-user result. A faster transfer also changes which scheduled small
requests overlap bulk. The unit and media-call reduction is more directly
explained by the removed downloads than the variable throughput gain.

An earlier successful ABBA sequence also favored the prototype, but did not
isolate the exit session per arm and omitted canceled hedge attempts from
quota. It is retained under `.skirk-runs/audit-live-inline-abba/` as preliminary
evidence rather than pooled into the final table. The first attempt at the
isolated sequence failed in its **baseline** arm: two 1 MiB exit uploads waited
until the 15-second stall guard canceled the bulk request; all eight small
requests succeeded. No 403, 429, or backend backoff was observed. Its report
remains in `.skirk-runs/audit-live-inline-isolated-abba/`. The repeat succeeded,
but that failed trial prevents a claim of demonstrated sustained reliability.

Two additional bounded compatibility checks sent an exact 1900-byte response
with the new exit and older client binary, then with the older exit and new
client. Both passed. These prove those two small requests across mixed
versions, not every platform or a long-running mixed-version deployment.
Evidence: `.skirk-runs/audit-inline-compat/summary.json`.

### Reproduction and current gates

To enable the prototype manually for an isolated CLI kit, add
`"inline_small_objects": true` inside the existing `drive` object in both exit
and client JSON, then restart those processes. Preserve all other kit fields.
New setups and imported profiles keep the feature off unless explicitly set.

```sh
go build -o .skirk-runs/audit-memory-bench ./tools/skirk_memory_bench.go
python3 tools/skirk_experiment_matrix.py --binary .skirk-runs/audit-memory-bench --out .skirk-runs/new-matrix
go run ./tools/inline_probe --help
python3 tools/skirk_live_ab.py --help
python3 -m unittest discover -s tools -p 'test_skirk_benchmarks.py'
```

The live tools require an explicit `--live` argument and an isolated test-kit
path. The A/B tool creates private temporary config copies and deletes them
after stopping each arm. It records sanitized results and logs under the
ignored output directory. Do not run its exit alongside another exit using
the same kit. Final full preflight and Go race checks passed; evidence is in
`.skirk-runs/audit-preflight-final.log` and `.skirk-runs/audit-race-final.log`.

The client audit also passed Android build/emulator persistence and 1.3x text
checks, desktop frontend build and mock keyboard/dialog checks, and an iOS
Simulator build plus a disposable-profile Cancel/Remove UI test. iOS packet
forwarding still requires a signed physical device; Windows runtime and Apple
distribution signing remain unverified. No claim of platform-wide live VPN
coverage follows from a compile or mock UI test.

The macOS application and Windows x64 portable archive were rebuilt with the
latest core and UI; the archive passed CRC verification. Build logs are
`.skirk-runs/audit-{macos,windows}-build.log`. Test processes are stopped. The
post-test cleanup deleted 38 stale mux files totaling 47,195,160 bytes, then
four small compatibility-test files totaling 4592 bytes, with zero deletion
failures. The final dry run found zero stale runtime files. These records are
`.skirk-runs/audit-cleanup-result.json`,
`.skirk-runs/audit-compat-cleanup-result.json`, and
`.skirk-runs/audit-final-cleanup-verify.json`. This was explicit cleanup after
short-lived processes and a failed bulk trial, not a long-running automatic
cleanup soak.

## Other architecture options revisited

| Option | Evidence and decision |
| --- | --- |
| Drive HTTP batch requests | [Google](https://developers.google.com/workspace/drive/api/guides/performance) excludes media upload/download and charges each inner operation separately. Batching deletes may save connection overhead, but cannot remove the hot-path media call or multiply quota. |
| Workspace Drive events through Pub/Sub | [Drive events](https://developers.google.com/workspace/drive/api/guides/events-overview) can deliver file IDs and reduce idle discovery. They add a subscribed control plane and lifecycle handling; preview status is recorded in the [release notes](https://developers.google.com/workspace/drive/release-notes). No paired live event-latency result exists here. Keep as a discovery experiment rather than an assumed lower-latency default. |
| Known IDs, mutable slots, range reads | Existing primitives and archives show feasibility but not a mixed-workload win. They require rendezvous, framing, stale-generation protection, and retries. A new design must measure all control calls and cleanup, not just a cached metadata read. |
| Payload compression | Encrypting already compressed TLS/media produces little compressible content. Compression of ciphertext is ineffective; combining unrelated plaintext streams before compression also complicates confidentiality. No generic compression default is justified. |
| Independent quota projects | The disabled-API control and working header prove API activation behavior and compatibility. Console attribution remains unconfirmed. Keep optional, surface errors, and do not silently transfer load back to a shared project. |
| Direct owned HTTPS relay | Removes Drive's object round trips where reachable, but requires an endpoint reachable in the user's region. Preserve Google front as the user's default and keep route selection explicit. No new reachability result is implied. |

The [current Drive limits](https://developers.google.com/workspace/drive/api/guides/limits)
also distinguish older projects from projects created after May 1, 2026. The
unit estimates describe the newer model and must not replace the actual
project's Console limits. Neither the prior ten-client run nor this work proves
200,000 active users. Publisher OAuth branding, per-project grant rate, idle
exit discovery, active traffic, daily egress, cleanup, and account policy remain
separate scaling constraints.
