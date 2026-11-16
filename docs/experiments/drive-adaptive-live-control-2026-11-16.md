# Adaptive Drive-only live control: two failed bounded arms

These were separate, frozen, secondary-account runs through pinned
`www.google.com`. Both used the same real SOCKS/TLS origin and 16 MiB exact
GET/POST plus one warm and forty scheduled 64-byte echo requests, with one
second adaptive Drive discovery. The text client used one HTTP/1.1 connection
per origin. Scheduled-to-completion delay therefore includes application
request serialization and must not be labeled the tunnel's minimum RTT.
Neither run is a completed comparison against a candidate.

| Arm | Frozen source / binary SHA-256 | Exact files | Text origin echoes | Guard and provider outcome | Cleanup |
| --- | --- | --- | --- | --- | --- |
| Lifetime guard | `4a2fbd8` / `45891e9b0130b5ec733df726433df0dc96a9c336af7fd10dd0f543ca35eaa0b7` | Both 16 MiB exact | 25/41 | Local lifetime guard at 375 visible calls, 246,040 eight-attempt upper units; no provider rate limit | Joined, two exact prefixes empty immediately and at 31 s |
| Rolling guard | `eee57c79dac024414860b6cee7610b85a0ed3e62` / `9f4dffb7b44b6812782fc1a24c266436cb33f8af6fd66ad4ca32abfe1d23de16` | Both 16 MiB exact | 25/41 | Two local aggregate denials at 387 visible calls, 249,600 whole-arm upper units; no provider rate limit or transport error | Joined, two exact prefixes empty immediately and at 31 s |

The rolling arm's 16 MiB download took 10.576 s (12.69 decimal Mb/s), and
the 16 MiB upload took 11.009 s (12.19 Mb/s). The warm echo took 4.118 s.
Twenty of twenty download echoes completed, but their scheduled-to-completion
p95 was 34.462 s; three of twenty upload echoes completed before cancellation.
The resulting forty-echo p95 is **undefined**. The origin saw one GET, one
POST, 25 text echoes and zero exact-body errors. Its guard reported 387
HTTP/2 responses, 12 expected cleanup 404 responses, 249,600 whole-arm upper
units and zero transport errors. The final one-minute upper counter was
103,560 after the delayed cleanup window; it does not reveal the active
counter at denial. The denial condition also checks request/response byte and
call bounds, so the exact triggering subcondition cannot be inferred from this
summary alone. The local guard retains eight-attempt upper accounting and
40,000 upper units of same-minute cleanup headroom.

The lifetime-arm origin also saw 25/41 texts. Its guard reported 375 visible
calls, 246,040 upper units and one local denial. Both runs terminated and
removed the exact owned files; neither created Firestore resources or changed
the secondary Cloud project. Tokens and copied credentials were removed.
The first attempt at the lifetime arm used a non-executable copied test binary
and stopped before a Drive transport call; its fresh retry above used a
properly compiled executable.

The control remains too slow for chat-like traffic in this workload, and the
eight-attempt conservative minute reservation can stop it before any provider
quota response. A lower real RTT or a quota-safe throughput ceiling cannot be
deduced from these failures. The next test uses an explicitly pipelined framed
TLS echo to separate application serialization from transport scheduling, and
the Drive v2 conditional-metadata carrier is a distinct candidate requiring
its own bounded live proof.

Private evidence is under `.skirk-runs/fs-drive-cadence-20261116b/` and
`.skirk-runs/fs-drive-rolling-20261116/`, especially each
`arm-drive-pilot/summary.json`, `progress.jsonl`, `recovery-resources.json`
and `orchestrator-ledger.json`. The run directories are ignored because they
contain temporary credentials during execution.
