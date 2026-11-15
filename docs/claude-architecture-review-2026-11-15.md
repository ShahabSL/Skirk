# Claude architecture review — November 15, 2026

Status: all three architecture passes completed. The external model was used
for read-only design,
not implementation or measurement. A short CLI health probe resolved the
`fable` alias to **`claude-fable-5-2`** in returned `modelUsage` and
`canonicalModel` metadata, with one response turn and no permission denials.
Two independent CLI invocations request that alias at maximum effort, with
built-in tools disabled and explicit no-tool instructions. One receives
the historical experiment evidence, including strong positive results; the
other receives only the product constraints and API/quota assumptions. Each
audit's returned model identifier and turn count are recorded below.
A targeted third pass reviews the current slot integration
and the hypothesis that its processing-ACK wait blocks lane workers.

The review requests 20 materially distinct carriers/protocols, explicit API
critical paths and ACK meanings, bounded queues and quotas, exact cleanup,
crash/reordering safety, and falsifiable primitive and mixed-workload tests.
No credentials, real profiles, or ignored runtime logs were supplied. The
input evidence consists of tracked documentation and a sanitized constraints
summary. Raw CLI outputs remain ignored under `.skirk-runs/`.

## Returned model evidence

The CLI requested Fable 5.2, but the evidence-aware response records both
`claude-fable-5-2` and an automatic `claude-opus-4-8` fallback. Fable's reported
12,180 output tokens were all counted as thinking; Opus reported 37,895 output
tokens, including 25,144 thinking tokens. The visible 32,878-character audit
therefore appears to be the fallback's answer. This is a **mixed/fallback
Claude review**, not evidence of a pure Fable-authored final audit. It completed
in 739 seconds with two turns, no permission denials, and zero web searches.

A separate short architecture-priorities request was rejected by the external
service's cybersecurity safeguards. Its result had `is_error: true`, no design
recommendations, and both model IDs in usage. No reworded retry or safeguard
bypass was attempted. The initial short health check succeeded with Fable 5.2;
that verifies model selection, not acceptance of every research request.

The targeted slot-code review returned **only `claude-fable-5-2`** in model
usage, one turn, zero web searches, and no permission denials. It completed in
975 seconds. The 89,994 reported output tokens included 82,859 thinking tokens;
the visible answer was 18,899 characters. This pass is evidence of a real
Fable 5.2 architecture audit. It received three tracked slot source files and
the measured substrate results, with tools and MCP servers disabled. It did
not receive `mux.go`, so its conclusions about mux scheduling include explicit
assumptions that require code inspection and tests.

The constraint-led 20-design response also returned **only
`claude-fable-5-2`**, one turn, zero web searches, and no permission denials.
It completed in 1,451 seconds with 127,422 reported output tokens, including
113,359 thinking tokens, and a 36,839-character visible answer. The prompt
withheld historical evidence, but the model disclosed that the session
harness attached the repository handbook. Its initial design is therefore
constraint-led **rather than perfectly blinded**. No later evidence-reveal
request was made; the comparison below is the engineering team's assessment,
not a fourth Claude answer.

## Evidence-aware recommendations and independent checks

The strongest useful recommendation is to separate **carrier performance**
from **integration scheduling**. The audit highlights the fixed-ID metadata
cumulative-queue substrate's 71–73% text-p95 reductions with better bulk rate
and lower estimated units. It argues that a later full-mux regression is reason
to investigate coordination overhead, not evidence that the carrier itself
cannot help. This is a hypothesis supported by the contrast between workloads;
the experiments did not isolate a causal variable.

Direct inspection confirms that `trySlotDelivery` runs synchronously inside
`uploadPreparedBatchV4` and may hold a lane upload worker for up to 1.5 seconds
waiting for receiver processing ACK. The slot writer also serializes PATCHes
with a minimum interval. A safe experiment should transfer a prepared batch to
a bounded independent scheduler, retain its exact sealed identity for fallback,
and distinguish successful Drive publication from receiver processing. Merely
reporting the original upload ACK after a memory enqueue would weaken its
meaning and risk unbounded local reads.

The audit's 20 candidates, in its proposed order, were:

The [complete unedited answer](claude-architecture-raw-evidence-aware-2026-11-15.md)
is retained with a warning header; the table below records the independent
disposition rather than adopting every assertion.

