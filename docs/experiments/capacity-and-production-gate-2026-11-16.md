# Skirk capacity and production gate — November 16, 2026

This is a quota model from current Google documentation, **not** a 200,000-user
load test. The completed November mixed runs used one local client and exit,
exact 16 MiB transfers and 41 short echoes. Their timings belong in the
[pipelined live comparison](pipelined-live-comparisons-2026-11-16.md). A local
guard's eight-attempt quota reservation is deliberately conservative; it is
not a measurement of units billed by Google.

## Published limits and arithmetic

Google currently documents, for new Drive API projects, **1,000,000 quota
units/min/project**, **325,000 units/min/user/project**, a **400,000,000
units/day/project threshold before charges**, and **1 TB/day/project egress**.
Examples in its method table are `files.get` **5**, `files.list` **100**,
`files.update` **50**, and `files.download` **200** units. Google says older
projects that used the API between November 2025 and April 2026 retain their
previous quotas for a transition period; inspect each actual project's quota
page. It may return `403` or `429` under rate limits and recommends exponential
backoff. [Drive API limits](https://developers.google.com/workspace/drive/api/guides/limits),
[2026 quota change](https://developers.google.com/workspace/tools-safety).

The following assumes every user is simultaneously online, two **metadata
GETs** per poll cycle, no cache hits, and no writes, downloads, retries or
reconnects. It is intentionally a generous ceiling, not a claimed supported
user count. Decimal units and a full 24-hour day are used.

| Poll interval | Units/user/min | Shared project peak ceiling from 1M/min | Shared project free-daily ceiling from 400M/day |
| --- | ---: | ---: | ---: |
| 30 seconds | `2 × 5 × 2 = 20` | 50,000 | 13,888 |
| 2 minutes | `2 × 5 / 2 = 5` | 200,000 | 55,555 |
| 7.2 minutes | `2 × 5 / 7.2 ≈ 1.389` | 720,000 | 200,000 |

For 200,000 users at the 30-second example, demand is **4,000,000
units/min** and **5,760,000,000 units/day**: 4× the published minute project
limit and 14.4× its free daily threshold. If each poll instead uses two
`files.list` calls, that same 30-second cadence costs 400 units/user/min;
the corresponding project ceilings are 2,500 peak and 694 continuous free.
The table does not assert Skirk uses either exact poll pattern; it shows how
quickly a shared project's budget disappears under simple patterns.

Even without any polling, one 50-unit metadata update per minute from each
of 200,000 users costs **10,000,000 units/min** and **14.4 billion/day**.
One update per user per day costs 10 million/day. The idealized daily ceiling
for that single write operation is 40 updates/user/day at 200,000 users;
all reads, replies and bulk actions reduce it. Google also limits project
creation per user or service account, so a publisher cannot assume it can
automatically create an unlimited number of projects.
[Resource Manager project quotas](https://docs.cloud.google.com/resource-manager/docs/limits).

Firestore Standard edition's free tier is **one database/project**, 1 GiB
stored, **50,000 document reads/day**, **20,000 writes/day**, **20,000
deletes/day**, and **10 GiB outbound/month**. A listener result update also
counts as a document read; a no-op write still counts as a write. At 200,000
users, merely one document write per user per day requires 200,000 writes,
10× the shared project's free tier. A request and response requiring one
write apiece permit at most **10,000 such exchanges/day/project** even before
other protocol writes. The observed prototype used more writes than this
idealized minimum. [Firestore quotas](https://firebase.google.com/docs/firestore/quotas),
[listener/read and no-op billing](https://docs.cloud.google.com/firestore/native/docs/billing-questions).

The [bounded zero-limit contrast](quota-project-enforcement-live-2026-11-16.md)
now demonstrates that `X-Goog-User-Project` selects an **independently enforced**
Drive project-minute bucket for one pinned-front metadata GET and the
secondary test grant. The same published OAuth application can therefore be
used with an operator-owned quota project for that tested method. Per-user
projects distribute these **project-level** quotas only if each operator
truly owns and configures a separate project. This still does not show that
200,000 projects, their Google accounts, the exit's permissions, live
sessions, API backend, or the front will sustain that scale, and the test did
not establish numerical method weights or billing attribution.
It also does not make Firestore's finite free allotment unlimited. The shared
exit and each user's Google account remain separate bottlenecks requiring
measurement.

## OAuth and provisioning gate

An External OAuth app in **Testing** issues a refresh token with a seven-day
lifetime when it requests Drive access. Switching the OAuth app to
**Published** removes that Testing-specific clock, not revocation: Google
lists user revocation, six months without use, token count, time-limited
grants and organizational policy among other invalidation causes. The client
needs a clear reconnect state and renewal flow. Google's published/unverified
100-user cap is specified for **sensitive or restricted** scopes;
`drive.file` is currently listed as **non-sensitive**. Verification and
branding requirements still depend on the real public app configuration and
scope set. [OAuth token behavior](https://developers.google.com/identity/protocols/oauth2),
[OAuth app states](https://developers.google.com/identity/protocols/oauth2/production-readiness/overview),
[Drive scope classifications](https://developers.google.com/workspace/drive/api/guides/api-specific-auth).

A user who authorizes **Skirk's publisher OAuth app** does not need a personal
domain: the publisher owns its OAuth homepage and domain. Google's policy
requires a public production OAuth app to have a homepage on a verified owned
domain. A **personally self-managed OAuth project** is a different setup;
Google explicitly classifies a personal-only app separately from a public
production app. We should guide that owner through their actual console
requirements rather than promise a domain-free automated publish flow for
every project. [OAuth policy](https://developers.google.com/identity/protocols/oauth2/policies),
[production domain guidance](https://developers.google.com/identity/protocols/oauth2/production-readiness/policy-compliance).

The existing `drive.file` token cannot enable the Firestore API, create a
database or grant its peer access. Enabling a service needs Service Usage
permissions (typically `roles/serviceusage.serviceUsageAdmin`); creating and
managing Firestore databases needs `roles/datastore.owner`; document access
uses Firestore data permissions, and the Firestore REST API documents the
`datastore` OAuth scope for Google Identity tokens. An owner-guided client
tab can display status and open the necessary Cloud steps, but the user or a
properly authorized Cloud principal must complete them. Do not embed a
publisher's Cloud owner token in released clients.
[Service Usage roles](https://docs.cloud.google.com/service-usage/docs/enable-disable),
[database roles](https://docs.cloud.google.com/firestore/native/docs/manage-databases),
[Firestore REST authentication](https://docs.cloud.google.com/firestore/native/docs/use-rest-api).

## Transport gate

Google's official Drive endpoints use `www.googleapis.com`; Firestore's use
`firestore.googleapis.com`. Skirk's **pinned `www.google.com` front** is an
observed property of the bounded experiments, not a Google-documented
availability or latency guarantee. Keep the user-selected Direct route and
the Google.com front; test each API method and redirect, and fail visibly
when the selected route cannot serve it. The free-product choice cannot rely
on a paid service as a mandatory hidden dependency.
[Drive v2 REST paths](https://developers.google.com/workspace/drive/api/reference/rest/v2),
[Firestore REST](https://docs.cloud.google.com/firestore/native/docs/use-rest-api).

Drive v2 exposes a file-resource ETag while Drive v3 does not expose the
corresponding field in its comparison reference. The owned-file probes
observed `If-Match` fencing and `412` on stale v2 PATCH/PUT in this account;
that empirical result warrants a fresh per-run capability probe and a safe
fallback, not an unconditional permanent protocol assumption.
[Drive v2 file resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files),
[v2/v3 comparison](https://developers.google.com/workspace/drive/api/guides/v2-to-v3-reference),
[live ETag probe](drive-v2-metadata-etag-cas-2026-11-15.md).

**Decision:** Neither the two-pair Firestore result nor the Drive v2 test
carrier proves 200,000-user capacity or a production protocol winner. A
product launch gate needs versioned peer negotiation, native client support,
crash-safe exact cleanup, a bounded reconnect path, sustained multi-client
measurements, and quota admission accounting for the actual project and
account layout. Keep the measured, working default while these candidates
are evaluated under those conditions.
