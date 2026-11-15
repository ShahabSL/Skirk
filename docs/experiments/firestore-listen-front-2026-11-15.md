# Firestore Listen through the selected Google front

The repaired primitive delivered all six exact encrypted states and shut down cleanly. Its five warm write-to-listener observations had a median of **248.227 ms**. The earlier attempt also delivered all six states, with a 233.385 ms warm median, but **failed its local observer teardown**. Both attempts remain in the record; these samples do not establish a latency ranking.

| Attempt | Outcome | Cold callback | Five warm callbacks (ms) | Warm median |
|---|---|---:|---|---:|
| Initial | Failed local EVENT_DELIVERY/shutdown validation; REMOVE acknowledgement absent | 969.608 ms | 326.311, 371.637, 233.385, 230.786, 214.228 | 233.385 ms |
| Repaired | Passed state validation and joined observer shutdown | 1,360.767 ms | 252.742, 248.227, 330.980, 233.188, 240.159 | 248.227 ms |

Each attempt wrote six distinct 2 KiB random plaintext states into one newly owned document, using fresh AES-GCM nonces. The receiver verified the exact document identity, ciphertext, authenticated plaintext, sequence and hash. No prior duplicate state was observed. The official Node WebChannel observer used a local authenticated bridge; every Google-facing dispatch used the pinned `www.google.com` route. There was no direct-route fallback.

`ListenMS` starts immediately before the Go PATCH and ends at the Node document callback timestamp, using the same host's wall clock at approximately millisecond resolution. The first callback also includes writing the initial document, starting Node and establishing its listener; it is a cold-start observation. `WriteAckMS` measures the PATCH response separately. A callback can arrive before that response. These are encrypted-state notification measurements, not application echo RTTs or a complete TCP tunnel benchmark.

| Accounting | Initial | Repaired |
|---|---:|---:|
| Visible Google dispatches | 21 | 23 |
| Completed HTTP/2 responses | 20: 18 × 200, 2 × expected 404 | 21: 19 × 200, 2 × expected 404 |
| Owned canceled dispatches without response | 1 | 2 |
| Conservative internal-attempt upper bound | 168 | 184 |
| Encoded request-body upper bound | 140,592 B | 141,752 B |
| SDK observer request count | 15 | 14 |

The dispatch limit was 36 active calls and 40 including cleanup, with a 1 MiB per-response bound. These are local dispatch and conservative retry bounds, not observations of Google acceptance or billing. Canceled shutdown requests remain visible; no transport-error flag was observed in either dispatch journal.

The repaired observer joined in **418.681 ms**, exited with status zero, acknowledged target removal, closed its channel and retained zero pending requests, events or SDK timers. Two local owned-shutdown abort diagnostics remain recorded; both had `closing`, `owned_shutdown_abort` and `signal_aborted` set. The initial failed observer did not acknowledge removal and reported a local error, despite its six valid deliveries.

Both attempts deleted the exact owned document and confirmed its absence immediately and after 31 seconds. The temporary database was also absent on both checks. Firestore returned to its previous disabled API state; borrowed access tokens and observer configuration were removed. Neither attempt enabled billing or changed existing user data. The repaired cleanup completed by 21:29:07 UTC.

The initial run began at 20:43:18 UTC from source `577f67a`; the repaired run began at 21:27:42 UTC from `f60802f`. Exact executable hashes, every sample, observer counters, selected cleanup fields and private evidence locations are in the [sanitized JSON](firestore-listen-front-2026-11-15.json). Compilers, tests and emulators were stopped before both launches. Ordinary desktop background activity was uncontrolled; a read-only external Claude audit could receive small background traffic during the first attempt.

This is a promising small-state push primitive. It does not prove file transfer, full SOCKS/TCP recovery, multi-client capacity, restart durability, or a production deployment model. It uses Cloud owner authorization rather than the existing `drive.file` grant; least-privilege provisioning, retention, duplicate handling, quotas and full mixed-traffic transport behavior still require separate work.
