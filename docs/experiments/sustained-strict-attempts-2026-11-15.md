# Retained sustained strict attempts and cleanup accounting

These are two separate incomplete cohorts. The first used frozen42e6742 and
the second planned the new cleanup-counter fix. Neither is replaced by a later
successful repeat or retroactively relabeled as a pass.

Both planned main/async/async/main, a100MiB exact local file,128KiB persistent
prelude in each direction, and100distinct64B echoes at1250ms with1s bulk lead.
Limits fixed equally before every arm were300s overall,180s file,60s without
progress, and local estimated units of125,000/rolling60s,150,000/arm and
600,000/cohort. These estimates omit some opaque retries and are not a guarantee
against Google quota exhaustion. The earlier50k interruption remains separately
documented in the [HTTP2 and scale report](h2-dependency-and-scale-2026-11-15.md).

| Cohort and arm | Exact file | Echo evidence | Observed units/errors | Outcome |
| --- | --- | --- | --- | --- |
|125k main0 |100MiB,34.972466s,23.986Mb/s |100/100; scheduled first-byte p9512.019s |116,220/0 | Complete strict control |
|125k async1 |100MiB,32.039930s,26.182Mb/s | Full100-frame report absent |103,745/4metadata404 | Strict guard interruption |
| Counterfix fresh main0 | No completed file checkpoint |19exact receipts retained |31,210/4normal upload timeouts | Strict guard interruption; candidate not run |

The complete main control dispatched28frames during the file and72after it.
Its all-frame request/scheduled first-byte p95 was12.015/12.019s; the28-frame
bulk-overlap stratum was17.016/17.019s. This is one control, not a balanced
candidate comparison. The interrupted older candidate has no receipt journal;
partial echo latency/counts cannot be inferred from upload counters.

Both candidate slot writers reached revision64 and stopped before the older
125k arm's four metadata404 counters. Source inspection then reproduced a real
observer defect: retiring two exact slot targets performs four post-delete
metadata reads, and the expected404 responses were counted as API failures.
Commit4d91f160fcbac50546d373951cc7b4a3f9ef5a58 adds a private exact-ID/name
cleanup-probe marker for those reads only. Four such GETs still cost four calls
and20estimated units, with zero expected404 errors. Ordinary or mismatched404,
401/403/429, transport timeouts and earlier recovered500 failures remain errors;
ordinary DELETE404 classification is unchanged. Full Go tests, vet, a6.326s
targeted race run, and independent source review passed. Calls, routing, wire
bytes, retries and product defaults are unchanged. The historical candidate
remains interrupted; its error count is never edited by subtracting four.

The fresh counterfix cohort then failed in its first unchanged clean-main
control. Four exit uploads of1,048,631sealed bytes each, normal priority and
idempotent, timed out after about28.000–28.001s with zero upload-slot wait.
The role counters were15,255client units/zero errors and15,955exit units/four
errors, without quota backoff or an estimate-limit interruption. The receipt
journal preserves19exact echoes before shutdown. No100MiB completion exists;
no full-workload p95 or candidate result is invented. This strict RPC failure
does not establish whether mux's existing bounded retries could recover.

The clean-main binary predates the detailed HTTP trace instrumentation. The
exact private failure lines preserve operation, sealed size, priority,
idempotency and duration, but acquire/write/header timings are unavailable.
The earlier HTTP2 diagnostic's post-write trace is a different request and is
not substituted here.

Cleanup deleted eight residual mux objects in the125k cohort; all four exact
mux/slot prefixes were empty immediately and31s later. The fresh counterfix
cohort deleted nine initially and one late mux object. A second31s sweep after
that deletion verified both exact prefixes empty. Generated config copies were
removed in both cohorts. These finite slot prototypes stop after64PATCHes,
320GETs or two minutes; continuation uses standard Drive fallback. This record
does not claim indefinite slot operation.

Current frozen binary:
`c6c42930544e03694fdaaaa1fd1e14e538ceb47e598250c5cc7e26e52503c9d9`,
source42e6742. Clean main:
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`,
source2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a. Planned new counterfix
candidate:
`c47a214ef00db17e8bbee7f40e6ec45d0de7b2174394829ab0919dd61e31082f`,
source63532c8a92fef95cdd30a8f1e45eb20dbe207d50; it never ran in that cohort.

[Sanitized evidence](sustained-strict-attempts-2026-11-15.json) preserves all
three attempted arms, role counters, exact completed-file checkpoints and the
partial receipt journal. A future bounded robustness mode must be separately
labeled and retain all actual errors.
