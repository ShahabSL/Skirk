# Owned benchmark process teardown

This change is benchmark tooling only. Its initial 14 focused tests passed on
this Mac at source `492c253` in the authorized offline window (0.014 seconds
reported by unittest). The peer-reviewed hardening `5d87324` passed all 17
tests in 0.013 seconds, adding malformed-token, unknown-argv, and stale-native-
errno regressions. It does not change the transport or make Google requests.

## Failure addressed

A capacity teardown raised `PermissionError` from a process-group SIGKILL. The
exception skipped the remaining service stops and exact mailbox cleanup. The
workload exception was not persisted before that teardown exception, so the
original async workload failure cannot be recovered. Later runtime metrics do
not establish that missing original cause. Manual owned-PID termination and
exact immediate/delayed cleanup were completed separately.

`tools/skirk_owned_processes.py` makes termination best effort across every
record, retains signal errors, and returns unresolved writers to the caller.
Callers must durably save the workload result before teardown, stop benchmark
workers and services even after an earlier failure, and block Drive cleanup or
subsequent arms if any owned Google writer remains unresolved. This module
does not itself decide whether Google cleanup is authorized or safe.

## API

* `capture_identity(pid, expected_argv=...)` returns PID, PPID, PGID, SID, effective UID, kernel
  creation token, executable, and exact argv. It never reads environment values
  into the record. On Darwin an exact known argument sequence with nonempty
  argv0 is required; unknown boundaries fail closed. Unsupported or inaccessible
  identities fail explicitly.
* `append_process_record(path, pid, role, argv=None)` appends one fsynced JSON
  record with advisory file locking. The immediate parent must be owned and
  private under `.skirk-runs`; the regular manifest must be owned and mode
  `0600`, with final-component symlinks rejected. Native capture failure is
  recorded as `identity_status=unavailable`, so the spawn is not silently lost.
  `argv` is the original `Popen.args` sequence. It validates native argument
  capture and a supplied original handle. The native argv remains a separate
  identity observation. All current benchmark spawns supply this sequence.
* `load_process_records(path)` returns those records, with a 4 MiB file bound
  and truncated or invalid records rejected.
* `terminate_records(records, known_handles=None, term_timeout=5.0,
  kill_timeout=3.0)` returns `ok`, `all_stopped`, per-process `results`, and
  structured `errors`. Handles are an explicit `{pid: original_Popen}` mapping.
  Every result includes `verified_stopped`, `status`, and signal `events`.
  `ok` is false if a signal failed even when a later observation proves exit.
  Only `all_stopped` establishes the helper's writer-exit gate.

Capture immediately after each spawn. A parent should retain original service
and benchmark Popen handles. Inner curl spawns must journal themselves before
communicating because a Python benchmark may exit before its curl child.
Manifest append failure must fail the experiment rather than discard ownership.

## Identity and termination bounds

On macOS, the installed SDK's `proc_pidinfo(PROC_PIDTBSDINFO)` supplies kernel
start seconds/microseconds. `proc_pidpath` and `KERN_PROCARGS2` supply executable
  and argv. NUL padding cannot distinguish a leading empty argv0, so native
  argument candidates are returned only when they exactly match the known
  nonempty-argv0 sequence. No environment-derived strings are returned or stored
  on a boundary mismatch. The SDK struct is checked for 136-byte layout and its
creation fields are covered by a regression. Linux uses `/proc` start ticks
with boot ID, effective UID, executable, and NUL-delimited argv.

Identity is checked around argument capture and before each signal. Creation
tokens must match their platform's strict format; malformed tokens cannot count
as exit proof. Native errno is reset before every Darwin query so a short native
result cannot inherit stale ESRCH. A reused
PID is recorded as the original process having exited and is never signaled.
Changed UID, executable, or argv fails closed. An inaccessible identity can use
an explicitly supplied original, still-unreaped Popen handle from the current
observer process; mismatched handles cannot enable that fallback.

All verified processes receive individual TERM before one shared wait of at
most five seconds. Still-verified processes then receive individual KILL before
one shared wait of at most three seconds. EPERM and other errors remain visible
and never stop attempts on later records. There is no group signal, name-based
process discovery, privilege escalation, or discovery of unrecorded children.

Linux uses pidfd signaling where supported, after identity verification. macOS
does not provide an atomic signal conditioned on a recorded kernel creation
time: its native check followed by signal still has a narrow race. The caller
must serialize teardown and avoid competing Popen waiters; this helper does
not claim an atomic Darwin process handle or ownership of unknown descendants.

## Validation

`cd tools && python3 -m unittest -v test_skirk_owned_processes` passed all 17
tests at `5d87324`, including actual Darwin native capture and termination of one recorded
Python sleeper while the unrecorded sleeper remained alive. Both children were
reaped in `finally`. The focused suite covers SDK layout, argv/environment separation, Linux empty
arguments and process names, capture races, private manifest rejection,
malformed/reused/changed identities, original-child fallback, EPERM without
skipping other records, global timeout bounds, and a real recorded sleeper
while leaving an unrecorded sleeper untouched. A later independent read review
found no blocker in the hardened ownership checks. The real case starts only public
Python sleepers and cleans both up in `finally`; it performs no network I/O.

Linux parser behavior was tested with synthetic fixtures; the actual child test
ran on macOS, so it does not establish live Linux pidfd behavior. The integration
must separately prove durable workload-failure retention,
always-stop-all behavior, unresolved-writer cleanup blocking, profile removal,
and accounting despite teardown failure. A successful local test does not
explain why the earlier macOS group signal returned EPERM.

A reused PID with different arguments can conservatively remain unknown before
its creation token is compared; it is never signaled. That may block a cohort
even when the original process has actually gone, rather than invent exit proof.
