# Capacity observer teardown correction

The first five-client async attempt exposed a harness defect. A rejected
process-group SIGKILL escaped the first teardown statement, skipped six service
shutdowns and erased the original workload failure before its row reached the
parent summary. The exact manual cleanup and all final timeout counters remain
in the historical report; the missing original failure text is not invented.

The correction changes benchmark ownership and evidence, not the Skirk binary,
its routing, retry policy or product defaults:

- Profile writes and binary hashing run inside teardown protection, so partial
  setup cannot leave credential copies behind.
- A private immutable workload outcome, including the last guard snapshots, is
  flushed and synced before any teardown operation. A separate teardown outcome
  retains signal errors and cleanup results without replacing that failure.
- Native per-spawn identities and original Popen handles authorize individual
  PID signals. No process-group signal, process-name kill or sudo is used.
- Every parent is attempted even after an unexpected helper exception. Native
  curl identities are loaded only after benchmark exit, then verified separately.
  A pending spawn intent is an unresolved record, never authority to signal a
  guessed PID. A live benchmark's possibly locked journal is not read.
- Exact Drive cleanup starts only after every original Go writer has exited.
  Otherwise the row records zero cleanup calls started and the cohort stops.
- Runtime estimates survive blocked cleanup. A later verification failure keeps
  already known cleanup commands, deletions and minimum units. Missing or
  regressed final metrics use the last valid snapshot and mark telemetry failure.
- Curl error recovery bounds its final reap wait to three seconds and preserves
  the original failure when kill, reap or exit-journal cleanup also fails.
- Every profile removal is attempted, including after signal or cleanup errors.
  Each client now retains partial exact receipts and a completed-file checkpoint.

Native-helper validation is recorded in the companion ownership audit,
including a recorded sleeper terminated while an unrecorded sleeper stayed
alive. Integration and existing benchmark tests passed 93 cases in 5.015 s.
Subsequent focused runs covered the added live-journal-lock and failed-reap
cases, for 95 distinct integration/existing cases; the final affected 78-case
run passed in 4.999 s. These include real native child
journaling, EPERM continuation, all-writer attempts, live-writer cleanup blocking,
profile/binary setup failures, immutable original evidence, partial cleanup
timeouts and unchanged default curl execution. They make no Google calls.

Native identity checks narrow PID reuse races; macOS provides no atomic
signal-by-creation-time syscall. Linux parsing and mocked branches have tests,
but native Linux runtime behavior was not measured on this Mac. No new live
capacity result or broader timeout policy is claimed by this correction.
