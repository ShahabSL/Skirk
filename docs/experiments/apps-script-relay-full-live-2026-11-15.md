# Full Apps Script relay: mixed SOCKS, TLS, files and chat

All four measured variants completed an exact 16 MiB download, an exact 16 MiB upload and 25 exact 64-byte echoes through the selected Google front, Apps Script and one owned relay exit. This is **128 MiB across eight files and 100 echoes**, with zero client retries. The packed 2 MiB variant recorded the highest file rates in this small matrix: **4.116 Mb/s download and 2.700 Mb/s upload**. Packing reduced observed chat delays at both frame sizes, while increasing download time to first body byte at both sizes.

There was one observation per cell. The launch order was scalar 768 KiB, packed 768 KiB, packed 2 MiB, scalar 2 MiB. After the first two arms, the two remaining unstarted allocations were changed to 2 MiB frames; that choice was recorded before either new server or client started. This is an **unreplicated 2 × 2 matrix**, not a repeated ABBA comparison or a universal winner. Network and backend variability remain possible explanations for some differences.

| Variant | GET first byte | GET complete / rate | POST complete / rate | Total workload |
|---|---:|---:|---:|---:|
| Scalar 768 KiB | 12.005 s | 59.599 s / 2.252 Mb/s | 75.270 s / 1.783 Mb/s | 146.699 s |
| Packed 768 KiB | 33.109 s | 51.861 s / 2.588 Mb/s | 68.364 s / 1.963 Mb/s | 130.613 s |
| Packed 2 MiB | 8.263 s | 32.608 s / 4.116 Mb/s | 49.701 s / 2.700 Mb/s | 92.980 s |
| Scalar 2 MiB | 2.785 s | 38.373 s / 3.498 Mb/s | 67.087 s / 2.001 Mb/s | 121.886 s |

The bulk timer starts with the local HTTP request and ends after complete hash validation. Download first-body time measures the first received file byte. Upload first-body time measures the first byte of the origin's completion confirmation; it does not measure arrival of the first upload byte. The unchanged maximum mux object size was 4 MiB; the exchange frame carried at most 768 KiB or 2 MiB of binary piece data. The JSON preserves observed object counts separately.

The client established end-to-end TLS through actual SOCKS, verified the owned certificate, sent one warm echo, then offered twelve distinct 64-byte HTTP echoes every 250 ms on the same persistent TLS connection during each bulk direction. Every mixed echo completed while its bulk transfer was still active. This stream had **no 128 KiB demotion prelude**; it tests small priority traffic under bulk, unlike the earlier demoted persistent-flow Drive experiments.

| Variant / bulk direction | Request full median / max | Scheduled full max | Request first-body median |
|---|---:|---:|---:|
| Scalar 768 KiB / download | 5.925 / 14.397 s | 14.398 s | 5.925 s |
| Scalar 768 KiB / upload | 21.751 / 23.099 s | 23.101 s | 21.751 s |
| Packed 768 KiB / download | 2.367 / 3.017 s | 3.017 s | 2.367 s |
| Packed 768 KiB / upload | 7.657 / 11.238 s | 11.238 s | 7.657 s |
| Packed 2 MiB / download | 1.873 / 2.612 s | 2.612 s | 1.873 s |
| Packed 2 MiB / upload | 5.811 / 10.169 s | 10.169 s | 5.811 s |
| Scalar 2 MiB / download | 5.359 / 10.573 s | 10.573 s | 5.359 s |
| Scalar 2 MiB / upload | 14.229 / 27.276 s | 27.278 s | 14.229 s |

Request latency starts just before writing that echo's local HTTP request and ends after verifying its full response. Scheduled latency starts at its intended offer time, retaining local queuing. First-body latency ends at the first response byte. Exact values for all three measures and every sample are in the [sanitized JSON](apps-script-relay-full-live-2026-11-15.json). With twelve observations, the nearest-rank p95 is the maximum; this is not a stable tail estimate.

