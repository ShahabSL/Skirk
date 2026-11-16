# Drive v2 priority latency attribution plan

Status: isolated test-only instrumentation; no new Google timing or product
behavior. The completed v2/control live pairs remain in the
[pipelined comparison](pipelined-live-comparisons-2026-11-16.md).

The live v2 arms used 100 ms active metadata polls and reported text p95 near
two seconds. A sender's `fallback_attempts` includes records that were too
large or otherwise ineligible for the inline priority queue, so that counter
cannot explain the forty text frames. More frequent polling would spend quota
without locating the actual delay.

The optional trace added in this isolated branch records a finite sequence of
events from both peers in the **same** test process. Every event has a
per-run HMAC of the Drive object name, direction, priority sequence, phase,
and monotonic elapsed milliseconds. Admission events also retain at most four
numeric mux stream IDs and the total stream count to permit correlation with
the private mux log. No name, sealed body, OAuth token, file ID, route address,
or HMAC key is exported. The trace holds at most 1,024 events, copies snapshots,
and reports its dropped count; a run with drops cannot claim complete
per-claim attribution. It is absent from ordinary product binaries.

The phases are `queued_inline`, `immutable_only`, `patch_started`,
`patch_completed`, `peer_metadata_observed`, `receiver_processed`,
`sender_processing_ack`, `fallback_started`, `fallback_completed`,
`fallback_error`, and `peer_physical_observed`. `patch_completed` includes a
byte-identical lost-response readback that proved commitment. Pair events by
HMAC claim and direction; never equate a two-second fallback timer with the
text p95 merely because the values look alike. A full path may be observed in
a different order if a peer sees the authenticated PATCH before its HTTP
response or if physical fallback wins. Stream IDs identify multiplexed TCP
streams, not individual TLS frames; the private workload/mux log must establish
which stream carried the text series before attributing its tail.

## One bounded live attribution arm

If a live follow-up is run, freeze this source and executable hash, retain the
existing 16 MiB down/up plus one warm and forty pipelined 64-byte frames, the
approved secondary account, fresh owned v2 files, pinned `www.google.com`
front, and the existing 280,000 conservative units/minute admission with
cleanup reserve. Run **one** v2 arm to inspect the trace first. Require exact
file hashes, all forty-one replies, zero unexpected Drive errors or guard
denials, joined writers, exact object absence immediately and after 31 seconds,
and `dropped=0` before any per-claim latency analysis. The experimental five
minute epoch and single-host time base limit interpretation. Existing A/B
cohorts are not reclassified by this new instrumentation.

Only if the trace shows a repeatable avoidable phase should a separate frozen
binary compare one controlled change in candidate/control/control/candidate
order. For example, compare 1 s versus 2 s fallback deadlines **without**
changing poll cadence, API guard, run length, payloads, or cleanup. Repeated
fallback may consume more Drive calls and bytes; the experiment stops on its
original local guard. Record all per-arm text p95, file goodput, phase timings,
fallback identity counts, expected 412s, errors, minute reservations and exact
cleanup. A failure or two small positive arms do not establish a universal
latency optimum or 200,000-user capacity.
