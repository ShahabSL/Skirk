# Fixed known-ID exit rendezvous: isolated local prototype

Status: test-only local prototype in
`internal/skirk/drive_rendezvous_local_test.go`. It changes no product code,
configuration, default, Google account, or client profile. The local fake ran;
**no Google call or real latency measurement** was made for this candidate.

## Why this is different from the rejected known-ID design

The archived `OLD-020` experiment generated a different candidate ID for
each data/control object. Its primitive made 964 metadata checks, of which 916
were 404, versus 48 list calls, and had multi-second visibility tails. It
rightly rejected that **per-object precreation polling** path. A persistent
exit rendezvous instead creates one known file *before* the client connects.
The quiet exit rereads that same ID; there are no routine 404 probes for
future object IDs. Many OPEN claims can be appended to one bounded snapshot.
This difference is structural, not evidence of a faster real tunnel. Exact-ID
visibility tails, conditional-write contention, and rollover can still erase
the theoretical gain. [Older result](../experiment-ledger-2026.md).

The present setup places the same mailbox account's OAuth credentials in the
exit and client configs. In that **current same-account kit**, a client can
attempt to edit an app-owned rendezvous file with its existing `drive.file`
grant. A different-account design would need Drive writer permission **and**
the app's per-file authorization; an `appDataFolder` is not a cross-account
mailbox. The v2 file resource exposes ETags and earlier owned-file probes
observed `If-Match`/412 fencing, but this specific multi-client use needs a
fresh feature probe on every owned generation. Google's public v2 method page
does not give Skirk a permanent latency or CAS promise.
[Drive scope](https://developers.google.com/workspace/drive/api/guides/api-specific-auth),
[sharing](https://developers.google.com/workspace/drive/api/guides/manage-sharing),
[v2 file resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files),
[v2/v3 guidance](https://developers.google.com/workspace/drive/api/guides/v3versusv2).

## Local model and correctness questions

An invite names a stable anchor ID and pins a separate exit public key. The
exit signs the current generation ID in that anchor. The local fake requires
the same mailbox principal and OAuth app identity for reads and writes. Each
OPEN has a distinct client/run/claim identity and an HMAC of its opaque body.
Clients read the anchor and current 16 KiB snapshot, merge their claim, then
conditionally PATCH using the exact strong tag read. A stale tag returns 412;
the client rereads and merges rather than overwriting another client's claim.
An injected lost response after commit is resolved by exact readback, including
an exit-signed processing receipt. The exit merges signed receipts using the
same conditional-write rule. A bounded retry or full snapshot uses an
immutable identity-preserving fallback that the exit discovers by list.

Rollover seals the old generation with a fenced PATCH before a signed anchor
switch. A client paused just before its old-generation PATCH must see 412,
refetch the anchor, and publish in the new generation. The old file remains
available for draining. Cleanup checks exact ID, name, owner, app, and parent
before deletion; an unrelated sentinel remains.

This is a protocol model, not a durable product. It does **not** currently
persist an exit's deduplication record across a process crash, prove remote
TCP side-effect exactly-once, implement a real Drive HTTP adapter, or measure
the actual Google.com front. The shared client profile already grants every
importing device the same Drive principal; an exit signature lets clients
reject a forged anchor/receipt but cannot prevent a malicious holder of that
refresh token from deleting files or causing denial of service. Rollover needs
a persisted multi-generation janitor and a bounded immutable-list reconciliation
schedule before production.

## Cost hypothesis and decisive comparison

At one quiet poll per second, a five-unit known-ID metadata GET would consume
`5 × 86,400 = 432,000` Drive units/day per exit, versus
`100 × 86,400 = 8,640,000` units/day for one `files.list` per second: a
20-fold **call-cost hypothesis** before writes, retries, fallback sweeps and
cleanup. If the exit must list every five seconds to discover a fallback,
that adds `1,728,000` units/day and reduces the theoretical saving to 4-fold.
At 30-second fallback sweeps, it adds `288,000` units/day but makes a failed
CAS OPEN wait up to 30 seconds for list discovery. Neither is automatically a
good user experience. [Current Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits).

One hot document serializes client PATCHes, and its 16 KiB capacity holds only
a bounded number of unacknowledged OPENs. For high fan-in, parallel immutable
creates plus a list page may beat it. The next live test, after local validation,
should compare the same-account fixed queue with immutable create/list at
1/2/4/8 concurrent cold OPENs through the selected pinned front. Record
scheduled-to-ready p50/p95/max, exact success/duplicate counts, CAS 412s,
GET/PATCH/list units, full/overflow rate, delayed visibility, restart and
rollover, exact ownership cleanup, and no unbounded fallback tail. No claim
about 200,000 simultaneous users follows from a small local or live test.

## Local validation and metered contention

The focused suite passed with `-race`, including twenty repeated runs. It
forced all clients in each 1/2/4/8 case to read the **same** initial ETag
before any first PATCH, making conditional conflicts unavoidable. Every
unique OPEN was consumed once, signed receipts replaced pending claims, and
the exact anchor/generation files were deleted while an unrelated sentinel
survived. Separate cases passed a lost-after-commit PATCH readback, a
client/exit rollover race, immutable-list fallback, wrong-principal denial,
forged-receipt rejection, same-ID/different-body rejection, and a deliberately
ignored `If-Match` failing the capability probe. A repeated OPEN across two
generations had one local exit effect; a different body with the same identity
was rejected. This deduplication map is volatile, so the test does not prove
crash-safe delivery.

The following is one local scheduling observation, **not** a Google cost or
latency comparison. The fixed total includes two initial files, four probe
PATCHes, two exact deletes and their verification reads. The immutable control
creates one file per OPEN, lists once, then exact-deletes and verifies each.
The fake weights GET at 5, list at 100 and PATCH/create/delete at 50 units.

| Simultaneous OPENs | Fixed total fake units | Fixed conditional 412s, including two probe checks | Fixed units less 430-unit setup/probe/cleanup | Immutable-list fake units |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 550 | 2 | 120 | 205 |
| 2 | 670 | 3 | 240 | 310 |
| 4 | 1,090 | 8 | 660 | 520 |
| 8 | 2,290 | 24 | 1,860 | 940 |

The eight-client row varied with Go scheduling (an earlier passing run used
2,470 total fixed units and 27 conditional 412s). Even in this tiny fake,
the forced concurrent burst crosses from a fixed-file variable-cost advantage
at one or two OPENs to a disadvantage at four or eight. The fixed file can
still save idle discovery units between OPENs; whether those savings outweigh
real CAS contention and cold-OPEN tails needs the stated live A/B.

Validation: the full `go test ./...` suite passed twice after the prototype
was added (the long `internal/skirk` package took 269.341 and 264.685 seconds).
The final cross-generation identity case was then covered by a focused
race-enabled run and twenty repeated focused race runs; final `go vet ./...`,
all-package focused compile/run, and staged `git diff --check` passed. No
native app or real Google transport was changed or tested by this branch.
