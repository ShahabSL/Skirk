# Retained HTTP2 diagnostic and first sustained scale attempt

Neither attempt completed its planned comparison. They remain separate from the
completed16-arm matrix and any subsequent scale cohort; missing arms/results are
not replaced with successful repeats.

## Dependency-only HTTP2 diagnostic

All Go product code was frozen42e6742. The control changed only the `x/net`
replacement to0.54 with two clean-main checksums; candidate retained0.61. Both
CLI builds used Go1.26.4 and actual default configuration, pinned Google front,
16MiB exact files, twelve persistent64B echoes,128KiB prelude and equal timing
traces. No slot or other experimental setting was enabled.

| Arm | x/net | File Mb/s | Request/scheduled first-byte p95 s | Units | Result |
| --- | --- | ---: | ---: | ---: | --- |
| Control0 |0.54 |17.182 |8.013/8.014 |21,410 | Exact file+12echoes,0API errors |
| Candidate1 |0.61 |17.535 |6.477/6.483 |19,210 | Exact file+12echoes,0API errors |
| Candidate2 |0.61 | incomplete | incomplete |24,010 last observed | One normal exit upload timeout |
| Remaining control |0.54 | not run | not run | — | Guard stopped comparison |

The failed1,049,162-byte multipart request acquired a reused connection at
0.016ms, finished writing after3661.745ms, then observed no response byte before
the28.001s deadline. Client counters were11,155units/zero errors; exit12,855/one
timeout. Later shutdown cancellations are not additional proven timeout events.
This identifies a post-write response tail for this request, not a
connection-local cause or a causal dependency regression. One completed arm per
version is insufficient for a ranking.

Control binary SHA-256:
`eb49ef7350ca91c3e1b8a7a7893f69a520d88c3e05875f757aaa29ecc86ddbdc`,
sourcec56451f. Candidate:
`c6c42930544e03694fdaaaa1fd1e14e538ceb47e598250c5cc7e26e52503c9d9`.
Cleanup deleted19residual mux objects. The first31s delayed sweep found one late
mux object; after its exact deletion, a second31s sweep verified all six mux/slot
prefixes empty. Generated configs were removed.

## First100MiB,100-frame sustained comparison: self-imposed budget stop

The first clean-main control completed its exact104,857,600-byte file in
32.132425s,26.106Mb/s. The file proof was atomically persisted before the remaining
text schedule completed. This positive file result is retained despite the
later interruption.

The planned workload was100distinct64B persistent echoes at1250ms intervals,
with1s bulk lead and300s overall/180s file deadlines. It was designed to extend
beyond the slot prototype's64PATCH/320GET/two-minute bounds, explicitly separating
bulk-overlapped and post-bulk frames. Both arms were to use an equal strict local
50,000-estimated-unit rolling60s ceiling; no mid-run expansion was allowed.

The main control stopped after about55s when the local quota guard fired. Last
observed counters were26,205client+24,655exit=50,860units, zero API errors and no
quota backoff. This was our conservative experiment budget, not Google quota
exhaustion or proof of a TCP failure. No complete100-frame report exists, and no
candidate or later control arm ran. Therefore this attempt gives no candidate
latency, lifetime or mixed QoS comparison; the interrupted frame distribution is
not reconstructed from upload counters.

Cleanup deleted14residual mux objects. Both exact mux/slot prefixes were empty
immediately and after31s, and config copies were removed. Source harness723b112
used unchanged frozen current/main binaries. A later authorized125,000-unit
ceiling applies only to a fresh, separately reported cohort, equally in all arms.
Both ceilings use local estimates that can omit opaque HTTP2 retries; neither
is a mathematical guarantee against Google quota exhaustion.

[Sanitized evidence](h2-dependency-and-scale-2026-11-15.json) retains all attempted
arms, per-role counters, the completed file checkpoint and host conditions. Raw
private directories are `.skirk-runs/xnet-v054-v061-download16m-20261115/` and
`.skirk-runs/async-vs-main-download100m-sustained-20261115/` in `slot-async-wt`.

The later125k cohort uses harnessc2dec7f with explicit per-arm150k and cohort600k workload estimates; ordinary defaults remain100k/350k. Cleanup is separate. These volume caps permit the requested long schedule without changing the rolling-rate choice, and apply identically before all fresh arms. Trace timestamps can describe the last opaque HTTP2 retry stage, not an exact count of every network request.
