# End-to-end stress v2: stopped after a real timeout burst

The first candidate stopped after **five real request timeouts exceeded the predeclared allowance of four per rolling minute**. It verified six small echo responses and no completed client file. The remaining main/main/candidate arms were not attempted. There is no candidate median, throughput result or winner.

This v2 policy accepted the narrowly source-inferred bare upload form that interrupted v1. The new stop was the actual `end_to_end_timeout_60s_cap`, not a parser rejection or Google quota exhaustion. It does not establish whether the tunnel would eventually recover under a different, separately declared experiment.

| UTC | Client operation | Count | Timeout |
|---|---|---:|---:|
| 22:45:20 | Download | 2 | 6 s each |
| 22:45:20 | Metadata read | 1 | 6 s |
| 22:45:24 | Priority multipart upload | 1 | 10 s |
| 22:45:26 | Priority multipart upload | 1 | 10 s; fifth event stopped the arm |

All five events remain observed; only four were permitted. Last periodic metrics reported three API errors and 21,295 estimated units, so the two later upload failures remained ahead of counter publication. The physically non-idempotent priority creates retain their source-bound logical-dedupe classification; the report does not claim API idempotency, observed request URLs or hidden wire-attempt counts for bare errors.

The partial receipt journal contains an observation-start record and six verified text responses. The origin accepted one echo TCP connection, the prelude and twelve text frames. Its GET writer completed into the exit-side path, but **no client file checkpoint exists**. An origin write is insufficient evidence of an end-to-end download. The remaining frames had no verified return receipt before interruption; no partial latency percentile is published.

The planned workload and all original ceilings were unchanged: exact 100 MiB download, 100 distinct 64-byte echoes at 1,250 ms intervals after a verified 128 KiB prelude in each direction, one-second bulk lead, 300-second overall / 180-second file / 60-second progress bounds, and local estimates of 125,000 units per rolling minute, 150,000 per arm and 600,000 per cohort. Quality ceilings were eight eligible timeouts per arm, four per rolling minute and sixteen per cohort.

Every owned writer and benchmark/curl child was proven stopped before cleanup. The wrapper removed 33 residual mux objects, confirmed both exact prefixes empty immediately and after 31 seconds, and removed copied profiles. Post-stop origin counters and the incomplete final origin proof remain in the [sanitized JSON](sustained-end-to-end-recovery-stress-v2-2026-11-15.json).

The frozen runtime remained clean source `8c9829dc9ccbe98d62c4357d60ab762e02ac6373`, executable SHA-256 `2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b`; v2 observer source was `dc81870`. V1 records remain unchanged. A future burst-recovery cohort with larger **predeclared** error allowances is a different experiment; it cannot turn this failed v2 arm into a pass. API quotas, byte limits, timing bounds, correctness and terminal/authentication/integrity stops remain separate requirements.
