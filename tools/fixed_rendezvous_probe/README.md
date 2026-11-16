# Fixed known-ID rendezvous: bounded live discriminator

This is an **isolated test tool**, not a Skirk transport or a product default.
It compares a precreated, hot, fixed Drive v2 metadata snapshot against one
immutable metadata file per cold OPEN, discovered by `files.list`. Both arms
use the same personal-test OAuth grant, visible folder, selected pinned
`www.google.com` front, authenticated opaque 64-byte claims, and the same
process binary. It makes no cross-account permission claim. The local model
and its limitations are in
[`fixed-rendezvous-local-prototype-2026-11-16.md`](../../docs/experiments/fixed-rendezvous-local-prototype-2026-11-16.md).

Unlike archived `OLD-020`, the reader polls an **existing generation ID**.
It does not query a different not-yet-created ID for each OPEN and accumulate
expected 404s. This also differs from Skirk's current prewarmed 64-slot
known-ID ring with read-ahead. The result cannot rank that ring.

## Safety and method

- Requires an explicit `-live` flag, fresh JSON output directly in
  `.skirk-runs`, and the local `personal-test/kit/{client,exit}.json` configs.
  The visible folder, OAuth client ID, refresh token, and quota project must
  match; a configured quota project is sent on every Drive call. Default route
  is `front`; `direct` must be explicitly selected. The front code uses the
  normal `GoogleHTTPClient` route: TLS SNI `www.google.com`, HTTP authority
  `www.googleapis.com`, and the kit's pinned Google IP.
- Each arm reserves and durably records **exact generated IDs and names
  before creation**. Its precreated generation and authenticated anchor are
  excluded from the timed cold OPEN. Each generation must first pass a v2
  capability check: bogus `If-Match` returns 412; valid tag succeeds and
  changes; stale tag returns 412; reset succeeds and changes the tag again.
  The probe stops on any mismatch. Google documents v2 ETags, but its REST
  method description does not promise permanent conditional-write semantics.
- For 1, 2, 4, then 8 writers, each fixed writer reads the known anchor and
  generation before a shared release gate. All start the first PATCH with
  the same ETag, then merge on 412 with at most eight attempts. The reader
  polls the **same known generation ID** every 100 ms. The immutable control
  uses parallel creates and 100 ms prefix list discovery. Order alternates
  across writer counts. Each arm has a 25-second timeout. The report records
  scheduled-to-observed p50/p95/max from before client preparation, preparation
  gate time, and post-gate p50/p95/max. A timestamp is the first reader
  response containing the claim, so the 100 ms poll interval bounds the
  visibility resolution. It also records 412 count, exact delivered count,
  calls, nominal/8x units, and request/response bytes.
- A separate lost-response check sends one PATCH, deliberately discards its
  response and error at the caller boundary, then resolves the exact claim
  by authenticated readback. This only proves the
  OPEN record is not duplicated in the observed snapshot. It does not prove
  exactly-once remote TCP effects or crash-safe exit deduplication.
- Requests use `RequestSingleAttempt`: no hidden request replay after an
  uncertain PATCH. The active hard guard is **80,000 eightfold units**
  (10,000 nominal), cleanup has a separate **16,000 eightfold units**
  (2,000 nominal), and a request timeout is 10 seconds. Every returned body
  is bounded to 64 KiB. Because the cheapest permitted call is 5 nominal
  units, the maximum possible request counts are 2,000 active and 400
  cleanup, or 2,400 total; the mixed run uses far fewer. The 8x margin is a
  conservative accounting allowance, not eight requests actually sent.
- All writer and reader goroutines join before cleanup. The janitor GETs
  every recorded ID, requires exact ID/title/owner/app/parent before DELETE,
  then requires two exact 404 reads. It checks the unique session prefix
  immediately and after 31 seconds. A recovery ledger remains on disk if
  exact or delayed cleanup cannot be proved. Do not rerun while that ledger
  remains; inspect it and complete cleanup first.

The clean path reserves 23 exact IDs. Cleanup then costs at most 1,895
nominal units (23 × [GET 5 + DELETE 50 + two verification GETs 10] + two
lists 200), or 15,160 eightfold units. Failed or uncertain requests can
consume the 840-unit reserve; an incomplete result leaves the recovery ledger
for manual inspection. The four-minute top-level deadline covers experiment
only; cleanup has its own two-minute deadline.

## Invocation

Run this only with the approved secondary account's kit, when no other live
Drive test is running:

```sh
go run ./tools/fixed_rendezvous_probe \
  -live \
  -client-config .skirk-runs/personal-test/kit/client.json \
  -exit-config .skirk-runs/personal-test/kit/exit.json \
  -out .skirk-runs/fixed-rendezvous-unique.json
```

Do not interpret local fake timings as Google timings. In the deterministic
fake, all 1/2/4/8 arms pass, including 412 contention and lost-response
readback. One local run used 23 ID reservations, 79 GET, 23 insert, 11 list,
and 55 PATCH calls: 5,510 nominal / 44,080 eightfold active units. That is
only a dry-run call-cost and safety check. A failed live capability check or
large CAS tail falsifies this specific hot-snapshot candidate even if the
known-ID ring remains viable.

References: [Drive v2 File resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files),
[v2 generateIds](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/generateIds),
[v2 files.patch](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/patch),
[Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits).
