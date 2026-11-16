# Drive v2 priority lane: product integration plan

Status: the conditional metadata carrier and cumulative queue remain research
code in `_test.go` files. This branch adds route parity for the research
carrier, but it does **not** enable the lane in CLI, desktop, Android, or iOS.
The ordinary Drive mux remains the product path. This document records the
work required to make an opt-in implementation safe and testable.

## Why direct promotion is unsafe

The live fixture creates two fresh v2 files, writes their exact IDs to an
ignored local journal, probes both write fences, and then constructs both
ends in one process. A distributed client and exit do not share that setup
process. The existing queue also stops after one five-minute epoch or 4,096
claims. It has no generation rollover, crash-durable cleanup, or authenticated
peer negotiation. The carrier and queue use test-only types and cannot be
selected through `Config` or `StoresFromConfig`. Moving files out of the test
build would not solve these gaps.

The measured v2 file improvement is a bounded same-binary result, while its
interactive advantage over the same-binary Drive control was small or
variable across two pairs. It does not justify changing the default. The
Drive v2 API exists for the observed conditional resource ETag behavior; Google
recommends v3 for new work where v2-only features are unnecessary.
[Google's v2/v3 guide](https://developers.google.com/workspace/drive/api/guides/v3versusv2)
and [v2 file resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files)
are the API contracts to recheck when implementing this path.

## Product protocol

1. Keep the normal mux and immutable Drive objects authoritative until both
   sides have explicitly agreed to the new generation. Use a distinct versioned
   capability magic in authenticated mux frames; old peers ignore it and keep
   working. Never infer support from an OAuth scope or app version alone.
2. The client reserves two IDs and creates both exact files in the configured
   mailbox, with unpredictable generation names. Persist IDs, names, parent,
   generation, creator, and cleanup state before the first create. Read back
   uncertain creates by exact ID; verify the configured folder or app-data
   space, untrashed state, MIME type, ownership/app authorization, and zero
   media bytes before advertising them. The exit repeats these checks before
   accepting the offer. A file outside the mailbox is never a queue target.
3. Probe conditional writes and the maximum selected description capacity on
   **both** writer accounts/routes before admitting application snapshots.
   Probes must finish while the documents are empty and no queue reader can
   treat a probe body as application state. A rejected or ambiguous probe
   leaves the ordinary immutable path active. Bind the two IDs, direction,
   generation, session/client/run identity, cap, and route-independent protocol
   version into the authenticated offer/accept/ready exchange.
4. An accepted generation is additive: the existing immutable path remains a
   fallback for oversized snapshots, uncertain writes, observer failure, and
   older peers. Prepared ciphertext and the original ETag remain identical
   across an uncertain conditional retry. Processing receipts, rather than
   file deletion or PATCH success, retire virtual and physical identities.
   The receiver must deduplicate a late immutable fallback against a queue
   delivery, including after reconnect.
5. Create the next generation before a finite epoch retires. Do not let two
   generations publish the same sequence. Freeze the old writer, drain or
   replay outstanding identities through immutable fallback, switch only
   after authenticated acceptance, and retain the old read window until its
   receipts settle. A restart must load the journal or recover exact owned
   files and continue without treating a new epoch as proof of old delivery.
6. Use the selected transport route on each peer: pinned
   `www.google.com` remains the default, while Direct is an explicit user
   choice. Do not silently change route after an error. Refresh OAuth through
   each peer's own token source; never copy an access token from one peer to
   the other or add a Cloud owner credential.

## Quota and cleanup ownership

The v2 carrier currently has a finite per-instance call/byte/unit bound. The
product needs a shared Drive admission controller across v2 GET/PATCH, normal
bulk/list/upload/download, provisioning, and cleanup. Reserve cleanup capacity
before admitting foreground polls; apply per-user and project budgets, idle
backoff, and fair scheduling across clients. The published quota units are
5 for metadata GET and 50 for metadata update, with project/user limits and a
daily free threshold. A one-second idle poll from two endpoints costs about
600 units/minute per pair, or 864,000 units/day if continuous, before writes or
bulk. This is arithmetic from [Google's Drive quota page](https://developers.google.com/workspace/drive/api/guides/limits),
not a measured user-capacity result. The eight-attempt research reservation is
a conservative local bound, not Google's billed count.

Close must first fence new operations, cancel and join writers/readers, then
delete only exact journaled IDs whose current metadata still proves the
expected name, owner, mailbox parent/space, and generation. A janitor must
recover after process crashes using the same validation. A failed or uncertain
DELETE stays in the journal for later exact-ID recheck; a missing owned file is
already clean. Quota denial must never disable cleanup or cause a broad folder
delete. Only the user-owned Skirk mailbox may be touched.

## Work sequence and promotion evidence

The minimum implementation sequence is: (1) extract protocol and carrier into
normal source with explicit production names and tests; (2) add a versioned
offer/accept plus old-peer tests; (3) implement durable owned-target lifecycle,
route choice, token source, shared quota and fallback; (4) implement rollover
and restart recovery; (5) expose an **off-by-default** setting in CLI and all
clients, with clear status and fallback reason. Keep the current default until
mixed and long-running evidence supports a change.

Before enabling the opt-in path, verify real SOCKS/TLS through separately
packaged client and exit binaries, 24-hour rollover, restart at every handshake
phase, lost POST/PATCH/DELETE responses, stale/ignored ETags, stale peer,
old-peer fallback, quota pressure, multiple concurrent clients, browsing and
bulk overlap, exact cleanup, both Direct and pinned-front routes, and native
Android/Windows/macOS/iOS runtime behavior. A simulator build is not a
physical iPhone packet-tunnel test. Report p50/p95/p99 and failure counts
without combining different workload definitions. No absolute speed or user
capacity ceiling follows from the existing short cohorts.
