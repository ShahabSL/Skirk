# Selected Drive quota project: bounded enforcement contrast

Status: **project-selection enforcement demonstrated for one read method and
one principal**, not a 200,000-user capacity or billing test. The test changed
only the disposable `flowing-precept-514715-p1` project's Google Drive API
**Units per minute** limit; the published Skirk OAuth project and the
`shahabpre` mailbox stayed unchanged. The temporary override was removed and
the original 1,000,000-unit limit was restored in Cloud Console. No Drive
file was created or deleted by this experiment.

## Design

The existing separate-project test had already shown create/read/list/delete
compatibility and roughly 100 attributed request-count chart events. It had
not proven that the selected project's actual limit controls Drive calls.
Google's [quota-project guide](https://docs.cloud.google.com/docs/quotas/quota-project)
describes `X-Goog-User-Project` as the quota consumer, subject to the
authenticated principal's project permission. The [Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits)
lists 5 units for metadata `files.get` and 1,000,000 units/minute/project.

The test-only `tools/quota_enforcement_probe` uses the isolated
`personal-test/kit/client.json` grant, the pinned `www.google.com` front,
single-attempt HTTP, no automatic Drive retry, no cache reuse requested, and
one known app-created folder ID. Each read requests only `fields=id` and
checks the exact ID locally. A `selected` read adds the A-project header;
a `publisher` read omits it and uses the unchanged OAuth project's default.
Each phase refreshes from the **same** `shahabpre` OAuth grant; access token
bytes were not held constant across phases. The private status-only reports
contain neither tokens nor folder IDs:

- `.skirk-runs/quota-enforcement-baseline-20261116.json`
- `.skirk-runs/quota-enforcement-challenged-20261116.json`
- `.skirk-runs/quota-enforcement-restored-20261116.json`
- `.skirk-runs/quota-enforcement-restored-20261116-b.json`

| Phase | Consumer sequence | Observed status/reason | Interpretation |
| --- | --- | --- | --- |
| Before override | A, publisher | 200 exact, 200 exact | Both routes to the same owned ID work. |
| Cloud Console A limit 1,000,000→0, effective value shown 0 | publisher, A, A, publisher | 200 exact; 403 `RATE_LIMIT_EXCEEDED`; 403 `RATE_LIMIT_EXCEEDED`; 200 exact | A's limit blocked only header-selected calls; publisher calls around them succeeded. |
| Console removed override and displayed default 1,000,000 | A | First 403 `RATE_LIMIT_EXCEEDED`, later 200 exact | Enforcement restoration lagged the UI briefly, then recovered. |

The A-denial response took 50–52 ms; these are rejection timings, **not**
transport latency samples. Baseline A/B read durations were 139/95 ms; the
recovered A read took 188 ms. They are too few and different in time to rank
routes or payload throughput. The test spent eight Drive reads total, with
no file writes, no paid resource and no retained quota override. The Cloud
Console final row showed 1,000,000 units/minute with no override indicator;
the final selected-project exact-ID read returned HTTP 200.

## What this proves and what it does not

For this token principal, pinned route, and metadata `files.get`, the header
selected an independently enforced project minute limit. A generic auth
failure would also have affected the surrounding publisher controls; a
disabled-API error would have a different reason. This closes the previous
request-count-only gap for **bucket selection**. It does not measure 5-unit
weight accounting, sustained throughput, egress or billing attribution,
per-user and backend limits, OAuth publisher authorization limits, or many
concurrent operators. A zero-limit denial cannot establish a particular
nonzero rate ceiling. Temporary limits also showed propagation delay, so a
client UI should treat quota changes as eventually effective and display
the selected project's explicit failure rather than silently switching to
the publisher project.

The credible self-managed direction is one published Skirk OAuth app, each
operator's Google mailbox grant, and an **optional operator-owned** quota
project with Drive enabled and `serviceusage.services.use`. The operator
does not need a domain or their own OAuth app solely for quota attribution.
This is an onboarding/design direction, not proof that 200,000 users can be
served for free or that a centralized fleet of quota projects is permitted.
The product already passes `drive.quota_project` through DriveStore; its
setup and client UI still need to make that advanced choice understandable.
