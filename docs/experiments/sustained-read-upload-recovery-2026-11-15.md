# Sustained read and normal-upload recovery observations

Three of four planned arms completed. One candidate delivered the exact 100 MiB
file and all 100 persistent echoes, including delivery after both slot writers
retired and beyond their advertised two-minute bounds. The last candidate
stopped on a real mutable PATCH timeout, outside this observer's allowed classes.
There is no balanced candidate median or winner.

The order was candidate/main/main/candidate. Every arm offered the same local
100 MiB file and 100 distinct 64-byte echoes at 1,250 ms intervals, after an exact
128 KiB prelude in each direction and a one-second bulk lead. The candidate was
async slot delivery plus interaction epochs; main used its actual defaults.
All traffic used the selected pinned Google front. This measures the carrier
with a local owned origin, not an external Internet server or every region.

| Arm | Exact file | Verified echoes | Full-echo request / scheduled p95 | Observed units | Failed RPC logs |
| --- | --- | ---: | ---: | ---: | --- |
| Candidate 0 | 44.253703 s, 18.956 Mb/s | 100/100 | 12.127 / 12.134 s | 132,145 | One metadata timeout |
| Main 1 | 42.418073 s, 19.776 Mb/s | 100/100 | 19.253 / 19.256 s | 115,220 | None |
| Main 2 | 54.516235 s, 15.387 Mb/s | 100/100 | 25.899 / 25.902 s | 107,270 | None |
| Candidate 3 | No completion checkpoint | 11 partial receipts | Not a complete distribution | 21,635 last snapshot | Metadata 6 s, PATCH 10 s |

Full-echo latency ends after the exact echo body, not just its first byte.
Scheduled latency includes delay from the intended dispatch time. Percentiles
use nearest rank within that arm; missing samples are never replaced by the
eleven partial receipts. All three completed files passed byte-count and SHA
checks. Three hundred complete-arm echoes and eleven partial echoes are retained.

Candidate 0 is labeled `completed_with_permitted_timeouts`, with
`clean_api_run=false`. Its bare metadata deadline has no observed URI or HTTP
stage; its GET semantics are bound to the reviewed exact binary/source, not
inferred from a guessed connection cause. Both main controls were clean.

Candidate 3 first encountered an allowed metadata deadline, then a one-attempt
mutable slot PATCH deadline after 10 s with no HTTP status. The observer stopped
before the protocol's ambiguous-publication readback or fallback outcome could
be assessed. This does not prove that the tunnel could not recover. Its last
periodic metrics contained only the earlier metadata error; both actual failed
RPC logs remain recorded. There were three observed failed RPCs across the
cohort, although the last snapshots counted two. No quota, auth or backoff error
was observed, and no limit was expanded during the run.

## Continuation after finite slots

Candidate 0 dispatched 35 frames while its file operation was active and 65
after completion. Their scheduled full-echo p95 was 14.940 s and 3.046 s,
respectively. Forty post-file frames arrived after both writers logged their
precise PATCH-budget retirement, before the later lifetime-boundary groups.
Six frames arrived beyond both advertised two-minute bounds; those six had
scheduled full-echo p95 3.046 s. Four frames also began more than 120 s after
the mixed workload started. These are overlapping cuts and must not be summed.

This is real bounded continuation through the standard immutable path, not a
claim that slots run indefinitely. A writer can stop while its reader remains
active. Log timestamps have one-second precision, with boundary cases reported
as uncertain. The configured 320-GET cap has no exact exhaustion timestamp in
these logs. The controls' post-file scheduled full-echo p95 was 6.228 and 6.195 s.

## Policy, provenance and cleanup

The separately reviewed observer allowed only narrowly classified read and
normal idempotent multipart timeout events: at most eight tolerated events per
arm, four per rolling minute and sixteen per cohort, shared across classes.
Mutable PATCH, auth/quota/backoff, terminal and integrity failures stayed fatal
to this experiment. Final per-role/per-operation counters had to match eligible
logs, with the existing five-second metric publication cadence explicitly
bounded. The observer never issued or retried a request. Strict and earlier
normal-only attempts remain unchanged.

Limits fixed before all arms were 300 s overall, 180 s file, 60 s without
progress, and estimated units of 125,000/rolling minute, 150,000/arm and
600,000/cohort. The separately recorded outer wrapper allowed 1,600 s for four
arms plus startup. Quota values are local estimates that can omit opaque HTTP/2
attempts; they are not Google billing telemetry or a never-exhaust guarantee.

The candidate CLI came from clean source
`6ecd41e92e44df1256c3bb8fb14a81ff6d8e0919`, SHA-256
`bba4bc72e1964ab521d7e65f7739ae413fb601600231979cd7978d0ed026ae6f`.
Its production Go delta from frozen c47 is only the exact local PATCH-budget
error identity and log label. Timers, caps, wire bytes, fallback and defaults
are unchanged. The final minimal source passed full Go tests, vet and targeted
race checks; earlier unstamped builds were retained and excluded.

Main was source `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`, SHA-256
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`.
Observer source was `f9d9b65`, with 87 local cases passing before timing.
All agent compiler/test/emulator work was stopped; an external read-only Claude
UI audit could still use the network, and ordinary desktop activity was not
fully isolated.

Exact cleanup deleted 34 residual mux objects and two slot objects. All eight
generated prefixes were empty immediately and after 31 s, and all temporary
profiles were removed. No later result replaces any interrupted earlier cohort.
[Sanitized JSON](sustained-read-upload-recovery-2026-11-15.json) preserves every
arm, actual error observations, role counters, all retained samples, precise
source/harness hashes and lifetime strata. Private raw files remain under
`slot-async-wt/.skirk-runs/async-vs-main-download100m-bounded-read-recovery-20261115/`.
