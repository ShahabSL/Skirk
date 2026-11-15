# Fixed metadata lane: experimental protocol design

The November 2026 [architecture frontier](architecture-frontier-2026.md)
measured a promising Drive primitive: 20 synthetic 64-byte frames crossed two
known metadata files, survived nine skipped Drive versions during a one-second
reader pause, and were acknowledged in order. The probe is not integrated
with Skirk's TCP mux. This document defines the smallest safe opt-in protocol
candidate and its rejection gates.

## Scope

Use fixed Drive files only for bounded priority records after an authenticated
bootstrap. Keep muxv4 OPEN/bootstrap, oversized priority records, and all
normal/bulk data on the existing immutable object path. The user continues to
choose Direct or the pinned `www.google.com` front; the front remains default.
The fixed lane must never be required for a legacy peer to connect.

## Bootstrap and ownership

Each client process has a fresh run ID. For its `(session, client ID, run ID)`,
create two files with versioned names outside the ordinary `muxv4/` janitor
prefix. Advertise their IDs, a random generation, protocol version, and
supported description cap inside the existing authenticated OPEN object. The
exit validates the file IDs' names and parent folder, then accepts the slot
capability in the authenticated first reply. Until that reply, both peers use
legacy p0 objects. One writer owns each file: client for uplink and exit for
downlink. Multiple devices never share a writer slot.

## Record and ACK rules

Each description is a separately AEAD-sealed snapshot, derived from the
existing session secret with a distinct slot key and nonce domain. It carries
the generation, writer direction, strictly increasing slot sequence, the
highest contiguous ACK for the reverse direction, and **every unacknowledged**
`{slot sequence, mux object name, mux sealed bytes}` record. Keep the encoded
description below a conservative cap supported by live tests (at most 8 KiB
initially). Google Drive's `version` is only a change hint. A reader may skip
versions and still recover the full unacknowledged suffix from the latest
snapshot.

The receiver validates AEAD, generation, order, and mux identity, deduplicates
records seen through either the slot or immutable fallback, and delivers them
through the existing mux receive path. It advances its ACK only after the
records are processed in contiguous order. The sender may trim only through
an authenticated ACK. It serializes PATCH requests so a delayed old snapshot
cannot overwrite a newer one. Ambiguous PATCH outcomes require readback or
fallback of the same logical records; a timeout must never silently drop them.

An overflowing description, repeated stale GETs, PATCH errors, or a slow
backend opens a circuit breaker. The sender then publishes the original mux
record as an immutable p0 object and keeps deduplication active. Virtual slot
records must **never** enter the Drive object deletion queue. Legacy p0
listing remains for new clients and fallback; it can slow only after slot
health and bootstrap latency are measured.

## Work and lifetime limits

Google prices file metadata GET at 5 units and file PATCH at 50; Skirk's quota
accounting now prices both `patch` and `update` at 50. A pair of 4 Hz known-ID
pollers costs 2,400 units per minute before writes and ACKs. Four one-way
PATCHes per second add 12,000 units per minute. Cap active slots, poll rate,
PATCH rate, unacknowledged bytes, and fallback work per process; budget
conservatively for other devices on the same account. A single shared project
cannot serve 200,000 continuously active pairs under the published
1,000,000-unit/minute project limit. Self-managed projects distribute project
quota but require real setup and sustained-load validation.

Active idle slots need an explicit lifetime policy outside the current
24-hour `muxv4/` stale-object janitor. On restart, create a new generation and
run ID, reject stale snapshots, and clean old IDs with exact-ID verification.
Do not use Drive's revision history as a packet log.

## Promotion gates

1. Parser, AEAD, ordering, duplicate, ACK-loss, ambiguous-write, restart,
   circuit-breaker, and cleanup fault tests.
2. Local five- and ten-client mixed tests, then pinned-front order-balanced
   text plus 16 MiB upload and download, with every byte verified.
3. 100 MiB file completion, five-client live file goodput, Drive quota and
   storage-debt checks, and sustained multi-account operation.
4. Android VPN browser/video/bulk testing, iPhone packet tunnel on a signed
   physical device, and native Windows execution.

Do not promote a smaller text p95 if file completion, bulk rate, quota,
ordering, or cleanup regresses beyond the release gate. The current muxv4
remains the proven default while this lane is researched.

Official references: [Drive usage limits](https://developers.google.com/workspace/drive/api/guides/limits),
[file update](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/update),
[file fields](https://developers.google.com/workspace/drive/api/reference/rest/v3/files), and
[revision behavior](https://developers.google.com/workspace/drive/api/guides/manage-revisions).
