# Skirk engineering evidence — November 14–15, 2026

## Decision

The work is isolated on `codex/skirk-architecture-20261114` and
[draft PR #32](https://github.com/ShahabSL/Skirk/pull/32); `main` remains at
`2c52ff85`. This folder uses the personal `ShahabSL` Git identity and
credential, without switching the global work account.

The personal GitHub repository had 792 stars, 94 forks, and 68,854 cumulative
asset download counts across 172 paginated release assets on November 14.
Asset counts are not unique users or simultaneously active clients.

The release transport remains muxv4. Users can choose Direct Google or the
pinned `www.google.com` front; the front remains the default. Normal-priority
Drive discovery now requests 1,000 objects per page, with
`drive.normal_list_page_size` available to select 100 or 500. Fixed metadata
slot delivery, separate slot HTTP pools, combined discovery, and
interaction-epoch priority remain opt-in experiments. An adaptive combined
discovery candidate is also opt-in; both its first and staged variants have
bounded sparse live A/B results, with different text/file tradeoffs. No tested
variant wins on both message delay and file goodput
across **all** full-app workloads,
and no global speed or latency limit has been established.

The published Skirk OAuth research app is **In production** with `drive.file`.
This removes the automatic seven-day refresh-token lifetime specific to an
External app in Testing; it does not prevent revocation, inactivity, policy,
or other token failures. The publisher's verified domain is infrastructure for
that app, not a domain each person needs to import a profile. A user who
uses the shared published app needs neither their own domain nor their own
Cloud project. A user who publishes a separate OAuth app is responsible for
that project's setup. The client recovery UI presents exit-grant renewal and
profile replacement when needed, and labels Cloud Console settings as an
owner action. Google's
[OAuth documentation](https://developers.google.com/identity/protocols/oauth2)
describes the Testing limit and other expiry causes. A client button cannot
publish a Cloud project it does not own or revive a revoked refresh token;
the exit owner must renew a failing exit grant. Existing clients can keep
their profiles when their own grants remain valid and the mailbox, secret,
and session stay the same: the reporter of
[issue #29](https://github.com/ShahabSL/Skirk/issues/29) saw three clients
reconnect after renewing only the exit. Distribute an updated profile when a
client grant has also expired or the user chooses a new grant; the setup guide
now says this explicitly (`b5eaf9f`). The separate request for
[multiple OAuth clients per server](https://github.com/ShahabSL/Skirk/issues/25)
is not implemented by this renewal flow.

Commit `6944745` makes rejected **client** access an actionable state on
Android, iOS, and desktop: **Import renewed profile** becomes the primary
action, the error says Google rejected this device's access, and Cloud
production steps sit in a disclosure for project owners. A still-valid client
keeps its profile after an exit-only renewal. These are UI and message changes;
they do not establish that a revoked grant was recovered end to end on every
platform.

Renewal now identifies a kit by session, secret, Drive space and folder, and
OAuth client ID. Client transport settings such as list page size, pool mode,
quota project, and exported slot mode no longer cause a false kit mismatch;
focused tests verify the renewed token preserves those settings and still
reject a different Drive space (`dc6db7f`). The non-root systemd user-service
example now stops restart loops on terminal OAuth exit status 78 (`efc1e90`).

## What the transport measurements establish

ABBA tables below use the median of two arm-level results per mode unless
stated otherwise. “Scheduled p95” starts at the intended message send time;
“request p95” starts when the client actually begins it. These small samples
are not population latency percentiles.

An earlier default-mux versus clean `main` 16 MiB download sequence was
incomplete when the second `main` arm hit three 28-second exit uploads. A
fresh exact-binary comparison after the normal-list page-size change used
main/current/current/main order with all experimental flags off. All four
SHA-verified files and 48/48 small messages completed with zero Drive API
errors, and exact cleanup verified zero objects across eight mux/slot
prefixes after deleting 18 residuals. Two-arm medians were:

| 16 MiB download, pinned front | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Clean `main` (`2c52ff85`) | 15.461 s | 4.739 s | 11.712 Mb/s | 24,805 |
| Current branch | 19.674 s | 7.366 s | 8.994 Mb/s | 21,980 |

The current branch used about 11% fewer estimated units, but its request p95
was about 55% higher and file goodput about 23% lower in this first bounded
pair. The source of that gap is not isolated. A
[media diagnostic](main-current-media-diagnostic-2026-11-14.md) found that
the nine bulk media GETs per arm had medians of 0.755/1.869 s on the two
main arms and 3.265/2.666 s on the middle current arms; priority and list
medians were much closer. A temporary Google or network slowdown during the
middle arms could mimic a code regression. The result rules out claiming the
current default is consistently faster than clean `main`. Raw data are private under
`.skirk-runs/main-vs-current-16m-20261114-02/`; the earlier incomplete
attempt is under `.skirk-runs/final-main-abba-16m-20261114/`.
An attempted reverse-order recheck produced no balanced comparison: a first
harness invocation requested an exit proof unsupported by clean `main`, and
the corrected invocation stopped in its first current arm after a pinned-front
bulk upload returned HTTP 502 and other uploads hit 28-second context
deadlines. Neither run changes the pair medians or identifies a code cause.
The first run's two sessions had three residual objects deleted; the
corrected run's one session had one. Every exact mux/slot prefix was zero on
immediate and delayed checks. Private summaries and cleanup records are under
`.skirk-runs/main-vs-current-download-reverse-16m-20261114-{01,02}/`.

A third **reverse-order** download attempt then completed
current/main/main/current using the same two prebuilt binaries. All four
SHA-verified 16 MiB files and 48/48 messages completed with zero API errors.
Its two-arm medians were:

| Reverse 16 MiB download | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Clean `main` | 15.063 s | 3.994 s | 11.418 Mb/s | 23,480 |
| Current branch | 15.564 s | 6.329 s | 12.444 Mb/s | 24,080 |

Current request p95 was worse in both complete orderings, but file goodput
changed sign: the current arms were 13.775/11.113 Mb/s here versus main's
11.516/11.320. These short samples do not isolate a code cause or support a
stable file-throughput regression claim. Private raw summary:
`.skirk-runs/main-vs-current-download-reverse-16m-20261114-03/`. Exact
cleanup deleted 19 residual objects; all eight generated mux/slot prefixes
were zero immediately and after a delay, without sweep errors or truncation.

A reverse-order **upload** ABBA (current/main/main/current) also completed
four exact 16 MiB files and 48/48 messages with zero API errors. Exact
sweeps deleted 37 residual objects; immediate and delayed checks found zero
across eight mux/slot prefixes:

| 16 MiB upload, pinned front | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Clean `main` | 16.990 s | 5.609 s | 10.366 Mb/s | 23,905 |
| Current branch | 18.223 s | 6.709 s | 9.185 Mb/s | 23,630 |

The current upload pair was about 20% slower by request p95 and 11% slower
by file goodput, with nearly equal estimated units. Main's two request-p95
arms ranged widely (3.719/7.500 s), so the pair median is fragile. This
reinforces the measured concern without isolating which code or Google timing
caused it. Raw data are under
`.skirk-runs/main-vs-current-upload-16m-20261114-01/`.

The corrected interaction-epoch plus metadata-only candidate did beat the
default in two short 16 MiB pairs: download scheduled text p95 was 2.608
versus 6.603 s and file goodput 16.600 versus 10.671 Mb/s; upload text was
3.755 versus 7.911 s and file goodput 14.882 versus 11.928 Mb/s. The
five-client 16 MiB upload pairs reversed the bulk result: candidate text p95
was 5.098 versus 7.767 s, while aggregate file goodput was 20.444 versus
25.020 Mb/s. Candidate 100 MiB arms stalled or stopped before a complete
balanced comparison. This is why a short-message win cannot be called a
generally better transport. Methods and raw paths are in
[architecture frontier](../architecture-frontier-2026.md).

Skirk sees encrypted TCP bytes, not a messenger's text-versus-attachment
intent. Priority can favor a small, recently quiet stream when text and file
use separate connections, but it cannot reorder bytes already placed before
the text on one TCP connection. Google request scheduling and backend tails
are shared, so giving small frames faster service can consume opportunities
that carry file chunks. The five-client and 32 MiB results measure that
tradeoff, which a synthetic slot RTT alone cannot reveal.

Each full-app slot comparison used one pinned-front binary in both arms, a
fresh session per arm, twelve new-stream messages alongside a SHA-verified
16 MiB file, and an authenticated exit check.

| Full-app comparison | Scheduled message p95 | Request p95 | File goodput | Estimated Drive units, both peers |
| --- | ---: | ---: | ---: | ---: |
| Download, default mux control | 11.163 s | 3.365 s | 15.373 Mb/s | 23,355 |
| Download, opt-in fixed-slot delivery | 11.364 s | 4.552 s | 14.256 Mb/s | 22,043 |
| Upload, default mux control | 12.735 s | 3.175 s | 13.299 Mb/s | 25,105 |
| Upload, opt-in fixed-slot delivery | 11.143 s | 4.124 s | 12.234 Mb/s | 22,193 |

All eight arms delivered 96/96 messages, eight exact file bodies, and eight
exit proofs with zero Drive API errors and verified exact-session cleanup.
Slot delivery saved estimated units but worsened request p95 and file goodput
in both directions. The sender's slot ACK means in-memory mux processing; it
does not prove bytes reached the destination socket. Neither path guarantees
crash-durable byte delivery. Source and raw locations are in
[slot delivery](../metadata-slot-delivery-experiment.md).

The separate slot-pool experiment kept fixed-slot delivery **on in both
arms**. It changed only whether known-ID slot GETs and PATCHes used separate
same-route HTTP connection pools. Its 16 MiB download and upload ABBA runs
each completed 48/48 messages, four exact files, four exit proofs, and zero
Drive API errors. All 16 generated mux/slot prefixes across both runs passed
immediate and delayed zero-file checks. Pair medians were:

| Slot-pool mode | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Download, shared slot connection pool | 11.911 s | 4.787 s | 12.362 Mb/s | 22,653 |
| Download, separate slot GET/PATCH pools | 11.375 s | 3.227 s | 10.770 Mb/s | 21,400 |
| Upload, shared slot connection pool | 11.119 s | 2.786 s | 10.342 Mb/s | 24,013 |
| Upload, separate slot GET/PATCH pools | 10.624 s | 2.675 s | 10.543 Mb/s | 21,520 |

The split lowered download request p95 by about 33% but lowered download
file goodput by about 13%. In the upload pair, the split showed a smaller
request p95 change and overlapping bulk rates; control file rates ranged from
8.350 to 12.334 Mb/s. It is not a universal lower-latency result or a reason
to enable slot delivery by default. Raw data are private under
`.skirk-runs/slot-pools-{download,upload}-20261114-01/`.

The final-code **32 MiB download** ABBA repeated the pool comparison under a
larger file. Both modes again used slot delivery and the new normal-list page
size; only candidate arms split the pools. All four files, 48/48 messages,
and four exit proofs verified with zero API errors. The two-arm medians were:

| 32 MiB download | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Shared slot connection pool | 14.240 s | 4.997 s | 21.758 Mb/s | 23,443 |
| Separate slot GET/PATCH pools | 10.758 s | 2.788 s | 12.622 Mb/s | 25,998 |

The split's request p95 was about 44% lower while file goodput was about
42% lower; estimated units were about 11% higher. Both control file rates
(21.019/22.496 Mb/s) exceeded both split rates (14.275/10.969 Mb/s) in this
pair. This stronger file penalty rejects making the pool split a default,
despite its repeatable short-message signal. Raw data are under
`.skirk-runs/slot-pools-download-32m-20261114-01/`. Exact sweeps deleted 19
residual objects; immediate and delayed checks found zero across all eight
generated mux/slot prefixes.

Other Drive methods did not supply a proven faster full-app path. A synthetic
two-slot substrate, outside Skirk TCP, reached **1.265 s scheduled RTT p95**
for 20 request/reply exchanges without a forced pause, and survived skipped
Drive versions; the full-app slot comparisons above did not reproduce a joint
text/file win. In eight sequential 1 MiB upload-mode arms, multipart median
was **888.5 ms** versus **1,155.9 ms** for resumable upload including session
creation. The first Drive watch attempt failed public HTTPS preflight before
any Drive call. A later bounded
[live push probe](drive-push-notification-live-2026-11-15.md) used temporary
public receivers: `files.watch` delivered six of six known-file update
notifications at **2.531 s median** after PATCH response, versus **0.188 s**
for immediate known-ID GET; `changes.watch` delivered five of six within ten
seconds at **5.450 s observed median**, versus **0.159 s** for GET. Those GET
times were measured from the same PATCH-response anchor, not after push.
The arms used 755 and 860 estimated units with zero API errors. Both channels
were stopped, exact objects were deleted and rechecked, and temporary Worker
and D1 receivers were removed. A known-file watch cannot discover new files;
change-feed notifications still require `changes.list`. Both need a public
callback, which does not replace polling for a Google-front-only client.
Neither arm measured a full-app latency win. See
[slot substrate](../architecture-frontier-2026.md),
[earlier upload and watch probe](drive-watch-and-upload-mode-2026-11-14.md), and their
bounded methods; none resolves observed 16–30-second Google response tails.

An opt-in **combined discovery** mode queried one legacy mux prefix rather
than separate priority and normal class lists. Same-binary pinned-front
16 MiB download and reverse-order upload ABBAs kept page size and all other
transport choices equal. Each direction verified all four exact files,
48/48 messages, and four exit proofs with zero API errors. Two-arm medians
were:

| Discovery mode | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Download, default split p0/p1 lists | 13.240 s | 7.065 s | 14.050 Mb/s | 24,480 |
| Download, opt-in combined list | 14.939 s | 4.233 s | 12.693 Mb/s | 22,755 |
| Upload, default split p0/p1 lists | 18.530 s | 8.326 s | 9.429 Mb/s | 24,030 |
| Upload, opt-in combined list | 15.074 s | 5.217 s | 11.778 Mb/s | 24,455 |

On downloads, combined discovery cut request p95 by about 40% and units by
7%, but scheduled p95 rose about 13% and file goodput fell about 10%.
Scheduled latency includes client queueing and is closer to an end user's
wait, so the request-only improvement is not a clear UX win. On uploads,
combined improved all three timing/rate medians but used about 2% more units;
individual file rates varied widely in both modes (combined 15.724/7.831,
split 11.051/7.806 Mb/s). The broad list uses up to 16 pages of 100 per poll
and retains a page token; a dense normal backlog can delay older priority
objects until subsequent pages are read. These sparse runs do not test that
case. Keep split discovery as the default pending backlog and repeated mixed
workloads. Raw private summaries are under
`.skirk-runs/combined-list-{down,upload}-16m-20261114-01/`. The download
run's exact cleanup deleted 28 residuals and found zero files across eight
mux/slot prefixes immediately and after a delay. The upload cleanup deleted
17 residuals and likewise found zero across eight exact prefixes on both
checks.

The separate adaptive discovery candidate combines a sparse one-list path
with a bounded priority-list fallback when the broad result is truncated or
incomplete. Local tests passed for an untagged older-peer object, 8,001 newer
normal objects hiding a later priority object, retries of partial pages with
and without continuation tokens, and independent priority cursor
continuation. A deterministic HTTP test asserts that the adaptive broad
query uses page size 1,000 by default and continues its token, while the
legacy combined query remains at 100. Full Go, focused race, vet, and 29
adaptive-branch Python benchmark tests passed; after integration, the full
Python harness suite passed 49/49. The harness also rejects an incompatible
exit-probe/main-binary combination before creating output. These tests
establish the local discovery cases. The adaptive broad path can advance up
to 4 × 1,000 objects per poll, and the priority fallback adds up to 4 × 100.
Four large broad pages can run before the priority fallback under a dense
backlog; that dense case remains unmeasured on live Drive. The opt-in
implementation is integrated in commits `b0fb34b`, `6d9b89e`, and
`41688a9` on the research branch.

Code review found that the former default p1 sliding scan restarted from the
newest page on every poll. With more than 4,000 persistent newer p1 objects,
an older p1 object could remain beyond its four-page window. Commit `52cf03b`
now saves the normal-list page token and the first scan's watermark. The
initial poll reads up to four pages; continuation polls use up to three older
pages and one fresh head page. It continues after the last stream goes idle,
replays arrivals from the saved watermark when the scan completes, and
restarts from the head if Drive rejects the token. Deterministic tests cover
older-page progress under repeated newer inserts, more than one head page of
new arrivals, idle continuation, and rejected-token recovery
([test source](../../internal/skirk/p1_pagination_test.go)). The head check
runs after up to three continuation pages, so its real interactive-latency
effect under backlog is unmeasured. This fixes the specific deterministic
starvation case without proving delivery under arbitrary concurrent Drive
index churn or persistent API errors. A subsequent
[bounded live Drive test](dense-p1-live-2026-11-14.md) used 402 real p1
objects with the supported 100-object page setting: the older target was
absent after four pages, then appeared on poll two through the saved token.
The fresh-head check also ran, and all 402 objects were deleted and verified
missing by ID, with the prefix empty immediately and 30 seconds later. The
run took 520.37 seconds and an estimated 43,515 Drive units. This tests the runtime
list method under one steady mailbox, not the default 1,000-object page size
or a full app file transfer. Adaptive
discovery had continuation first, but it no longer has a
unique correctness path through this tested backlog case.

The same-binary pinned-front adaptive **download** ABBA measured the
integrated four-broad-pages-before-p0-fallback implementation. It used an 8 MiB
file and twelve scheduled new-stream messages in each candidate/control/
control/candidate arm. All four file hashes, 48/48 messages, and exit proofs
passed with zero API errors. Two-arm medians were:

| 8 MiB download discovery | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Separate p0/p1 default | 15.252 s | 4.221 s | 7.100 Mb/s | 25,305 |
| Opt-in adaptive broad plus p0 fallback | 11.717 s | 3.989 s | 9.269 Mb/s | 21,305 |

Both candidate medians improved in this sparse download pair, with about 16%
fewer units. Individual control file rates were 8.854/5.345 Mb/s and candidate
rates were 7.814/10.724 Mb/s, so the pair does not establish a universal or
causal speed win. Private raw summary:
`.skirk-runs/adaptive-list-down-8m-20261114-01/`. Exact cleanup deleted six
residual objects; all eight mux/slot prefixes were zero immediately and
after the delayed check. Dense live backlog behavior remains unmeasured.

The reverse-order **upload** ABBA also completed four exact 8 MiB files,
48/48 messages, and four exit proofs with zero Drive API errors. Its two-arm
medians were:

| 8 MiB upload discovery | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Separate p0/p1 default | 16.408 s | 5.721 s | 6.000 Mb/s | 27,030 |
| Opt-in adaptive broad plus p0 fallback | 11.292 s | 3.086 s | 9.528 Mb/s | 21,230 |

Both candidate file arms (9.669/9.386 Mb/s) exceeded both controls
(6.718/5.282 Mb/s), and the candidate used about 21% fewer estimated units.
The pair still cannot rule out time-varying Google behavior. Its private raw
summary is `.skirk-runs/adaptive-list-upload-8m-20261114-01/`; exact cleanup
deleted seven residual objects; all eight generated mux/slot prefixes were
zero immediately and after 30 seconds, with no failed or truncated sweep.
These two sparse 8 MiB pairs did not exercise a dense backlog or larger files,
so adaptive discovery remains opt-in. A second opt-in 1+3 variant now queries
p0 after the first 1,000-object broad page, before up to three more broad
pages (`a6dbea7`). It also preserves pending priority work after an enqueue
failure (`ac38998`) and uses a bounded exact-ID metadata retry to confirm
mailbox membership, with oldest-first selection and two separated 404 checks
before retiring a missing object (`b04bed2`). Isolated full Go, focused race,
and vet checks and deterministic retry tests passed. On the integrated branch,
the full preflight, `go test -race ./...`, and 49/49 Python harness tests also
passed. The earlier 8 MiB A/B numbers above measured the first adaptive
version and do not apply to this 1+3 variant. Its own live result follows.

The integrated staged binary then ran a same-binary **8 MiB download** ABBA
in control/candidate/candidate/control order. All four exact file hashes,
48/48 messages, and four exit proofs passed with zero Drive API errors.
Two-arm medians were:

| Staged 8 MiB download | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Separate p0/p1 default | 11.895 s | 3.145 s | 8.027 Mb/s | 23,230 |
| Opt-in staged adaptive 1+3 | 13.941 s | 4.127 s | 8.652 Mb/s | 20,580 |

This staged variant saved about 11% estimated units and had slightly higher
file goodput, while both message-delay medians worsened. Its file arms also
varied (candidate 10.145/7.159, control 8.617/7.436 Mb/s). This reverses
the earlier first-adaptive-version text signal and reinforces the opt-in
decision. Private raw summary:
`.skirk-runs/adaptive-staged-down-8m-20261114-01/`. Exact cleanup deleted
seven residual objects; all eight mux/slot prefixes were zero immediately
and after 30 seconds.

The first staged **upload** ABBA stopped in its fourth candidate arm after a
10-second client upload deadline; its first three files and 36/36 messages
verified, but it cannot supply pair medians. Exact cleanup deleted 33
residual objects and found zero immediately and after a delay across eight
generated prefixes. A reverse-order control/candidate/candidate/control
retry then completed all four exact 8 MiB files, 48/48 messages, and four
exit proofs with zero Drive API errors. Its two-arm medians were:

| Staged 8 MiB upload | Scheduled message p95 | Request p95 | File goodput | Estimated units |
| --- | ---: | ---: | ---: | ---: |
| Separate p0/p1 default | 14.212 s | 4.769 s | 6.584 Mb/s | 24,430 |
| Opt-in staged adaptive 1+3 | 13.100 s | 3.432 s | 7.981 Mb/s | 20,755 |

The retry favored the candidate on these medians and used about 15% fewer
units, but file arms varied widely (candidate 6.614/9.347, control
8.428/4.740 Mb/s). Together with the staged download text regression and
the failed first upload sequence, it does not establish a general joint
latency/file win. Private raw directories:
`.skirk-runs/adaptive-staged-upload-8m-20261114-{01,02}/`. Exact cleanup
for the complete retry deleted six residual objects; all eight mux/slot
prefixes were zero immediately and after the delayed check.

## Drive discovery and scale

A separate pinned-front probe created 528 production-shaped metadata names:
16 priority and 512 normal, including untagged older-peer objects. On legacy
normal discovery, page size 100 needed six calls, 600 units and 1.54–1.67 s
to see all 512 names; page size 1,000 needed one call, 100 units and
0.53–0.61 s in two sweeps. The runtime's four-page cap saw only 400/512 at
size 100 and all 512 at size 1,000. Priority discovery found all 16 in one
page regardless of size. Indexed property search missed untagged older peers,
so it was not promoted. All 528 IDs were deleted, confirmed 404, and absent
from delayed prefix checks. [Probe details](drive-list-page-size-2026-11-14.md)
include response bytes and rollback setting. Google's
[files.list reference](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list)
sets 1,000 as the maximum page size. The new runtime default has local tests;
this probe measured its API query shape rather than a full-app backlog run.

A same-binary 16 MiB sparse-mailbox ABBA then compared page sizes 100 and
1,000 with default mux and the pinned front. All four exact files, 48/48
messages, and four exit proofs verified with zero API errors. Page 100
two-arm medians were request p95 **6.386 s**, file **10.628 Mb/s**, and
25,455 estimated units; page 1,000 medians were **4.468 s**,
**11.839 Mb/s**, and 26,230 units. The individual page-1,000 file arms
spanned 7.259–16.418 Mb/s, far more than the page treatment's median gap;
these samples do not establish a sparse-path latency advantage or attribute
the main/current slowdown to page size. They do not support reverting the
page-size change. Private raw summary:
`.skirk-runs/page-size-sparse-16m-20261114-01/`. Exact cleanup deleted
nine residual objects; all eight mux/slot prefixes were zero on immediate
and delayed checks.

The [published Drive quota](https://developers.google.com/workspace/drive/api/guides/limits)
is 1,000,000 units/minute/project and 325,000 units/minute/user/project;
`files.list` costs 100 units and metadata `files.get` costs 5. At eight slot
GETs per second (4 Hz in each direction), one continuously active pair spends
2,400 units/minute on reads alone: a single project can support at most 416
such pairs by this arithmetic before writes, media, or retries; one user quota
would cap the same calculation at 135 pairs. Four metadata PATCHes/second add
12,000 units/minute per pair and lower those quota-only ceilings to 69 per
project and 22 per user. A 200,000-person simultaneously polling deployment
cannot use one shared project: even **one** 100-unit list per person per
minute is 20 million units/minute, twenty times the project limit. With the
same OAuth grant, 100 read-only `files.list` calls carrying
`X-Goog-User-Project` to a second enabled project all returned HTTP 200. That
project's one-hour Drive chart showed about **100.02 requests** after
rounding its minute buckets, with no corresponding OAuth-project points. A
disabled-API control had returned `SERVICE_DISABLED` for the selected
project. This directly supports bounded **request-count attribution** to the
header-selected project, not consumed quota units, billing, sustained rate
limits, or 200,000-user capacity. Each advanced operator would still need a
project, Drive API activation, and `serviceusage.services.use` access. See
[the quota-project record](../research-2026.md#candidate-separate-quota-project-with-the-same-oauth-client)
and private `.skirk-runs/quota-attribution-header-burst-20261115.json`.
The shared app's non-sensitive `drive.file` grant and verified branding do
not themselves impose the unverified-sensitive-scope 100-new-user cap;
OAuth grant rates, user policy, and sustained Drive use remain separate scale
questions. See [OAuth and audience analysis](../research-2026.md#oauth-lifetime-and-user-experience).
The 400-million-unit daily project threshold before potential charges is a
separate economic constraint, not a hard concurrency guarantee.

## Platform verification and remaining boundaries

At `b04bed2`, the integrated preflight passed after the staged adaptive and
OAuth changes, covering Go/vet, desktop build, and Android APK. Full Go race
tests and 49/49 Python harness tests also passed. Private logs are
`.skirk-runs/{preflight,go-race,python}-integrated-final-20261114.log`.

On a Pixel 7 API 36 emulator with a full-device VPN and the pinned front,
the on-demand authenticated exit proof took **2.014 s**. A synthetic HTTPS
origin served a page, a SHA-verified 1 MiB download in **4.298 s**, and a
SHA-verified 1 MiB upload in **7.877 s**. During the 310-second session, the
exit recorded 745 Drive calls, estimated at 75,455 units, with zero API errors or
backoff. Five residual exact-session objects were deleted; the immediate and
30-second delayed checks found zero. A separate final synthetic UI smoke at
`41688a9` imported a Direct-declaring profile with Google front as the
default, persisted an explicit Direct choice after restart, hid Test exit
in idle, connecting, and failed states, suppressed stale `invalid_grant`
recovery while another profile connected, and showed copy/replace recovery
for the current failed profile. It used no real credentials, Drive traffic,
or VPN packets;
its private evidence is `.skirk-runs/android-final-ui-20261114/result.md`.
These are single-device samples, not peak throughput or capacity results.

The iOS app and packet extension built for the iPhone 17 Pro Simulator. The
recorded UI suite had **2 passed, 1 skipped, 0 failed**: profile lifecycle
and route choice passed; the live profile test was skipped. Simulator Connect
previously stopped at `NEConfigurationErrorDomain Code=11` before the packet
extension made Drive calls. A later unsigned generic iPhoneOS build initially
failed with unresolved lwIP/yaml symbols from stale ignored HEV archives in
its first device slice. Commit `944e588` cleans the copied build tree before
that slice; generic iPhoneOS and Simulator builds then passed, along with Go
engine race tests. A signed physical iPhone packet-tunnel run remains
unverified; the available iPhone was not connected for this test. Private UI
result: `.skirk-runs/ios-ui-integrated-final-20261114.xcresult`. The exact
final code at `0942237` also rebuilt and repeated the Simulator UI suite
with **2 passed, 1 skipped, 0 failed**; the private result is under the
isolated `ios-final-ui-wt/.skirk-runs/` worktree. The macOS
ARM64 Tauri app bundle built locally and its 23 Rust command tests passed.
An isolated synthetic native GUI smoke at `dc6db7f` verified Google front as
the default, explicit Direct choice across restart, and renewal-style profile
replacement preserving route and ports. The tested bundle rebuilt at `b04bed2`
contained an ARM64 Go sidecar reporting that commit. In a separate fresh
`shahabpre` mailbox session, the native GUI selected the pinned Google front,
connected its local SOCKS proxy, and showed an authenticated **Test exit**
response in **3.494 s**. Through that app-managed SOCKS listener, a local
text GET returned HTTP 200 with exact body in **2.050 s**, a 1 MiB download
matched its SHA-256 in **3.470 s**, and a 1 MiB upload received the matching
SHA-256 in **5.084 s**. The client recorded 4,105 estimated Drive units and
the longer-running exit 22,055, with zero API errors or backoff; their run
durations differ, so these totals are not a paired per-minute rate. The app
disconnected and removed its imported profile. Exact cleanup deleted one
remaining mux object, then verified zero objects across both generated
`muxv4/` and `slotv1/` prefixes immediately and after 30 seconds. Private
evidence is under `.skirk-runs/macos-native-live-final-20261114/`, including
`exact-cleanup.json`; build log:
`.skirk-runs/macos-final-integrated-app-build-20261114.log`. This proves the
native proxy path for one bounded local-origin test, not automatic browser
proxy configuration or sustained Mac traffic; the later `5c8739a` and
`52cf03b` transport fixes were not in that bundle. Strict `codesign` and
`spctl`
checks failed on the locally built bundle. No Developer ID Application
identity or configured notary CI credentials were available, so a public
signed package and the reported Mac
[“damaged” app](https://github.com/ShahabSL/Skirk/issues/16) remain unresolved.

The [November 15 signing audit](macos-signing-audit-2026-11-15.md) separates
that distribution failure from browser routing. Re-signing an isolated copy
with an ad hoc identity and packaging/extracting it with `ditto` preserved
strict deep `codesign` verification; Gatekeeper still rejected the ad hoc
copy. The release workflow has a Developer ID/notarization path, but its five
required repository secret names and a local Developer ID Application
identity are absent. Xcode showed a team Admin with a Developer ID creation
option; actual certificate issuance and team-matched notary access were not
tested. The bundle in that earlier test started SOCKS/HTTP listeners without
configuring macOS system proxy settings. An optional **System proxy** mode was
subsequently added. In a bounded native Mac run, the GUI authenticated exit
proof took 3.927 s; a proxy-aware `URLSession` client with no explicit proxy
setting delivered exact text in 2.232 s, a SHA-verified 1 MiB download in
3.731 s, and a matching 1 MiB upload in 5.530 s through the pinned Google
front and Drive. Both peers had zero API errors, exact session cleanup deleted
59 residuals, and Wi-Fi proxy settings were restored. A separate temporary
network-service test covered restart recovery and external changes. The
[macOS system-proxy report](macos-system-proxy-live-2026-11-15.md) records
methods and limits. Safari and Chrome were not individually checked, and
signed/notarized distribution remains unresolved.

Native Windows x64 full Go tests and CLI smoke, plus Windows ARM64 Go/CLI
smoke, passed at commit `9704579` in
[native Windows CI run 43674155603](https://github.com/ShahabSL/Skirk/actions/runs/43674155603)
for [draft PR #32](https://github.com/ShahabSL/Skirk/pull/32).
The Windows x64 runner also passed 22 Rust command tests, built the portable
package with the Go sidecar and SHA-checked VPN sidecar, and kept the
packaged GUI running for 10 seconds. This is a native launch smoke, not a VPN
tunnel or file-transfer test on Windows. A later
[native Smoke run at `ae127e8`](https://github.com/ShahabSL/Skirk/actions/runs/43680170733)
failed five x64 and four ARM64 adaptive retry tests: pending retries made no
exact metadata GET. Their same-poll eligibility depended on wall-clock
`addedAt.Before(pollStarted)`. Commit `5c8739a` replaces that ordering with
an atomic poll sequence; local tests cover both withholding a new retry in
its creation poll and probing an older retry when the clock is simulated in
the future. Integrated Go race tests pass. The final code at `0942237` passed
[native Smoke run 43681029045](https://github.com/ShahabSL/Skirk/actions/runs/43681029045)
with all 14 jobs, including Windows x64 and ARM64 CLI and the native desktop
package matrix; [CI run 43681029059](https://github.com/ShahabSL/Skirk/actions/runs/43681029059)
passed all three jobs. The later `9bc514f` commit adds only a guarded live
test and evidence document. At `18ba457`,
[CI run 43686916494](https://github.com/ShahabSL/Skirk/actions/runs/43686916494)
passed 3/3 jobs; [Smoke run 43686916482](https://github.com/ShahabSL/Skirk/actions/runs/43686916482)
passed 14/14 jobs, including macOS x64 Tauri packaging and smoke. These
results are for exact code tip `18ba457`; `d86ce98` adds only the separate
watch evidence document.

A separate [native Windows CLI SOCKS run](windows-native-live-2026-11-15.md)
used a disposable session on the pinned front. Authenticated exit proof took
**5.509 s**; exact small text GET **3.860 s**; exact 1 MiB GET **9.671 s**;
and 1 MiB POST with matching length/SHA acknowledgment **29.500 s**. Both
Windows processes logged zero Drive errors, and exact cleanup deleted three
mux objects, then found both generated mux/slot prefixes empty immediately
and after 30 seconds. This validates the native CLI and SOCKS path, not the
Tauri GUI or a whole-device VPN.

A second [scoped Windows TUN run](windows-native-live-2026-11-15.md#targeted-windows-tun-traffic)
installed only `1.1.1.1/32` and left the default route unchanged. With curl
proxies disabled, the pinned endpoint returned HTTP 200 through the TUN in
**12.000 s**; a separate explicit SOCKS control took **9.422 s**. Client
uploads rose by four and exit downloads by five around the TUN request, with
zero Drive errors. The route and adapter were restored; exact cleanup deleted
three mux objects and found both generated prefixes empty immediately and
after 30 seconds. Exit and client shared the Windows runner, so a test-only
process-name exemption prevented the exit's own traffic from re-entering the
TUN. This proves one targeted native TUN data path, not whole-device default
route capture, the GUI Connect flow, a separate exit host, or sustained mixed
traffic.

The client checks cover route choice, profile recovery, exit reachability,
native Windows SOCKS, and scoped TUN traffic;
there is no comparative usability study behind a “best UI” claim.

The largest archived short live transfers were **74.928 Mb/s** for one
fronted 128 MiB file and **78.244 Mb/s aggregate** for ten 32 MiB clients.
They were local, short runs, not sustainable maxima. The largest measured
live concurrent workload in these records is ten clients, not 200,000.

The current mux also has a 30-second exit startup catch-up window. An `OPEN`
uploaded while the exit is offline longer can be skipped after restart; the
upload ACK is not a remote accept ACK. The existing TCP stream has no
resumption. A durable or bounded automatic recovery design needs an explicit
exit generation and replay rules; simply widening the window risks repeating
external side effects. See [outage analysis](../architecture-frontier-2026.md#exit-restart-and-in-flight-streams).

**Decision rule:** keep the default and user route choice for this research
branch. The new bounded p1 continuation removes the demonstrated deterministic
starvation case and reached the older target in one steady live Drive mailbox
at page size 100; the default page size 1,000 and arbitrary concurrent index
churn remain unverified live. Treat page size 1,000 as a bounded discovery
improvement with rollback. Keep fixed slots and pool splitting opt-in until
repeated, larger full-app mixed workloads show both interactive and file
benefit, verified cleanup, and acceptable quota. The first adaptive version's
sparse 8 MiB text advantage did not persist in the staged download. Larger
mixed workloads remain unmeasured.
