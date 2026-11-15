# Bounded read and normal-upload recovery observation

This is a separate benchmark observation mode, not a transport change, a retry
implementation, or a revision of earlier strict/upload-only results. It has not
run against Google. No performance or completed 100 MiB recovery result is
claimed by this source change.

`tools/skirk_live_ab.py --bounded-read-upload-timeouts` requires `--observe`
and persistent text traffic, and is mutually exclusive with
`--bounded-normal-upload-timeouts`. With neither flag, strict behavior remains
unchanged. The existing upload-only policy implementation is unchanged.

## Eligible observations

Every eligible Drive failure must explicitly report `attempts=1`, `status=none`,
and a URL-bearing Go GET/POST error ending in `context deadline exceeded` or
`i/o timeout` (including the standard Client.Timeout header-wait suffix), except
for the narrowly source-bound bare-read case below.
There is no allowance for an HTTP status error, final `attempts>1`, arbitrary
connection failure, or bare timeout outside the source-bound exception.

The actual retained candidate metadata failure is the bare line
`drive request failed op=metadata attempts=1 status=none duration=6.002s error=context deadline exceeded`.
The root authorized this separate allowance: **exactly** bare `context deadline
exceeded`, only for source-verified read labels, only with the same one-attempt,
no-status conditions. It is pinned to these frozen executable SHA-256 values:

- `c47a214ef00db17e8bbee7f40e6ec45d0de7b2174394829ab0919dd61e31082f`,
  source `63532c8a92fef95cdd30a8f1e45eb20dbe207d50`: all four read labels.
- `34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`,
  source `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`: metadata, download, list;
  this main source has no private slot-cleanup label.

`driveRequestLabel` restricts those labels to GET; the current private cleanup
marker also explicitly checks GET. This exception records
`request_shape=source_inferred_not_observed`. It does **not** infer a URI, host,
dispatch stage, or physical wire-attempt count. A retry wait can replace an
earlier URL-bearing error with a bare context error, so bare does not establish
that a request was queued or never dispatched. Bare uploads, bare i/o timeouts,
unknown labels/binaries, status errors and attempts>1 remain ineligible. A
URL-bearing error always receives the strict URL checks; a bad URL cannot fall
back to inferred semantics. New executable hashes require explicit source review
before any corresponding source-bound allowance is added.

Allowed hosts are exactly `www.google.com`, `google.com`, `www.googleapis.com`,
and `drive.googleapis.com`, over HTTPS without credentials, a nonstandard
authority, or a fragment. The harness does not change the selected route.

| Operation | Required method and path | Media policy |
| --- | --- | --- |
| `metadata` | GET `/drive/v3/files/{one bounded ID}` | no `alt`, or exactly `alt=json` |
| `download` | GET `/drive/v3/files/{one bounded ID}` | exactly one `alt=media` |
| `list` | GET `/drive/v3/files` with a query | no `alt`, or exactly `alt=json` |
| `slot_cleanup_probe` | GET `/drive/v3/files/{one bounded ID}` | no `alt`, or exactly `alt=json` |
| `upload` | POST `/upload/drive/v3/files?uploadType=multipart` | matching same-role mux timeout must say `priority=false`, `idempotent=true` |

IDs contain only ASCII letters, digits, `_` and `-`, with a 256-byte bound;
`generateIds` is excluded. Changes, ID generation, batch POST, mutable PATCH,
other upload methods/types, and upload commit probes are not eligible. Read
timeouts are observed only. The unchanged core decides whether/how to recover.

Core download recovery can emit a timeout mux-download record followed by
`mux process retry` or `requeue`. The new policy admits those announcements
only with timeout classification, valid lane/sequence/class fields, and matching
same-role object/lane/sequence/priority timeout counts. Normal-upload retry
announcements must match a same-role normal-upload timeout lane count. Exhausted
retry budgets, decrypt/other processing errors, overflow, auth, quota, backoff,
terminal failures, unknown error logs, and priority/mutable uploads still stop.

