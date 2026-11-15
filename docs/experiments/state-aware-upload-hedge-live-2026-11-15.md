# After-write upload stall recovery: guarded live comparison

The download ABBA completed, but recovery never triggered. The reverse upload
ABBA stopped after its first control hit a real small-request timeout. This
does not establish a recovery benefit, a complete upload comparison or a new
default. Keep the feature opt-in.

## Exact scope

Both modes used frozen source `42e6742` and CLI SHA-256
`c6c42930544e03694fdaaaa1fd1e14e538ceb47e598250c5cc7e26e52503c9d9`.
The selected route was `google_front_pinned`, TLS front `www.google.com`; fixed
hedging was zero. Only the candidate enabled after-write recovery at 2,500 ms,
with a 16 MiB rolling multipart duplicate-body reservation per minute per
store. The actual saved and cloned `tunnel.chunk_size` was 8,388,608 bytes;
`--chunk-size` remained zero, preserving that value. Runtime bulk coalescing
still produced approximately 4 MiB multipart uploads in the observed traces.

Every planned arm used a 16 MiB exact-hash-checked bulk file plus twelve
persistent, distinct 64-byte pipelined echoes, spaced 250 ms after a 1,000 ms
bulk lead. The default scheduler was identical in both arms. The existing exit
probe, quota/error/stall checks and random-session cleanup wrapper were used.
The harness's `small_response_bytes:17` field describes its unused HTTP
response template; persistent echo payloads are 64 bytes in
`text_payload`, not seventeen.

The opt-in recovery path also selects one physical HTTP attempt per explicit
authentication attempt, letting the coordinator own retry behavior; ordinary
control retains its normal helper retry behavior. This therefore compares
recovery policies rather than only the value of a timer.

## Observations

| Direction/arm | File Mb/s | Twelve echoes | API errors | Recovery probe/spare |
| --- | ---: | --- | ---: | --- |
| Download control 0 | 13.799 | 12/12 | 0 | 0/0 |
| Download candidate 1 | 13.598 | 12/12 | 0 | 0/0 |
| Download candidate 2 | 11.400 | 12/12 | 0 | 0/0 |
| Download control 3 | 12.237 | 12/12 | 0 | 0/0 |
| Upload candidate 0 | 12.463 | 12/12 | 0 | 0/0 |
| Upload control 1 | incomplete | no completed report | 1 timeout | not applicable |
| Upload remaining control/candidate | not run | not run | — | — |

Descriptive download medians across two arms per mode: control/candidate file
goodput was 13.018/12.499 Mb/s; median arm request-completion p95 was
6.667/8.162 seconds, and median arm scheduled-completion p95 was 6.671/8.166
seconds. Estimated units including warm-up and tail were 20,310/23,410 per
arm. These are medians of arm summaries, not a pooled-message p95 or a causal
regression estimate. No commit proof, secondary launch, duplicate reservation
or budget denial occurred in any completed candidate arm. The observed
variation cannot demonstrate the recovery mechanism's benefit.

HTTP timing traces were enabled identically for all attempted upload arms;
download arms did not enable them. The successful upload candidate's three
4,194,867-byte multipart requests acquired reused connections in
0.027–0.075 ms, completed writing after 1.141–1.820 seconds and observed first
response bytes after 1.854–2.498 seconds. Their approximately 0.68–0.74-second
post-write waits did not meet the 2.5-second trigger.

The failed upload control logged a 736-byte request acquiring a reused
connection at 0.023 ms and completing its write at 0.087 ms, then receiving no
response byte before its 10.001-second deadline. Quota telemetry recorded one
upload timeout. Other small requests were canceled during shutdown; no mixed
result/integrity proof was produced. This identifies a post-write response
tail, not a demonstrated connection-local cause. The 736-byte object is below
the recovery mode's 128 KiB eligibility threshold, so there is no evidence this
feature would have recovered the failing request. The exit probe had completed
in 3.155 seconds before the interrupted mixed workload.

## Cleanup and retained failure

Six arms were attempted and five completed. The download comparison exited
zero; the incomplete upload comparison exited one and was not silently
restarted to replace its failure. Download cleanup removed sixteen residual
objects across eight exact random mux/slot prefixes; upload cleanup removed
thirteen across four prefixes. Every prefix was empty both immediately and
after the 31-second visibility check. All generated arm configs were removed.

Sanitized numeric evidence is in
[`state-aware-upload-hedge-live-2026-11-15.json`](state-aware-upload-hedge-live-2026-11-15.json).
Raw summaries, timing logs, interrupted run artifacts and exact cleanup records
remain under ignored `.skirk-runs/stall-hedge-live-20261115/`. No claim of
200,000-user capacity, minimum achievable latency or maximum possible speed
follows from this bounded experiment.
