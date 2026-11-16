# Matched muxv5 versus Drive v2 window-eight: stopped first arm

Status: **incomplete campaign; no product-versus-v2 result**. The preset
rolling-minute quota guard stopped the first v2 arm during upload. No Drive
control or fourth-arm candidate was launched, and the quota cap was not
changed. This arm is a quota/correctness stop, not a partial latency sample.

The frozen test-only source was `b52f80a18756c7bd0f77d220440e955770378c29`;
the single test executable SHA-256 was
`f56831901420df5ea01551a606d4930d79f27bde2888e4ce022721269592976a`.
The approved secondary account's client and exit each refreshed their own
access token through the pinned route. Both separate read-only Drive preflight
calls returned HTTP 200 over HTTP/2. No preflight or workload HTTP 500 was
observed. The transport arm used the pinned `www.google.com` front and the
fresh owned Drive v2 files recorded in its private recovery ledger.

The first arm verified its 384 KiB established-stream prelude and exact 16 MiB
download. The origin saw one GET, one POST, one prelude, three text streams,
68 valid 64-byte echoes, and zero bad bodies. The client verified 61 of the
80 scheduled text echoes. The upload was canceled before its final exact-hash
proof. Both bulk phases recorded at least 1 MiB of origin progress during the
text schedule, but the second phase is incomplete and yields no p50/p95/p99
or file-rate comparison.

At the stop, the shared Drive gate had reserved 239,720 of its 240,000 active
rolling-minute upper units; the next dispatch needed 800 more. Its separate
40,000-unit cleanup reserve remained available. The final ledger counted 683
visible Drive calls, 108 cleanup calls, 261,800 conservative whole-arm upper
units, one guard denial, zero timeouts, and zero transport errors. The 52 API
errors were four expected conditional 412s and 48 expected cleanup 404s; the
observed statuses were 200, 204, 304, 404, and 412. The arm's `workload_ok`
is false, while `stop_ok`, `writers_joined`, and `drive_cleanup_verified` are
true. Exact mux namespace and both owned v2 files were absent immediately and
after the fixture's 31-second second check. No matched test process remained.

Raw private evidence is in
`.skirk-runs/matched-v5-v2-window8-20261116a/arm-v2-first/`:
`progress.jsonl`, `workload-outcome.json`, `summary.json`, `mux.log`,
`run.stdout`, `recovery-resources.json`, `drive-cleanup-targets.json`, and
`drive-v2-owned-targets.json`. The failed cohort's active lock remains as a
stop marker. The spent arm's access-token files were removed and its token
validity reset to an expired value. The three unstarted arm directories have
no access tokens. No product binary, root checkout, or `main` branch changed.