| Rank | Candidate | Independent disposition |
| --- | --- | --- |
| 1 | Interactive known-ID description ring plus immutable bulk, separate pools | Strong candidate; remeasure thin integration. |
| 2 | Immutable metadata-only description packets discovered inline by list | Retain earlier positive results and test larger mixed workloads. |
| 3 | Immutable media plus prefix polling | Compatibility/control path. |
| 4 | Bulk pool isolation plus a stall-triggered idempotent hedge | Useful hypothesis; duplicate work and quota require guards. |
| 5 | Description ring for exclusively small traffic | Same carrier as rank 1, narrower workload, not a separate universal protocol. |
| 6 | Property-indexed class discovery with immutable media | Requires mixed-version fallback and visibility tests. |
| 7 | Exit-side Drive push upstream, client polling downstream | Reachability-compatible asymmetry; earlier callback delays do not establish speed. |
| 8 | Exit-side watch of known ring IDs | Could reduce idle polling; renewal and missed/duplicate events remain. |
| 9 | Small appProperties sequence/ACK controls piggybacked on metadata | Tiny fields; capacity and OAuth-app visibility are constrained. |
| 10 | Encrypted file-name payload returned by list | Feasible only within tested caps; no demonstrated advantage over description. |
| 11 | Exit-side HTTP/3 while the client uses its selected route | Measure independently; do not silently change client reachability. |
| 12 | Comments/replies as an append log | Append ordering is not guaranteed by the cited docs; caps, API cost, deleted-comment accumulation need probes. |
| 13 | Revisions as a description-history log | **Invalid as stated:** metadata changes are not blob content revisions. |
| 14 | Batched metadata creates and deletes | API overhead experiment; no per-operation quota discount. |
| 15 | Changes cursor feed plus media | Must measure pollution and reconciliation costs. |
| 16 | Workspace folder events and Pub/Sub on exit | Additional control plane; not a measured interactive win. |
| 17 | Prewritten media slabs with range reads | Requires index/rendezvous; no streaming append assumption. |
| 18 | Whole-file media overwrite ring | Prior slow-tail evidence; explicit overwrite/revision semantics required. |
| 19 | Resumable upload with receiver tail reads | Undocumented partial visibility; do not implement as if guaranteed. |
| 20 | Pub/Sub StreamingPull control plane plus Drive bulk | Not Drive-only; pinned-front API reachability must be proved, not assumed impossible or available. |

The audit underemphasized the **pre-generated immutable ID credit queue**.
That deserves a separate comparison against mutable rings: no repeated
overwrite, no per-packet processing ACK needed to make a slot reusable, and
known-ID metadata reads can avoid prefix-index discovery. It still pays ID
exchange/replenishment, 404/visibility retries, retention, and delete costs.

Several confident statements in the generated answer are not established by
the evidence. A second connection beating a stalled request does not prove a
connection-local cause; per-request backend variance remains possible. Zero
HTTP errors does not exclude backend load effects. Ring traffic avoids
per-message deletes but still needs bounded unacknowledged records, teardown,
orphan recovery, and exact-ID cleanup. Notification/HTTP3 asymmetry can change
costs, but neither provides free latency improvement. These corrections are
part of the review rather than silently adopting model suggestions.

The audit also misread the implementation status of metadata-only priority:
it is already implemented and benchmarked on the research branch. Its
recommendation to pursue that path is a recommendation for further controlled
evaluation, not discovery of an unimplemented feature. The 128 KiB
`contentHints.indexableText` input limit does not enlarge a readable metadata
carrier because those fields are not returned in responses.

## Targeted Fable 5.2 slot review

The [complete answer](claude-architecture-raw-slot-review-2026-11-15.md) ranks
asynchronous slot integration as the immediate experiment and immutable
pre-shared ID trains as a promising alternative. It correctly distinguishes
freeing a lane worker after a bounded ownership handoff from completing the
original upload ACK: the latter must still wait for confirmed Drive
publication, successful exact readback, receiver acceptance, or successful
immutable fallback, according to the explicitly chosen ACK definition.

Independent inspection of the supplied `f9a2413` source confirms several
useful leads beyond the synchronous 1.5-second wait:

- The slot writer handles one queued record per PATCH and shares its pacing
  with ACK-only PATCHes. Grouping queued records is an experiment to isolate.
- `metadataSlotPeer.receive` checks ACK monotonicity before its incoming
  revision-replay check. An authenticated old snapshot with a regressed ACK
  could close the link instead of being ignored. This needs a deterministic
  stale-read test.
- After virtual processing, mux sends `ackWake` but does not immediately offer
  the next staged head. The reader loop reoffers it later, potentially adding
  its GET/ticker cycle. Event-driven reoffer needs gap and readiness tests.
- A PATCH-budget error stops the writer without immediately setting
  `sendClosed`. Until the parent reader ends and closes the link, later sends
  can encounter a queue whose writer has exited. This is a temporary lifecycle
  defect to reproduce, not a measured explanation of prior live latency.
- The outer eligibility check uses raw sealed length, while final admission
  includes Base64, AEAD, and record/name overhead. Precise admission avoids
  queueing records that can never fit.

The proposed independent fallback scheduler must retain the exact prepared
name, sealed bytes, ID, and ACK ownership. Late successful peer processing
must be observed even after the fallback timer fires, or failed immutable
retries can reset a stream whose slot data already arrived. Window records,
encoded bytes, active handoffs, fallback concurrency, and shutdown drain all
need hard bounds.

The generated answer sometimes describes ID trains as needing no ACK or
fallback machinery. More precisely, they can avoid a per-packet reuse ACK
because each ID is used once, but credit exchange, ID-map retention, empty
polling, exact deletes, and restart/visibility fallback remain. A successful
metadata-only train packet costs a create, known-ID read, and delete before
amortized setup and empty polls; it does not inherit a mutable slot's roughly
55-unit PATCH-plus-read calculation. Any claim about a contiguous lane or
stream watermark must be checked against the actual mux state machine before
using it as the authority after digest eviction.

