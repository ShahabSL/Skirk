# Drive About and start-page-token as idle watermarks: live primitive

Status: **completed, negative for a sole low-latency discovery gate**.
This is a same-account API primitive, not a tunnel speed run or product
change. The isolated `shahabpre` client and exit configurations share one
OAuth grant. The test used the pinned `www.google.com` front and was run after
other heavy local test/build work paused. Its private status-only report is
`.skirk-runs/about-watermark-probe-20261116-a.json`; it contains no file ID,
token, credential, filename, or response body. The test-only source and
guard are in `tools/about_watermark_probe`.

## Hypothesis

Google's [v2 `about.get`](https://developers.google.com/workspace/drive/api/reference/rest/v2/about/get)
allows `drive.file`, and its `largestChangeId` field might wake a mailbox
reader for 5 units instead of a 100-unit `files.list`. Google's
[v3 `changes.getStartPageToken`](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
is another 5-unit account change signal. The
[quota table](https://developers.google.com/workspace/drive/api/guides/limits)
prices both; the references do not promise either signal is visible before
matching file-list results.

Both methods passed independent preflight through the front with the current
grant. Eight quiescent samples of each signal stayed unchanged. The tool
reserved and journaled two exact IDs, then created, updated, and deleted one
primary owned file and one separately prefixed same-account file. Each phase
captured the signal and an exact-prefix list before mutation, then sampled
v2 About, v3 token, and list afterward. When list had changed before a
signal, it reread both signals to establish a stronger ordering witness.

| Mutation | Exact list first showed target | v2 change first seen | v3 token first seen | List-visible/unchanged witness |
| --- | ---: | ---: | ---: | --- |
| Primary create | 0.678 s | 0.786 s | 0.882 s | Neither |
| Primary update | 0.590 s | 2.526 s | 2.618 s | Both |
| Primary delete | 0.622 s | 2.562 s | 2.646 s | Both |
| Other-prefix create | 0.674 s | 2.574 s | 2.674 s | Both |
| Other-prefix update | 0.502 s | 2.327 s | 2.385 s | Both |
| Other-prefix delete | 0.534 s | 2.451 s | 2.520 s | Both |

These are observed intervals from mutation start, **not** guaranteed API
propagation bounds. Both signals eventually advanced in all six phases.
The five paired lag witnesses mean that a reader which skips `files.list`
whenever either signal is unchanged would miss already-list-visible work for
roughly two seconds in this sample. The other-prefix events also changed
both account-wide signals while the primary prefix stayed empty: they can
cause false wakeups. This test does not show whether a different account's
shared-folder mutation advances either signal on the reader account.

The run made 53 About GETs, 53 start-page-token GETs, 36 exact-prefix lists,
two each of generate-ID/create/update/delete, and six exact-ID GETs. There
were 156 HTTP/2 responses, 4,470 nominal method units and 35,760 local
eight-attempt upper reservations, not provider billing telemetry. The median
single-call durations were 88 ms About, 82 ms token and 108 ms list, but
this sequential microprobe does not show full-tunnel text latency. The local
guard was 120,000 upper active units, 12,000 cleanup units, eight seconds
per request and two minutes active. Both exact owned IDs returned 404 twice
after deletion; both prefixes were empty immediately and after 31 seconds.
The recovery ledger was removed after those proofs. No Google resource
remains from the probe.

## Decision

Neither signal should be a correctness gate or a fast replacement for the
active `files.list` discovery path. A quota-saving idle mode may poll one
5-unit signal and reconcile with a periodic list, but that trades cold-start
latency for lower idle units and requires matched full-tunnel measurements.
Using both signals doubles the advisory poll cost without fixing their
observed lag. Even one 5-unit poll every 30 seconds for 200,000 continuously
online pairs implies 2 million units/minute and 2.88 billion/day on one
project before any actual traffic—above Google's published 1 million/minute
limit and 400 million/day free threshold. A persistent known-ID mailbox
anchor remains a different hypothesis: it directly reads the mailbox state
with a 5-unit GET instead of waiting for an account change feed.
