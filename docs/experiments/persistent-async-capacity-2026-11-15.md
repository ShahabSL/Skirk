# Five-client persistent async capacity comparison

This extends the existing `skirk_live_capacity_compare.py` harness. Product Go
code and frozen CLI binaries remain unchanged. No live capacity result is claimed
by the implementation or its local tests.

Use `--order baseline,async,async,baseline` for a balanced comparison. The async
arm enables only slot negotiation, slot delivery, asynchronous handoff and
interaction epochs; baseline clears these and every other saved experiment.
`--control-binary` optionally selects a separate baseline binary; otherwise both
arms use the same frozen CLI. Both kit peers must select pinned Google front.
Live execution additionally requires the approved secondary-account quota-kit
path and explicit `--account-label isolated-secondary`. That label is the
runner's assertion, not independent token identity introspection.

Each fresh arm owns one exit, five SOCKS clients and one random shared session.
Client IDs differ; actual CLI-generated run IDs are verified unique in startup
logs, rather than assuming configured run IDs survive CLI initialization.
Every client first verifies a 128 KiB echo in each direction on one persistent
connection. Prelude seeds differ across clients. A file barrier releases bulk and text
only after all five preludes succeed. Each client then uploads an exact 16 MiB synthetic file to the owned
loopback HTTP origin, which verifies its SHA-256, while sending twelve distinct
64-byte framed echoes at 250 ms intervals after a one-second lead. The origin
never contacts external user endpoints. Reports require five simultaneous,
verified files and all sixty exact echoes; they retain first-byte and complete
body latency separately.

Async success also requires a handshake and post-barrier handoff, publication and sender processing ACK matched by the same record fingerprint
with the exit's post-barrier receiver-processing log for every client. Reverse
common-record proofs are reported separately; missing downstream ACKs do not
become a claim of proven downstream slot delivery. Baseline
slot activity is an error. Per-role quota counters, binary hashes, namespace
fingerprints and original interrupted artifacts remain private. A single API
error, backoff or global quota ceiling stops the comparison. Counters and
cleanup-unit estimates can omit opaque HTTP2 retries and are not Google billing
measurements.

Each service, benchmark and curl child records a private native identity before
teardown. Individual signals require that identity or a still-unreaped original
Popen handle; a dead leader never authorizes signaling its old process group.
The original workload outcome is synced before teardown. Every parent is
attempted even if another signal fails; child manifests are read after parent
exit, and ambiguous spawn intents remain explicit failures. No further Drive
cleanup starts unless all original Go writers have exited. Workload failure,
teardown failure and partial cleanup counters remain separate. Cleanup touches only exact
`muxv4/<session>/` and `slotv1/<session>/` prefixes, checks immediate absence,
waits 31 seconds and checks again. A late object is deleted and requires another
full 31-second empty check; three delayed passes bound this work. Every generated
client/exit config is removed. Cleanup failure stops subsequent arms.

Planned invocation (requires root's coordinated live lease):

```sh
python3 tools/skirk_live_capacity_compare.py \
  --binary /absolute/.skirk-runs/skirk-final-runtime-42e6742 \
  --kit-dir /absolute/.skirk-runs/personal-test/quota-kit \
  --out /absolute/.skirk-runs/persistent-capacity-async-new \
  --order baseline,async,async,baseline --file-mib 16 \
  --text-count 12 --text-interval-ms 250 --tail-seconds 8 \
  --bulk-stall-seconds 60 --between-arm-seconds 65 \
  --max-rolling-units 50000 --max-recent-units-per-minute 250000 \
  --max-arm-units 80000 --max-total-units 250000 \
  --cleanup-reserve-units 20000 --account-label isolated-secondary --live
```

The projected short-window rate guard is separate from the actual rolling
50,000-unit estimate. With five clients on one mailbox/account, passing this
bounded test does not establish a universal client count or 200,000-user scale.
Files and64-byte payloads are shared across clients; this verifies five
complete flows and unique authenticated namespaces, not a fully adversarial
cross-client payload-isolation test.

Local tests cover saved-config isolation, actual run-ID reuse, flag-only and
prelude-only carrier activity rejection, global guard failure, exact origin
hashes, simultaneous file overlap and late slot cleanup. The pre-existing real
loopback SOCKS/echo tests cover the shared persistent probe and start barrier.

The first live baseline/async pair is retained in
[the bounded recovery and capacity report](bounded-recovery-and-capacity-2026-11-15.md).
Its baseline completed five files and sixty exact echoes. The interrupted async
arm and its process-group EPERM led to the separate teardown correction; no
post-fix live capacity result is implied. The observer now also retains each
client's receipt journal and atomic file checkpoint when interrupted.