## Bounds and correlation

All eligible read and upload classes share **8 observed timeouts per arm,
4 in an observed rolling 60 seconds, and 16 per cohort**. Direction, role, and
operation do not receive separate budgets. The cap-triggering event remains
in observed counts; permitted counts remain separate. Existing quota estimates,
workload/progress deadlines, account selection, payloads, and cleanup rules
are unchanged. The harness issues no retries or new Google methods.

Per-role **and per-operation** quota error counts must match classified Drive
failure-log counts exactly at completion. Per-operation counters cannot regress;
unknown operations, non-timeout reasons, `retry_recovered`, an unexplained extra
error, or unmatched mux failures stop the experiment. Every real API error
remains in the report; a successful workload with errors is
`completed_with_permitted_timeouts`, with `clean_api_run=false`.

There is an important publication cadence distinction. The CLI writes its
runtime metrics every **five seconds**. Therefore eligible logs can lead metrics
for at most 5.25 seconds, measured from the oldest still-unmatched per-operation
event (counter progress releases earlier events); longer lag stops.
Counters ahead of eligible logs, mux-pair mismatch, or final mismatch uses the
existing single 0.25-second correlation refresh. That refresh rechecks quota
estimates/backoff before any allowance. Final equality is mandatory; the policy
does not change the metrics publisher or wait indefinitely for it.

The logs have no shared request ID between Drive and mux records. Correlation
is by role/operation and bounded mux-object counts, **not individual request
identity or hidden HTTP/2 wire attempts**. A read RPC timeout alone does not
establish stream loss, successful recovery, or a better protocol.

## Durable end-to-end proof

At final observation, the fsynced frame journal must contain exactly one
verified >64 KiB prelude, the configured number of unique 64-byte text frames,
all indices, successful integrity checks, and exact sent/received sizes. The
atomically published bulk checkpoint is mandatory and must report successful
integrity verification plus the configured response and upload sizes. The
existing workload-result integrity checks remain required too. Failed receipts
or a failed bulk checkpoint stop before final observation.

Missing/truncated/invalid journals, duplicate or missing frames, changed
configured counts, wrong lengths, a missing checkpoint, or an unchecked file
cannot become a completed recovery result. The SHA-256/body verification itself
is performed by the existing benchmark worker before it writes that checkpoint.

## Offline validation

81 focused/existing Python tests initially passed in 3.787 seconds, including the new
parser/correlation/budget/proof cases, unchanged normal-only tests, comparison
safety tests, and real loopback persistent-frame/file tests. No Google calls
were made. Test log: ignored `.skirk-runs/read-upload-tests.log` in the isolated
`bounded-read-upload-recovery-wt` worktree.

New cases cover all four read classes; shared read/upload budgets and retained
cap-triggering events; rejected hosts, paths, methods, media mixes, duplicate
`alt`, attempts/status errors and non-idempotent/priority uploads; per-operation
and role mismatch; hidden recovered retry counters; metrics lag expiry;
counter regression; matched and mismatched read recovery; early integrity
failure; missing/duplicate/unchecked frame or bulk evidence; and mutually
exclusive CLI flags before any live work.

Follow-up tests cover the actual bare c47 metadata line with exact per-role/op
counter correlation; strict executable/label/message binding of the exception;
and metrics advancing while a newer, still-unmatched event remains within its
own bounded publication interval.

The full affected/existing suite then passed **84 tests in 3.778 seconds** after
those additions. Its raw local log is `.skirk-runs/read-upload-tests-final.log`.

Independent review also found a remaining live applicability issue: finite
metadata-slot retirement can emit the generic `reason=transport_error`, which
the observer still rejects. That string can also represent integrity or other
failures. There is no blanket healthy-retirement exemption; a trustworthy,
specific classification is needed before a long-lived slot-recovery cohort can
claim to have exercised that transition.

Independent source review and a root-coordinated live lease are required before
using this mode for any actual account measurement. Historical failed arms stay
failed; a later bounded recovery cohort must be reported separately.
