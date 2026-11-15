# Transport Research

This document records the current transport conclusions so future changes do
not repeat failed protocol shapes. Raw experiment artifacts live in the ignored
`.skirk-runs/` directory and are intentionally not committed.

## Goal

Skirk needs a generic SOCKS/HTTP/VPN transport over Google Drive for hostile
paths where the Google API route must use the configured Google-looking fronted
or pinned path. The target is:

- lowest practical latency;
- highest practical throughput;
- stable browsing during bulk downloads;
- multiple clients on the same generated profile;
- no website, hostname, path, content-type, or app-specific filters.

## Current Answer

Mux v4 is the best proven default under the current constraints:

- one Google Drive mailbox;
- current credential scope and setup model;
- Skirk-owned Drive folder or legacy `appDataFolder` runtime objects;
- prefix-scoped `files.list` discovery;
- no inbound connectivity requirement for clients;
- Google-fronted/pinned client API route when needed.

That does not mean muxv4 is theoretically optimal forever. It means the tested
alternatives have not beaten it on the workload that matters: normal browsing
and media behavior while bulk downloads are active.

## Why Large muxv4-Only Gains Are Unlikely

Mux v4 already spends most of its engineering budget on the two things that
matter most for Drive:

1. reduce object count by coalescing many streams into a few lane objects;
2. keep discovery simple by listing one direction prefix and downloading by
   file ID.

Pushing muxv4 harder can still move the ceiling, but the expected gains are
incremental because the carrier still requires whole-object upload, Drive
visibility, prefix list, media download, and cleanup. Larger objects can improve
bulk throughput, but they increase head-of-line delay and reassembly pressure.
Smaller objects improve interactivity, but they add Drive calls and can collapse
throughput.

A "way above" result is more likely to require changing a core constraint:

- multiple independent Drive mailboxes or credentials;
- a broader OAuth scope with separate visible data objects and appData control;
- a non-Drive carrier for data;
- a public webhook or inbound control plane;
- a different storage provider with lower object visibility latency.

Those are valid future product choices, but they are not muxv4-only tuning.

## Rejected Protocol Shapes

Several protocol families were tested or primitive-tested and rejected because
they failed mixed workload gates, added Drive call pressure, or increased tail
latency:

- control/data split designs that added extra control uploads before bytes were
  usable;
- change-feed designs that lost prefix locality and had to filter unrelated
  appData changes;
- range-read slab designs that proved the safety primitive but paid too much
  control-plane overhead;
- strict-priority, credit, and mailbag designs that added small reverse-control
  objects or worse tail behavior;
- generated-ID rendezvous and optimistic metadata polling designs that created
  404 and metadata-call pressure;
- resumable upload and mutable update-slot designs that made Drive write tails
  worse for the hot path.

The repeated failure pattern is consistent: candidates that look cleaner inside
the mux often create more Drive objects, more list/change pages, more metadata
polls, or worse upload tails. Those costs dominate the live transport. Exact raw
artifacts stay in `.skirk-runs/` rather than public docs.

## Cloudflare Free Relay Boundary

Cloudflare's free tier gives Skirk useful control-plane pieces: Workers,
SQLite-backed Durable Objects, Cron triggers, limited KV/D1/Queues/R2 usage, and
the edge cache. Those are enough for a Drive webhook receiver, session state,
metrics, cleanup scheduling, and a bounded HTTP fetch relay.

They are not enough for a VPS-equivalent Skirk exit. Worker-only deployments are
request/event driven, cannot run the current Go exit daemon continuously, and do
not provide a production-quality generic SOCKS/VPN/CONNECT runtime. The
implemented free-tier product under `experiments/cloudflare-worker-exit/` is
therefore intentionally scoped to:

- Drive-backed HTTP `GET`/`HEAD` relay through Worker `fetch()`;
- Drive push/pump management with Durable Object state;
- local HTTP proxy testing for plain HTTP URLs;
- explicit rejection of HTTPS `CONNECT`, SOCKS, VPN, UDP, and arbitrary
  long-lived TCP.

An opaque SOCKS probe was implemented and deployed on 2026-05-25 to test the
hard boundary directly. The local path successfully uploaded the stream-open
Drive object, woke the Worker, and received a Worker-side error object. The
Worker failed at `cloudflare:sockets.connect()` for `example.com:443` with the
runtime message that HTTP-based services should use `fetch` instead. A single
failed connect attempt took about 6.9 seconds, consumed roughly 1,700 estimated
Drive units in the local bridge accounting, and left zero `cfstream/v1/`
leftovers. That failed destination did not establish a blanket ban on opaque
HTTPS. The November 14 recheck below supersedes that inference.

