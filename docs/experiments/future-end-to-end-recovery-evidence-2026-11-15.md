# Evidence for a future end-to-end recovery observer

The current complete candidate proves bounded delivery after one metadata
deadline and finite slot retirement. The final candidate was stopped at a
mutable PATCH deadline before its recovery outcome was observed. A future mode
must be separately named, bounded before calls, and retain both outcomes.

Required evidence includes:

- Exact executable/source and configured carrier flags, binding any admitted
  failure to a known operation. Bare context deadlines provide no URI, dispatch,
  connection or backend-stage proof. Preserve earlier attempt/status information
  when available; a final timeout must not hide auth/quota or recovered HTTP errors.
- Separate memory enqueue, successful durable publication, authenticated exact
  generation/revision readback, receiver processing and sender completion. A
  timed-out PATCH can be committed, uncommitted or still unknown. Do not label
  any of those states from an enqueue log or an absent response alone.
- Immutable fallback identity from the same prepared name and AEAD bytes,
  processing ACK/deduplication evidence, and the actual caller outcome after the
  failed RPC. Slot-link retirement is different from terminal stream failure.
- The entire original offered workload: every exact frame/index and file/hash,
  receipt checkpoints before teardown, schedule-based latency, time without
  progress, and post-retirement/lifetime strata. A partial distribution is not a
  complete p95 and a successful text subset does not prove a file succeeded.
- Real per-role/per-operation failure counts, prior finite event/time/byte/quota
  limits shared across classes, and no mid-run expansion or deletion of failed
  arms. Completion with errors remains explicitly different from a clean arm.
- Verified ownership and termination of every Google writer before exact
  immediate/delayed cleanup, durable original failure plus separate teardown
  failures, and retained partial cleanup counters. Reuse the tested native
  ownership approach instead of blind process-group signals.

Auth/quota/backoff, terminal stream and integrity failures must still stop.
Unclassified errors or unresolved state stay visible; they must not be silently
recast as healthy. A future observer observes the transport's existing recovery;
it does not add hidden retries or change the protocol. No such mode or live result
is implemented by this evidence inventory.