## Constraint-led Fable 5.2 design

The [complete answer](claude-architecture-raw-constraint-led-2026-11-15.md)
independently recommends two carriers: small/control traffic in known-ID
mutable metadata registers, and bulk in immutable objects at pre-announced
IDs. It proposes immutable known-ID metadata packets as the alternative when
mutable writes prove slow or difficult. It also identifies exit-only push,
epoch folders for batch cleanup, incremental changes, conditional requests,
comments, and revisions as distinct experiments or contingencies. It rejects
write-only indexable text and symbol/toggle channels as useful general payload
carriers, and does not assume unfinished uploads can be read.

The 20 entries are register bank; ID-tape bulk; ID-tape metadata micro-objects;
large resumable/range objects; exit push wake; epoch folders; prefix/parent
media discovery; inline metadata listing; changes-cursor discovery; synthetic
modified-time cursor; mutable media slots; shared downlink bus; HTTP batch;
revision log; comment/reply log; Workspace events; conditional register;
indexable-text/thumbnail carrier; unfinished-upload tailing; and flag/rename
symbol channels. Several entries are explicitly refinements or rejected
ideas, not 20 implemented protocols.

Comparing this answer with the supplied experimental evidence favors testing
thin mutable-slot integration and immutable ID trains before inventing an
entirely unrelated carrier. The previous substrate observations support
fast known-ID metadata reads; they do not prove the proposed lease, polling
ladder, full protocol, or bulk ID tape. The existing description measurements
also make description a simpler payload candidate than spreading a packet
across many 124-byte property strings.

Load-bearing cautions from independent review:

- A monotonically increasing register counter lets a reader reject an old
  snapshot, but cannot stop an old PATCH from overwriting a new one. A writer
  still needs single-flight publication and explicit ambiguity resolution;
  blind reassertion is not a bounded correctness proof.
- Atomic creation of a generation marker does not fence an already running
  old exit's destination sockets or create a distributed lease by itself.
  Generation-bound frames help prevent the same frame executing at two
  processes, but application retries remain capable of repeating effects.
- Folder deletion as one cleanup operation needs explicit creation/deletion
  race, ownership, descendant, and delayed-visibility tests. It must never be
  applied to shared or unacknowledged application data on assumption alone.
- The answer's API prices for comments, revisions, events, failed reads,
  conditional responses, and ID generation are stated assumptions. They are
  not verified quota telemetry. The token endpoint shown in the generated
  text is also unverified; production route/auth rules remain authoritative.
- A commit marker for an immutable bulk object can introduce an extra metadata
  write and dependency. The claimed two-operation path must count that write
  or prove that it is piggybacked and off the measured critical path.
- The zero-idle-call claim applies only to dormant clients. Its exit bell and
  safety polling still consume quota. Neither the example capacity arithmetic
  nor a metadata-register plan demonstrates 200,000 concurrent users.

No extra Claude calls were made after the external short-request rejection.
The three already-running passes were allowed to finish, and all generated
recommendations were retained with provenance and corrections.

## Official API facts checked independently

The [file creation guide](https://developers.google.com/workspace/drive/api/guides/create-file)
documents batches of up to 1,000 pre-generated file IDs. Creating a binary or
metadata-only file with one of these IDs is supported, and repeating a
successful create returns HTTP 409 instead of creating a duplicate. This
supports testing an immutable queue of previously shared IDs as a distinct
alternative to repeatedly overwriting a known metadata slot. It does not prove
that a GET of a newly created ID becomes visible before list discovery, or
that the queue removes its setup/replenishment/empty-poll cost.

The [custom-property guide](https://developers.google.com/workspace/drive/api/guides/properties)
limits each UTF-8 key plus value to 124 bytes, with at most 30 public properties
per file and 30 private properties per app per file. These are useful small
indexes or control fields; spreading payload across them needs measured
encoding and JSON overhead.

The [v3 file resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
exposes `name`, `description`, `properties`, and `appProperties`. Its
`contentHints` fields are never populated in responses, so its 128 KiB
indexable-text input is not a normal readable packet carrier. Name and
description length limits are not specified in that reference; a candidate
must establish a bounded accepted size experimentally instead of inventing a
documented cap.

The [resumable-upload guide](https://developers.google.com/workspace/drive/api/guides/manage-uploads)
returns the file resource after the full upload completes. Chunk sizes are
multiples of 256 KiB except the final chunk, and an upload session URI can
expire after a week. This API provides interruption recovery; it does not
document a receiver reading an unfinished upload as a live byte stream.

The [revision guide](https://developers.google.com/workspace/drive/api/guides/manage-revisions)
has retention and download conditions: a file can keep at most 200 blob
revisions forever; other revisions may be purged. An unbounded revision log
cannot simply assume retained, downloadable history.
