# Mux seen and closed state at capacity

Status: isolated correctness fix on `codex/mux-bounded-state-20261116`; no
Google calls, performance result, carrier change or main-branch edit.

## Reproduced failure

At 200,001 seen object names, `markSeenLocked` replaced the entire `seen`
set and nearly all of `processed`. Two recently processed objects disappeared
after one unrelated new name. At more than 100,000 closed stream keys,
`rememberClosedStreamLocked` discarded every unexpired two-minute tombstone,
so an old OPEN could be claimed again. `dropStaleMuxObject` also closed an
already established stream when an exact Drive GET returned 404 for a name
that had already been processed and cleaned up. The new deterministic tests
failed on the parent commit before the fix.

## Bounded behavior

`seen` now keeps the latest 200,000 names in insertion order. Crossing the
cap removes only the oldest name and its related `processed` and physical-ID
bookkeeping; queued claims remain separate. The order queue periodically
compacts its own storage. This preserves recently processed deduplication
through the threshold instead of resetting it all at once.

`closed` now removes expired tombstones when its threshold is reached. It
never exceeds 100,000 entries. If all entries are still live, it evicts at
the cap and sets a global OPEN fence until each evicted key's original
tombstone would have expired. During that fence, the exit refuses **all new
OPENs** and treats unknown keys as closed for pending-frame admission; active
stream keys continue to receive data. If extreme churn keeps causing live
evictions, the fence extends and fresh connections can be unavailable. This
is the explicit availability cost of a hard memory cap without silently
reopening evicted stream IDs. Normal operation does not enter this path.

A Drive 404 now leaves an established stream alone only when the exact mux
object name already has a successful `processed` marker. A missing unique or
unprocessed object still terminal-closes its stream, rather than leaving an
unrecoverable sequence gap hanging indefinitely. This does not implement
alternate-physical-ID recovery; it would require finite tried-ID tracking
and integrity proof before changing failure behavior.

## Residual limits

The 200,000-name cache remains a finite deduplication window. An extremely
old physical object rediscovered after eviction is not covered by its former
`processed` marker. Existing stream sequence checks suppress duplicate
DATA/FIN during a still-active stream, and the closed-key fence protects
recently closed stream IDs, but this patch does not claim indefinite replay
protection if physical cleanup fails beyond the retention window. A 404 for
such an evicted, formerly processed name can still take the conservative
terminal path. A larger protocol-level replay watermark or durable receipt
would be a separate, versioned design.

The tests prove local map/fence invariants and the two distinct 404 cases.
They do not measure Drive visibility, speed, or capacity. After the live
benchmark ended, the merged root branch passed focused race tests and full
`SKIRK_FULL_PREFLIGHT=1 make preflight` with the installed Android NDK
selected through `SKIRK_ANDROID_NDK_VERSION=27.1.12297006`.
