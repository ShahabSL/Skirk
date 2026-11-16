# Bounded Drive v2 retry accounting: local guard repair

Status: **test-only harness change**, with no new live Google measurement or
performance claim. The previously interrupted FSH2/FSH1 campaigns H and I
remain failed/incomplete. Their fronted POST 502 statuses and campaign H's
four bulk-upload timeouts have not been reclassified as successful arms.
The separate generated-ID upload fix requires exact ID, name, size and
Drive-reported SHA-256 before accepting a 409 as an idempotent commit; this
guard observes that proof without changing the production retry policy.

## What the guard now admits

The old live guard canceled immediately on every raw HTTP status at or above
400, including a POST 502 that the Google HTTP client might retry and the
subsequent 409 for a file already committed. For `drive_v2` research arms
only, it now treats POST 502/503/504/409 as **provisional** when the request
is a generated-ID, multipart normal-lane mux upload through the pinned
Google front. It reconstructs the request's exact ID, name, media byte count
and SHA-256 from the existing bounded body. Retries for one ID must carry
the identical data. The exception allows at most eight such raw statuses
and eight distinct IDs per arm, without loosening the existing five-minute,
call, whole-arm unit, rolling-minute unit, request-byte, response-byte,
timeout, or separate cleanup reserve limits.

At the method-price level, eight provisional POST responses and three
five-unit proof reads for each of eight IDs would reserve at most 4,160
additional eight-attempt units. These calls still pass the tighter whole-arm
and rolling-minute admission checks; this is a ceiling for the exception,
not a quota grant or measured Google charge.

The only satisfying proof is a product-initiated, known-ID metadata GET for
`id,name,size,sha256Checksum` with all fields matching the pending upload.
Google's [Drive file resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
documents that SHA-256 is returned when available for files with content
stored in Drive; the guard fails closed if it remains absent.
The guard allows at most three such reads per ID, including a temporary 404
or absent checksum while Google propagates the object. It retains the raw
502/409/404/200 status counts, error count, upper unit reservations, and
sanitized recovery events separate from hard failures. It snapshots the raw
HTTP Content-Encoding before
the product client removes a gzip header and decompresses the response;
this avoids falsely rejecting a correct compressed checksum response. Any
wrong ID, name, size, SHA-256, changed retry body, excessive raw status,
unscoped API error, or unresolved provisional upload fails the arm. A 502
followed by an ordinary 200 upload without a checksum GET remains
unproven and fails conservatively.

The live-run harness checks outstanding proofs after the exact workload and
before writing `workload-outcome.json`. A complete file and all text echoes
are still required, as are joined writers, provider budget checks, and exact
owned cleanup. A recovered raw 502 is visible in the report rather than
silently removed; `workload_ok` can only be true after the correlated exact
proof. Other modes retain the strict prior status policy.

## Local evidence and next live plan

The fake pinned-front route commits a generated-ID bulk object, returns
502, receives an identical retry yielding 409, and returns an exact checksum
from the known ID. The full local SOCKS/TLS fixture completed exact 16 MiB
files in each direction and 40 overlapping text echoes, joined both
writers, verified processing receipts and fallback, cleaned its owned
objects, and retained an unrelated sentinel. The three visible upload/proof
requests reserved 840 conservative units (105 nominal); the guard retained
one raw 502 and one raw 409 while accepting one exact proof. Separate cases
prove the guard fails a never-committed 502 at workload end, allows a bounded
409→404→missing-checksum→exact-gzip-proof sequence, rejects a same-size
wrong checksum or a changed retry body, rejects an unrelated list 502, and
stops at the ninth raw generated-upload error. These are local fake-route
tests, not Google throughput or latency results.

A fresh live campaign should freeze one binary and compare FSH2/FSH1/FSH1/FSH2
with separate exact owned generations, the pinned `www.google.com` front,
window eight and the same 16 MiB down+up plus 41-echo mixed workload. Make
the Mac CPU quiet during every timed arm. Save the raw status and proof
counters even if the arm recovers. Keep the conservative 280,000-unit
rolling-minute guard and serial account use; stop on any unrecovered error,
budget denial, incomplete body/hash/text, writer join failure, or cleanup
failure. Run exact owned-prefix checks immediately and after 31 seconds,
remove borrowed test tokens, and keep failed arms out of medians. A complete
small ABBA still would not prove multi-client fairness, account/project
allocation at 200,000 users, or a global speed ceiling.
