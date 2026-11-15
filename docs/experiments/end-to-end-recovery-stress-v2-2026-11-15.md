# Source-bound bare upload observation, policy version two

This separately labelled policy changes only observation. It does not change or
rebuild the transport. Version one's fourth sustained arm remains failed:
`op=upload attempts=1 status=none duration=10s error=context deadline exceeded`
was rejected before the pending PATCH readback completed. Subsequent cancellation
artifacts do not become the cause of that earlier rejection. The retained
version-one cohort, its three complete application workloads, and its teardown
evidence remain unchanged.

The new mutually exclusive flag is
`--bounded-end-to-end-recovery-stress-v2`; reports explicitly name
`bounded_end_to_end_recovery_stress_v2` and arm rows carry
`recovery_policy_version: 2`. Version one and all earlier strict/read/upload-only
classes and registries are unchanged.

The only additional eligible failure is exact bare `context deadline exceeded`
with operation `upload`, visible attempts `1`, and status `none`, under either of
these exact frozen executable/source pairs:

| CLI SHA-256 | Source revision |
| --- | --- |
| `2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b` | `8c9829dc9ccbe98d62c4357d60ab762e02ac6373` |
| `34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3` | `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a` |

Both source versions have one caller of the labelled `/upload/drive/v3/files`
path: multipart `POST` creation (`drive.go:490` in the diagnostic source and
`drive.go:186` in the main source). This is source inference; the bare line itself
does not disclose a method, URI, host, dispatch stage, individual request
identity, or physical wire count. A timeout can arise before dispatch or in a
retry/context path. The report records
`source_inferred_immutable_upload_not_observed`, never an observed URL or API
idempotency claim.

Every admitted failure still requires exact role/operation-count correlation
with an eligible mux upload timeout. Normal records require `priority=false` and
`idempotent=true`. Priority records require the actual source's `priority=true`,
`idempotent=false` logical-deduplication class: the prepared name and encrypted
bytes repeat, but a physical Drive create can duplicate files. Distinct physical
aliases must be cleaned and all class/error counters remain visible. An
unmatched, wrong-role, non-timeout, or unknown upload still rejects the arm.

URL-bearing failures keep strict URL validation and never fall back to the bare
rule. Attempts greater than one, HTTP error statuses, bare I/O timeout strings,
context cancellation, bare JSON `create`, delete, and unknown operations are not
newly permitted. Authentication, quota/backoff, terminal/generic transport,
integrity, and conflicting PATCH outcomes remain hard stops.

The existing shared maxima remain eight permitted timeouts per arm, four per
rolling minute, and sixteen per cohort; the rejecting event is retained in
observed counters. All unit/deadline/progress limits, full exact file/hash and
echo proofs, origin side-effect checks before and after verified writer
shutdown, durable original outcome, native process teardown, and cleanup gates
are inherited unchanged. Passing this policy would prove the configured
application workload recovered within its finite bounds, not a clean RPC run or
durable exactly-once delivery across restart.

Validation covers the retained actual bare log shape, both source pins, normal
and priority classification, missing/wrong-role mux pairs, unchanged version-one
rejection, malformed/URL/status/attempt exclusions, shared cap accounting,
integrity and duplicate-origin hard stops. The historical policy regressions are
also rerun. No Google request is made by these tests; the same frozen diagnostic
CLI is required for the next separately labelled live cohort.

The final source passed 187 Python tests in 3.878 seconds with:

```sh
python3 -m unittest test_skirk_end_to_end_recovery_v2 test_skirk_end_to_end_recovery test_skirk_read_upload_recovery test_skirk_bounded_recovery test_skirk_live_ab_scale test_skirk_live_slot_mix test_skirk_ab_finalize test_skirk_concurrent_process_journal test_skirk_stress_origin_proof test_skirk_persistent_text_bench test_skirk_benchmarks test_skirk_owned_processes
```

The command ran from `tools` and includes 25 V2 cases (17 inherited historical
contract tests and eight added bare-upload regressions). Independent source
review confirmed the two sole upload call sites and that URL/status/attempt,
integrity, origin, pairing, and version-one rules remain unchanged. There was no
Go/runtime source edit or executable rebuild for this observer version.
