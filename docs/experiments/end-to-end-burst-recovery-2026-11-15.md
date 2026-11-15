# Fixed burst-recovery observation experiment

This is a new, separately labelled campaign. The V2 cohort remains failed at its
predeclared four-per-minute cap: two download deadlines and one metadata deadline
arrived together, followed by two priority upload deadlines within six seconds.
The new budget does not turn that incomplete file into a pass and does not change
the interpretation of any earlier strict, V1, or V2 arm.

The engineering question is whether the existing engine completes the exact
mixed workload after one or two concurrency-sized network hiccups within the
same real resource and application limits. A single stalled connection can affect
several independent in-flight RPCs. Four errors per minute was an observation
allowance, not a Google quota or a demonstrated transport limit. The following
larger allowance is a fixed experimental choice, not a claimed optimum:

| Limit | Historical V1/V2 | New burst campaign |
| --- | ---: | ---: |
| Permitted RPC timeouts per arm | 8 | 32 |
| Permitted RPC timeouts per inclusive rolling 60 seconds | 4 | 16 |
| Permitted RPC timeouts per cohort | 16 | 64 |

The rejecting 33rd/17th/65th event is retained in observed counts. The constructor
exposes no arbitrary limit parameters, and constructing a burst cohort does not
mutate historical cohorts. Limits cannot be expanded mid-run.

Use `--bounded-end-to-end-burst-recovery`, mutually exclusive with every earlier
observation flag. Reports identify `bounded_end_to_end_burst_recovery_v1`; arm
rows separately identify the burst family and version one. It inherits exactly
V2's two reviewed binary/source pins and classifications, including the explicit
source-inferred bare upload rule and the physically non-idempotent priority
create class. It sends no request or retry of its own.

All other gates are unchanged: the intended cohort still uses the same frozen
diagnostic/main executables, 125,000 rolling estimated Drive units, 150,000 units
per arm, 600,000 units per cohort, 300-second application deadline, 180-second
file deadline, and 60-second progress deadline. Those values remain explicit
campaign arguments rather than becoming new transport defaults. Authentication,
quota/backoff, terminal, generic transport, integrity, unsupported-operation,
counter-correlation, and origin-duplication failures still stop immediately.
Every visible RPC error remains in its class and operation counters.

Passing requires the exact full file/hash/direction, all scheduled echo receipts,
the required prelude, matching origin effects before and after verified writer
shutdown, durable primary outcome, complete native process teardown proof, and
bounded exact-prefix cleanup of all aliases. A workload that stalls or corrupts
still fails regardless of the remaining timeout allowance. A successful arm is
an application-recovery result within a finite experiment, never a clean RPC,
unlimited free-capacity, physical-idempotency, or restart exactly-once claim.

Tests reuse the V2 source/shape/pairing/integrity/origin/teardown contracts and add
fixed-limit isolation, all timeout classes sharing the new rolling budget,
inclusive-window boundary, rejection and retention of the 33rd arm and 65th
cohort event, and CLI mutual exclusion. No Go/runtime source or frozen executable
changes for this experiment.

The final combined Python run passed 216 tests in 3.945 seconds, including 29
burst cases and the unchanged V2/V1/read/upload, CLI, origin, teardown, native
process, and persistent/mixed benchmark regressions. Command from `tools`:

```sh
python3 -m unittest test_skirk_end_to_end_burst_recovery test_skirk_end_to_end_recovery_v2 test_skirk_end_to_end_recovery test_skirk_read_upload_recovery test_skirk_bounded_recovery test_skirk_live_ab_scale test_skirk_live_slot_mix test_skirk_ab_finalize test_skirk_concurrent_process_journal test_skirk_stress_origin_proof test_skirk_persistent_text_bench test_skirk_benchmarks test_skirk_owned_processes
```

No live Google request or transport compilation was made by this implementation.
