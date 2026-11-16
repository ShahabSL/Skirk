# Fixed known-ID Drive rendezvous: local prototype

This isolated `_test.go` prototype is at
`internal/skirk/drive_rendezvous_local_test.go`. It is not wired into the
shipping transport or any platform client. It uses a metered in-memory Drive
model with the same mailbox principal and OAuth app for exit and clients. No
Google calls are made by these tests.

The existing setup copies the same OAuth credentials into the exit and client
profiles. That makes an exit-created file accessible to current-kit clients,
subject to the usual `drive.file` app authorization. A future cross-account
client needs both Drive writer permission and per-file app authorization, and
the current app-data mailbox must not be assumed shareable between accounts.

## Distinction from OLD-020

`OLD-020` generated a different known ID for every data/control object. Its
live primitive incurred 964 metadata polls, 916 of them 404, versus 48 list
calls, with multi-second visibility tails. This prototype polls one
**already-created** fixed rendezvous ID for sparse inbound OPEN claims. Its
steady-state empty polls therefore need no missing-object probes. That does
not prove lower real latency: metadata visibility, write contention, or the
fallback list may repeat the old loss. The candidate must beat the immutable
create/list control in a same-binary live test before promotion.

## Fail-closed write rule

Every generation requires a fresh owned-file capability probe before use:
read a strong quoted v2 ETag, reject a random impossible `If-Match`, accept
one exact conditional update, reject the old tag, and reread the exact new
body/tag. If any step is ignored, missing, ambiguous, or unsupported, the
rendezvous is not advertised and the existing immutable/list path remains
authoritative. Never retry a PATCH without its exact prepared body and fence.
After 412, reread, authenticate all surviving claims and receipts, and merge
the new OPEN before trying the new strong tag. After a lost response, exact
readback may prove the claim or an exit-signed receipt. If it cannot, publish
an immutable fallback with the **same** claim identity and rely on durable
exit deduplication. A successful PATCH is not an application processing ACK.

The local fake enforces conditional tags and counts 5 units for GET, 50 for
PATCH/create/delete and 100 for list. This is a conservative local meter, not
Google billing telemetry. At one poll/second, one fixed GET costs 432,000
units/day per exit versus 8.64 million for one list/second, before writes and
fallback. A five-second list reconciliation adds 1.728 million units/day and
allows a failed-CAS cold OPEN to wait up to five seconds for discovery. A
hot document serializes writers and cannot serve arbitrary fan-in.

## Planned validation

The focused local tests for 1/2/4/8 simultaneous OPENs, lost PATCH response,
old-generation race, immutable fallback, exact owned cleanup, forged receipt,
same-ID collision, and ignored-CAS behavior passed under Go's race detector.
An additional cross-generation duplicate was suppressed while a changed body
with the same identity was rejected in the same process.
Twenty repeated focused race runs also passed; full Go test/vet gates are
recorded in the linked research note. The prototype does
not yet persist exit deduplication through a crash, test a real Drive adapter,
or implement production rollover/cleanup. Only after those correctness gaps
close should a bounded fronted Google A/B compare it to immutable create/list
with exact hashes, scheduled cold OPEN p95/p99, 412/retry/fallback counts,
idle quota, and owned-file cleanup.

See the [research note](../../docs/experiments/fixed-rendezvous-local-prototype-2026-11-16.md)
for the protocol and source references.
