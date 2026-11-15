# Bounded recovery and five-client capacity observations

These are fresh, separately labeled cohorts. Earlier strict failures remain
unchanged. Product binary63532c8 (SHA-256
`c47a214ef00db17e8bbee7f40e6ec45d0de7b2174394829ab0919dd61e31082f`)
was frozen. No compiler, test or emulator load was scheduled during timing.
The host record discloses possible read-only audit network traffic and a few
separate-project Firestore administrative calls. The desktop/network was not
fully isolated.

## Narrow normal-upload recovery cohort

The independently reviewed benchmark policy tolerated at most eight observable
normal multipart upload timeout RPCs per arm, four per rolling minute and
sixteen per cohort. Other API failures, auth/quota/backoff, priority failures,
terminal errors and integrity failures still stopped the attempt. It changes
neither the transport nor its retry behavior. Fifty-two Python tests passed.
Role/count correlation is explicitly weaker than per-request identity or
complete HTTP/2 wire-attempt accounting. Error counters are never subtracted.

The planned candidate/main/main/candidate comparison used a100MiB file and
100persistent64B echoes at1250ms. Its first candidate stopped after nine exact
echo receipts on a client metadata request timeout: one attempt, no HTTP status,
6.002s, context deadline exceeded. That operation was outside the permitted
normal multipart class. No file checkpoint or complete100-frame report exists,
and no main control ran. The normal-timeout allowance consumed zero events.

The last periodic metrics showed20,350estimated units and zero counter errors;
the failed metadata log arrived before that snapshot caught up. Zero in those
stale counters is not zero actual API failures. The real timeout remains
recorded. This is an observer stop, not proof that the tunnel could not recover.
Exact cleanup deleted25residual mux objects; both mux/slot prefixes were empty
immediately and31s later, and generated profiles were removed.

## Actual five-client strict pair

One owned exit served five clients with distinct actual run/client namespaces.
A common barrier followed separate exact128KiB preludes. Each client uploaded
an exact16MiB file while sending twelve64B persistent echoes at250ms, for80MiB
and60echoes total. The same frozen binary ran baseline first, then async slots
plus interaction epochs. Limits were fixed at80k estimated units per arm,
125k/rolling minute,250k/cohort with20k cleanup reserve, and a separate250k
projected-rate guard. This remained a strict API-error comparison.

The baseline completed all five exact files with simultaneous overlap and all
60exact echoes, with zero API errors:

| Baseline observation | Value |
| --- | ---: |
| Aggregate verified file rate |27.456Mb/s |
| Full-echo request p95 |11.333s |
| Full-echo scheduled p95 |11.342s |
| Runtime calls / estimated units |737 /75,280 |
| Explicit residual mux deletion |41 |

Its exact mux/slot prefixes were empty immediately and31s later. Five real
flows are demonstrated; this does not establish200,000-user capacity or an
adversarial cross-client payload-isolation proof. Text/file payloads were
shared, although preludes and authenticated namespaces differed.

The async arm has no complete workload report, file aggregate or latency
percentile. Its earliest observed API failures were four exit metadata timeouts
of about6.000–6.002s. Teardown then raised `PermissionError: [Errno 1] Operation
not permitted` at `tools/skirk_live_capacity_compare.py:105`, the unconditional
`os.killpg(process.pid, SIGKILL)` in benchmark cleanup. That exception skipped
the remaining service shutdown, exact cleanup and profile deletion. It also
prevented the original async arm row/failure string from being persisted.
The discarded original guard exception is unavailable; the early timeout
observations are not substituted for that missing string.

Six orphaned Skirk services were selected by this run's exact config-path
prefix and stopped with individual SIGTERM. No sudo or broader process kill
was used. OS creation timestamps had not been captured, so none is invented.
While those services remained alive after the teardown exception, their final
observed counts rose to eight metadata and two download timeout errors,
81,655estimated units total. The1,655-unit excess above the chosen80k arm cap
is retained as an observer cleanup defect. Client error counters were zero;
all ten exit errors remain. This is not a clean async result or an inferred
candidate speed comparison.

Manual exact cleanup deleted40residual mux objects. Both prefixes were empty
immediately and31s later, all six profile copies were removed, and no matching
Skirk process remained. A teardown correction must persist the original
outcome before cleanup, attempt every owned service even after one signal
error, verify identities before signaling and prevent further Drive cleanup
traffic while a writer is unresolved. A future broader resilience observer is
a separate experiment; no tolerance is expanded mid-run.

[Sanitized evidence](bounded-recovery-and-capacity-2026-11-15.json) retains
the narrow policy counts, all nine partial receipts, baseline role counters,
all ten final async timeout events, the precise cleanup exception and both
cleanup outcomes. Local quota/cleanup estimates can omit opaque retries and
are not Google billing telemetry.
