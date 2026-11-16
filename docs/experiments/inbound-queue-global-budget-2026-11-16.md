# Aggregate inbound-channel bound — November 16, 2026

The first receive-worker isolation fix bounded each stream's *deferred* tail
and then the mux-wide deferred total. It left the primary `inbound` channel at
64 entries **per stream** without an aggregate byte cap. A large population of
stalled local readers could therefore retain many queued payloads even when
the deferred tail remained bounded. This is a memory-safety/fairness repair,
not a measured network speedup.

The product receive path now reserves a mux-wide **8,192 frames or 64 MiB**
before enqueueing into primary inbound channels. When that reservation is
full, it can use the separately bounded, ordered deferred tail; if both
budgets are full, only the overflowing stream is closed and a best-effort RST
is sent. Consuming a primary frame releases its reservation. Closing a stream
drains its channel and releases any remaining channel and deferred budget.
The blocked-reader versus close race drops a frame after close rather than
delivering it to a closed socket. Direct test preloads were changed to use the
same budgeted enqueue path.

An independent source review found no lock inversion or FIFO violation in
this change. Focused race-enabled inbound, deferred, receive-worker,
normal-pause and writer-RST tests passed ten repetitions; `go vet ./...` and
`git diff --check` passed. Exact-source full preflight and live mixed results
are reported separately. No Google call or OAuth grant was used by these
local tests.

This caps **queued channel payloads**, not one payload already dequeued and
being written by each active local socket writer. Other mux receive queues
have their own limits; a claim of fixed total process memory or 200,000-user
capacity would require a broader admission and concurrency study.
