# Drive v2 metadata ETag write-fence discriminator

Status: separate bounded v2 PATCH and PUT probes both observed the tested
write fence and completed exact cleanup. The separate v3 fixture failure and
corrected negative result remain unchanged in the v3 companion report.

## Official contract and remaining uncertainty

The official [v2 File resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files)
documents an output `etag` field and uses `title` for the file name. The
[v2-to-v3 comparison](https://developers.google.com/workspace/drive/api/guides/v2-to-v3-reference)
explicitly maps this File ETag to no v3 field. These facts motivate a separate
v2 measurement; they do not establish that a conditional write is enforced.

The official [v2 files.patch](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/patch)
uses metadata PATCH. [V2 files.update](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/update)
uses metadata PUT and says to populate only fields to change; it also supports
patch semantics. Both list `drive.file` as an accepted scope. Neither reference
explicitly promises `If-Match` behavior. The generic performance guide's
conditional ETag description is not treated as a stronger guarantee here.
Context7's official Drive corpus confirmed the method/schema differences and
did not supply an explicit v2 write-precondition contract.

[V2 generateIds](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/generateIds)
uses `maxResults=1`, rather than v3's `count=1`. V2 insert uses `title` and
parent-reference objects. Application data keeps the documented generation
space and translates the parent alias to v2's `appdata`. No new grant, API
enablement, sharing, billing or IAM change is part of this probe.

## Bounded experiment

The shared reviewed owned-file guard now has an explicit test-only v2 dialect.
V3 remains its default dialect, and the previous executable/results retain
their original byte and source hashes. There are no production Go changes or
dependency additions.

Each v2 run selects **exactly one** mutable method, PATCH or PUT, before
opening the private fixture. It gets one fresh generated ID and nonce name,
one independently randomized opaque A/B/C/D description set, and its own
pre-create durable recovery report. It cannot invoke the other mutable method,
address a v3 path, add query parameters, or operate on any other file. PATCH
results cannot stand in for PUT results, or the reverse.

The initial projection is `id,title,description,version,etag`; only if no
usable tag exists does the probe try explicit `fields=*`. Every conditional
GET and write then keeps that selected projection. Header ETag and resource
`etag` are retained separately. A resource-only tag is usable; disagreement
between two supplied tags or a malformed/unbounded tag stops the probe without
authorizing a write. This conservative rule may yield an inconclusive result
if provider projections legitimately use different ETags; it cannot invent a
common fence. A quoted weak tag can be observed but is not assumed to be a
strong write precondition.

The sequence is separate conditional-cache GET when a tag exists, impossible
quoted If-Match description D, exact owned readback, current-tag description B,
and the exact now-old tag with description C. A positive observation requires
bogus rejection, valid acceptance, stale rejection and B still present. A 304
read validator by itself is not write-CAS evidence. Missing/unusable tags,
ignored headers and description-insensitive cache tags remain distinct.

The maximum is **20 total visible client dispatches including cleanup**, with
12 active and eight cleanup reservations. Conservative bounds remain 160
HTTP/2 attempt upper, 8,000 quota-unit upper, one MiB encoded-request upper and
one MiB per-response cap. These upper reservations do not measure raw wire
traffic or prove which opaque HTTP/2 attempts actually ran. Token acquisition
is one separate setup call. Pinned `www.google.com`, approved secondary profile
and its quota project are preserved; account identity is operator asserted.

Cleanup first verifies the exact generated ID and nonce title, then deletes
only that owned file and requires two immediate plus two delayed 31-second
404s. Unexpected title means no deletion. The independent 120-second cleanup
context, finite redeletion policy, retained errors, fsynced private report and
borrowed-profile removal are shared with the reviewed v3 fixture. Reports
contain bounded conditional tags, hashes and counters, never credentials or
opaque ciphertext bodies. Raw IDs and nonce names stay in ignored private
artifacts.

## Offline evidence and opt-in

Four new v2 test functions exercise 14 method/tag matrix subtests, inconsistent
or malformed tags, a fixed read-only ETag, app-data parent references, selected
method/dialect/path fencing, foreign-title no-delete and an actual
GoogleHTTPClient PUT with preserved If-Match, selected front and authority.
Together with the ten existing guard/cleanup test functions, the focused race
final snapshot passed in 1.371 seconds; vet passed. These are local fake HTTP tests, not
Google evidence or transport speed measurements.

Root independently reviewed and serialized each live call. The
entrypoint is:

```sh
SKIRK_DRIVE_ETAG_V2_ACCOUNT=isolated-secondary \
SKIRK_DRIVE_ETAG_V2_METHOD=PATCH \
SKIRK_DRIVE_ETAG_V2_PRIVATE_DIR=<fresh-private-directory> \
<frozen-test-binary> -test.run '^TestLiveDriveV2MetadataETagCAS$' \
  -test.v -test.timeout=300s
```

The fresh 0700 directory must be below an ignored `.skirk-runs` directory and
contain only the approved copied 0600 `config.json` before launch. Select PUT
only as a separate fresh run and lease. No API token or Cloud owner credential
is exported to an application profile.

## Observed separate PATCH and PUT results

The two fresh owned-file experiments used the same frozen test executable,
with one explicit method and one separate private fixture per run. Both made
16 visible HTTP/2 client dispatches and completed this sequence:

| Phase | PATCH experiment | PUT experiment |
| --- | --- | --- |
| Generated-ID create and exact projected metadata read | 200, resource ETag present | 200, resource ETag present |
| Conditional cache GET with current resource tag | 304 | 304 |
| Impossible quoted If-Match writing D | 412, exact readback remained A | 412, exact readback remained A |
| Current resource tag writing B | 200, exact readback B | 200, exact readback B |
| Same now-old resource tag writing C | 412, exact readback still B | 412, exact readback still B |
| Exact identity check, owned deletion, absence proof | GET 200, DELETE 204, two immediate 404 and two delayed 31-second 404 | GET 200, DELETE 204, two immediate 404 and two delayed 31-second 404 |

The projected resource `etag` supplied the write fence. The metadata GETs did
not expose an HTTP ETag header; the resource field and the HTTP response header
are not interchangeable assumptions. Both reports have
`WriteCASObserved=true`, `BogusRejected=true`, `Cache304=true`,
`ValidAccepted=true`, `StaleRejected=true` and `StalePreserved=true`. Every
listed phase was actually executed. Both have an empty controlled failure,
verified cleanup and removed borrowed configuration, with zero report-write
errors. No raw tag, ID, nonce name, token, ciphertext or full report is tracked.

Each run retained 16 reservations, 128 conservative HTTP/2 attempt upper,
2,440 quota-unit upper and 8,784 encoded-request-byte upper reservations. Token
acquisition was one separate setup call each. These upper figures are not
physical HTTP-attempt counts, actual billed quota or raw network bytes. This
was a capability/correctness primitive, not a latency or throughput benchmark.

Both results establish observed server-enforced write fencing for these
specific v2 metadata operations and fresh owned files on the pinned
`www.google.com` route. They do not yet establish rapid-revision ETag uniqueness,
lost-response recovery under concurrent completion, an indefinitely safe slot,
cross-account behavior, or full-transport performance. A separate rapid-revision
discriminator and deterministic transport fault tests remain required.

The official [v2/v3 guide](https://developers.google.com/workspace/drive/api/guides/v3versusv2)
continues to describe v3 as the latest version and recommends it unless a
needed v2 collection warrants v2. No explicit shutdown was found in the
consulted reference, but continued v2 behavior is a support risk, not a
promise. Any research transport must feature-detect the actual fence, fail
closed on its loss, and retain immutable Drive fallback. This primitive does
not authorize silently assuming v2 CAS forever or changing the product default.

## Live provenance

Clean source: `28d10bde8621772fb002f02a18eb1f84e0bdf089`.
Frozen test executable:
`.skirk-runs/drive-etag-probe-20261115/drive-etag-v2-probe.test`.
Its SHA-256 is
`21682216b2c72d3c4095a517d851bf96f6bfe1a7e2d8ff3f77cc8d62feaaf678`.
This Go1.26.4 darwin/arm64 test executable has no embedded linked-worktree VCS
stamp; retained clean source and binary hashes provide its provenance.

| Separate selected method | Ignored private report suffix | Private report SHA-256 |
| --- | --- | --- |
| PATCH | `.skirk-runs/drive-etag-v2-patch-live-20261116-0010/etag-probe-result.json` | `a0d55f2e0ea84f6d6f1e103588e3bf3ef9eec52ddef55ba924fd118e9f7c195a` |
| PUT | `.skirk-runs/drive-etag-v2-put-live-20261116-0012/etag-probe-result.json` | `a30ddbcdc0fbb28c9f3b96d583698772df4de6538413d26776758d11b5f83f22` |

## Separate rapid-revision discriminator

A separately gated PATCH-only variant is prepared to challenge ETag reuse
across close successive updates. One fresh owned file is created/read, then
three distinct opaque B/C/D values are written without timestamp sleeps.
Immediately after each accepted write, an A write with that revision's old
tag must return 412, and an exact readback must still contain the new value
and its new tag. This variant requires a strong v2 resource `etag`; a header
alone cannot qualify. All four observed resource tags must be globally distinct,
including nonadjacent revisions. There are no competing writers.

This needs 12 active calls plus the same six-call exact cleanup, for 18
visible calls total within the unchanged 20-call guard. Client start/finish
times and total revision-phase elapsed milliseconds are recorded to describe
the actual spacing; they are not server timestamps or transport latency
benchmarks. A positive result covers only that observed cadence, not arbitrary
simultaneous revision races or every possible clock granularity.

Two additional test functions cover the successful three-revision sequence,
constant tags, a nonadjacent tag alias, ignored If-Match, missing/header-only tags,
wrong-version/method rejection and exact bounded cleanup. The complete focused
16-function local race run passed in 1.438 seconds; vet passed. No rapid live result is
recorded at this source checkpoint.

The distinct opt-in is `SKIRK_DRIVE_ETAG_V2_RAPID_ACCOUNT=isolated-secondary`
and `SKIRK_DRIVE_ETAG_V2_RAPID_PRIVATE_DIR=<fresh-private-directory>`, running
`-test.run '^TestLiveDriveV2MetadataETagRapidCAS$'` on a new frozen test binary.
It cannot select PUT or reuse either earlier private directory.
