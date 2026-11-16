# Inbound fairness reserve — isolated test-only candidate

The product's 64 MiB primary and 64 MiB deferred receive budgets prevent
unbounded queued payloads. A deterministic adversarial test fills both with
eight blocked local sockets, then sends four short frames to a new stream.
On the unchanged product source at `6ee88eb`, the new stream resets on its
first frame: primary and deferred each hold exactly 64 MiB. This is a
capacity/fairness failure under artificial saturation, not a live Google
failure.

The isolated `codex/skirk-inbound-fairness-20261116` branch at `62d919f`
keeps 8 MiB and 256 frames *inside* the existing deferred hard
cap for sockets with no primary backlog, limited to 1 MiB and 32 frames per
eligible stream. The older stalled sockets may use 56 MiB and 7,936 frames
of deferred space. At the adversarial point the eighth stalled socket resets
when it reaches the normal limit; the new stream then delivers all four frames
in order. A separate test fills the primary budget and normal deferred budget
and checks that a four-frame new-stream burst enters the reserved tail in
order. The original hard cap remains 64 MiB/8,192 frames.

This is **not a product promotion candidate yet**. It sheds a legitimate
large stream earlier under pressure and provides only bounded admission: enough
new stalled streams can fill the reserve too. Selecting the largest occupant
as a victim is also unsafe without progress/age evidence, because a healthy
bulk transfer can briefly be the largest. A stronger remedy needs per-stream
receive backpressure before object processing or progress-based admission,
then mixed workload, recovery, cleanup and high-concurrency tests.

Baseline red: `go test ./internal/skirk -run
'^TestMuxDeferredReserveKeepsNewDrainingStreamAlive$' -count=1 -v` on unchanged
`6ee88eb` failed with primary/deferred `64/64 MiB` and the new stream reset.
The same test on that isolated candidate passed with `56/56 MiB` after the stalled
stream was shed. The complete focused inbound/deferred suite and race suite
passed; `go vet ./...` and `git diff --check` passed. No Drive object, OAuth
grant, live speed test or root product worktree was touched.
