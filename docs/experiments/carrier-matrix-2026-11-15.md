# Encrypted Drive carrier matrix, November 15, 2026

This is an independent API primitive experiment. The same pinned
`www.google.com` route and isolated test account were used for all 34 trials.
Each trial generated random plaintext, sealed it with Skirk's authenticated
envelope, created one immutable file, discovered it with a unique prefix,
verified/decrypted exact bytes, measured a further known-ID read, and deleted
the exact ID. A seeded shuffle interleaved two repetitions of each case.

| Carrier | Plaintext | Verified trials | Median create → list → verified bytes | Median known-ID read |
| --- | ---: | ---: | ---: | ---: |
| Media | 256 B | 2/2 | 1,139.9 ms | 210.3 ms |
| Name | 256 B | 2/2 | 632.7 ms | 163.1 ms |
| Description | 256 B | 2/2 | 572.1 ms | 124.1 ms |
| Properties | 256 B | 2/2 | 525.5 ms | 131.5 ms |
| Media | 2 KiB | 2/2 | 1,121.8 ms | 226.9 ms |
| Name | 2 KiB | 2/2 | 600.3 ms | 141.5 ms |
| Description | 2 KiB | 2/2 | 552.5 ms | 164.0 ms |
| Properties | 2 KiB | 2/2 | 520.7 ms | 154.5 ms |
| Media | 8 KiB | 2/2 | 1,158.6 ms | 231.4 ms |
| Description | 8 KiB | 2/2 | 578.7 ms | 155.6 ms |
| Media | 32 KiB | 2/2 | 1,140.5 ms | 270.1 ms |
| Media | 128 KiB | 2/2 | 1,442.9 ms | 253.3 ms |

Both attempts to carry 8/32/128 KiB plaintext in `name`, and both attempts
to carry 32/128 KiB plaintext in `description`, returned HTTP 400. These are
observed limits for Base64URL-encoded Skirk envelopes, not claimed documented
field limits or precise boundary measurements. Properties were only tested
at 256 B and 2 KiB: sealed Base64URL was split into keys `p00` onward with
120-byte values, staying inside the documented 30 public properties and
124-byte key-plus-value limit.

A 20-trial followup tested 2,560/3,072/4,096/12,288/16,384 bytes in names
and descriptions, twice each. Names accepted up to4,096 tested bytes and
rejected12,288/16,384; the first matrix already rejected8,192. Descriptions
accepted every tested followup size, including16,384 bytes (median617.7ms
create-to-list verification). The first matrix rejected32,768. These
brackets include envelope and Base64 expansion and are not exact field caps.
The followup used3,780 estimated units and71 calls; all exact IDs were
deleted, with zero unique-prefix objects immediately and after31seconds.
Raw evidence: `.skirk-runs/carrier-boundary-20261115.json`.

Metadata paths remove the separate media GET. In this run their core path
cost 150 estimated units (create50 + list100), versus350 for media
(create50 + list100 + mediaGET200). Diagnostic known-ID reads and exact
deletion are excluded from that core figure and included in the total:
9,670 estimated units and119 counted HTTP attempts. The ten rejected creates
are included. The private raw JSON is
`.skirk-runs/carrier-matrix-20261115.json`. All created IDs were deleted and
the unique prefix was empty immediately and after31seconds.

At2KiB, list response bodies were150 bytes for media discovery,2,954 for
name,2,979 for description, and3,440 for properties. The media body was
downloaded separately. At8KiB, a description response was11,171bytes.
Packing metadata into large discovery pages can increase bandwidth and
parse cost, so single-object latency does not establish a full transport win.

The known-ID column presumes the peer already has that ID. It excludes ID
exchange, setup, polling before publication, sequence control, restart
behavior, and congestion. It is not a tunnel round trip or a maximum speed.
Two repetitions cannot distinguish the small differences between metadata
fields. Names do work as an encrypted carrier, but did not beat descriptions
or properties in these samples. No encryption keys or real traffic were
placed in metadata.

Official sources: [file metadata fields](https://developers.google.com/workspace/drive/api/reference/rest/v3/files),
[custom property limits](https://developers.google.com/workspace/drive/api/guides/properties),
and [partial responses/compression](https://developers.google.com/workspace/drive/api/guides/performance).
`contentHints.indexableText`'s128KB limit is distinct: Google says
`contentHints` is not populated in responses, so it cannot directly serve as
the receiver's payload field. No numeric description limit was found in that
reference.

Reproduce with `go run ./tools/carrier_matrix --live --config <isolated-exit.json>
--out .skirk-runs/new-carrier-matrix.json`. Use `--sizes`, `--carriers`, and
`--repeats` for bounded followups. Keys remain only in process memory.
