# Generated-ID metadata slot compatibility — November 14, 2026

The opt-in fixed metadata lane needs to create a pair of empty Drive files
with IDs known before publication. A bounded compatibility probe exercised
that exact API sequence on the isolated personal test mailbox over the pinned
`www.google.com` front. It did not run a Skirk mux or app workload.

`tools/metadata_slot_compat` used the isolated kit without changing its saved
config. It enabled `drive.metadata_slot_v1` and the pinned route only in memory,
generated one file ID, created one `slotv1/compat/<random>/up` file by metadata
`files.create`, read it from an independent HTTP pool, PATCHed an 8,192-byte
synthetic base64url description once, and read the exact description back.
The random test content was not a real Skirk frame or credential.

| Check | Result |
| --- | ---: |
| Generated-ID metadata-only create | Passed |
| Exact 8,192-byte description PATCH/readback | Passed |
| Drive file versions at create, first read, PATCH | 1, 2, 3 |
| Exact-ID 404 checks after delete | 2/2 |
| Unique-prefix objects, immediate/delayed 30 s | 0/0 |
| Estimated Drive units, including cleanup | 375 |
| API errors / recovery records remaining | 0 / 0 |

The ignored, private report is
`.skirk-runs/metadata-slot-compat-20261114.json` in the isolated backend
worktree. The tool has a 4,000-unit cap and keeps an exact-ID recovery record
until cleanup completes. It accepts only the isolated `personal-test/kit`
config path and writes reports under ignored `.skirk-runs`.

This establishes one API compatibility boundary, including the proposed 8 KiB
description cap. It does not establish two-writer protocol safety, app-level
round-trip latency, sustained quota behavior, or cross-platform operation.
The `version` values are Drive change hints; the protocol must use its own
authenticated sequence and cumulative acknowledgements.

Official API references: [pre-generated IDs](https://developers.google.com/workspace/drive/api/guides/create-file),
[file metadata](https://developers.google.com/workspace/drive/api/reference/rest/v3/files),
and [file update](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/update).
