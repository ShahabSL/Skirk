# Apps Script opaque echo through the Google front

The owned temporary Apps Script endpoint passed all twelve bounded AEAD echo
exchanges through `www.google.com`, including every Content Service redirect.
This proves a free Google-fronted request/response carrier is reachable on the
same pin used by Drive. It does not yet prove an arbitrary TCP transport,
reliable stream delivery or superiority on mixed traffic.

The probe used source `dbc52b9`, test-binary SHA-256
`e7927ee0b0bf14da490ffa33832314f830d882f6b31f0554ec38cfd5c163e325`,
route `google_front_pinned` and existing pin `216.239.38.120`.
Each exchange POSTed ciphertext to logical authority `script.google.com`,
received HTTP 302, then manually GETed the one-time response from
`script.googleusercontent.com`. Both hops retained TLS SNI and connection
destination front `www.google.com`; there was no direct-route fallback.
All 24 measured HTTP operations used HTTP/2 and one physical attempt each.

| Plaintext size | Samples | Round-trip observations | Warm median |
| --- | ---: | --- | ---: |
| 64 bytes, new client | 1 | 1,077.703 ms | — |
| 64 bytes, reused client | 5 | 624.779, 913.552, 650.347, 529.330, 577.365 ms | 624.779 ms |
| 16 KiB | 2 | 623.176, 635.255 ms | 629.216 ms |
| 256 KiB | 2 | 804.882, 814.141 ms | 809.512 ms |
| 1 MiB | 2 | 1,077.115, 1,061.150 ms | 1,069.133 ms |

All returned nonces, ciphertext authentication tags and decrypted bytes
matched. The local AES-GCM key never left the probe. Payloads were generated
random test data, not user content. The server only checked its temporary
shared secret and reflected the opaque envelope; it did not decrypt or fetch
anything. A one-MiB echo carries approximately 7.85 Mb/s of verified payload
per direction over its entire transaction, but this is not a steady-state
file upload/download benchmark. There are too few samples to claim tail
percentiles or a minimum attainable latency.

The service used the authorized secondary account, a 1.5 MB request bound and
three-hour expiry, with fresh user confirmation before public owner-executed
deployment. No Drive, Gmail, URL Fetch or other OAuth scope was granted and no
billing was enabled. The twelve echoes consumed twelve script POST executions
and twelve response GETs, not URL Fetch calls. A real external TCP-node bridge
would add URL Fetch authorization, another network hop, deployment work,
per-user fetch quota and its own reliable protocol. Google's current fetch
limits remain [documented separately](https://developers.google.com/apps-script/guides/services/quotas).

The existing Full Tunnel project's response-loss problem must not be copied.
A Skirk variant needs bounded retained responses, cumulative ACKs, write-offset
deduplication, replay-safe retries, cancellation and session cleanup. Only an
owned, authenticated encrypted relay with those properties should proceed to
the same persistent-message plus bulk comparison. Comparing this unloaded
echo's median with Drive's loaded mixed-workload p95 would be misleading.

Cleanup completed: the UI confirmed deployment archival and recoverable
project removal from active projects. A single additional secret-authenticated
POST through the same pin/front and HTTP/2 returned HTTP 404 in 396.667 ms,
confirming no active echo. Thus the bounded run made thirteen POSTs total,
including cleanup validation. Private endpoint, secret/settings and recovery
ledger files were removed only after that proof. The script remains in
recoverable Trash; it was not permanently purged. Google documents that
versioned deployments are archived rather than deleted.
[Deployment lifecycle](https://developers.google.com/apps-script/concepts/deployments#archive_a_versioned_deployment).

Sanitized per-hop timings and cleanup evidence are in
[`apps-script-front-echo-live-2026-11-15.json`](apps-script-front-echo-live-2026-11-15.json).
Ignored raw logs and UI archive evidence remain under
`.skirk-runs/apps-script-stage1/`. The production transport and defaults were
not changed by this feasibility test.
