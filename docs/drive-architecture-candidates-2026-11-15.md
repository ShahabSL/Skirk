# Fresh Drive transport design matrix

These are twenty materially distinct protocol or discovery shapes, not twenty
implemented production protocols. The user-selected pinned `www.google.com`
front remains the assumed client route; an explicit Direct route is possible.
An API mechanism is not a latency guarantee. Existing live evidence is linked
instead of treating a prior rejection as a permanent architectural law.

The order below is a **research priority**, based on removing hot-path calls
without adding a new service or weakening delivery integrity. It is not a
ranking of measured application performance. Calls describe one publication
and one successful read; empty polls, setup, retries and cleanup add work.

| Priority / shape | Critical path and purpose | API/implementation evidence | Correctness, cleanup and mixed-traffic cost |
| --- | --- | --- | --- |
| 1. Immutable description packets | Metadata `create → list`; removes media GET for eligible ciphertext. | Existing opt-in metadata-only and dual-write experiments have several positive live pairs; [results](architecture-frontier-2026.md#workload-and-measurements). | Capability negotiation, payload bounds and fallback required. Large pages amplify JSON bytes; failed 100 MiB workloads prevent an unconditional promotion. |
| 2. Larger description packets | Same two calls, packing medium responses into metadata. | Current runtime cap is an application choice (8 KiB sealed). Root carrier-size matrix is a fresh empirical test. | Description's numeric maximum is not documented in the file resource. Larger packets need receive-memory/page limits, exact byte tests and measured full-proxy comparisons. |
| 3. Pre-announced immutable ID train | Generate/share IDs once; `create → exact-ID metadata GET`. | [New guarded probe](../tools/id_train_probe/README.md), `generateIds` and explicit-ID create are documented. | Avoids overwrites and per-packet manifests. Expected precreate 404s cost calls; one missing ID blocks that ordered train. Authenticated vector transmission is not measured by the primitive. |
| 4. Record-authenticated streaming slabs | `create → list → one streaming media GET`, delivering each independently authenticated record as bytes arrive. | Current `httpclient.go` uses `io.ReadAll`; current mux envelope authenticates the whole object. A new record container is unimplemented. | Can reduce GET-side first-delivery wait/memory, but cannot read an uncommitted upload. Each record needs nonce/offset binding; retries must not redeliver records. Avoid unauthenticated partial plaintext. |
| 5. Exact prefix class queues | Put class at the start of the name and issue one class-prefix list. | Google documents `name contains` as prefix matching; current names put class in the interior. Existing property-class pilot found false matches in the legacy query. | Needs explicit version namespace and mixed-peer fallback. No need to change encrypted payload. Dense mailbox correctness/latency is more relevant than sparse synthetic speed. |
| 6. Per-stream interaction scheduling | Requalify quiet, small frames for a bounded priority epoch; retain normal bulk. | Existing epoch/metadata complete 16 MiB pairs include substantial wins; larger and multi-client pairs show tradeoffs. | Must preserve per-stream sequence, process/account budgets and bulk fairness. App message boundaries inside encrypted TCP remain unknown. |
| 7. Fixed metadata cumulative log | One writer per direction; `PATCH → known-ID GET`, piggyback cumulative ACK in reverse snapshot. | Existing fixed-slot probes and opt-in mux integration. | Every unacknowledged frame must remain in snapshots. Bounded queue, version skipping, fallback dedupe, ownership and crash semantics are essential; overwrite latest-only loses data. |
| 8. Nonblocking slot speculation | Publish priority into cumulative slot immediately; fallback immutable object after a deadline without holding unrelated streams. | Existing slot delivery waits up to 1.5 s for ACK. A transport-wide nonblocking variant is not demonstrated. | Shared prepared ciphertext and per-name digest claims must dedupe slot/object races. More speculative calls may erase quota benefit. |
| 9. Write-once precreated capsule queue | Share empty known IDs; each gets one payload PATCH, then receiver deletes it. | Distinct from mutable slot overwrite and direct create-at-reserved-ID. Not freshly live-tested. | Empty GETs are successful rather than 404; setup adds one create per capsule. Recycling needs generation and never write to an ID after delete. Break-even may be worse than ID train. |
| 10. Encrypted filename packets | `create → list`, payload encoded in `name`, which discovery already needs. | Root carrier matrix tests byte capacity and retrieval. | Names are visible metadata; only sealed records belong here. Size, query escaping, namespace length, list bandwidth and cleanup parsing need explicit bounds. No quota discount versus description. |
| 11. Custom-property packets | Shard ciphertext across bounded `properties` or `appProperties`, return in list. | [Official property limits](https://developers.google.com/workspace/drive/api/guides/properties). Root carrier matrix tests this path. | Thirty public properties and thirty private per app, 124 UTF-8 bytes per key+value. Framing/parity uses capacity. More JSON overhead; no inherent call-count gain over description. |
| 12. Indexed property discovery | Tag class/client/session in create; list indexed exact property values. | Existing eight-p0/100-p1 pilot verified exact classes in fewer pages. | Index visibility may differ from names. Old untagged peers require fallback; tagging alone does not establish latency. |
| 13. Change-feed packet discovery | `create → changes.list`, select returned ID/metadata. | [Changes guide](https://developers.google.com/workspace/drive/api/guides/manage-changes), old experiments recorded. | Account-wide pollution and delete changes can grow feed work. Persist tokens, handle removals, authenticate namespace and dedupe IDs. Can be useful when prefix queries are expensive, not assumed faster. |
| 14. Drive watch as idle wakeup | Notification wakes receiver; exact-ID GET or changes list fetches bytes. | Existing bounded watch probe: notification slower than immediate GET in that sample. [Record](experiments/drive-push-notification-live-2026-11-15.md). | HTTPS callback, expiring channels, notification authentication, duplicate/missing hints and periodic reconciliation. Treat as idle-quota design unless paired interactive test wins. |
| 15. Workspace Events + Pub/Sub | Folder event carries ID; subscriber does exact read, keeping Drive for bytes. | [Drive events](https://developers.google.com/workspace/events/guides/events-drive), [setup](https://developers.google.com/workspace/events/guides/create-subscription). | Billing, IAM, subscription renewal, delivery tails and fronted Pub/Sub reachability are separate product constraints. Root may inspect feasibility; no live full-proxy measurement in this branch. |
| 16. Blob revision mailbox | `update content → revision discovery/read`, store packet versions in one file. | [Revision semantics](https://developers.google.com/workspace/drive/api/guides/change-overview). | Non-head revisions may be purged and downloads require retained revisions. Adds discovery/lifecycle calls, bounded retention and cleanup complexity; no reliable append queue emerges for free. |
| 17. Replacement append slab + ranges | Append locally, upload replacement slab, fetch missing ranges of committed version. | Partial blob downloads are documented; old range-slab experiments exist. | Drive update is replacement, not atomic append. Growing rewrites amplify bytes; receiver must bind ranges to a stable revision and discard torn/mixed versions. |
| 18. Resumable upload as live stream | Initiate session, send chunks, attempt reader access before completion. | [Upload guide](https://developers.google.com/workspace/drive/api/guides/manage-uploads) documents resume and final file response after completion. | Google does not document reading unfinished upload bytes. This cannot be a reliable TCP carrier assumption. Retain resumable for interrupted large committed objects, not speculative streaming. |
| 19. Pre-reserved immutable media slabs | Share a window of IDs; upload media directly and read next known ID without listing. | Old generated-ID/range families exist; immutable metadata ID train isolates discovery cost now. | Media read still costs 200 units and whole-object visibility. Window credits avoid unbounded 404s; shards need ordered reassembly and fair bulk/interactive queues. |
| 20. Multiple independent mailbox shards | Partition clients or lanes across user-owned account/project mailboxes. | Existing bounded concurrent-client and quota-project probes; no sustained 200,000-user proof. | Can divide a bottleneck but adds accounts/grants, fairness and cleanup across shards. It cannot remove a single request's object commit/discovery delay. Measure per-user and project buckets correctly. |

The official [file resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
labels `contentHints.indexableText` as at most 128 KB, **but contentHints fields
are never returned in responses**. That field is not a readable packet carrier.
The thumbnail hint is also not returned raw. These are concrete API boundaries,
not an unexplored encoding opportunity. Neither `name` nor `description` gains
a numeric capacity guarantee from that 128 KB limit.

The [ID reservation API](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/generateIds)
provides IDs belonging to the requesting user. It does not make IDs a shared
counter, globally predictable sequence or zero-call rendezvous. A full ID-train
transport would reserve a bounded batch per direction, authenticate a vector
in an already existing OPEN/first-reply capability exchange, replenish before
exhaustion in normal traffic, and retain a prefix-list recovery path. A lost
manifest or missing head ID requires a bounded gap decision; skipping without
authenticated sequence proof can lose bytes.

HTTP/1 versus HTTP/2 versus HTTP/3, separate connection pools, partial response
fields, gzip, generated-ID idempotent hedges, batch deletes and chunk sizes are
cross-cutting tuning choices. They should be crossed with a promising carrier,
not counted as new application protocols. Drive's [partial response/gzip guide](https://developers.google.com/workspace/drive/api/guides/performance)
supports byte reduction; [quota rules](https://developers.google.com/workspace/drive/api/guides/limits)
still charge operations. Batch deletion reduces request overhead, not the
number of inner quota charges. Each carrier must preserve deferred exact-ID
cleanup and test cancellation before publishing can outlive its cleanup sweep.

The next full-proxy comparison should use at least one persistent demoted TCP
text stream plus simultaneous upload/download, rather than only short new
connections. Measure cold bootstrap separately from warm scheduled text,
include all failed arms and quota tails, and compare every candidate against
the same clean muxv4 binary before comparing it to `main`. This distinguishes
a carrier improvement from unrelated branch changes and Google backend drift.

## Evidence labels and next decisive measurement

The labels below describe evidence available before the fresh ID-train probe;
the probe's result record adds its measured observations separately.

| Design above | Evidence level | Next measurement that could change its rank |
| --- | --- | --- |
| 1 | Live mixed workloads, with positive and failed pairs | Repeat metadata-only plus interaction epoch against same-binary control at 100 MiB with persistent text and verified cleanup. |
| 2 | Fresh carrier capacity primitive | 16 KiB sealed batches versus 8 KiB inside the full mux; text p95, list response bytes, receive-memory peak and stalled-file count. |
| 3 | New bounded primitive | Paired scheduled burst with bounded parallel creates, then full proxy negotiation including ID-vector byte/call cost and manifest loss. |
| 4 | Code inspection and documented completed-media GET | Prototype per-record AEAD, force download stalls at record boundaries and verify early delivery plus retry dedupe without extra GETs. |
| 5 | Documented query semantics, old live class pilot | Create interleaved p0/p1 namespaces with prefix-class naming; exact-ID recall and time-to-visible at 1,000+ objects under churn. |
| 6 | Multiple live mixed pairs and deterministic tests | Controlled reentry/pacing matrix with independently scheduled persistent text and concurrent upload/download; repeat under five clients. |
| 7 | Live primitive, mixed substrate and opt-in mux | Separate the slot's ACK wait from carrier delay in a full mixed TCP trace; quota units per verified reply and fallback rates. |
| 8 | New protocol proposal | Implement asynchronous slot sender; force ACK loss, late immutable duplicates, process death and bounded queue pressure before a live ABBA. |
| 9 | Documented create/PATCH/GET components | One paired write-once capsule burst versus direct-create ID train, including empty-slot setup and exact-ID cleanup. |
| 10 | Fresh carrier matrix | Compare equal sealed bytes/name prefix length against description within the mux, including dense pages and query false matches. |
| 11 | Documented limits plus fresh carrier matrix | Equal ciphertext property-shards versus description; whole JSON bytes, parse time, mutation atomicity and prefix discovery delay. |
| 12 | Existing live class primitive | Full negotiated property query under index churn, mixed older peers and rejected-page-token recovery. |
| 13 | Prior live prototypes | Same-user mailbox pollution at 0/100/1,000 unrelated changes; exact recall, change pages per verified transport packet and idle units. |
| 14 | Existing bounded watch timing | Overnight idle wakeups plus expiry/renewal failure, then fallback-list completeness and idle quota comparison. |
| 15 | Current docs, no full-proxy live evidence | One folder event through the selected Google route with exact created ID/read, event delay, IAM/billing and cleanup; do not build on assumed availability. |
| 16 | Documented revision model | Test whether every published blob revision can be retained/discovered/read within bounded calls; account for retention cap and deletion. |
| 17 | Documented ranges and old slab primitive | Deliberately race update/read across versions; verify no mixed-version bytes and measure rewrite amplification against immutable objects. |
| 18 | Documented resumable protocol; no unfinished-read contract | One bounded partial upload/GET probe can characterize an observation, but no production dependence without an API guarantee. |
| 19 | Prior ID/media families, new metadata train isolates discovery | Advance-ID media window at 1/2/4 outstanding objects with verified gap handling, no unbounded 404 probing and full mixed traffic. |
| 20 | Bounded 10-client live workload and quota-header probes | Sustained operator-owned shard workload with Cloud quota telemetry, revocation, account failure, fairness and exact per-shard cleanup. |

Sender pacing and dynamic chunk size belong in the same-binary factorial test:
1/2/4 outstanding creates, 256 KiB/1 MiB/4 MiB bulk batches, and a bounded
interactive epoch. Report offered versus achieved send schedule and byte
verification; a higher measured goodput is insufficient if text stalls, files
miss their progress deadline or cleanup grows without bound.
