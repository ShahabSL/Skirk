# Live Drive check of normal-priority pagination

On November 14, 2026, the isolated `shahabpre` test folder ran the final
default mux normal-priority continuation code from `52cf03b` against real
Drive `files.list` pages. The guarded test source is
[`internal/skirk/live_dense_p1_test.go`](../../internal/skirk/live_dense_p1_test.go).
It deliberately selected the supported rollback page size **100** to cross
the same four-page boundary with 402 temporary objects, rather than creating
more than 4,000 objects at the default page size of 1,000. This exercised the
same runtime continuation method and limited test-account writes and cleanup.

The test reserved all 402 Drive IDs and wrote an fsynced private recovery
ledger before creating the first object. It created the target p1 object
first, then 401 newer p1 objects, at no more than three writes per second.
It waited until a complete five-page Drive query saw all 402 exact IDs.

| Runtime poll | Drive list calls | Page budget / actual pages | Objects returned | Older target |
| --- | ---: | --- | ---: | --- |
| First | 1 | 4 / 4 | 400 | Absent |
| Second | 2 | Continuation 3 / 1, fresh head 1 / 1 | 2 + 100 | Present |

The target was reached on poll 2 without exceeding four pages in either poll.
The fresh-head page repeats recently seen objects by design; it bounds
visibility for new arrivals while the saved token traverses older results.
This live run held the 401 newer objects steady. Local
[`p1_pagination_test.go`](../../internal/skirk/p1_pagination_test.go) covers
repeated newer inserts, a burst larger than one head page, idle continuation,
and a rejected page token with a deterministic store. Neither test proves
losslessness under arbitrary concurrent changes to Drive's search index.
Google says results can differ when a page token is used after items are
added or removed; see [Drive `files.list`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list).

The live test took **520.37 seconds** including paced setup, exact cleanup,
and a delayed check. It issued **1,220 Drive calls** and an estimated
**43,515 quota units**: 402 creates and 402 deletes at 50 units each, 402
metadata checks at 5 units each, 13 lists at 100 units each, and one
generated-ID request at 5 units. Upload, list, delete, and ID reservation
reported zero API errors. The 402 metadata "errors" in the quota snapshot
were the expected `404 notFound` responses confirming deletion. Exact cleanup
deleted 402 IDs, verified 402/402 as 404, found zero objects under the unique
run prefix immediately and after 30 seconds, and removed the recovery ledger.
The raw report remains private at
`internal/skirk/.skirk-runs/dense-p1-live-20261114.json` in the isolated test
worktree; it contains the unique prefix but no account tokens.

This proves bounded older-page progress for one steady, real Drive mailbox
at page size 100. It does not measure the production page-size-1,000 case,
file transfer, 200,000-user concurrency, or a sustained latency ceiling.
