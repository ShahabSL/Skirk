# Asynchronous slot upload ownership experiment

`drive.metadata_slot_async_v1` is off by default. With both slot flags enabled,
it changes local sender scheduling without changing the authenticated SKSD
wire capability. The lane hands an eligible prepared DATA/FIN batch to bounded
ownership tasks instead of waiting for its peer processing ACK. Each task
retains the exact mux name and sealed bytes. At most 64 tasks per mux and four
per stream are accepted; a full task or slot queue takes the ordinary immutable
path immediately. OPEN, RST, normal bulk and oversized records keep that path.

Memory enqueue is not an ACK. A successful or exactly read-back PATCH marks
Drive publication and completes the local upload ACK. A receiver processing
ACK is a separate stronger event and retires ownership. If that event has not
arrived after 1.5 seconds from handoff, the task uploads the identical prepared
immutable object with bounded retries. Existing digest and frame-sequence
checks suppress late duplicate copies. Cancellation fails pending upload ACKs,
closes affected streams and releases ownership. These are in-process guarantees;
the design is not a durable destination-delivery or crash-resumption protocol.

The slot writer drains up to the bounded queue size after its PATCH-rate wait
and before preparing the next cumulative snapshot. One PATCH may therefore
publish several queued records. Publication waiters are completed only through
the exact prepared snapshot's highest sequence, after success or exact ambiguous
readback. Delayed old requests remain serialized by the original writer rules.
The experiment adds no new Drive call type. A publication plus fallback can
count two actual writes in upload telemetry, although frame ACKs complete once.

The live A/B harness supports `--candidate-metadata-slot-async-v1`: both arms
enable slot delivery, while only candidate arms enable asynchronous handoff.
Use `--shared-interaction-epoch --small-traffic persistent --persistent-pipeline
--persistent-bulk-lead-ms 1000` to exercise a demoted long-lived text stream
after the quiet threshold, alongside the same 16 MiB file. Every arm must log
slot processing and sender ACK; candidate arms must additionally log handoff
and publication. Missing lane use is a failed arm, not a speed result. No live
result is claimed by this implementation document.

Deterministic tests cover delayed publication, delayed processing ACK with
exact immutable fallback, per-stream/global bounds, cancellation, ambiguous
PATCH committed and uncommitted outcomes, queued-record batching, and late
immutable deduplication. Existing slot ordering, ACK-loss, virtual/physical
claim and fallback tests remain applicable.
