# Fixed metadata slot delivery experiment

This path is off by default. Both peers must enable `drive.metadata_slot_v1`
and `drive.metadata_slot_delivery_v1`. The separate authenticated `SKSD`
capability keeps shadow-only and older muxv4 peers on immutable objects.

Only single-stream priority DATA/FIN batches are eligible. OPEN, RST, bulk
`p1`, and oversized batches retain the immutable path. The sender prepares one
mux name and one sealed envelope, offers those exact bytes in the cumulative
slot snapshot, and waits up to 1.5 seconds for an authenticated processing ACK.
On timeout, overflow, slot error, or unavailable peer it uploads the same
prepared batch as an immutable Drive object. A slot ACK skips that upload.

The receiver validates the slot AEAD and the nested mux envelope. It claims
the name in the regular priority receive path only when the destination stream
is ready. The worker processes the frames before advancing the slot ACK.
An immutable object that wins the claim records its sealed digest after frame
processing; a late slot copy then advances its ACK without redelivery. A slot
that wins retains a digest claim independently of the 200,000-name discovery
cache for up to 30 minutes and 65,536 names. Late physical copies are read by
exact Drive ID, compared with that digest, then queued for exact-ID cleanup.
For media objects the verifier reads media even if inline description bytes
appear to match. A mismatched physical copy closes the stream.

A physical object seen while a virtual claim is queued is retained by full
metadata and requeued immediately if virtual processing fails. At most eight
such copies are retained per name; overflow rewinds priority discovery after
the virtual claim resolves. If the first retained ID is stale, the next is
tried. A destination OPEN that never becomes ready gets no slot ACK, so the
sender publishes its immutable fallback.

The 30-minute digest retention is a bounded cache, not durable exactly-once
state across process restarts. After expiry, ordinary stream sequence checks
still suppress duplicate DATA/FIN application delivery when a late physical
copy is rediscovered, and normal cleanup handles that physical ID. This path
remains an experiment until a guarded live app-level comparison measures
latency, throughput, Drive quota, failure behavior, and cleanup with both
peers enabled. No performance or large-user-scale claim follows from local
tests alone.
