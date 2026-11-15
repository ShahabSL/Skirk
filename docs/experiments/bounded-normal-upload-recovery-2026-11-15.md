# Bounded normal-upload recovery experiment

This benchmark mode is separate from the strict comparisons. Their failed or
partial arms remain unchanged. The product binaries and their retry behavior
are unchanged; only the observer can wait for an already implemented normal
mux retry after a narrowly classified RPC timeout.

`tools/skirk_live_ab.py --bounded-normal-upload-timeouts` requires persistent
traffic and `--observe`. Limits are fixed before any call: at most eight
tolerated observable normal multipart timeout RPCs per arm, four per rolling
60 seconds, and sixteen per cohort. The event that exceeds a cap stops the arm
and is still retained in its observed count. The window uses monotonic observation timestamps with
the harness's polling precision and retains the boundary event. Existing
deadline, progress, estimated-quota, and full-workload verification guards still
apply. For the planned sustained comparison those are 300 seconds overall,
180 seconds for the file, 60 seconds without progress, and estimated units of
125,000 per rolling minute, 150,000 per arm, and 600,000 per cohort.

Every tolerated event must have a Drive log with `op=upload`, `attempts=1`,
`status=none`, a POST to the multipart Drive files endpoint, and an explicit
context deadline or I/O timeout. Each must pair by role and count with a mux
upload result having `priority=false`, `idempotent=true`, positive sealed bytes,
and `error=timeout`. Priority failures, other failure types, non-upload API
errors, auth/quota/backoff events, terminal failures, and failed verified frame
or file receipts stop the attempt. Incomplete or malformed evidence also stops
it. Logs are scanned incrementally with finite byte and line bounds.

There is no shared request identifier between these two legacy log families.
This is conservative one-to-one correlation by role and count, not proof of
individual request identity or every underlying HTTP/2 wire attempt. A reported
API error count greater than the matching observable events stops the attempt,
including an extra recovered HTTP error beside an unrelated timeout. A single
250 ms re-read handles a Drive log/counter preceding its mux log. At completion
the error counters must exactly match both event counts for each role. Metrics
may lag complete log pairs during an active arm; no error count is subtracted.

A completed file and every scheduled exact echo remain mandatory. A completed
arm with tolerated errors is labeled `completed_with_permitted_timeouts`, with
`clean_api_run=false`; it is never called a clean pass. Every arm retains its
actual per-role quota counters, policy counts, configured caps, receipt journal,
and atomic file checkpoint. Any interrupted attempt retains those observations
without inventing a full-workload percentile.

Offline validation: twelve adversarial classifier tests cover delayed/split log
writes, role mismatch, priority/non-idempotent failures, non-timeout and hidden
HTTP errors, quota/auth/backoff, finite caps across arms, truncated logs, failed
integrity receipts, and regressing counters. Parser and existing benchmark tests
also pass. Independent review and a new exclusive live lease are required
before using this mode against the isolated test mailbox. No live result is
claimed by this document.
