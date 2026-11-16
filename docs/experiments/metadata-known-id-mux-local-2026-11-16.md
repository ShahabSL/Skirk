# Metadata-only known-ID priority train: local architecture gate

Status: **isolated research branch; do not promote or run a full live A/B yet.**
The mux wrapper and fake Drive are test-only. The generic Drive v3 primitive is
implemented in `internal/skirk/drive_immutable_metadata.go`, but no product
flag enables this carrier. No Google account was accessed by this experiment.

## Mechanism

The sender requests Drive-generated IDs in batches of 128, below Google's
documented 1,000-ID request maximum. For each new sender generation it creates
four immutable, empty metadata boot files, one for each mux lane. Each boot's
`kidFirst` property is authenticated by `kidMAC` over the boot name and first
head ID. A tiny priority mux batch is then published as an empty file with its
AEAD-sealed bytes in `description`, using its already announced generated ID.
Its `kidPrev` and `kidNext` properties carry the bounded ID credit chain; the
HMAC binds the current ID, name, both credits, and SHA-256 of the sealed bytes.
The receiver validates boot and link HMACs before giving `InlineData` to the
ordinary mux, which verifies v5 AEAD and frame identity. These metadata files
have no media body; a 200-unit `alt=media` GET of a tiny priority record is an
explicit test failure. Normal bulk remains the current immutable media path.
Priority batches above 8 KiB also retain the current media path.

The wrapper is attachable to a store implementing the normal object methods
and the new immutable metadata ID interface. Its tests use a fake Drive that
rejects IDs not returned by `generateIds`, counts each method, and separates
metadata files from media files. The DriveStore primitive issues JSON
`files.create` with `id`, `description`, and public custom `properties`; reads exact ID
with `fields=id,name,description,properties,parents,spaces,mimeType,size,ownedByMe,isAppAuthorized`;
and proves an ambiguous create by exact-ID readback. It validates mailbox
parent, zero media size, description marker, and property bounds. No mutable
PATCH or arbitrary file ID is part of the design.

`appProperties` would hide the credits from a different OAuth app, so this
candidate uses public custom `properties` with HMAC validation. That only
solves property visibility. The `drive.file` scope can still deny a second
OAuth app access to the file (`appNotAuthorizedToFile`). Publisher/BYO OAuth
pairing is therefore an explicit live capability gate; this fake test does
not prove cross-app access.

## State and recovery

| Actor | State transition | Fault behavior |
| --- | --- | --- |
| Sender boot | Unbooted → generated four head/boot IDs → four committed boots | A pending boot pins its generated ID. Lost response or 409 gets exact-ID proof. A delayed unproved create cannot advance the lane. |
| Sender record | Head + one generated next credit → immutable create → proved commit → next head | An ambiguous create retries the same ID and exact sealed bytes. A committed retry returns its recorded ID without recreating a file already cleaned by the receiver. A different digest fails closed. |
| Receiver | Unknown generation → bootstrap list → exact boot GET → four known heads | Bootstrap scanning continues so a new client or generation can appear after earlier clients. HMAC rejects an altered first ID. |
| Receiver head | Exact metadata GET → link HMAC → mux AEAD → next head | A missing/deleted head triggers a bounded prefix-list fallback; a validated successor can restore progress. Reordered visibility and a late predecessor are both exposed to the mux. |
| Shutdown | Joined workers → delete exact owned boot IDs → exact session-prefix sweep in fixture | The wrapper clears its ID/state maps. A process crash still needs a durable cursor and owned-object cleanup protocol before product use. |

The wrapper caps sender and receiver lane entries at 256 each, issued-name and
fallback-name journals at 4,096 each, and reserves at most one 128-ID batch at
a time (at most 129 buffered when refilling). At the issued-name cap it fails
closed. Sender lane exhaustion uses the legacy list path. Receiver lane
exhaustion forces periodic fallback listing. These are bounded behaviors, but
they do not establish indefinite service or exactly-once destination effects
after a process crash. The fallback currently scans the fake prefix without
Drive pagination, so it must not be used as a production recovery path.

## Local gate and quota

