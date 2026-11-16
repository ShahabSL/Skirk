# Interaction priority: stranded token repair

Status: product scheduler correctness fix validated locally; **no live Google
latency, throughput, or quota measurement** follows from this change.

The interaction classifier makes a one-shot decision for each payload. A
denied payload proceeds through the normal lane; there is no waiting task
that can later claim a refilled priority token. Previously, the global
token-bucket queue retained that stream for one second. With a new one-shot
stream arriving every 250 ms, every fresh token could remain behind an older
caller that would never return. The deterministic
`TestInteractionPriorityRefillServesOneShotArrivals` failed on the old code at
the first refilled token.

The revised queue still gives a recently active stream a chance to yield a
token to a recurrent contender. It permits a fresh contender to bypass a
one-shot front waiter, and permits bypass after two refill periods when a
recurrent waiter stops calling. The waiter list and index now hold at most 128
streams; excess waiters lose only their priority claim, while their payloads
already use the normal lane. Existing 200 ms per-stream pacing, six-second
16 KiB epoch budget, and global eight-token burst / 250 ms refill remain.

Local validation: the new one-shot, stopped recurrent, and bounded-index
tests pass with race detection. The existing ten-stream sharing and the
broader interaction/sparse-priority tests passed ten repeated race-enabled
runs. `go vet ./internal/skirk` and `git diff --check` passed. This does not
establish a changed user-visible text percentile; the full mixed-workload
comparison remains necessary before attributing a speed win.
