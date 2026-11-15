# Drive normal-priority list page size, November 14, 2026

A bounded pinned `www.google.com` front probe used the isolated `shahabpre`
test folder. It created 528 production-shaped mux names: 16 older p0 and 512
newer p1, with eight untagged old-peer objects in each class. Priority objects
had random metadata-only descriptions. All objects were empty metadata files,
so this measured discovery rather than bulk transfer. The 24 query sweeps used
an order-reversed 100, 500, 1000, 1000, 500, 100 schedule. Every full sweep
found its expected exact IDs, and `incompleteSearch` was false.

| Legacy p1 page size | Pages | Units per full sweep | Two full-sweep times | IDs within mux four-page cap | Decoded bytes per full sweep |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 6 | 600 | 1.54 / 1.67 s | 400 / 512 | 150.3–150.7 KB |
| 500 | 2 | 200 | 0.66 / 0.71 s | 512 / 512 | 148.3 KB |
| 1000 | 1 | 100 | 0.53 / 0.61 s | 512 / 512 | 147.8 KB |

The p0 legacy query found all 16 IDs in one page at each size, taking
0.17–0.42 s. The indexed p0 query found only the eight tagged IDs. Indexed p1
found its exact 504 tagged IDs, with the same 6/2/1 page pattern, but missed
the eight untagged older-peer p1 objects by design. Therefore the property
query cannot replace legacy discovery without a capability rollout or a broad
fallback. It did not improve this p1 scan relative to a larger legacy page.

The isolated code change sets **only the broad mux p1 discovery query** to
page size 1000 by default. `drive.normal_list_page_size` accepts 100, 500, or
1000, with 100 and 500 available for rollback or larger-response environments.
Priority p0, targeted repair, control lookup, cleanup, and the probe-only
indexed query remain at 100. No transport protocol or peer negotiation changes.
The p1 query returned 16 false-match p0 records in this test; a mailbox dense
with metadata-only p0 descriptions could make a 1000-object response larger.
The two timing samples per cell demonstrate call count and bounded visibility,
not a universal latency optimum or sustained service capacity.

The private JSON report is
`.skirk-runs/list-page-live-20261114-a.json` in the isolated probe worktree.
The run used 60,645 estimated Drive units including setup and cleanup, deleted
all 528 exact IDs, verified each as 404, and found zero files under its unique
prefix in three sweeps including a delayed final sweep. The recovery ledger was
removed after verified cleanup.

Google documents a maximum `files.list` page size of 1000, allows smaller
returns than requested, and charges 100 units per list call:
[files.list](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list),
[usage limits](https://developers.google.com/workspace/drive/api/guides/limits).
