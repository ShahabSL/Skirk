# State-aware upload stall recovery

This opt-in experiment tests a narrower hypothesis than the previous fixed
three-second upload hedge: a fully written upload with no response headers may
benefit from exact commit resolution, then at most one budgeted duplicate.
It does not establish that a different connection fixes a Google tail, and no
performance improvement is claimed before paired live measurement.

## Configuration and scope

The production default remains disabled. A test profile can set:

```json
{
  "drive": {
    "upload_stall_hedge_delay_ms": 2500,
    "upload_stall_hedge_budget_bytes": 16777216
  }
}
```

Delay is zero or 1,000–10,000 milliseconds. The rolling duplicate body budget
defaults to 16 MiB per minute per `DriveStore`; an explicit budget must be
128 KiB–64 MiB. This budget includes the complete multipart body and therefore
its framing overhead. It is reserved conservatively before each secondary
physical HTTP call, including authentication retries, and is not refunded on
failure or cancellation. It is not a fleet-wide quota allocation.

This mode and `upload_hedge_delay_ms` cannot both be enabled. Only immutable
normal mux uploads of at least 128 KiB with an existing generated Drive ID
qualify. Metadata-only priority objects, control traffic and uploads without a
generated ID remain unchanged.

## Critical path and correctness

1. The primary POST uses the selected route. Its timer starts only after
   `httptrace.WroteRequest` reports successful completion of the body. A body
   still writing cannot start the timer. The first response-header byte disarms
   it, including an early interim response.
2. A new physical `requestOnce` resets observation, so a previous 401's write
   completion cannot arm the next POST while it is still writing. These scoped
   POSTs clear `Request.GetBody` to prevent opaque body replay by `net/http`.
   Observed attempts are included in Drive accounting.
3. After the delay, one global spare slot per `DriveStore` protects the entire
   commit-probe/spare operation. If unavailable or quota backoff is active, the
   original continues alone. The fresh client copies the exact selected route;
   no alternate-route fallback occurs in this mode.
4. The fresh client makes a two-second, 64 KiB-bounded exact-ID metadata GET,
   requesting `id,name,size,sha256Checksum`. Only all matching fields plus the
   SHA-256 of the uploaded encrypted media bytes prove commitment. A name and
   size match alone is insufficient. A valid proof cancels and joins the
   primary, then returns the existing object without uploading again.
5. Missing content or an unavailable checksum permits consideration of the
   duplicate. A malformed/mismatching proof or a probe error prevents it.
   The coordinator rechecks the primary result and header/write state after
   the probe. New response headers cancel the probe and suppress the spare.
6. If still stalled, one secondary POST may use the identical generated ID,
   name, sealed bytes and multipart body. Each actual secondary call reserves
   its full body budget first; denial issues no POST. Any 409 requires the same
   exact content proof. The first successful leg cancels and joins the other;
   both errors return failure. Cancellation joins all active operations.

The final state check and a secondary launch are not atomic with network
events: headers can arrive between them. This bounded race can issue an
unnecessary duplicate but cannot create a second Drive object because its ID
is identical. No assumption is made that canceling HTTP retracts a Google-side
commit. Joining the loser before return preserves existing per-object cleanup
ownership, and generated-ID idempotency handles a late commit.

The rolling budget charges a physical-call reservation even if request setup
or transport fails before the body is sent. This is conservative traffic
accounting, not a measurement of successfully transmitted bytes. The original
upload has no duplicate-byte charge. The exact-ID probe costs an estimated five
Drive units; each POST costs fifty. An expected probe 404 is counted as an API
call and units, not as a failed operation. Unexpected errors and 401/409 remain
visible; no quota or failure accounting is hidden.

## Validation and measurement

Focused race tests cover commit-without-response recovery; identical same-ID
spare and late primary completion; writing-body and header suppression; headers
arriving during the probe; stale write notifications across 401 refresh; global
one-spare concurrency; caller cancellation and loser joins; rolling byte
budgets, multipart overhead and budget denial; per-401 physical-call charges;
missing/wrong checksums and strict conflict resolution; and opt-in validation.
The prior fixed-hedge and separate-pool regressions remain exercised.

The live harness selector `--candidate-upload-stall-hedge-delay-ms 2500`
requires the same binary in both ABBA arms and `--observe`. It clones a fresh
random session per arm, disables the fixed hedge in both, uses zero delay for
control and the requested delay only for candidate, and preserves the route.
The optional `--upload-stall-hedge-budget-bytes` sets the same bound in both.
Saved kit contents are unchanged. Reports include proof calls, proven commits,
spare launch attempts, duplicate body reservations and budget denials. A run
with no stalls is a no-trigger observation, not proof that recovery improved.

Live comparison must use the existing quota/error/stall guards and exact random
session cleanup wrapper, including immediate and delayed zero-residual checks.
Benchmark upload and download separately, retain small-message completion and
latency alongside bulk goodput, and report duplicate cost with the results.
No live traffic was needed for the implementation and fake-transport tests.

## API assumptions

Google documents `sha256Checksum` as output-only and available for binary Drive
content, rather than Google Docs or shortcuts. Availability is not guaranteed
by this implementation; absence never becomes a positive proof.
See [Drive Files resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
and [pre-generated IDs](https://developers.google.com/workspace/drive/api/guides/manage-uploads#use_a_pre-generated_id_to_upload_files).
Trace semantics follow the locally installed Go `net/http/httptrace` package
documentation: `WroteRequest` includes the body; `GotFirstResponseByte` observes
the first byte of response headers.
