# Immutable advance-ID train, November 15, 2026

This is a bounded one-way metadata primitive, not application message RTT or
a complete proxy transport. Each arm created eight independently authenticated
256-byte payloads and verified all bytes in strict sequence. A fresh generated
ID vector was made available to the reader before its arm began. The generation
call is counted; **authenticated transmission of that vector is excluded**.
The [probe](../../tools/id_train_probe/README.md) preserves the pinned
`www.google.com` route and used only the isolated personal test mailbox.

The control creates the same metadata-only objects but discovers them through
`files.list`; the candidate reads only the next known ID with metadata GET.
Both avoid media downloads. Comparing this candidate directly to main's media
transport would confound the ID discovery change with the metadata carrier.

## First complete run: serialized publisher, fixed read cadence

Binary SHA-256:
`e02cbe826afdea0d3ff546d6ba349c51a932d9a8fe3db389df296b2e83c65ce7`.
The writer serialized creates. The offered packet interval was 250 ms but
most create responses took 330–590 ms, so scheduled latency includes a growing
publication queue. The candidate also slept 200 ms after each success, which
unnecessarily delayed catchup. These conditions are retained, not pooled with
the corrected parallel test.

| Arm order | Mode | Verified | Scheduled receive p50 / p95 | Core estimated units | Precreate 404s |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | ID train | 8/8 | 1.238 / 1.950 s | 465 | 4 |
| 2 | Prefix list | 8/8 | 1.041 / 1.501 s | 1,405 | 0 |
| 3 | Prefix list | 8/8 | 0.982 / 1.440 s | 1,405 | 0 |
| 4 | ID train | 8/8 | 1.032 / 1.416 s | 455 | 2 |

Pair-median p95 was 1.683 s train versus 1.471 s list. The candidate reduced
core units by about 67% but did not win scheduled latency. All 32 objects were
deleted, every ID returned 404 twice, and the unique prefix was empty immediately
and after 31 seconds. Total complete-probe cost was **5,850 units / 176 calls**,
including generation, cleanup, exact-ID verification and two prefix sweeps.
Response bodies totaled 88,806 bytes and request bodies 20,352 bytes. Unexpected
API errors were zero. Private raw report: `.skirk-runs/id-train-live-20261115.json`
in the isolated fresh-architecture worktree.

## Interrupted parallel attempt: false matches in Drive name search

The next attempt allowed two outstanding creates and read the next ID
immediately after successful delivery. Its binary SHA-256 was
`33d23ec110eaf0e19e09a5c458a49abf4f0c551cb82055239e5fb3ac9e2be102`.
The first ID-train arm verified 8/8 payloads with scheduled p95 **0.712 s**.
The first control query then returned a different arm's object despite the
slash-separated `name contains` value. The harness stopped on its literal
prefix check before publishing that arm. No paired conclusion follows.

Cleanup removed the eight created objects, verified all sixteen reserved IDs
missing twice, and found the full unique prefix empty immediately and after
31 seconds. Cost was 1,730 units and zero API errors. This was a harness
assumption failure, not a transport timeout. The corrected tool locally filters
and counts false matches; foreign results are never delivery candidates or
deletion targets. The failed attempt remains at
`.skirk-runs/id-train-parallel2-live-20261115.json`.

## Corrected complete run: parallel window two and immediate catchup

Binary SHA-256:
`056ea8d606bf7474532b5f612207f70525d8d4f7202f53b66918c2b5d29c5078`.
Both modes used a two-create maximum and 250 ms offered interval. The ID-train
reader sleeps 200 ms only after a missing next ID, then drains available
successors immediately. The control lists every 200 ms and buffers later
packets until their predecessor arrives. Tests deliberately delay the first
create past later creates and verify in-order output.

| Arm order | Mode | Verified | Scheduled receive p50 / p95 | Core estimated units | Precreate 404s |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | ID train | 8/8 | 0.561 / 0.766 s | 465 | 4 |
| 2 | Prefix list | 8/8 | 0.474 / 0.724 s | 1,105 | 0 |
| 3 | Prefix list | 8/8 | 0.596 / 0.825 s | 1,105 | 0 |
| 4 | ID train | 8/8 | 0.584 / 0.891 s | 465 | 4 |

Pair-median p95 was **0.829 s train versus 0.775 s list**, approximately 7%
higher for the candidate in this very small burst. Candidate core units were
**465 versus 1,105 per eight packets**, approximately **58% lower**. The tool
filtered **56 literal-prefix false matches**, all from other arms in the same
unique session. No foreign ID was processed or added to the deletion ledger.
The corrected run verifies the discovery/ordering path; it does not establish
a population latency percentile or a speed win.

All 32 objects were deleted, every ID returned 404 twice, and the unique prefix
was empty immediately and after 31 seconds. Complete cost was **5,260 units /
172 calls**, response bodies 111,600 bytes and request bodies 20,352 bytes.
Unexpected API errors were zero. The recovery ledger was removed after verified
cleanup. Private raw report:
`.skirk-runs/id-train-parallel2-filtered-live-20261115.json`.

The p95 of eight samples is the maximum. Some records were observed before
the publisher received its create response; the last candidate's first create
response took 1.410 s while its record was visible at scheduled +0.329 s.
Negative post-response durations in the raw report describe that ordering,
not negative latency. No bulk transfer, persistent TCP stream, process restart,
hostile network or additional client was exercised.

## Integration design and next decision

Keep the code as a primitive. A full candidate should piggyback a bounded
authenticated ID credit vector in existing OPEN/first-reply capability frames,
reserve separate vectors per direction/client/run, and replenish before
exhaustion using existing traffic. The cold initial OPEN still needs a known
bootstrap or prefix-list path. The sender must never reuse an ID with different
sealed bytes; the receiver must validate epoch, name, ID, stream sequence and
AEAD before processing and deleting it.

Only the receiver's next bounded window should be probed; prefetching an
unbounded future creates 404 pressure. A missing head needs an authenticated
bounded gap/restart decision. No transparent replay of a possibly delivered
side effect is permitted. A generation protocol can reject stale packets,
but receiver deletion is not a durable external-delivery acknowledgment.

At sparse traffic, 5-unit exact metadata GETs can reduce discovery cost. At
high fanout, one list can discover many objects in one call, so an adaptive
threshold and multi-client comparison are needed. These short probes do not
justify replacing muxv4, and the out-of-band vector assumption cannot be
hidden in an end-to-end performance claim.
