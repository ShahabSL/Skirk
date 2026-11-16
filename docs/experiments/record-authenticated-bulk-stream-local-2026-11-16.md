# Authenticated bulk-record streaming: isolated local prototype

This is a **test-only codec** on `codex/record-streaming-bulk-20261116`.
The product remains muxv5 with whole-object AES-GCM. There has been no live
Drive run, full mux/SOCKS run, or platform rollout of this candidate.

## Hypothesis and exact scope

The current `GoogleHTTPClient.requestOnce` reads a full `alt=media` body before
`processMuxObject` opens the one AES-GCM envelope and decodes any frames.
Authenticating smaller records in one *already committed* immutable object
could reduce bulk first-byte delay and peak receive buffer without adding a
Drive request. It cannot make a not-yet-published upload visible, shorten
`files.list` discovery, or directly reduce small text latency.

The prototype writes a 66-byte header with a fresh 128-bit salt, bounded
declared plaintext length (at most 4 MiB), record size (16–256 KiB), and a
SHA-256 digest of the exact object name and Drive ID. A domain-separated
AES-256-GCM key is derived from the mux lane key, salt and object identity.
Each record binds the header, monotonically increasing index, exact length,
and data/terminal type as authenticated additional data. A separate zero-byte
terminal tag authenticates completion. The receiver delivers **only a fully
verified record**; it does not mark the object processed or delete it until
the terminal tag and EOF pass. It retains the header digest and each delivered
record's tag for a full-GET retry, authenticates the replayed prefix, and skips
it so the callback is not invoked twice.

The callback itself must be atomic or idempotent for `(file ID, record index)`.
An error after the callback has written some bytes remains uncertain; this
codec alone cannot provide durable exactly-once delivery. A changed file ID,
name, ciphertext/header on retry, corrupt record, bad terminal, truncation,
or trailing bytes fails closed. All reads are capped before allocation.

## Local evidence

`TestBulkRecordFakeDriveEarlyVerifiedDeliveryAndCleanup` used an HTTP fake
Drive `files.get?alt=media` response. The server flushed the first 64 KiB
record and withheld the remaining 960 KiB. The receiver emitted the verified
first record **before the server released the rest**, then verified all bytes.
The test manually removed only that exact fake object after authenticated
completion; production cleanup is not connected to this codec.
Separate tests covered corrupt second record, truncated retry, full-GET
resume without duplicate delivery, terminal tampering, identity mismatch,
same-ID rewrite, trailing bytes and 16 concurrent independent objects.

In a synthetic local 4 MiB HTTP response paced at 64 KiB every 2 ms, three
paired runs produced the following median times:

| Local path | First authenticated byte | Complete 4 MiB |
| --- | ---: | ---: |
| Existing whole AES-GCM envelope | 166 ms | 166 ms |
| Record-authenticated prototype | 3 ms | 166 ms |

This is a **staged loopback demonstration**, not a pinned Google.com-front
measurement. The server pacing creates the head-of-line delay by design; it
does not show that Google Drive emits a 4 MiB media response progressively.
It also does not show an application throughput improvement. One `alt=media`
GET still costs [200 Drive quota units](https://developers.google.com/workspace/drive/api/guides/limits);
a full-GET retry adds another 200. Google documents
[`Range` partial downloads](https://developers.google.com/workspace/drive/api/guides/manage-downloads),
but a safe range retry would require exact authenticated record boundaries,
`Content-Range`/total validation, and immutable identity proof; it is not
implemented here.

## Product integration gates

1. Use a new negotiated mux wire format and explicit object marker; v4/v5
   readers must never treat record ciphertext as a whole envelope. Fallback
   to the existing format when a generated file ID is unavailable, because
   this prototype binds the ID before upload. Drive
   [supports pre-generated IDs for blob files](https://developers.google.com/workspace/drive/api/guides/create-file),
   but current Skirk can fall back when reservation fails.
2. Constrain normal mux DATA frame payloads to the record boundary, or add a
   bounded streaming batch parser. The current frame can be larger than one
   record and `processMuxObject` only handles decoded full batches.
3. Define restart-safe partial processing receipts and stream semantics.
   Early bytes can reach a TCP destination before a later record fails; an
   origin may already have side effects. Current per-stream sequence dedupe
   does not constitute a durable application acknowledgement across restart.
4. Implement streaming HTTP through the existing route, OAuth refresh,
   backoff, deadline, quota and download-slot coordinator. Keep the whole
   object maximum and account for one in-flight record per worker. Verify
   cleanup after terminal success and exact partial failure.
5. Run matched live pinned-front bulk GET and POST, with server-side
   incremental response timing, p50/p95 first-byte and total completion,
   exact hashes, API errors, quota units, interruption/restart, and no worse
   small-text tail. Test iOS, Android and desktop after a protocol rollout.

Given these unresolved gates, the candidate is **not** a product speed claim
or replacement for muxv5. It is a proof that the cryptographic part of
incremental authenticated delivery is possible under a streaming HTTP body.