The owned origin counted exactly one bulk GET, one successful bulk POST containing 16,777,216 bytes, and 25 echo requests in each arm. Its bulk-start journal contained the matching GET/download and POST/upload entries. These counters support the observed successful HTTP workload; they do not count failed TCP attempts or prove restart exactly-once behavior.

| Variant | Script POSTs | Dispatches / completed H2 | Request + response upper bytes | Client retries |
|---|---:|---:|---:|---:|
| Scalar 768 KiB | 153 | 305 / 304 | 513,208,869 | 0 |
| Packed 768 KiB | 124 | 247 / 246 | 449,754,507 | 0 |
| Packed 2 MiB | 90 | 179 / 178 | 688,144,171 | 0 |
| Scalar 2 MiB | 143 | 285 / 284 | 989,371,885 | 0 |

Each arm's raw `transport_errors` counter is **1**, preserved unchanged. The frozen executable predates the later separation of cancellation counters. In each arm the last observed Google trace had status zero and `canceled=true` during owned closing, after the workload; completed HTTP/2 responses were exactly one fewer than visible dispatches. This is a documented interpretation of the retained trace, not a rewrite to a zero-error counter. All client result statuses were passed, all sample ledger writes succeeded, and the client reported no retries.

Limits remained 256 Apps Script POSTs per allocation, 1,024 across the campaign, 4,096 conservative HTTP attempts per arm and 1 GiB combined request/response body upper bound per arm. The owned relay separately bounded calls and encoded bodies. The fixed campaign expiry was 21:51:21 UTC. Configured remaining lifetimes were 842, 570, 338 and 199 seconds in launch order; the client additionally capped its own run at eight minutes. Every arm completed within its bound. The carrier reserves up to eight request bodies per visible dispatch and the full permitted response size on each hop, even for an empty reply. Those are conservative local accounting bounds, not observed Google acceptance or billing. Larger frames can consume more of that response reservation budget despite making fewer requests; scalar 2 MiB retained 989,371,885 upper-bound bytes under the 1 GiB cap.

Three earlier first-arm setup generations remain separate from the measured matrix: OAuth 401, Cloudflare rejection of a Python User-Agent with code 1010, and strict reservation rejection of an extra field before the client consumption marker. These are retained setup failures, not three failed transport trials.

All four owned server/connector pairs joined with status zero. Each restored ingress to 404; exit streams, retained broker payload bytes, assembling objects, cached replies and live workers were zero after close. Some objects were reclaimed at broker close rather than by foreground deletion; the JSON retains both before/after counters. Global Apps Script and DNS cleanup is recorded separately below.

All arms used source `a805fb016b0563d243cf11b2fcf08a6dfb65a2d0` and SHA-256 `6710666a393895d6b2d10b6d681791887f674df56bff7fa8ba3f46fe3ee9fe1f`. No intentional competing payload benchmark, build, test or emulator ran during timed arms; normal desktop background activity was uncontrolled. This relay requires Apps Script external-request authorization and an owned reachable HTTPS exit in addition to the user's selected Google route. It is a research transport, with no same-cohort Drive baseline, multi-client test, long-session proof, restart proof, or capacity claim.

The exact Cloudflare tunnel was deleted and its connector token removed; the temporary DNS record deletion was verified in the UI at 21:53:51 UTC. Apps Script deployment was archived and its project recoverably trashed. A separate fronted HTTP/2 inactive check returned 404 in 458.567 ms at 21:55:01 UTC. The one inactive-check POST is separate from the 510 payload POSTs: 511 observed in total, without counting unrelated setup traffic as payload. The exact temporary app’s external-service grant was revoked and the research tabs closed; unrelated grants were untouched. At 21:57:12 UTC, all 20 copied private research files were removed, with zero copied settings/runtime configurations remaining and no connector token. The original Drive kit was preserved. Exact cleanup proof paths and controlled flags are in the JSON.
