# Generated-ID doorbell ladder: contract and capacity audit

Status: **read-only architecture audit**, no new Google call or product
change. Claude Fable proposed pre-generated Drive IDs as known poll targets
for metadata-only encrypted OPEN objects, with a slower list as recovery.
An independent review checked the primitive against current Google Drive
documentation and the repo's known-ID experiments.

## What Google documents

Drive [permits pre-generated file IDs in `files.create`](https://developers.google.com/workspace/drive/api/guides/create-file).
After a successful create using one such ID, a retry returns 409 Conflict
without creating a second file. The [create method](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create)
can create metadata without uploading media, and the
[File resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
has a `description` field. A small AEAD-sealed message there is a plausible
API primitive, subject to size, visibility and sharing tests. The 409
guarantee supports idempotent **retries**; it is not a documented fair
multi-device queue or a guarantee of exactly-once destination effects. An
ambiguous 409 requires an exact metadata GET and authentication of the
ID, name, parent and encrypted payload before deciding whether the creator
was oneself or another device.

The [Drive scope guidance](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)
limits `drive.file` to files the app created or a user opened with that app.
Google describes [`appNotAuthorizedToFile`](https://developers.google.com/workspace/drive/api/guides/handle-errors#appNotAuthorizedToFile)
when a different app lacks access. The
[`appDataFolder`](https://developers.google.com/workspace/drive/api/guides/appdata)
is per app. A known ID and public custom properties do not grant another
OAuth app access. The same-account/same-app path is plausible; copied kits
using different self-owned OAuth apps need a separate permission proof.

## Structural failure points

An immutable kit can hold only a finite vector of opaque generated IDs.
If multiple clients race to create the same rung, losers must read back and
authenticate before moving forward. The exit must know and poll every rung
that a client can use promptly. If it polls only the first, a client that
advances to a later rung waits for the fallback list. At one list every
30 seconds, an otherwise unobserved message can wait nearly 30 seconds;
under a uniform arrival assumption, the list-phase p95 alone is about
28.5 seconds before Drive RPC time. More simultaneous head GETs use more
quota.

After the finite vector fills, stale or offline clients need a crash-safe
authenticated way to discover the next vector. A mutable pointer without a
server-enforced stale-write fence can roll back; an immutable successor
chain can grow without bound. Existing live conditional-PATCH discriminators
[returned HTTP 500](fixed-rendezvous-live-2026-11-16.md), so no durable
rotation contract can be assumed. Google's create guide does not promise an
unused generated ID remains usable after 24 hours. This is a live gate, not
an inference that it expires.

The same-app [eight-record known-ID primitive](id-train-live-2026-11-15.md)
reduced a core discovery estimate but its short-message p95 was 0.829 s
versus 0.775 s with listing. It excluded the authenticated ID exchange,
rotation, crash recovery and a full SOCKS/TLS tunnel. The later
[metadata-only full-mux local prototype](metadata-known-id-mux-local-2026-11-16.md)
had a negative product idle-quota gate once dynamic bootstrap was included.
Neither proves the doorbell ladder's latency or throughput.

## Quota floor

One 5-unit exact metadata GET each second plus one 100-unit list each
30 seconds is **500 estimated units/min**, or 720,000/day, per continuously
idle exit. Current one-list/second polling costs about 6,000/min or
8.64 million/day. Under Google's current
[1 million units/min project limit and 400 million/day no-charge threshold](https://developers.google.com/workspace/drive/api/guides/limits),
those lower bounds fit at most 555 versus 46 always-idle exits under one
project's daily free threshold, before creates, conflicts, readbacks,
deletes, traffic and retries. The daily figure is a billing threshold,
not a hard API request cap. For 200,000 one-second pollers, even the
single five-unit GET alone needs 60 million units/min, 60 times the shared
project minute limit. A per-user project topology remains unproven at that
population. Failed-GET charging and actual quota counters need direct
verification; these are method-weight estimates.

The repo's pinned `www.google.com` route has worked for limited same-app
probes. Google's documented Drive endpoint is `www.googleapis.com`; it does
not promise the fronted route or its behavior after 24 hours, across users
or under this ladder's create/GET pattern.

## Falsifiable next gate

Under the approved secondary account and strict quota/cleanup limits, test
three copied clients with the same app and then distinct OAuth apps. Verify
409 ownership by exact authenticated readback, fresh and 24-hour generated
ID creates, pinned-front head visibility, lost create responses, exit
restart, finite-vector rollover, stale clients and exact owned cleanup.
Record provider quota counters, not only local weights. Only after that
should a frozen full-tunnel, both-direction sustained text/bulk comparison
be attempted.

**Decision:** plausible primitive, unproven carrier. The 500-unit idle
calculation is a useful target, not a measured speed or 200,000-user
free-scale result. Keep list-based muxv5 as the current default.