`TestIDTrainResearchFullMuxSOCKSTLSAndMixedBulk` drove the real v5 mux and
SOCKS5 frontend through verified end-to-end TLS. It verified exact 1 MiB
download and upload bytes, one warm 64-byte reply, and eight concurrent text
streams in each direction while the bulk origin response was held. All 17
text replies matched; the origin observed one upload effect. Workers joined
and an exact session-prefix sweep found no remaining fake Drive objects.
Focused tests cover committed lost responses, a delayed data create, a delayed
boot create, a committed retry after deletion, reordered visibility, a deleted-head restart, dynamic client and
generation discovery, tampered ID credit, generated-ID enforcement, and
carrier-owned boot cleanup. Focused race repeats, Go package tests, and vet
are the local gates; no Google latency or throughput is measured.
The focused carrier/Drive tests passed three race-enabled repeats and
`go vet ./...`; the broad `go test ./... -timeout=420s` passed (core package
273 seconds). An earlier 180-second broad run hit its imposed timeout in an
unrelated v2 hybrid test. One carrier full-tunnel repeat returned an EOF on a
parallel text request while the broad suite was competing for resources;
five isolated repeats of that full-tunnel test then passed. This variability
still needs more evidence before any latency ranking.

One 50 ms-poll fake run counted 58 metadata creates, 244 metadata GETs, 24
lists, 30 media creates, 30 media GETs, 88 deletes, and four ID-generation
requests. Using Google's published weights, that is **18,440 estimated
units** for this local run: 2,900 + 1,220 + 2,400 + 1,500 + 6,000 + 4,400 +
20. Counts vary with local scheduling; some repeats emitted larger priority
batches through media. **Zero tiny priority batches used media.** No local method count is a
live latency result or a free-scale claim.

| One continuously idle exit policy | Head metadata GETs | Dynamic-client boot lists | Nominal discovery units/min | Modeled uniform-arrival cold-bootstrap p95 before RPC time |
| --- | ---: | ---: | ---: | ---: |
| Current v5 list once/s | 0 | 60 | 6,000 | 0.95 s |
| Four heads once/s, boot list once/s | 240 | 60 | **7,200** | 0.95 s |
| Four heads once/s, boot list once/2 s | 240 | 30 | 4,200 | 1.90 s |
| Four heads once/s, boot list once/5 s | 240 | 12 | 2,400 | 4.75 s |

The model uses `files.get` metadata = 5, `files.list` = 100,
`files.generateIds` = 5, and create/delete = 50 each. Each successful tiny
record additionally costs 50 create + 5 exact receiver GET + 50 cleanup,
plus about 5/128 per generated credit, before any fallback or retry. The four
boots cost four creates and four eventual deletes per generation. Dynamic
bootstrap at the current one-second cold-open cadence is therefore **20%
more expensive at idle than current v5** even before fallback. To save idle
units with four one-second heads, the boot interval must exceed 1.25 seconds;
that raises uniform-arrival cold-bootstrap p95 above the current model's
0.95 seconds. A single stable rendezvous would need a safe concurrent writer
protocol, and the earlier pinned-front conditional PATCH preflight failed;
this branch does not assume that capability.

**Stop condition:** no full live performance A/B or product promotion while
dynamic-client idle discovery is more expensive at equal cold-open cadence,
or while the slower-list option exceeds the predeclared cold-open p95 budget.
A durable generation/cursor journal, real paginated reconciliation, crash
cleanup, and cross-process duplicate-effect proof are also required. The
correct local conclusion is a working metadata-only primitive and a negative
dynamic-idle architecture gate, not a speed or scale win.

If a pinned-front primitive check is still useful after the other mailbox
experiments finish, run only on an isolated test-owned folder with `drive.file`
and a fresh quota snapshot. The proposed first step has one generated-ID
request of 16 IDs, at most seven metadata creates (four boots and three
records), 12 exact metadata reads, two prefix lists, seven exact deletes, and
seven post-delete 404 checks. The nominal plan is 5 + 350 + 60 + 200 + 350 +
35 = **1,000 units**. Install a transport-level attempt guard capped at
**5,000 units** before the first call, reserving the last 2,000 for owned-ID
cleanup; stop on any identity/HMAC/AEAD mismatch, unexpected media GET,
unsupported `drive.file` behavior, or cap hit. No full tunnel live arm should
follow this primitive check until the stop condition above is resolved.

[Drive ID generation and 409 retry](https://developers.google.com/workspace/drive/api/guides/create-file) ·
[Drive quota weights](https://developers.google.com/workspace/drive/api/guides/limits) ·
[Drive custom property limits](https://developers.google.com/workspace/drive/api/guides/properties) ·
[Drive app-specific file access error](https://developers.google.com/workspace/drive/api/guides/handle-errors#appNotAuthorizedToFile)
