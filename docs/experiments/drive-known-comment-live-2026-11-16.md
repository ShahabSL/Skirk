# Drive known-comment reply carrier: bounded live primitive

Status: **incomplete feasibility probe, not a full tunnel or speed winner**.
The private raw report is
`.skirk-runs/comment-thread-probe-20261116-a.json`; it contains no credential,
file ID, comment ID, or payload. The test-only source is
`tools/comment_thread_probe`. No product route or default changed.

## Question and method

Google documents a comment's `replies[]` as the full chronological list. A
precreated, known comment therefore offers a 5-unit `comments.get` read and a
50-unit `replies.create` append, avoiding `comments.list` on every read. This
is structurally different from the older fresh-comment/list idea. The
[Comment resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/comments)
and [current unit table](https://developers.google.com/workspace/drive/api/guides/limits)
describe the API shape and prices; they do not promise tunnel latency.

The run used the isolated `shahabpre` test grant, one temporary owned file and
comment, and the pinned `www.google.com` front. The client and exit kit files
contain the **same OAuth refresh token and client ID**, so the two processes
do not test cross-account access. The tool appended eight exact authenticated
64-byte records in each of four phases, with 1, 2, 4, and 8 concurrent
writers; a peer process polled the known comment at 100 ms. The phases reused
one growing thread in that order. The local Go race tests passed before the
live run. A separate full Go suite was active on this Mac during some or all
of the primitive, so these latencies are feasibility observations, not a
CPU-quiet matched ranking against another carrier.

| Writers | Delivered | Start-to-visible p50 | Start-to-visible p95 | Comment GETs | Response bytes | Nominal units |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 8/8 | 1.052 s | 1.156 s | 16 | 25,462 | 480 |
| 2 | 8/8 | 2.458 s | 2.691 s | 10 | 42,456 | 450 |
| 4 | 8/8 | 3.723 s | 4.233 s | 9 | 62,831 | 445 |
| 8 | 8/8 | 4.308 s | 7.604 s | 9 | 86,663 | 445 |

All 32 normal records were read back exactly. The tool then deliberately
discarded one successful create response, found its authenticated sequence
through the known comment, posted a duplicate of that same sequence, and
counted two raw replies as one logical delivery. That tests only a
**committed** lost-response path; a true request failure and crash recovery
remain untested. A 2 KiB plaintext record was accepted and read back. The
next planned 8 KiB create returned HTTP 403. The tool stopped, so the 16 KiB
size and thread rotation cases did not run. The returned status was not
classified by its JSON `reason`; this evidence cannot distinguish a content
limit, rate limit, or another permission condition.

The whole attempted probe made 36 reply creates and 49 known-comment GETs,
received 259,766 response bytes, and used 2,415 nominal method units. Its
eight-attempt conservative reservation was 19,320 units, including cleanup;
neither number is provider billing telemetry. The active guard was 70,000
upper units with a separate 10,000 cleanup reserve. Exact owned file deletion
was followed by two ID 404s, deleted comment ID 404, an empty exact-name
prefix immediately and again after 31 seconds. The private recovery ledger
was removed only after all cleanup proofs passed. The failed 403 remains an
experiment failure despite successful cleanup.

## Decision

This primitive proves that a known comment can carry small, authenticated
append-only records through the selected front on the current same-account
kit. The increasing latency, cumulative response growth, unknown 8 KiB 403,
cross-account gap, visible comment activity, and lack of full-tunnel proof
make it unsuitable for promotion now. It does **not** replace the Drive v2
window-eight research candidate or establish a new maximum. If investigated
again, first capture a sanitized 403 `reason`, prove another account's
`drive.file` access and notification behavior, then run a CPU-quiet, frozen
same-binary full-proxy comparison. Steady polling also cannot satisfy a
200,000-simultaneous-pair shared-project budget: two 5-unit reads per pair
every 30 seconds imply 4 million units/minute before writes or traffic,
above the currently published 1 million-unit/minute project limit.
