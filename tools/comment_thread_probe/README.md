# Known-comment reply carrier probe

This is an isolated, test-only Drive API primitive. It does not alter Skirk's
default transport, profile, route selection, or product binary. No live Google
calls were made while authoring or validating it.

## Why this shape is distinct

The existing gap review prices a comment/reply log as `comments.create` (50
units) plus `comments.list` (100 units) to discover each message. A precreated
comment has a known `fileId` and `commentId`. Google's [Comment resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/comments)
documents `replies[]` as the **full list of replies in chronological order**.
Thus one [known-ID `comments.get`](https://developers.google.com/workspace/drive/api/reference/rest/v3/comments/get)
could read appended replies for 5 units without a list. Appending with
[`replies.create`](https://developers.google.com/workspace/drive/api/reference/rest/v3/replies/create)
costs 50 units, the same nominal write cost as a v2 file PATCH. These costs
come from Google's current [Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits). The API
documentation does not establish this path's latency, sustained reply count,
response bound, cross-account permissions, or front compatibility.

Unlike a cumulative mutable snapshot, appending a reply cannot overwrite an
earlier one. Each encrypted record carries a session and sequence; identical
replies with different provider IDs count as one logical record. This may
remove the v2 ETag write fence. Append-only records also permit selective
processing receipts, but do not themselves remove a cumulative ACK's
head-of-line constraint. A real tunnel still needs receipts, finite flow
control, and rotation.

## Bounded live procedure encoded by the tool

The live gate requires an explicit `-live`, fresh output directly under
`.skirk-runs`, and the existing isolated `personal-test/kit/client.json` and
`exit.json`. It uses the pinned `www.google.com` front by default;
`-route direct` is an explicit diagnostic choice. Both accounts must use the same
visible shared Drive folder. The client account owns the file and comments;
the exit account reads the comment. This probes one direction only.

The tool reserves a generated file ID and durably records it **before** file
creation. It creates one exact-ID file and one comment, then reads that
comment from the peer account. Four phases append eight independently sealed
64-byte records each with 1, 2, 4, then 8 concurrent create requests. A
100 ms peer `comments.get` loop records POST-start-to-verified-visibility,
POST duration, and response-to-visibility for every sequence. The same
thread grows across phases, so the four phases are a feasibility and load
shape check, not an unbiased concurrency ranking.

A successful create response is deliberately discarded at the protocol
boundary. The reader must find that exact authenticated application ID in
the known comment. A second identical create is then deliberate: two raw
reply IDs must produce one logical delivery. This tests a *committed*
lost-response path; a true transport error before commitment and bounded
retry remain for a later fault test. The tool then attempts sealed 2, 8 and
16 KiB records, recording 400/413 as size rejections and requiring exact
readback for accepted sizes. Finally it creates a second comment, announces
its ID through an authenticated reply on the old comment, and verifies the
first record on the new comment. A production rotation would also require a
reciprocal processing ACK and crash recovery before retiring the old thread.

All Drive calls use a single-attempt client. The local guard reserves **eight
attempts per visible dispatch**: at most 70,000 upper units for the active
probe and a separate 10,000 upper-unit cleanup reserve. It also limits a
single response body to 512 KiB and every active request to 10 seconds. These
are conservative local reservations, not provider-billed units or proof that
lower HTTP layers never retransmit. The report includes nominal method units,
upper reservations, response bytes, request bytes, call classes, each arm's
samples, the size outcomes, rotation, and cleanup. It includes no token,
payload, file ID, comment ID, or file name.

After all active writers join, cleanup validates the exact owned ID, name,
parent and ownership before deleting the file. It checks that file ID twice
returns 404, both known comment IDs return 404, and an exact-name prefix is
empty immediately and after 31 seconds. A private recovery ledger remains if
any cleanup proof fails. A forged or unrelated file is never deleted merely
because it appeared in a prefix query.

Example, **for a separately authorized live campaign only**:

```sh
go run ./tools/comment_thread_probe \
  -live \
  -client-config .skirk-runs/personal-test/kit/client.json \
  -exit-config .skirk-runs/personal-test/kit/exit.json \
  -out .skirk-runs/comment-thread-probe-001.json
```

Run the local fake first:

```sh
go test -race ./tools/comment_thread_probe
```

## Falsification and limits

The idea fails as a lower-cost known-ID append log if `comments.get` omits
replies or silently truncates them, if the peer cannot read the comment with
its existing `drive.file` grant, or if the selected pinned route rejects any
method. It fails as a low-latency alternative if reply visibility or POST
tails are slower than the same-cohort v2 anchor. It fails as a bounded
transport if the response grows too quickly, size is too small for normal
mux claims, replies are deleted or reordered without recoverable sequence,
rotation loses the new ID, or a lost response cannot be reconciled without
duplicate logical delivery. Any 401/403/429/5xx, timed-out read, malformed
AEAD record, unexpected status, guard denial, or incomplete exact cleanup is
a failed arm, not a successful sample. User-visible comment activity and any
notification side effects must be checked before product use. Drive's
[appDataFolder cannot share files](https://developers.google.com/workspace/drive/api/guides/appdata), so this
requires the visible shared-folder layout.

If the primitive passes, the next discriminating full-proxy comparison is a
frozen same-binary ABBA against v2 window eight: one persistent demoted TLS
text stream, simultaneous exact 16 MiB upload/download, 41 echoes per arm,
two accounts, fixed active poll and quota guard, joined writers, and exact
cleanup. Record text p95/p99, file rates, reply visibility, response-byte
growth, physical fallback, errors, and whole-arm/minute upper units. A
single primitive or a small ABBA cannot establish a 200,000-user default.

At 200,000 simultaneously polling pairs, **two** 5-unit `comments.get`
requests every 30 seconds consume 4 million units/minute and 5.76 billion
units/day on one shared project before replies, files, retries or cleanup.
The currently published limits are 1 million units/minute/project and a
400 million-unit/day free billing threshold. This method cannot solve that
idle polling budget. Any deployable 200,000-user design needs measured duty
cycles and a real project/account allocation, or a supported wakeup that does
not charge every idle client a steady Drive poll.
