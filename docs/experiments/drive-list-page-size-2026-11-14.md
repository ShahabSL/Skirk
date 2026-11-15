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

## Sparse live path comparison

The 528-object probe establishes a backlog benefit. To test whether the larger
page changes ordinary small-message latency or file speed when the mailbox is
sparse, `skirk_live_ab.py` has a separate same-binary ABBA mode. The control
sets `normal_list_page_size=1000`; the candidate sets it to `100`. It removes
slot and other transport experiments from both arms, preserves the test kit's
selected pinned Google front route, and checks the exact message/file hashes,
exit proof, API errors, and estimated Drive units on every arm:

```sh
go build -trimpath -o .skirk-runs/page-size-skirk ./cmd/skirk
python3 tools/skirk_live_ab.py \
  --binary .skirk-runs/page-size-skirk \
  --control-binary .skirk-runs/page-size-skirk \
  --config .skirk-runs/personal-test/quota-kit/client.json \
  --out .skirk-runs/page-size-sparse-001 --live --observe --probe-exit \
  --order control,candidate,candidate,control \
  --candidate-normal-list-page-size-100 \
  --bulk-mib 16 --bulk-direction download \
  --small-traffic message --small-count 12 --small-interval-ms 250
```

The four generated `muxv4/<session>/` prefixes still require exact cleanup
and a delayed zero-file check after the harness exits.

The same-binary sparse live run completed all four 16 MiB exact-hash file
transfers, 48/48 small messages, and four authenticated exit proofs with zero
Drive API errors. Its actual order was 100/1000/1000/100, preserving the
order-reversed treatment. Two-arm medians were:

| p1 page size | Scheduled small p95 | Request p95 | File goodput | Estimated units, both peers |
| ---: | ---: | ---: | ---: | ---: |
| 100 | 17.767 s | 6.386 s | 10.628 Mb/s | 25,455 |
| 1000 | 15.976 s | 4.468 s | 11.839 Mb/s | 26,230 |

Page 1000 was not slower in this pair, but individual file arms varied
8.376–12.880 Mb/s at size 100 and 7.259–16.418 Mb/s at size 1000. That
variation overwhelms a causal sparse-path conclusion. The run does not
explain the earlier clean-main/current gap or justify reverting the dense
mailbox page-size improvement. Private raw summary:
`.skirk-runs/page-size-sparse-16m-20261114-01/`. Exact cleanup deleted nine
residual objects; all eight mux/slot prefixes were zero on immediate and
delayed checks.
