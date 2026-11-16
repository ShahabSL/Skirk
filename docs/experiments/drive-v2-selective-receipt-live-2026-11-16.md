# Drive v2 selective receipts: interrupted live comparisons

The bounded FSH2 prototype adds an authenticated 4,096-bit selective
processing-receipt bitmap to the existing Drive v2 priority queue. It can
release processed claims behind a stalled earliest claim in local adversarial
tests. This report tests its ordinary mixed-traffic cost through the pinned
`www.google.com` front; it does **not** establish a live fairness win. FSH1
and FSH2 both used the corrected 16 KiB encoded-snapshot admission cap, so
the controls here must not be pooled with earlier window-eight controls.

Both campaigns used the same frozen test executable, SHA-256
`e26ce5fcbb67cb975a648790925993e408f8819447cf2444495abbd739877de5`,
from test-only source `c71184d6643152bb029ec43fc551b39c3b78f758`.
The order in each was FSH2 / FSH1 / FSH1 / FSH2, with one fresh owned
Drive mailbox generation per arm and 61 seconds between arms. The workload
was a real local SOCKS/TLS tunnel carrying one 16 MiB download, one 16 MiB
upload, a warm 64-byte echo, and twenty scheduled 64-byte echoes during each
bulk direction. The account, payload seed, 100 ms active polling, window
eight, fallback, route, and conservative 280,000-unit rolling-minute guard
were fixed within each campaign. The campaigns used different secret payload
seeds and fresh Drive files. The private ledgers are
`.skirk-runs/drive-v2-selective-20261116h/` and
`.skirk-runs/drive-v2-selective-20261116i/`.

The table reports nearest-rank scheduled text p95 from twenty frames in each
direction and decimal Mb/s from exact file completion. A dash means that
phase did not complete; its partial text observation is not a full-workload
performance sample. `Upper` is the local eight-attempt conservative Drive
reservation, **not** Google's billed quota.

| Campaign/order | Protocol | Complete | Text p95 down/up | File down/up | Drive calls / whole-arm upper |
| --- | --- | --- | ---: | ---: | ---: |
| H first | FSH2 | yes | 1.404 / 1.250 s | 30.628 / 39.952 Mb/s | 609 / 217,520 |
| H second | FSH1 | yes | 1.764 / 1.560 s | 25.908 / 38.367 Mb/s | 673 / 241,600 |
| H third | FSH1 | **no** | 1.646 s / — | — / — | 537 / 176,560 |
| I first | FSH2 | yes | 1.684 / 1.824 s | 22.092 / 35.032 Mb/s | 586 / 204,000 |
| I second | FSH1 | yes | 1.506 / 1.421 s | 24.795 / 28.057 Mb/s | 597 / 210,600 |
| I third | FSH1 | yes | 1.390 / 1.242 s | 26.910 / 34.309 Mb/s | 586 / 213,600 |
| I fourth | FSH2 | **no** | 1.248 s / — | — / — | 426 / 137,520 |

Every complete arm matched both file hashes and all 41 echoes, had one
origin GET and POST with zero bad bodies, joined both writers, and verified
that its exact owned Drive prefixes were empty immediately and after 31
seconds. Every interrupted arm still joined its writers and passed the same
exact cleanup checks; its origin received one GET, no POST, and 21 verified
echoes before cancellation. No copied test config or access-token files
remain. The secondary Cloud project's billing was disabled and these
Drive-only campaigns did not enable Firestore or create a database.

Campaign H stopped in its third arm after four simultaneous 1 MiB bulk
upload attempts reached Skirk's 28-second per-attempt deadline. One retry
then received HTTP 502 through the selected front. The strict research
guard canceled the arm on that unexpected status; the 16 MiB download and
the later upload/text phase did not complete. Campaign I stopped in its
fourth arm on a fronted HTTP 502 during the download, without preceding
timeouts. The guards were below their call and rolling-minute unit limits;
neither failure was a Google `429` or a local quota denial. The source of
the 502 responses cannot be assigned uniquely to Drive, the front, or the
network from these observations. The guard intentionally cancels on any
unexpected provider status, so the runs do not show whether product retries
would have recovered. Both 502s and all four timeouts remain in the ledgers.

**Decision:** five of seven attempted arms completed, including both FSH1
controls in campaign I, but neither campaign yielded a complete ABBA pair.
The successful-arm rows are useful diagnostics; their apparent speed and
quota differences are **not** a causal FSH2-versus-FSH1 winner. There was no
measured selective nonprefix-release count in these ordinary live workloads.
Keep FSH2 test-only and retain window-eight FSH1 as the better established
Drive-only mixed-traffic result. Next, test bounded recovery from an injected
502/lost response with exact-content readback and a full file; then use a
separately labeled multi-stream pressure/fault workload to exercise FSH2's
fairness claim before another live speed promotion attempt.
