# Pipelined full-transport live comparisons — November 16, 2026

Status: bounded, same-binary research on approved secondary accounts. These
carriers and the live fixture are test-only Go code. Neither is part of the
shipping CLI, Android, macOS, Windows or iOS transport. The default remains the
user-selected Google route, with `www.google.com` front as its default.

## Method and provenance

The first six completed arms below ran the same frozen Go test executable, SHA-256
`75d56f5d7f8c57efa88f67affe9f94480a9d56518349789ac65cce3002c0b0c9`,
from source `116b6eb26ed41ff40913166bd809d628e3add68e`. The reverse-order
Firestore pair ran a second frozen executable, SHA-256
`3a5a5ebb849db46b7880c336901628f1d9f9d3b5e64fc46065716eea496a0a49`,
from source `c5d52c4fbe8a19d820d8836e86fe231f7d8a9563`. That source adds
provider-error classification to the live fixture; the two Firestore pairs
must not be pooled as one exact-binary experiment. Each arm used the selected
`www.google.com` front for transport, one owned SOCKS/mux path to an
owned TLS origin, one exact seeded 16 MiB download, one exact seeded 16 MiB
upload, and one persistent upgraded TLS text connection. The text connection
sent one warm and forty independently authenticated 64-byte frames, with twenty
offered every 250 ms in each bulk direction. It pipelined writes and read full
ordered replies separately, so the text series does not inherit the earlier
HTTP/1.1 request-serialization delay. The origin accepted one GET, one POST,
41 exact text echoes and zero corrupt bodies in every completed arm.

Rates use decimal Mb/s and the exact 16,777,216-byte transfer's completion
time. Text values are **scheduled-to-completion** nearest-rank p95 of only
twenty frames in that direction, including offering delay. A frame could be
completed after its file ended; the completed-during-bulk counts are retained
below. These are small descriptive samples on a shared Mac and a changing
Google/network path, not population percentiles or a guaranteed floor. The
separate warm sample is excluded from each p95. The earlier main-branch
HTTP/1.1 mixed workload has a different schedule and cannot be numerically
pooled with these arms.

The two Drive v2 pairs ran candidate/control and control/candidate order in
separate bounded campaigns. The Firestore pairs ran hybrid/control and then
control/hybrid. Each pair used the same workload seed and executable; order and
remote load were not controlled between pairs. The private result paths are
listed at the end; their raw JSON and authentication files are intentionally
ignored by Git.

## Drive v2 conditional-metadata priority versus Drive-only control

The candidate feature-probed strong v2 resource ETag write fencing on fresh,
owned per-direction files before starting its stores. Its priority records used
conditional metadata PATCH with an immutable Drive fallback. Google returned
four 412 precondition responses in each candidate arm, all classified as
expected recovery races. The two-arm controls used the existing Drive-only
carrier with the same pipelined workload. Every arm completed and joined its
writers, with exact owned Drive cleanup verified immediately and at 31 seconds.

| Campaign/order | Mode | 16 MiB down | 16 MiB up | Text p95 while down | Text p95 while up | Drive calls / conservative unit upper |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| A, first | v2 metadata priority | 5.242 s / **25.605 Mb/s** | 3.136 s / **42.798 Mb/s** | 2.186 s | 2.247 s | 615 / 223,560 |
| A, second | Drive control | 5.698 s / 23.554 Mb/s | 3.361 s / 39.931 Mb/s | 2.239 s | 2.236 s | 489 / 228,320 |
| B, first | Drive control | 7.317 s / 18.343 Mb/s | 4.390 s / 30.575 Mb/s | 2.287 s | 2.863 s | 444 / 219,640 |
| B, second | v2 metadata priority | 4.831 s / **27.785 Mb/s** | 3.433 s / **39.099 Mb/s** | 2.253 s | 1.871 s | 641 / 228,360 |

