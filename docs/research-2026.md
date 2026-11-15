# Skirk research and decision record (2026-11-14)

See the subsequent [Astra audit](astra-audit-2026.md) for lifecycle corrections,
new measurements, and an opaque Worker TCP probe that revises the older
Cloudflare platform inference.

The later [independent audit and experiments](audit-experiments-2026.md)
records additional fixes and an opt-in inline-metadata candidate. The results
below describe the earlier baseline and remain identified by their test date.

This note combines current Google and Apple documentation with Skirk's existing
code, archived experiments, and bounded live Drive tests on November 14, 2026.
The live tests used one isolated personal test account and project, with the
exit and HTTP origin on this Mac. The raw experiment records are ignored under
`.skirk-runs/`; the older
[transport research](transport-research.md) contains the full promotion gates.

## Recommendation

Keep muxv4 as the measured Drive baseline, and keep the `www.google.com` front
as the default route. Users must be able to choose direct Google explicitly:
the direct route was faster in this local test, but may be unreachable in their
region. Leave burst polling off by default after the live comparison below.
Optimize *measured mixed-workload latency, reliability, and quota per useful
byte* before increasing parallelism. Do not advertise a user capacity or a
permanent OAuth grant based on API limits alone. Drive's file API cannot
provide a streaming latency guarantee.

