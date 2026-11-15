# Drive v3 metadata ETag write-fence probe

Status: the first bounded owned-file primitive stopped on a fixture projection
error and was cleaned; a corrected wildcard-projection probe is prepared.
This is not a transport or latency benchmark. The question is whether Drive
metadata PATCH provides a server-enforced description revision fence, which
could change the design of ambiguous slot-write recovery.

## Documented evidence and uncertainty

The official [Drive performance guide](https://developers.google.com/workspace/drive/api/guides/performance)
describes ETag changes after PATCH conditionally; it does not establish a v3
files.update If-Match contract. The current [files.update reference](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/update)
documents metadata PATCH without specifying that header's write-precondition
behavior. The official [Go client reference](https://pkg.go.dev/google.golang.org/api/drive/v3)
exposes cache-validation IfNoneMatch helpers and arbitrary request headers. That
does not prove write CAS. Absence of an etag JSON field likewise does not prove
the response header is absent or ignored. Context7's official Drive corpus was
queried and returned the same conditional performance guidance, not a stronger
v3 write guarantee. No prior repository ETag/If-Match experiment was found.

## Fixed scope

The opt-in test reads only a private copied approved secondary-account profile,
preserves its pinned `www.google.com` route and quota project, and acquires an
access token once through the existing token source. That token-source setup is
separate from the fixed maximum of 20 visible Drive dispatches. Account identity
is operator asserted, not independently proven by this probe.

One generated file ID is durably recorded before creating one fresh nonce-named
ordinary metadata file. Only that ID, its create, and one-ID generation are
allowed by the request guard. The synthetic descriptions are independent random
AES-GCM ciphertexts. There is no user message, existing-file modification,
sharing, content export, or personal-file listing.

The total bound is 20 visible Drive dispatches, 160 conservative HTTP/2 attempt
upper reservations, 8,000 quota-unit upper reservations, and one MiB of encoded
request-body upper reservations. Active work stops at 12 dispatches, preserving
eight for exact cleanup. The eight-attempt factor accounts conservatively for
opaque internal HTTP/2 retries; it is not an observed physical request count.
Individual response bodies are capped at one MiB. Headers/TLS/raw network bytes
are not included in the encoded request-body figure.

## Sequence and interpretation

1. Generate one ID, persist the recovery identity, create description A.
2. Read exact ID/name/description using the slot-sized projection and record
   the ETag response header. If absent, try one explicit `fields=*` metadata
   response; omitting `fields` selects Drive's default fields rather than all
   fields. Discard
   everything except the exact file identity, description and version.
3. If an ETag exists, issue a separate If-None-Match GET. A 304 means cache
   validation works; it says nothing by itself about write preconditions.
4. PATCH description D with a fresh impossible quoted If-Match. Record status
   and exact readback, including whether an ignored header changed the file.
5. With the current provider ETag, PATCH B; then PATCH C with that exact now-old
   value. Both GETs and PATCHes use the same projection when testing a fence.
6. A positive observation requires bogus rejection, successful valid write,
   stale rejection, and readback still exactly B. An ETag that validates reads
   but does not change with description writes is explicitly a negative result.
   Missing or unusable ETags are inconclusive about a generally supported write
   fence, rather than proof from JSON schema omission.
7. Verify exact ID and random name again before authorizing deletion (an
   already-absent ID needs no delete). Delete only that verified ID and require
   two immediate plus two delayed 31-second 404s. Cleanup has an independent
   120-second context and still runs
   after request/reporting failures. The borrowed profile is removed.

Redeletion validates its HTTP status as strictly as the first delete. An owned
file first seen late is best-effort deleted within the existing call cap, but a
fresh 31-second absence check is still required externally before that cleanup
can be called verified. Malformed generated aliases such as `root` are rejected
before creation or cleanup. Reports distinguish pre-dispatch reservations from
actual client dispatches; directory fsync makes the pre-create recovery identity
durable after its atomic rename.

Only controlled phases, HTTP status/protocol, raw bounded ETag values, version,
description hashes, guards, and cleanup evidence enter the private report. No
token, profile, description ciphertext, or arbitrary server error body is
logged. An unexpected auth/quota/server/transport failure aborts active probing
and preserves cleanup evidence. Negative CAS evidence is retained as a completed
primitive observation, not silently converted into support.

## Offline checks

Fake-server cases cover an honored write fence, ignored headers, read-cache ETag
without a description fence, absent ETag, full-metadata-only ETag, reserved
cleanup after active exhaustion, foreign path rejection, cancellation, reporting
failure, pre-create recovery identity, auth-status preservation and conditional
header/pinned-authority preservation through the actual Google HTTP client.
The first guarded live attempt returned no ETag for the explicit small
projection, then stopped because the original fallback omitted `fields` and
its default response omitted description. This is a fixture failure, not
negative write-CAS evidence. Root retained all ten actual calls and verified
the exact owned deletion plus two immediate and two delayed 404s; the borrowed
configuration was removed. A new regression models the omitted default fields,
requires the wildcard fallback and rejects unprojected owned-file paths.
Corrected live execution awaits independent source review and a serialized
root lease.

The original nine top-level offline tests (19 scenario variants) passed the focused
race run in 1.462 seconds; `go vet ./internal/skirk` passed. The conditional-header
test uses the actual Google HTTP client with a local RoundTripper and verifies
both request If-Match and response ETag survive while the selected front and
authority stay intact. No Google request was made by these tests.

After the projection correction, ten top-level tests (20 scenario variants)
passed the focused race run in 1.363 seconds; vet passed again. The extra case
independently checks Drive's omitted-field response shape and the guard's
rejection of a naked owned-file URL. This is offline fixture proof only.
