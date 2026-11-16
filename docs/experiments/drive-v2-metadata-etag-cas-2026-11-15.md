# Drive v2 metadata ETag write-fence discriminator

Status: test-only bounded probe prepared and checked offline. No v2 live
result exists yet. The separate v3 fixture failure and corrected negative
result remain unchanged in the v3 companion report.

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

Root must independently review and serialize any live call. The prepared
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