There is a distribution gate: Google's current [Workspace user data and
developer policy](https://developers.google.com/workspace/workspace-api-user-data-developer-policy)
allows Drive scopes for specified file-oriented use cases and explicitly forbids
using Drive as a large-scale CDN or multiple accounts to bypass account limits.
Skirk's generic TCP relay does not clearly match an approved Drive use case.
The narrower `drive.file` scope reduces file access but does not settle that
use-case question. The project owner reports that Google support approved the
project's use of Drive; this review did not inspect that correspondence. Keep
that determination with the project records before production load or a
multi-account scaling design. This is an assessment of the published text, not
a claim that Google has rejected Skirk.

The [public repository](https://github.com/ShahabSL/Skirk) showed 792 stars,
94 forks, and 17 open issues on November 14, 2026. Across its 17 GitHub
releases, [release asset download counters](https://github.com/ShahabSL/Skirk/releases)
summed to 68,174 for binaries and APKs, including 52,568 on the latest
release. These counters include repeat downloads and are not unique users;
they establish that migration and recovery UX matter at the current scale.

## What Drive currently supplies

| Mechanism | Skirk fit and constraint |
| --- | --- |
| `files.create` multipart upload | One request can publish metadata and a small body. Skirk uses whole encrypted mux objects; [Google describes multipart upload](https://developers.google.com/workspace/drive/api/guides/manage-uploads) as suitable when the body can be retried in full. |
| `files.list` plus `files.get?alt=media` | Current prefix discovery and ID download path. A successful upload still needs discovery and a separate media read; [Drive file search](https://developers.google.com/workspace/drive/api/guides/search-files) does not promise socket-like visibility or latency. |
| `changes.list` and `changes.watch` | Change records are ordered and watch notifications are empty hints requiring a later feed read. [Google's change guide](https://developers.google.com/workspace/drive/api/guides/manage-changes) and [push guide](https://developers.google.com/workspace/drive/api/guides/push) require a public HTTPS webhook for watch. Channels expire after at most one week for changes (one day for files) and need replacement; they do not deliver payload bytes. |
| Workspace Events API | A newer [Drive folder subscription](https://developers.google.com/workspace/drive/api/guides/events-overview) can send file-created CloudEvents with a file ID through Pub/Sub. This is a distinct, untested discovery candidate; see below. |
| Resumable upload and range download | Resumable uploads add a session initiation request; [Google recommends them](https://developers.google.com/workspace/drive/api/guides/manage-uploads) for large or interruption-prone files. Skirk's tested short hot-path objects did not benefit. Range reads require authenticated fragment boundaries; the repo's primitive worked but the tested protocol added control overhead. |
| `appDataFolder` | Google defines it as per-app, hidden from the Drive UI and accessible only to the creating app; it cannot be shared. [Google's app-data guide](https://developers.google.com/workspace/drive/api/guides/appdata). The current setup code first probes appData, then falls back to a visible Skirk-owned folder when the `drive.file` grant lacks appData access. The older transport note's appData constraint therefore describes only one supported mailbox mode, not every current kit. |

Google calls `drive.file` a [non-sensitive, per-file
scope](https://developers.google.com/workspace/drive/api/guides/api-specific-auth).
It does not authorize access to arbitrary Drive data. The same policy still
requires an approved use case for *any* Drive scope and bars account-limit
evasion. Broader `drive` scopes would add verification and security burden
without removing the file-operation round trips.

## Measured transport evidence and alternatives

An archived `google_front_pinned` muxv4 control downloaded 100 MiB in 24.868 s
(33.73 Mb/s). During that 52.105 s run, six concurrent small probes had 9.658 s
p95 time to first byte; the client process reported 226 Drive calls and 27,600
estimated units, with zero reported API errors. That is one old run with only
six small samples, not a population p95, current speed guarantee, or a full
client-plus-exit quota measure. Artifact:
`.skirk-runs/protocol-lab/final-20260514/stable-live-control-20260515T024838Z/bench-live-100mb.json`.

The [prior protocol study](transport-research.md) tested or primitive-tested
change-feed discovery, generated-ID rendezvous, range-read slabs, resumable
uploads, mutable update slots, control/data splits, priority mailbags, and
other mux shapes. Their recurring cost was another control object, metadata
poll, broad change page, or whole-file write tail. A change-discovery trial,
for example, recorded 18.99 Mb/s, 18.645 s small-probe p95, and 75,100
estimated units in its archived 100 MiB run. Conditions differed between
trials, so this is supporting evidence, not a clean A/B claim. Current muxv4
is the best **proven** candidate under the existing single-mailbox constraints.

### New bounded live Drive comparison

All rows below used the same new `drive.file` OAuth grant, local exit, random
32 MiB HTTP body, and eight scheduled small requests. Runs were sequential,
not randomized; the overlapping sample count varies because a faster bulk
transfer finishes before some small requests start. The figures are actual
SOCKS-through-Skirk measurements on this Mac and network, not simulated
transport values or a result from a constrained region. Every HTTP request
completed and the client and exit reported zero Drive API errors.

| Client Google API route | Run | Small requests overlapping bulk | Overlap TTFB p95 | Bulk goodput |
| --- | --- | ---: | ---: | ---: |
| `google_front_pinned` (HTTP/2) | A | 6 | 3.881 s | 33.421 Mb/s |
| `google_front_h1_pinned` | A | 8 | 3.988 s | 14.514 Mb/s |
| `direct` | A | 7 | 2.266 s | 38.146 Mb/s |
| `google_front` (HTTP/2) | A / B | 6 / 7 | 3.025 / 2.682 s | 35.793 / 36.434 Mb/s |
| `real_pinned` | A | 7 | 2.683 s | 36.666 Mb/s |
| `google_front_pinned` (HTTP/2) | B | 6 | 3.444 s | 48.573 Mb/s |
| `google_front` with burst polling on both ends | A / B | 8 / 7 | 3.452 / 3.596 s | 22.939 / 26.981 Mb/s |

A longer `google_front`, no-burst run transferred 128 MiB in 14.330 s
(**74.928 Mb/s**); all 24 small requests completed, and the 12 that started
while bulk was active had **3.064 s p95** first-byte latency. This is a
single long run, not a maximum or a service-level guarantee. Artifacts are
`.skirk-runs/personal-test/{h2-a,h2-b,h1-a,direct-a,front-a,front-b,realpin-a,frontburst-a,frontburst-b,front-128}.json`.

The short-run results vary substantially: the same pinned front went from
33.4 to 48.6 Mb/s, so small differences cannot establish a route winner.
HTTP/1.1 had a clear bulk regression in its one paired setup, and the two
burst runs increased both observed latency and Drive request rate. Neither
is a justified default here. A constrained-network route test, repeated
same-day A/B order, and longer stability traces remain necessary before
changing the front implementation or claiming regional performance.

The repo's Cloudflare Worker experiment supports bounded HTTP `GET`/`HEAD`
fetch, but its opaque SOCKS probe failed at Worker socket connection in the
tested configuration; it is not a demonstrated generic TCP/UDP exit. A direct
HTTPS/WebSocket/QUIC relay to an owned exit would remove Drive's file
publish/discovery steps when a route is reachable, but the hostile-path route,
operating cost, and failure behavior have not been tested as a Skirk replacement.
It is a separate product/transport experiment, not a promised muxv4 speedup.

Changing the encoding of the existing encrypted payload (for example, putting
it in strings or Base64) does not remove the upload, discovery, download, or
cleanup operations and therefore cannot improve their latency or quota-unit
cost. Base64 would expand raw ciphertext by about one third before container
overhead. The payload is already opaque to Drive; an encoding change alone is
not a new transport mechanism.

The next useful Drive-only candidates are conservative: object-size bounds
conditioned on queue delay, and worker limits conditioned on Drive latency
and error rate. Bounded burst polling is implemented but its first live A/B
did not justify enabling it by default. Existing results
do not justify indiscriminately increasing workers or splitting one user's
traffic across accounts. A watch receiver could reduce idle polling in a
deployment with a public HTTPS endpoint, but a notification still requires a
feed read and does not itself carry bytes; measure it against prefix listing
before adoption.

The Android client's new optional per-app VPN list can avoid sending unrelated
device traffic through Drive. That should reduce quota demand for users who
only need a few apps, but no live before/after traffic trace has measured the
savings yet. Unselected apps use their normal network, which the picker states
explicitly. An API 36 emulator made one successful HTTPS request from a
disposable probe app while the Skirk VPN connected through the dedicated live
Drive test mailbox; that checks the packet path, not the quota savings.

### Upstream cross-check: HTTP version and batching

Google's [Drive performance guide](https://developers.google.com/workspace/drive/api/guides/performance)
allows at most 100 subrequests in a batch, charges each subrequest separately,
and excludes media uploads and downloads from batching. Batching can help
deferred metadata or delete work, but it cannot collapse muxv4's hot upload and
media-read round trips. Skirk already requests partial response fields and
gzip on the list path. Rclone's [fast-list](https://github.com/rclone/rclone/blob/master/docs/content/drive.md)
combines many *folder* queries; Skirk's receiver polls one mailbox prefix, so
that optimization does not apply directly.

Rclone still [disables HTTP/2 for its Drive backend](https://github.com/rclone/rclone/blob/master/docs/content/drive.md)
after an [intermittent stream-error report](https://github.com/rclone/rclone/issues/3631).
Its later [HTTP/2 discussion](https://github.com/rclone/rclone/issues/8379)
shows that connection reuse can also hurt parallel transfers on some networks,
although that report used OneDrive and cannot predict Skirk's result. Skirk
currently has both `google_front_pinned` (HTTP/2) and
`google_front_h1_pinned` (HTTP/1.1) route modes. An archived H1 100 MiB mixed
run reported 30.49 Mb/s bulk, 5.583 s small-request p95 from six samples,
and 26,000 estimated client units; the later H2 control reported 33.73 Mb/s,
9.658 s small-request p95 from six samples, and 27,600 units. The small
endpoints returned different HTTP statuses and the runs were not paired, so
neither protocol wins on that archived evidence. The H1 artifact is
`.skirk-runs/protocol-lab/muxv4-h1-full-20260514T192338Z/bench-live-100m.json`.

The new 32 MiB same-exit comparison above favored HTTP/2 for bulk, but had
only one H1 run and was not randomized. Repeat on the constrained network and
over longer transfers before removing the H1 option. Preserve the user's
front/direct choice regardless of the local speed ordering.

### Workspace Events API folder subscription: untested candidate

Google now documents a `google.workspace.drive.file.v3.created` subscription
on a Drive folder (`//drive.googleapis.com/files/{folderID}`), with
`includeDescendants` for children and `drive.file` as an accepted scope.
An event without expanded resource data still contains the created file ID,
so a Skirk receiver could try `files.get?alt=media` directly and avoid a
`files.list` call for that object. See [Drive event types](https://developers.google.com/workspace/events/guides/events-drive),
[subscription setup](https://developers.google.com/workspace/drive/api/guides/events-overview),
and [scope table](https://developers.google.com/workspace/events/guides/auth).
Google's [Drive release notes](https://developers.google.com/workspace/drive/release-notes)
announced Drive Events subscriptions as **generally available** on May 18,
2026, but the current generic [subscription guide](https://developers.google.com/workspace/events/guides/create-subscription)
still labels Drive targets **Developer Preview**. Treat the guide's note as a
documentation conflict and verify the actual API version and availability in
a smoke test before building a production dependency. Google's
event catalog says `created` while its scope table says `added`; validate the
accepted event type in a smoke test rather than assuming the two pages agree.

This path needs a billing-enabled Cloud project with Workspace Events,
Pub/Sub, and Drive APIs, a Pub/Sub topic authorized for Google's Drive event
publisher, and a subscriber reachable by the exit and clients (or a relay).
[Google's setup guide](https://developers.google.com/workspace/drive/api/guides/events-overview)
describes those prerequisites; [Pub/Sub has usage-based charges](https://cloud.google.com/pubsub/pricing)
after its free throughput allowance. A client whose network only reaches the
current Google API route may not reach Pub/Sub, so routing or a relay is part
of the experiment, not assumed solved. A subscription without resource data
expires within seven days; one with resource data expires within four hours
for normal user grants. Schedule renewal and monitor suspension, because
[expired subscriptions are deleted](https://developers.google.com/workspace/events/guides/events-lifecycle).
[Pub/Sub can redeliver events](https://docs.cloud.google.com/pubsub/docs/subscription-overview),
so deduplicate by file ID and keep periodic prefix-list reconciliation.

The visible Skirk-owned mailbox folder is a concrete folder target. Google's
documented target-resource list does not explicitly establish whether the
special `appDataFolder` can be subscribed to; test that mode separately and
retain list polling if unsupported. Google's [subscription guide](https://developers.google.com/workspace/events/guides/create-subscription)
warns that an event may take up to a few minutes to generate a Pub/Sub
message. That makes Events more credible as an idle wakeup than as a
replacement for the current one-second active poll. There is no Skirk
publish-to-receive p95 guarantee; only a paired live trace can tell whether
saved list calls improve latency, quota use, and cost under the actual route.
Once authorization and project setup permit testing, first verify one created event
and direct ID download in the visible mailbox. Then run the same 100 MiB mixed
workload with and without Events, recording upload completion, event publish
time, subscriber receive time, first media byte, missed/duplicate IDs,
fallback-list frequency, Drive units, Pub/Sub bytes and charges, and small
request p95. Promote only if the existing mixed-workload gates pass.

## Quota and multiuser model

[Google's current Drive API limits](https://developers.google.com/workspace/drive/api/guides/limits)
apply the May 2026 unit model to new projects, with grandfathering for some
projects that used the API between November 2025 and April 2026. The new model
documents 1,000,000 units/min/project and 325,000 units/min/user/project;
400,000,000 units/day/project and 1 TB/day/project egress are thresholds
before charges, not measured Skirk capacity. Google lists a 750 GB/day upload
limit for **Workspace users**, plus backend throttles that can produce `429`;
`403`/`429` should trigger exponential backoff. Verify each actual project's
quota and billing state in Google Cloud Console.

Skirk's local accounting matches Google's current per-method table for its
main calls: list and changes 100 units, media download 200, create/upload and
delete 50, metadata get and generated IDs 5. The counter is a local estimate;
it excludes other clients, the exit, janitor work, other apps, and any project
usage outside this process. Both ends normally use the same OAuth grant, so
their usage competes in the same user/project bucket. A personal OAuth project
separates that project's bucket from Skirk's shared client, but it does not
remove per-user limits or the Workspace policy requirements.

### Candidate: separate quota project with the same OAuth client

Google's [Cloud API system parameters](https://docs.cloud.google.com/apis/docs/system-parameters)
define `X-Goog-User-Project` as a caller-specified project for quota and
billing, requiring `serviceusage.services.use` on that project. Its
[quota-project guide](https://docs.cloud.google.com/docs/quotas/quota-project)
says this override can apply to client-based APIs and that the project checked
for API activation is also checked for rate quota. The [Drive limits
page](https://developers.google.com/workspace/drive/api/guides/limits) describes
per-project units but does not say explicitly whether a Drive OAuth request
accepts a *different* quota project from the one that owns its OAuth client.
A [Google Workspace CLI report](https://github.com/googleworkspace/cli/issues/729)
observed Drive rejecting a user without Service Usage permission when this
header was sent; that is evidence of an IAM check, not proof of where successful
requests consume Drive quota.

`tools/skirk_quota_project_probe.py` prepares one read-only `files.list` call
without the header and one with a separate test project's header using the
same isolated grant. It logs only HTTP status, a sanitized machine-readable
error reason. If Drive reports the second project's API as
disabled when the header is present, that would be a stronger activation and
quota-project signal under Google's guide. Two successful calls alone would
not establish quota attribution; the follow-up below compares Drive API
usage telemetry for both projects after propagation. Even if the override
works, every user-owned quota project
would require project creation, Drive API activation, and the grant holder's
Service Usage permission. That setup is too heavy for the default client flow;
it could be an optional advanced capacity path if measured and supported.

The November 14 disabled-API control used the same refreshed test OAuth grant
for both calls. `files.list` without the header returned HTTP 200; with
`X-Goog-User-Project: flowing-precept-514715-p1` it returned HTTP 403 with
machine reason `SERVICE_DISABLED` while Drive API was not enabled on that
second project. This supports that Drive checks the header-selected project
for API activation. After the second project's Drive API showed **Enabled**
in Cloud Console, the identical bounded pair returned HTTP 200 both without
and with the header. This is a direct compatibility result for one shared
OAuth client and user-owned quota project. Google's quota-project
guide says the activation-checked project is the rate-quota project, so quota
movement is a reasonable *inference*, not independently measured usage
attribution at that point. The API metrics pages for both test projects showed
no request data at this initial check, despite the successful live requests.
The time-series follow-up below provides a separate request-count observation.

A November 15 follow-up found populated Cloud Console Drive method tables for
both projects. Using the same isolated `shahabpre` OAuth grant, a bounded burst
of **100 identical, read-only `files.list` requests** with
`X-Goog-User-Project: flowing-precept-514715-p1` returned **100 HTTP 200**
responses and no API error between **06:04:08 and 06:05:17 UTC**. These calls
represented about **10,000 estimated Drive units** and created no files.
The status-only private summary is
`.skirk-runs/quota-attribution-header-burst-20261115.json`.
The planned Windows Drive test was held for this quiet window. After
propagation, Cloud
Console's **one-hour Drive traffic by HTTP response code** chart showed these
minute-bucket HTTP 200 rates (Console local time, UTC−6):

| Minute | Header-selected project (requests/s) | OAuth-client project |
| --- | ---: | --- |
| 00:04 | 0.058 | No point |
| 00:05 | 0.878 | No point |
| 00:06 | 0.675 | No point |
| 00:07 | 0.056 | No point |

The displayed rates total **about 100.02 requests** after multiplying each
one-minute bucket by 60; the 0.02 difference reflects chart rounding. These
are Console aggregates, not a per-request trace, and their displayed bins can
straddle the probe's actual 06:04–06:05 UTC run. They nevertheless match the
100 successful header-bearing calls while the OAuth project's chart has no
corresponding points. This is bounded, direct evidence that Google **recorded
Drive request count on the header-selected project**, separate from the
OAuth-client project. It does not directly measure consumed quota units,
billing, sustained rate limits, or the capacity of 200,000 active users.

The second project's rolling six-hour `files.list` total read **6,744** shortly
after the burst and **6,752** about five minutes later; the OAuth project's
read **14,794** at both observations. The second project's rolling one-hour
total fell from **444** before the burst to **382** afterward as older traffic
left the interval. Those rolling totals alone would have been misleading.
Google's
[`serviceruntime` metric reference](https://docs.cloud.google.com/monitoring/api/metrics_gcp_p_z)
says `api/request_count` is sampled every 60 seconds and can remain invisible
for up to 180 seconds after sampling. Google's
[quota-project guide](https://docs.cloud.google.com/docs/quotas/quota-project)
states that the project checked for API activation is also checked for rate
quota; the disabled-API control and this project-specific request metric
support using a separate quota project for bounded operations. A sustained
rate-limit test, quota-unit telemetry, or Drive-specific Google confirmation
would still be needed before promising independent capacity at scale.

The Go Drive transport now accepts optional `drive.quota_project` in a kit
profile. It validates the standard Google Cloud project ID form and adds the
header to Drive API calls only; OAuth refresh and revocation remain on their
normal token endpoints without that header. An empty field keeps existing
behavior. A configured project that is disabled or denies Service Usage access
must surface an error rather than silently falling back to the shared OAuth
project's quota. Setup must place the same field in both client and exit
profiles and use it for mailbox creation when that advanced option is chosen.

On November 14, 2026, the Google Cloud Console for the isolated
`skirk-transport-research-2026` test project displayed
1,000,000 units/min/project and 325,000 units/min/user for Google Drive API.
Usage was zero at the initial, pre-test inspection. The console also listed legacy
12,000 requests/min quotas labeled "Previous quota"; the documented unit
model is the basis for estimates here. This confirms configured limits for
this test project, not an achievable user capacity.

Capacity must be computed from a *paired client and exit* 24-hour trace:

```text
sum(all Skirk processes' units/min for project) < project units/min
sum(processes using one Google user in project) < user units/min
sum(project units/day, project egress bytes/day) < operational budgets
```

One archived **paired** muxv4 run started five clients together. All five
completed a 64 MiB download with HTTP 200; the slowest took 77.945 s and the
archived aggregate rate was 34.44 Mb/s. Forty small requests completed without
failure, but the archived summary reported 9.44 s p95 total latency. Aligning the
first four 15-second quota windows from each client with the exit's first
one-minute window gives 102,650 client plus 44,900 exit estimated Drive units,
or **147,550 units in about one minute**, with zero reported API errors. The
client and exit logs are in ignored artifact directory
`.skirk-runs/protocol-lab/multiclient-20260514T165626Z/`; the exit did not
record a final partial quota window. This short run shows five simultaneous
clients worked for that specific workload, not sustained service capacity.

Linear scaling of that one-minute rate to the current 325,000 user-unit limit
would yield about 11 identical clients sharing one grant; the project limit
would yield about 34. These are **arithmetic projections, not supported-user
counts**. The five-client run was only about 78 seconds, the later exit tail
was unreported, and Drive latency, cleanup debt, daily egress, storage,
backend throttles, and policy may bind first. The separate archived 100 MiB
control's 27,600 units over 52.105 seconds is **client-only** and cannot fill
these gaps. Multi-account sharding to circumvent limits is explicitly
disallowed by the [Workspace policy](https://developers.google.com/workspace/workspace-api-user-data-developer-policy).

### New bounded concurrent-client test

The new test account and project also ran a controlled `google_front` workload
with 32 MiB of random bulk data and four small requests per client. Client
and exit estimated units were captured together by
`tools/skirk_live_capacity_bench.py`. A rolling 60-second observed-unit guard,
absolute workload-unit cap, 120-second deadline, and immediate error/backoff
stop bounded each run.

| Concurrent clients | Bulk completions | Small completions | Aggregate bulk goodput | Small TTFB p95 | Client + exit estimated units |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 3 | 3/3 (96 MiB) | 12/12 | 28.655 Mb/s | 3.519 s | 67,210 |
| 5 | 5/5 (160 MiB) | 20/20 | 60.690 Mb/s | 3.281 s | 89,110 |
| 10 | 10/10 (320 MiB) | 40/40 | 78.244 Mb/s | 4.941 s | 209,075 |

All completed runs had zero Drive API errors and no quota backoff. The
ten-client harness ran for 40.175 seconds; the overlapping bulk-transfer
window was 34.308 seconds. Its 209,075 estimated units average roughly
312,000 units/min over the harness wall time, already close to the displayed
325,000 user-unit/min limit. The sampled rolling 60-second maximum of
185,025 units missed the final metrics update, and a five-second normalized
rate peaked at 539,461 units/min. Neither short-window figure proves
whether Google would throttle a sustained ten-client workload.
The ten-client result is the **largest completed new test**, not a maximum
supported-user count or a sustained-capacity claim. Its aggregate speed grew
far less than client count and interactive p95 rose. The harness first stopped
some runs at self-imposed safety guards; those partial runs are not Google
quota failures. Private results are in
`.skirk-runs/personal-test/capacity-three-rolling100/summary.json`,
`.skirk-runs/personal-test/capacity-five-final/summary.json`, and
`.skirk-runs/personal-test/capacity-ten-final/summary.json`. Repeat over
hours, with real exits and regional paths, before setting a user-capacity
target.

At the default settings, a connected but quiet exit still discovers new
sessions with roughly one priority `files.list` each second (about 6,000
estimated units/min). The newest client loop pauses receive listing once it
has **zero open streams**; a successful local upload wakes it before an exit
reply can arrive. A live client made no calls in an initial 18-second idle
window, and a separate post-request trace was flat from 20 through 70 seconds
after curl returned. This is zero *client receive-list* usage in that state,
not zero whole-system usage. A quiet but open SOCKS or HTTP CONNECT stream
continues to poll; exit discovery, cleanup, uploads, and downloads consume
Drive units. When the final stream closes, the client schedules one delayed
list five seconds later to catch late reply objects, then pauses. Objects that
arrive later still need a future request or the stale-file janitor; storage
debt needs measurement.

With the prior 60-second client idle backoff, 200,000 concurrently running
clients on one shared project would have spent roughly 20 million list
units/min before exits or traffic, 20 times the displayed project limit. The
new zero-stream pause removes that particular steady client cost once streams
close. If all 200,000 users also ran separate quiet exits on the same project,
their exit discovery alone would still be about 1.2 billion units/min.
Installed users who have stopped Skirk generate no polling calls. Per-user
quota projects can distribute the measured request count, but scaling that
design to 200,000 active users remains untested and depends on Cloud setup,
rate quotas, publisher grant limits, and Drive's actual behavior. The
published per-user quota still constrains each active user.

## OAuth lifetime and user experience

The reported [seven-day expiration issue #29](https://github.com/ShahabSL/Skirk/issues/29)
asks for a renewal command that keeps the existing session and mailbox. The
current implementation supplies that command and updates exported client
profiles as well as the exit config. Existing imported clients must receive
the renewed grant too: each client calls Drive directly and cannot fetch a
new profile from the mailbox after its own refresh token has expired.

An external Google OAuth app in **Testing** grants Drive refresh tokens that
expire seven days after consent. Moving it **In production** removes that
specific Testing expiry, but does not guarantee indefinite service or
verification. Google documents other failures: revocation, six months of
inactivity, token-count limits, time-based grants, and administrator policy.
See [Google's audience rules](https://support.google.com/cloud/answer/15549945)
and [refresh-token guidance](https://developers.google.com/identity/protocols/oauth2#expiration).
The isolated `skirk-transport-research-2026` project has the non-sensitive
`drive.file` scope, a Desktop client, and a working mailbox under the
dedicated personal test account. Its OAuth app is now **In production**. We
verified the
`getephemra.com` Search Console Domain Property through DNS, published the
[Skirk homepage, privacy policy, and terms](https://skirk.getephemra.com/),
and published verified branding. The new test-account grant was issued after
publication and `setup renew-oauth` updated the isolated exit and client kit.
This removes the Testing-specific seven-day refresh-token expiry for that
grant; it cannot prevent other Google revocation conditions. A separate
[draft site PR](https://github.com/ShahabSL/shahablavasani.com/pull/1)
contains an alternate Skirk information page, deployed directly from its
branch without merging into the site's main branch.

The domain is publisher infrastructure for this OAuth app, not a Drive
transport dependency or a domain each end user must own. Google's
[domain verification requirements](https://support.google.com/cloud/answer/13804266)
apply when an OAuth publisher needs its app domain verified. For 200,000
independent users, the [OAuth audience rules](https://support.google.com/cloud/answer/15549945)
say the 100-new-user cap is for apps that present an unverified warning when
requesting unapproved sensitive or restricted scopes. Skirk's current
`drive.file` scope is non-sensitive and this app's branding is verified, so
that particular cap does not by itself require a separate OAuth client for
every user. Google's new-user grant rate, account policy, Drive quotas, and
the application's actual use case still need validation at that scale; this
one project is an isolated test, not a deployment capacity claim. A separate
production project is Google's recommended isolation; changing the OAuth app's
Cloud project changes its Drive app identity, so existing `drive.file` mailbox
access must be revalidated and a new kit may be needed. Google identifies the
Drive Picker app ID as the [Cloud project number](https://developers.google.com/workspace/drive/picker/reference/picker.pickerbuilder.setappid),
and recommends a separate OAuth client type for each platform. Skirk's current
renewal command intentionally requires the same client ID, while its
exit-generated profile is imported into mobile clients. A phone-native renewal
flow therefore needs platform credentials and a tested mailbox-access handoff;
the desktop loopback OAuth flow is [deprecated for mobile client types](https://developers.google.com/identity/protocols/oauth2/resources/loopback-migration).

The implemented `skirk setup renew-oauth --kit skirk-kit --restart-exit`
reauthorizes the original OAuth client and updates the kit while preserving
the tunnel secret, session, and mailbox identity. An already imported client
still holds its old refresh token: every device needs the updated profile
re-imported when that grant is invalid. The UI should show a specific
"Google authorization expired" recovery action, a clear device-update step,
and separate messages for rate limiting, storage, and policy denial. It should
never display a fabricated remaining project quota or promise "permanent"
login. The profile contains a refresh token and tunnel secret; treat export,
QR, backups, and logs as credential handling.

## iOS device and distribution gates

The new iOS app and extension build from source for iOS Simulator, and the app
launches on an iPhone 17 Pro simulator with a disconnected profile-import UI.
The tested iOS 26.5 and 27.0 Simulators lack the `nehelper` service: the VPN
manager fails to load preferences with `NEConfigurationErrorDomain Code=11`
before the extension starts. Explicit ad-hoc packet-tunnel entitlement signing
made the Simulator reject the app launch. There is no signed physical-device
packet or throughput result. Its default
IPv4 and IPv6 routes pass packet flow through a local HEV SOCKS converter; the Go
engine's own Google API connections use ordinary networking. Apple's
[Packet Tunnel documentation](https://developer.apple.com/documentation/networkextension/nepackettunnelprovider)
defines included/excluded routes and a separate through-tunnel connection API,
but that does not prove how this Go networking stack will route on a device.
A signed device smoke test must record Drive destination connections and
packet-flow destinations after network settings apply, and verify no recursive
Drive traffic on Wi-Fi, cellular, and an IPv6-capable network. Also verify DNS,
lock/wake, stop/reconnect, and that IPv6 traffic follows the intended product
policy. No speed or leak claim should be made from the unsigned build.

The vendored HEV bridge now enables mapped AAAA answers only for iOS, using a
configured ULA `/96` prefix. Android keeps its existing IPv4-only mapping.
A native ASan/UBSan harness passed A/AAAA conversion, mixed questions, SOCKS
destination recovery, and 50,000 malformed DNS inputs. This validates the C
mapping path, not DNS behavior inside a signed iPhone tunnel.

The host and extension now include a shared `ProfileVault`, a configured
common Keychain access group, and an extension fallback to that vault
when `startTunnel(options:)` has no profile. Apple supports this design for
targets signed by one team in its [Keychain sharing guide](https://developer.apple.com/documentation/security/sharing-access-to-keychain-items-among-a-collection-of-apps).
It remains **unverified on a signed device**: confirm the provisioning profiles
contain the shared group, migration from the app-private group preserves the
new shared item, and an OS-initiated reconnect can read both profile and
stable client ID. Apple says a Keychain query without an explicit group may
search every group available to the app, so migration reads and deletes must
target the legacy group explicitly. The `AfterFirstUnlockThisDeviceOnly`
accessibility supports background use after the first unlock, according to
[Apple](https://developer.apple.com/documentation/security/ksecattraccessibleafterfirstunlockthisdeviceonly);
it cannot reconnect before that unlock after reboot.

There is a separate Apple platform gate: [TN3120](https://developer.apple.com/documentation/technotes/tn3120-expected-use-cases-for-network-extension-packet-tunnel-providers)
says packet-tunnel providers should not host local listeners or proxy servers.
The current extension starts an internal loopback SOCKS listener for the HEV
packet converter. The technote does not discuss this exact internal bridge, so
whether Apple considers the design a supported use case is unresolved. Obtain
Apple guidance or redesign it before claiming distributable iOS VPN support.

## Next experiments, in order

1. Preserve the owner's reported Google support determination and resolve the
   Apple Network Extension use-case question. Bounded tests on one dedicated
   account can validate engineering behavior; avoid an unbounded account load
   test or a production capacity claim without paired measurements.
2. With authorized test accounts and approved use, run randomized same-day
   paired muxv4 controls against one candidate at a time: 100 MiB and 1 GiB
   bulk plus small probes, browser/media overlap, five simultaneous clients,
   restart/cleanup soak. Capture both endpoints and Cloud Console quota.
3. Measure p50/p95/p99 small-request time to first byte, bulk Mb/s, terminal
   failures, Drive `403`/`429`, calls and units/GiB, project egress bytes,
   object count, and stale cleanup debt. Use the existing
   [promotion gates](transport-research.md#promotion-gates), especially mixed
   traffic and no more than 1.15x muxv4 units/GiB.
4. On a signed iPhone, do a low-volume routing and lifecycle smoke test before
   any iOS performance run. Verify provider egress, IPv4/IPv6 behavior, DNS,
   Keychain reconnect, and a real HTTPS page through the tunnel.
5. Test a direct-carrier prototype only where its network path is permitted
   and reachable. Compare the same user workload, reliability, and cost with
   Drive muxv4; do not infer performance from a direct socket microbenchmark.