V2 file goodput was 8.7%/7.2% higher for down/up in pair A and 51.5%/27.9%
higher in pair B. The control's download itself varied 23.554 to 18.343 Mb/s
between campaigns, so the pair-B difference cannot be assigned wholly to the
carrier. Download text p95 improved 2.4% and 1.5%. Upload text p95 was 0.5%
worse in A and 34.7% better in B. These four arms support a *candidate* with
promising file performance, not a stable universal throughput or latency win.

Each candidate logged two successful feature probes, no carrier errors and no
guard denial. V2 repeatedly read metadata while active: candidate A/B recorded
142/154 HTTP 304 responses and 615/641 visible Drive calls, versus 489/444
in their Drive controls. The candidate's total conservative quota reservation
was similar to its controls, but its busiest rolling-minute upper was
219,200/222,880 units versus 114,640/118,800 for controls. These are
eight-attempt **upper reservations**, not measured billable units or physical
HTTP attempts. Each arm stayed inside the local 280,000 upper-unit minute
guard, which reserves headroom below the
[published 325,000-unit per-user-project minute quota](https://developers.google.com/workspace/drive/api/guides/limits).
Expected cleanup 404s were 32/35 in candidate A/B and 84/67 in controls;
their `api_errors` counters include those expected responses. There were zero
unexpected provider errors, timeouts, transport errors or guard denials in
these four arms. The first candidate had 7 down and 3 up frames completed
during their respective file; the second had 6 and 5. Control A had 5 and 0;
control B had 7 and 4. The twenty-frame scheduled p95 includes later replies.

V2 remains test-only. Before product use it needs peer capability negotiation,
feature-loss fallback, product Direct-route wiring, epoch rotation, fair
multi-client budgets, crash-durable exact-object cleanup and platform-specific
validation. The [official v2/v3 comparison](https://developers.google.com/workspace/drive/api/guides/v3versusv2)
recommends v3 as the current API; an observed v2 ETag fence is not a future
compatibility guarantee.

## Firestore priority plus Drive bulk versus Drive-only control

The hybrid used Firestore document writes and WebChannel Listen through the
same Google front for interactive priority records, while Drive carried bulk
and bounded fallback. Two complete campaigns passed the full SOCKS/TLS
workload and exact hashes. The candidate was first in D and second in E. E
used the later diagnostic-only source/binary stated above.

| Campaign/order | Mode | 16 MiB down | 16 MiB up | Text p95 while down | Text p95 while up | Drive calls / conservative unit upper | Firestore calls |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| D, first | Firestore priority + Drive bulk | 5.000 s / **26.845 Mb/s** | 3.474 s / **38.636 Mb/s** | **0.560 s** | **0.416 s** | 129 / 83,440 | 417 |
| D, second | Drive-only control | 6.157 s / 21.801 Mb/s | 4.375 s / 30.676 Mb/s | 2.076 s | 2.331 s | 468 / 222,360 | 0 |
| E, first | Drive-only control | 7.393 s / 18.154 Mb/s | 4.712 s / 28.483 Mb/s | 2.117 s | 4.461 s | 419 / 225,800 | 0 |
| E, second | Firestore priority + Drive bulk | 3.782 s / **35.491 Mb/s** | 4.454 s / **30.136 Mb/s** | **0.414 s** | **0.378 s** | 135 / 81,400 | 463 |

In D, file rate was 23.1%/25.9% higher for download/upload and scheduled
text p95 was 73.0%/82.2% lower. In E, file rate was 95.5%/5.8% higher and
text p95 was 80.4%/91.5% lower. The control's download rate varied 21.801 to
18.154 Mb/s across these campaigns, and the two binaries differ, so these
paired percentages remain descriptive. The warm text sample was 0.540 versus
1.996 s in D and 0.427 versus 2.014 s in E (hybrid versus control). D's
hybrid/control completed 16/7 download and 2/4 upload frames during their
respective bulk files; E completed 12/11 and 8/3. This is a real application
path but only two small successful pairs. It does not establish a steady-state
minimum RTT or many-user capacity.

In D, the hybrid's two observers recorded 127 and 118 Listen requests and 84
REST writes per role (168 total). In E, they recorded 137 and 143 Listen
requests and 89/90 REST writes (179 total). Every observer stayed within its
256-request role allowance, received REMOVE acknowledgement, joined its
child/control owners and ended with zero pending requests, events or SDK
timers. D/E hybrid Drive guards had 129/135 visible calls, 16/23 expected
cleanup 404s, and zero denials, timeouts or transport errors. The Firestore
guards had 417 and 463 HTTP/2 responses respectively. **Each recorded one
HTTP 400 near teardown** and therefore `api_errors: 1`, despite all 41 exact
frames and both files completing. E's new classifier identifies its 400 as an
owned cleanup **GET Listen** request that had been canceled; it was not a
WebChannel terminate request or an active-workload write. E's two carrier
summaries recorded zero API/transport errors. This supports a canceled-cleanup
interpretation for E, while D's frozen binary did not capture method/route
details and cannot be retroactively assigned the same cause. Neither arm's
raw guard counter should be rewritten to zero.

The Firestore project had billing disabled and was eligible for its free
database. In both campaigns, the owned mailbox documents and entire fresh
database were removed, checked absent immediately and after 31 seconds; the
previously disabled Firestore API was restored to **DISABLED**. No persistent
shared backend was created. [Firestore's free quotas](https://firebase.google.com/docs/firestore/quotas)
include 20,000 writes and 50,000 reads per day, 1 GiB storage and 10 GiB
outbound per month for the eligible free database. The observed 168/179 writes
in one small workload alone show why this result cannot be extrapolated to
200,000 users on one free project. It also requires owner Cloud credentials,
IAM and fresh database lifecycle work that the current consumer onboarding
does not provide.

### Setup attempts that did not enter the tunnel workload

Campaigns A, B and C used the same source/binary as D but failed while Google
propagated a just-enabled Firestore service. They are recorded separately and
contribute **zero** transport samples:

| Campaign | Setup failure | Verified recovery |
| --- | --- | --- |
| A | 403 during fresh mailbox document setup after service enablement | Owned database/docs absent immediately and at 31 s; API restored DISABLED. |
| B | 403 `PERMISSION_DENIED` during database-create POST; Google explicitly reported recent API enablement may need propagation | Exact target 404 and database list empty immediately and at 31 s; API restored DISABLED in external recovery proof. |
| C | 403 `PERMISSION_DENIED` while polling the create operation; same propagation message | Exact owned database deleted and absent immediately and at 31 s. API remained temporarily enabled for the single bounded D retry and was restored DISABLED by D. |

The setup failures demonstrate service-provisioning friction, not transport
throughput or an OAuth-token expiry. They also show why a Cloud database per
ordinary end user is not a simple immediate sign-in flow.

## Decision and private evidence

The strongest **observed** chat-like response in these completed pipelined
arms is the Firestore hybrid's 0.414/0.378-second scheduled p95 in E. The Drive
v2 carrier showed a less complex and potentially cheaper path with good file
rates, but its text p95 remained roughly 1.9–2.3 seconds. No experiment here
establishes an architectural wall: remote variance, only two hybrid pairs,
the classified but still recorded teardown 400, finite free quotas and missing
product integration prevent that claim. Keep both candidates behind research
gates and retain the shipping Drive route while further controlled tests and
product work proceed.

Private, ignored evidence (no raw IDs, credentials or sealed payloads in this
tracked report):

- `.skirk-runs/drive-v2-pipeline-20261116a/` and
  `.skirk-runs/drive-v2-pipeline-20261116b/`: `campaign.json`,
  `orchestrator-ledger.json`, and each arm's `summary.json`.
- `.skirk-runs/fs-hybrid-pipeline-20261116a/` through
  `.skirk-runs/fs-hybrid-pipeline-20261116e/`: campaign and recovery ledgers;
  B/C external recovery proofs; D/E hybrid/control arm summaries.

The earlier [Drive ETag primitive](drive-v2-metadata-etag-cas-2026-11-15.md),
[first failed full hybrid](firestore-hybrid-first-live-2026-11-15.md), and
[adaptive Drive control failures](drive-adaptive-live-control-2026-11-16.md)
remain separate experiments with distinct workloads or completeness states.
