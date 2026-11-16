# Mux media, socket read memory, and Drive rate errors — November 16, 2026

This is an isolated code and localhost MemoryStore experiment from source
`47f882e0754fb8a9dc76550f629ce612e94fd962`. It did not use a Google
account or make live API calls.

## Changes and commits

| Commit | Change | Focused result |
| --- | --- | --- |
| `2c419e9e7a2df3214c8053d37859331861fad591` | Normalize legacy camel-case and newer `error.status: RATE_LIMIT_EXCEEDED` for HTTP retry classification and Drive-wide quota backoff. DriveStore still returns its first rate-limit response to its shared backoff rather than replaying it immediately. | Legacy, status-only, ordinary-permission, and cooldown tests pass. |
| `fce7b999333504b216dab595270c30e578d4db21` | Parse typed `google.rpc.ErrorInfo.reason` in `error.details[]`; prefer it to legacy `errors[].reason`, then top-level status. | Details-only rate-limit, ordinary `IAM_PERMISSION_DENIED`, and conflicting legacy/detail tests pass. |
| `a7f4701370573f4f6cb77cd64944db4ae8f8b227` | Limit mux media HTTP responses to `4,194,359` bytes: the full 4 MiB raw batch, 39-byte envelope header, and 16-byte GCM tag. Preserve a stricter caller limit, stop retrying oversize responses, and reject oversized results from stores that ignore HTTP context. | Exact maximum sealed batch accepted; one-byte-over plain and decompressed gzip bodies rejected after one attempt, including a misleading `Content-Length`; non-HTTP oversize rejected. Focused race and vet pass. |
| `2bdf41cc3137f3926a4af0b178f36edd65a0819b` | Start each socket reader with 32 KiB; admit a roughly 4 MiB buffer only after a full read, with a 64 MiB mux-wide large-buffer budget. Coalesce the first small read into the large buffer before framing; release large capacity after 2 seconds idle, on short sends, or on close. Denied readers keep operating with 32 KiB and retry admission. | Focused race tests, local SOCKS/mux tests, vet, microbenchmark, and paired full-tunnel MemoryStore runs pass. This **supersedes** `adc0297`, whose first small frame caused repeatable gap repair. |

Google's current [Drive limits guide](https://developers.google.com/workspace/drive/api/guides/limits)
calls for exponential backoff on 403/429 quota responses. Google's
[ErrorInfo documentation](https://docs.cloud.google.com/java/docs/reference/gax/latest/com.google.api.gax.tracing.ErrorTypeUtil)
identifies `google.rpc.ErrorInfo.reason` as the specific cause when present.

## Adaptive buffer evaluation

The eager reader allocated about 4 MiB per stream, including idle streams.
The corrected reader retains 32 KiB per idle reader and admits at most 16
default-size large readers within the 64 MiB budget. This budget covers only
socket read buffers, not the base buffers, frame queues, HTTP bodies, or other
stream state.

The first adaptive version (`adc0297`) emitted a separate priority frame from
its initial full 32 KiB read. In six paired 32 MiB bulk plus 24 short HTTP
request MemoryStore runs, it logged a receive gap repair in **6/6 candidate
arms versus 0/6 baseline arms**. The corrected path coalesces that read before
priority classification. In six new alternating pairs, it logged **0/6
candidate gap repairs versus 1/6 baseline**. All 12 runs verified the exact
32 MiB body by SHA-256 and all 24 short requests, with zero failures. Median
bulk rate was 1,114.2 candidate versus 1,121.9 baseline simulated Mb/s;
median short-request p95 was 132.2 versus 132.7 ms. Median upload/download/list
calls were 27/26.5/15.5 candidate versus 27/27/16 baseline. A separate
64 MiB plus 24-short ABBA had exact bodies and zero failures or gap repairs
in all four arms; timings varied enough to avoid a speed claim.

A second localhost full SOCKS/mux ABBA used a persistent 384 KiB warmup, eight
exact 128-byte echoes at 250 ms spacing, and four exact 4 MiB bulk HTTP
responses. All four arms verified the echoes and 16 MiB bulk bytes. Baseline
echo p95 values were 1,998.2/1,997.8 ms and candidate values were
1,997.2/1,747.5 ms; bulk rates were 31.6/31.5 and 31.6/31.6 simulated Mb/s,
respectively. No gap repairs occurred. These timings reflect the harness's
fixed operation delays and scheduling, not Drive latency.

Five 32 MiB read-allocation microbenchmark samples on an Apple M5 Max showed
eager 630–680 microseconds/op with about 4,194,306 B/op and one allocation;
adaptive 607–638 microseconds/op with 4,227,072 B/op and two allocations.
The distributions overlap. The extra 32 KiB allocation belongs to active
bulk readers; quiet readers avoid the large allocation.

Validation commands included focused `go test -race ./internal/skirk` runs,
socket-buffer race repetition (`-count=10` before the correction and focused
race again after it), `go vet ./internal/skirk`, `git diff --check`, and the
localhost `tools/skirk_memory_bench.go` and `tools/persistent_bench` binaries.
No full package test run or authenticated test was used in this isolated worktree.

## Verdict and limits

Promote the **corrected** adaptive commit `2bdf41c` after the three preceding
commits; do not promote `adc0297`. The local evidence supports the memory
admission bound and shows no repeatable local throughput or text regression.
It does not establish live Drive speed, quota cost, or 200,000-user capacity.
On flaky links, repeated idle demotion can reallocate large buffers; a live
matched transport test remains the performance gate.

The 4 MiB sealed cap applies to mux object downloads and direct
`GetNormalByID`, not arbitrary `DriveStore.Get`, generic `GetByID` outside the
mux, or range downloads. Those paths should receive their own operation-sized
limits before any claim of a global media-memory bound. Future Google error
envelopes may also need additional structured reason handling.