On November 14, a separate authenticated, fixed-destination probe opened raw
TCP port 443 to Google and GitHub and carried client-terminated, certificate-
verified TLS 1.3 through a Worker WebSocket bridge. Both returned HTTP 200;
the Cloudflare-hosted control was blocked. The same runtime error therefore
cannot be interpreted as a ban on all web ports. [Cloudflare documents blocked
Cloudflare IP ranges](https://developers.cloudflare.com/workers/runtime-apis/tcp-sockets/),
along with socket lifecycle restrictions. The probe used direct Worker access,
not the Drive path, and proves neither free-plan capacity nor a full Skirk exit.
The code is in `experiments/cloudflare-tcp-probe/`; the temporary deployment was
removed after the test. This reopens a bounded serverless exit experiment but
does not remove the destination-coverage, Drive-discovery, and lifecycle work.

Cloudflare Containers are the closest Cloudflare-hosted path to a real Skirk
exit because they can run the existing daemon shape. Containers require Workers
Paid, so they are not part of the free-tier product.

## Drive Primitive Lessons

`files.list` on a narrow prefix remains the best proven hot discovery path.
`changes.list` is durable, but it is scoped by Drive space rather than by Skirk
object prefix, so appData data objects pollute the change feed.

Generated IDs are useful for idempotent upload retry and manifest references.
They do not make new objects visible faster by themselves.

Range reads are safe only when Skirk validates HTTP `206 Content-Range` and
authenticates independently encrypted fragments. The primitive works, but tested
range transports added enough control overhead that they lost to muxv4.

Resumable upload is useful for large unreliable file uploads in general Drive
applications. In Skirk's hot path it adds an initiation round trip and did not
beat multipart upload for the tested chunk sizes.

`files.update` is a whole-file media update. It avoids create/list/delete churn
but creates expensive revision/write tails and is not a good byte-queue
primitive.

## Promotion Gates

Any future transport must beat same-day muxv4 controls before becoming the
default:

- 100 MiB mixed bulk plus small probes;
- 1 GiB mixed bulk plus small probes;
- browser/media overlap while bulk is active;
- five parallel 100 MiB clients;
- zero terminal stream failures;
- no sustained increase in Drive API errors versus the same-day muxv4 control;
- no gap-repair loop or skipped-object behavior;
- tiny downstream objects below 5% of normal objects;
- Drive calls and estimated quota units per GiB no more than 1.15x muxv4;
- cleanup leaves no stale active-prefix debt.

Single-stream throughput is not enough. A candidate that wins a raw download
test but freezes browsing should be rejected.

## Mux v4 Work Worth Doing

Muxv4 still deserves careful incremental work:

- better structured observability for queue depth, object size, gap age, socket
  write blocking, and cleanup backlog;
- same-day paired benchmark harnesses that run real concurrent small and bulk
  traffic;
- conservative object-size and receive-window experiments with hard rollback
  gates;
- restart and cleanup soak tests;
- documentation that keeps user-facing performance expectations honest.

Expected gains from those changes are stability and smaller tail latency first,
then moderate throughput improvements. They should not be described as a
guaranteed path to an order-of-magnitude speedup under the current Drive-only
constraint.

The November 2026 [architecture frontier](architecture-frontier-2026.md)
adds persistent messenger-style text under concurrent upload and download,
an isolated Drive class-query pilot, corrected bounded priority experiments,
100 MiB failure evidence, and five/ten-client synthetic stress results.

## Opt-in sparse priority replenishment

`tunnel.sparse_priority: true` is an experimental muxv4 scheduler setting for
long-lived TCP connections that pass the initial 64 KiB priority allowance.
After demotion, it can place a payload of at most 4 KiB in the priority lane
only after a 750 ms quiet gap, at most once per stream every 2 seconds, and
only when that stream has no outstanding normal upload. A shared budget allows
eight such regrants at once and refills at four per second. Sustained writes
stay on the normal lane. Priority FIN remains disabled after demotion.

The read loop reevaluates coalescing after the first byte arrives, so a quiet
connection that has previously sent 256 KiB does not retain the forced bulk
read delay for its next short message. This option changes only local mux
scheduling; the Drive object protocol and Google route are unchanged. The
default is off. Local MemoryStore benchmarks can expose scheduling behavior,
but do not model Drive visibility, rate limits, or backend stalls. Promotion
requires the mixed live gates above, including tiny-object ratio and quota.

For a local persistent TCP echo with concurrent verified bulk responses, run
`go run ./tools/persistent_bench` once with and once without
`-sparse-priority`, using the same delay and workload flags. The tool uses
localhost sockets and MemoryStore only.
