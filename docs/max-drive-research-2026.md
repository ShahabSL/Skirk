# Skirk main comparison and Drive transport limits — November 14, 2026

This follow-up uses `codex/skirk-max-drive-20261114`. The preceding audit is
preserved as commit `93f4a75` on `codex/skirk-audit-experiments-20261114`.
`main` is `2c52ff85`. The test account is the isolated personal kit in the
ignored `.skirk-runs/` directory. The user's main Gmail accounts were not used.
No changes were pushed to `main`.

## Matched main versus current run

The run used `tools/skirk_live_ab.py` in a fixed main/current/current/main
order, one random 64 MiB body and 24 small requests scheduled every 250 ms per
arm, eight small workers, equal warmup and 15-second tails. Both binary builds
used fresh session IDs and the same pinned `www.google.com` front. The harness
removed `drive.quota_project` and all experimental Drive flags from private
copies of the same source config for **both** builds, because `main` does not
honor the optional quota-project header. The original kit was not edited.
Every HTTP response passed exact length and SHA-256 checks. The current binary
was built before the new client file-import changes, which do not alter the Go
transport. Main binary SHA-256 is `603ae01f9ea1a6685ed620fed40d445ec9cdc62353b09811dcab7538f61fb0c3`;
current binary SHA-256 is `9e33442115938f3be3c60c4a81971e4f305f908878b86e154786904d2d6e595d`.
This measured binary predates the final redirect and custom-token proxy safety
fixes. They do not change successful, non-redirecting Google API requests, but
the exact final binary was not rerun under this live workload.

| Order | Build | 64 MiB Mb/s | Small verified | Scheduled small p95 | Estimated units, warmup/tail included | Drive API errors |
| ---: | --- | ---: | ---: | ---: | ---: | ---: |
| 1 | main | 70.988 | 24/24 | 2.648 s | 36,105 | 0 |
| 2 | current | 54.545 | 24/24 | 3.493 s | 36,155 | 0 |
| 3 | current | 57.173 | 24/24 | 3.054 s | 35,655 | 0 |
| 4 | main | 21.309 | 24/24 | 17.548 s | 37,255 | 0 |

The two-run medians are 46.149 versus 55.859 Mb/s, and 10.098 versus 3.273 s
scheduled small p95, for main versus current. With only two arms each and
main varying from 21.309 to 70.988 Mb/s, these are descriptive, not a reliable
claim that the current engine is faster. Main's quota accounting also omits
some attempts that the current build counts, so the small unit difference is
not an established efficiency gain. The complete evidence is in
`.skirk-runs/max-api-main-vs-current-64m/`.

The slow main arm had client normal-download slot-wait p95 of 8.440 s,
priority upload p95 of 6.137 s, priority download p95 of 6.079 s, and priority
poll p95 of 1.886 s. Other arms had normal-download slot-wait p95 of 0.54–0.62 s.
This suggests slow Google-facing requests occupied the normal receive slots
and amplified a broad delay. These are concurrent operation distributions
that include warmup/tail, not a causal sum of one HTTP request's stages. The
identifier-free analyzer is `tools/skirk_stage_analysis.py`; its private
result is `.skirk-runs/max-api-main-vs-current-64m-stage.json`.

## HTTP/3 front reachability

[Google's HTTP guidance](https://docs.cloud.google.com/apis/docs/http) supports
HTTP/3 for Cloud APIs in general. That did not establish that Skirk's split
front name and API authority would work. A pinned-front request over HTTP/3
with SNI `www.google.com` and authority `www.googleapis.com` returned the
expected unauthenticated Drive JSON error. Eight subsequent authenticated
metadata reads, four per protocol in a balanced sequence, all returned HTTP
200 and the exact test-folder ID. HTTP/3 TTFB was 158–198 ms (median 176.7 ms);
HTTP/2 was 173–260 ms (median 246.5 ms). These were **fresh curl connections**
on this Mac and cost at most eight metadata reads. They do not measure reused
connections, Skirk throughput, hostile-network UDP reachability, or tail
latency. Raw private data: `.skirk-runs/max-api-h3-authenticated.json`.

The new opt-in `google_front_h3_pinned` Go route then made one authenticated
read through the same pinned Google front. The Drive folder ID matched and the
HTTP response reported `HTTP/3.0`, confirming the native transport actually
used QUIC. It took 329.598 ms including a fresh connection; that single read
is not a throughput estimate. The JSON proof is
`.skirk-runs/max-api-native-h3-authenticated.json`.

A full Skirk ABBA run attempted HTTP/2, HTTP/3, HTTP/3, HTTP/2 with the same
binary, fresh mailbox session per arm, a 64 MiB body, and 24 scheduled small
requests. The first HTTP/2 arm verified all traffic at 43.166 Mb/s and 3.300 s
small scheduled p95, using 35,705 estimated units. The first HTTP/3 arm also
verified all traffic but reached 32.762 Mb/s and 3.397 s, using 40,405 units.
The second HTTP/3 arm encountered 25–30 second bulk upload delays; two 30
second upload deadlines fired and the harness stopped at its first API-error
guard, before the final HTTP/2 arm. Thus no complete paired winner can be
claimed, and the observed failure is a reason **not to promote HTTP/3** on this
route. The default remains Google-front HTTP/2. The failed arm's four leftover
objects were removed by an exact-session cleanup; a delayed check found zero
matches. Private evidence:
`.skirk-runs/max-api-h3-abba-64m/`.

The route implementation never uses QUIC through a configured SOCKS proxy;
it uses the same Google-front HTTP/2 path in that case. A pre-request QUIC dial
failure can use a bounded HTTP/2 fallback, without replaying an already sent
write. OAuth refresh and Drive request retries now stay within the user's
selected Direct or Google-front mode rather than silently switching between
them. Automatic HTTP redirects are disabled for every Google HTTP client:
a redirect could otherwise change the dial target and TLS SNI without
reapplying route selection. The original 3xx response reaches the caller,
with no automatic credential-bearing follow-up. Focused Go tests cover this
307 POST behavior across Direct, proxy, fronted, and HTTP/3 routes.
An explicitly configured non-Google `auth.token_url` is a separate endpoint,
so it does not use the Google front. Its refresh follows a configured SOCKS
proxy and does not follow HTTP redirects.

## Separate bulk media connection pool

The 64 MiB baseline failure involved a near-simultaneous 8–10 second pause of
client priority uploads, a priority download, list, and delete while the exit
remained responsive. That is consistent with shared HTTP/2 connection
head-of-line delay or a local network pause; the logs do not prove which.
`drive.separate_bulk_media_pool` is therefore an opt-in experiment: large
normal media uploads and ID downloads use a separate same-route HTTP client,
while priority traffic, small normal objects, list, and cleanup retain the
control pool. Auth, quota accounting, backoff, and Google-front selection are
shared. The default is off.

A same-binary 32 MiB/24-small-request ABBA on the pinned Google front gave:

| Order | Mode | Bulk Mb/s | Small scheduled p95 | Estimated units | API errors |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | control | 17.330 | 10.477 s | 31,205 | 0 |
| 2 | split pool | stalled | 3.970 s for 24 small only | 34,605 (incomplete) | 0 |
| 3 | split pool | 21.226 | 4.281 s | 32,605 | 0 |
| 4 | control | 19.270 | 4.787 s | 31,705 | 0 |

All 24 small bodies verified in every arm. The first split-pool arm's normal
uploads hit 28-second deadlines and its bulk body did not pass the 15-second
progress gate. The successful split-pool arm improved small p95 against the
nearest control but used slightly more units; one failed arm prevents a
reliability or throughput win claim. Its two residual files were deleted by
exact-session cleanup, while the other arms had none. A delayed check found
zero matching files across the four arms. Raw evidence:
`.skirk-runs/max-api-bulk-pool-abba-32m/`. Do not enable the split pool by
default on this evidence.

One final bounded combination enabled that split pool and the existing
three-second normal-upload hedge. In 32 MiB/24-small ABBA, the opening control
bulk stalled despite 24/24 small successes; the two combined arms both
verified all bytes, with **16.347 and 13.647 Mb/s** bulk rates and **3.831
and 6.312 s** small scheduled p95. The closing control completed at **20.100
Mb/s** and **8.802 s** p95. Estimated units were 36,705 and 36,455 for the
combined arms versus 32,905 for the completed control. Several delayed
upload hedges won on a second connection. This is evidence that the retry can
rescue slow individual uploads and protect small requests; it also costs more
units and the completed control had higher bulk speed. The failed opening
control's 35,205 units cover an incomplete transfer and are not comparable.
Its two leftover objects were removed by exact-session cleanup; the other
arms had none. A delayed check found zero matching files across the ABBA.
The combination remains opt-in, not a default performance
claim. Raw results: `.skirk-runs/max-api-bulk-pool-hedge-abba-32m/`.

## Further API candidates

[Drive's files.create reference](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create)
allows metadata-only files under `drive.file`. Sealed tiny frames in a file's
description might remove a media write and download, unlike the current opt-in
dual-write prototype. Google does not document a numeric description limit.
The isolated 27-file probe under `experiments/metadata-only-probe/` measures
create, discovery, exact data, media behavior, and cleanup before any protocol
change is considered.

That probe has now completed on the pinned Google front with 27/27 exact
payloads and identities verified, zero failed creates/deletes, and zero files
remaining in its immediate reserved-ID sweep. All three methods were tested
three times at each of 256, 2,048, and 8,192 bytes, with rotated method order.
The core-path timing includes create, list visibility, and the media GET only
where needed. It excludes the equal diagnostic reads and cleanup:

| Small-object method | Median core path, nine files | Range | Estimated core units per file |
| --- | ---: | ---: | ---: |
| Production-style multipart media | 1,163.9 ms | 994.2–1,726.7 ms | 350 |
| Dual description + media, opt-in prototype | 872.0 ms | 709.9–1,226.0 ms | 150 |
| Metadata-only description | 508.3 ms | 418.9–644.4 ms | 150 |

Metadata-only was 56.3% faster than the production-style primitive median and
42% faster than dual-write in these short samples. It removes the 200-unit
media GET relative to production, as dual-write does; it also avoids writing
the duplicate media body. Drive returned HTTP 200 with **zero media bytes** for
metadata-only files. This confirms that a reader must take the sealed bytes
from the description: an older media-only reader cannot consume such an object.
The 11,040 total estimated units include diagnostic GETs and the final list;
the table's core units do not. Exact raw results are private in
`.skirk-runs/max-api-metadata-only-probe.json`. This primitive timing does not
yet establish full-tunnel latency, mixed-version safety, or long-running
storage cleanup.

The next opt-in candidate, `drive.metadata_only_priority`, places eligible
sealed priority batches in a metadata-only file after an authenticated
per-stream capability handshake. The challenge and echo ride existing OPEN
and first-reply objects, so negotiation creates no extra Drive file. The
opening batch stays on the legacy media path; old peers ignore the new frame
and never receive a metadata-only object. The `.mo1` name marker makes a
missing or damaged description a hard error, with no empty media fallback.
Local tests cover old/new fallback, nonce mismatch, 8 KiB bounds, zero-byte
list parsing, and AEAD corruption; focused race tests and the full internal
race suite passed.

A 32 MiB/8-small-request end-to-end ABBA smoke completed all four arms with
exact byte hashes and zero API errors. The two metadata arms made 31 media
downloads each, versus 39 and 40 on controls; they made nine metadata creates
each, including warmup. The first control overlapped a Go race test, so these
arms are **functional evidence only**, not a fair speed claim. The clean
64 MiB/24-small-request matched run follows separately. In that first clean
attempt, the opening **baseline** arm hit four simultaneous 10-second client
Drive deadlines (three uploads and one download); a list and delete on the
same side were also slow. The guard stopped before any candidate arm. This
is a baseline/network-path failure, not evidence for or against metadata-only.
The failed session's 46 remaining files were removed by an exact-session
cleanup. Raw run: `.skirk-runs/max-api-metadata-abba-64m/`.

The reversed, matched 64 MiB retry ran metadata/control/control/metadata with
24 small requests scheduled every 250 ms and eight workers. All 24 small
responses verified in every arm, but the first metadata arm delivered only
about 1 MiB of the bulk body before the 15-second no-progress guard; slow
normal uploads were observed on its exit. The other three arms verified the
whole 64 MiB body. The successful arms were:

| Order | Mode | Bulk Mb/s | Small scheduled p95 | Estimated units, warmup/tail included | Media downloads | API errors |
| ---: | --- | ---: | ---: | ---: | ---: | ---: |
| 1 | metadata-only | stalled | 3.991 s for 24 small only | 29,055 (incomplete) | 49 | 0 |
| 2 | control | 24.109 | 4.602 s | 40,105 | 85 | 0 |
| 3 | control | 22.782 | 15.745 s | 33,605 | 72 | 0 |
| 4 | metadata-only | 25.944 | 10.412 s | 30,005 | 55 | 0 |

The completed metadata arm used fewer media downloads and estimated units
than both completed controls, but latency varied and one metadata bulk arm
failed the progress gate. The incomplete arm's units cannot be compared with
completed transfers. **Do not promote metadata-only as the default** from
this evidence. After the test, exact-session cleanup removed two leftovers
from the failed arm; the three completed arms had none in the immediate
sweep. A later exact-session sweep found zero matching files for every 32 MiB
and 64 MiB arm. The successful metadata arm made 22 metadata creates and its
priority-upload p95 was 704 ms, versus 812–966 ms on controls. Aggregate
response bytes were 67.73 MB for metadata, versus 67.63–67.76 MB on controls;
the metric does not isolate list-response bytes. Private raw data:
`.skirk-runs/max-api-metadata-abba-64m-retry/` and
`.skirk-runs/max-api-metadata-delayed-cleanup.json`.

The [Drive limits](https://developers.google.com/workspace/drive/api/guides/limits)
put 100 units on a list and 200 on a media download in the newer quota model.
One list per second consumes 6,000 units/minute before any traffic. Dividing
the 325,000 user/project unit-minute limit by that cost gives a theoretical
upper bound of about 54 such idle pollers **sharing one user/project**;
project limits, billing, egress, cleanup, and other calls tighten it. Separate
user-owned projects change the accounting but do not prove 200,000 active
users or eliminate backend delay. Batch deletion and larger list pages may
save request overhead under backlog; they do not remove per-operation quota
charges or make Drive a streaming API.

At the published 1,000,000 project units/minute limit, 200,000 exits each
doing one discovery list per second would require about 1.2 billion units per
minute—1,200 times that project limit, before uploads and downloads. The
simple project arithmetic gives at most about 166 such **idle** exits on one
project at that poll rate, not an observed capacity figure. Per-user projects
can distribute project buckets only with correct Cloud setup and attribution;
they cannot make one mailbox or active user unconstrained. The prior live
10-client workload is a bounded concurrency test, not a 200,000-user result.

The most promising separate discovery architecture is the now generally
available [Workspace Events Drive folder subscription](https://developers.google.com/workspace/events/guides/events-drive).
It can emit a created file ID through Pub/Sub, potentially replacing many
active `files.list` calls with `files.get` by ID. It requires a Cloud Pub/Sub
topic, publisher permissions, another event delivery route, renewal of
subscriptions, duplicate handling, and list reconciliation. Google [announced
Drive event subscriptions as generally available in May
2026](https://developers.google.com/workspace/release-notes), although a generic
setup guide still labels Drive targets Developer Preview. The event's actual
delivery delay and availability on Skirk's regional route remain unmeasured;
it is not a proven latency or quota improvement. Larger list pages, batch
deletion, and `changes.watch` remain backlog or idle-wakeup candidates rather
than replacements for the tested hot data path.

An isolated Events pilot was stopped at setup: the two authorized personal
test projects both show no linked billing account, while
[subscription prerequisites](https://developers.google.com/workspace/events/guides/create-subscription)
require billing. No Events/Pub/Sub resources or extra Drive test calls were
created, and no other Google account was used. This is an external test gate,
not evidence that Events would be faster or slower.

## Client setup and route choice

The native iOS, Android, and desktop clients now accept a `client.skirk` file
from their system file picker, in addition to paste. Every read is bounded to
256 KiB, rejects invalid UTF-8, and passes through the existing identity and
replacement checks. iOS uses a coordinated, security-scoped read and does not
erase pasted text when the picker is canceled. The selected Google access
route remains visible and user-controlled; Google front stays the default.

The Android file picker was exercised on a disposable 320dp emulator with a
synthetic profile and reached Ready with Google front displayed. The iOS
Simulator lifecycle UI test passed and its app built; actual Files selection
and a physical VPN packet tunnel were not exercised. Desktop file selection
accepted a valid profile, rejected invalid UTF-8 and oversized files, and its
frontend built. These client checks did not use the private Google grant.
Screenshots and logs are under `.skirk-runs/max-ux-*`.

The isolated Skirk OAuth app was already published **In production** and its
test grant renewed in the preceding pass. [Google's OAuth audience
rules](https://support.google.com/cloud/answer/15549945) remove the
Testing-specific seven-day refresh-token lifetime in that state. This does
not make a grant permanent: revocation, prolonged inactivity, account policy,
and other [refresh-token expiration
conditions](https://developers.google.com/identity/protocols/oauth2#expiration)
still apply. The publisher's verified domain is infrastructure for this app;
an end user does not need a personal domain just to import a client profile.
The client-file picker makes a renewed profile easier to install on each
device, but it does not silently replace an expired grant on another device.

## Decision and measured envelope

The best **proven default** is still muxv4 on the user-selected
`google_front_pinned` HTTP/2 route. This branch adds route-choice correctness,
native client profile-file import, and opt-in H3, metadata-only priority, and
bulk media pool experiments. None of the three new transport modes met the
mixed-workload reliability and repeatability gate for default promotion.
Upload hedging with the split pool did rescue some slow uploads, but at a
quota and bulk-speed cost in this bounded sample.

The fastest previously archived **single** 128 MiB fronted transfer was
74.928 Mb/s with 24/24 small requests; ten simultaneous 32 MiB clients
reached 78.244 Mb/s aggregate with 40/40 small requests. Those are local,
short workloads from the [validation record](validation-2026.md), not a
maximum speed or sustained concurrency guarantee. This pass's clean main
control reached 70.988 Mb/s in one 64 MiB arm while another main arm reached
21.309 Mb/s. That variance and the recent 10–30 second Drive stalls are the
practical ceiling on any claim of guaranteed low latency. There is no honest
finite experiment that proves no lower-latency or higher-speed design exists.

Promotion still needs repeated 100 MiB and 1 GiB mixed tests, multi-client
stress, restarts, cleanup under sustained load, hostile-region UDP/direct
reachability, and physical iOS and Windows execution. The missing billing
link blocks the separate Workspace Events pilot. These are concrete open
gates, not reasons to label an unmeasured result as 200,000-user capacity.

## Final build verification

On the frozen branch, `scripts/preflight.sh` and `go test -race ./...` passed.
The Android Debug APK was rebuilt with both Go PIE ABIs and HEV; the full iOS
build produced Go and HEV XCFrameworks, a Simulator app, and its packet tunnel
extension. The macOS Tauri app and Windows x64 portable GUI were built with
matching Go sidecars. Linux amd64 and arm64 CLI packages were cross-built.
APK and ZIP integrity checks passed, and the bundled
sidecar hashes matched the rebuilt binaries. The signed Simulator synthetic
profile lifecycle UI test passed (1/1). The manifest and build logs are
in ignored `.skirk-runs/max-final3-artifact-manifest.json` and
`.skirk-runs/max-final3-*.log`.

These checks establish build and package integrity on this Mac. The Windows
and Linux binaries were not run on their native systems, and a signed physical
iPhone packet tunnel was not exercised. The live Drive measurements above used
the Mac transport and the isolated test account, not all packaged clients.
