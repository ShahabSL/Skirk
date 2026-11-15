# Sustained mixed traffic: complete burst-recovery comparison

All four arms completed an exact 100 MiB download, all 100 scheduled echoes and their origin/process cleanup proofs. The candidate bundle's descriptive two-run medians were **25.484 Mb/s** file goodput and **9.016 seconds** scheduled full-response p95, versus actual main's **19.686 Mb/s** and **20.545 seconds**. Estimated units increased **14.40%**, and the candidate retained **six real RPC errors versus zero for main**. This is a positive mixed-traffic result with a real cost/error tradeoff, not universal promotion.

| Launch order | File goodput | Request full echo p95 | Scheduled full echo p95 | Estimated units | RPC errors |
|---|---:|---:|---:|---:|---:|
| Candidate 0 | 25.017 Mb/s | 10.083 s | 10.083 s | 129,300 | 6 |
| Actual main 1 | 14.874 Mb/s | 27.948 s | 27.953 s | 109,070 | 0 |
| Actual main 2 | 24.497 Mb/s | 13.128 s | 13.137 s | 120,170 | 0 |
| Candidate 3 | 25.950 Mb/s | 7.949 s | 7.950 s | 132,945 | 0 |

| Descriptive median of two arms | Candidate | Actual main | Change |
|---|---:|---:|---:|
| File goodput | 25.4835 Mb/s | 19.6855 Mb/s | +29.45% |
| Request full-response p95 | 9.015992 s | 20.537872 s | −56.10% |
| Scheduled full-response p95 | 9.016415 s | 20.545152 s | −56.11% |
| Estimated units per arm | 131,122.5 | 114,620 | +14.40% |

These are medians of **two per-arm measurements**, including medians of per-arm p95s; they are not a pooled percentile or a confidence interval. Candidate file rates ranged from 25.017 to 25.950 Mb/s and scheduled p95s from 7.950 to 10.083 seconds. Main ranged from 14.874 to 24.497 Mb/s and 13.137 to 27.953 seconds. The wide control spread and small replication limit causal conclusions.

The treatment is the current research runtime **plus interaction-epoch scheduling and negotiated asynchronous metadata slots**, compared with clean actual main. It does not isolate slots from scheduling, other accumulated runtime fixes or dependency changes. Exact effective option fields and executable/source identifiers are retained in the [sanitized JSON](sustained-end-to-end-burst-recovery-2026-11-15.json).

## Workload, measurement and fixed limits

Each arm first verified a 128 KiB prelude in both directions on one persistent TCP stream, then downloaded an independently checked 100 MiB file while offering 100 distinct 64-byte echoes every 1,250 ms, with a one-second bulk lead. All four files passed exact size/hash checks; all 400 echoes and four preludes passed their byte checks. File goodput is decimal megabits per second over the complete file request. Request latency starts at actual echo dispatch; scheduled latency starts at its intended offer time and includes local queuing. Both end only after the complete response is verified.

This new experiment declared a fixed allowance of **32 eligible timeouts per arm, 16 per rolling minute and 64 per cohort** before launch. Earlier strict, v1 and v2 runs remain unchanged, including their failed four-per-minute experiment. The new allowance observes finite concurrent RPC waves; it does not change Google quotas, issue harness retries or waive actual errors.

All other ceilings stayed unchanged: 300 seconds overall, 180 seconds for a file, 60 seconds without progress, and local estimated-unit limits of 125,000 per rolling minute, 150,000 per arm and 600,000 per cohort. Authentication, quota/backoff, integrity, terminal, unclassified-error and incomplete byte/origin-proof conditions still stop the run. Units include observed warm-up/tail but exclude the external cleanup commands; they are local estimates, not Google billing or a mathematical quota guarantee.

## Chat under file load and afterward

Only 26–45 messages per arm were offered while the file was still active; the others were offered after it completed. The during-file group uses the actual bulk-future state at dispatch. A request in that group can finish after the file, retaining its full queuing cost.

| Arm | Offered during file | Scheduled full p95 | Offered after file | Scheduled full p95 |
|---|---:|---:|---:|---:|
| Candidate 0 | 27 | 13.626 s | 73 | 2.982 s |
| Actual main 1 | 45 | 31.679 s | 55 | 6.776 s |
| Actual main 2 | 27 | 15.584 s | 73 | 6.427 s |
| Candidate 3 | 26 | 11.699 s | 74 | 2.858 s |

The two-run medians for during-file scheduled p95 were 12.663 versus 23.631 seconds, a 46.42% reduction. After-file p95 medians were 2.920 versus 6.601 seconds, a 55.77% reduction. Group sizes and composition differ because files completed at different times; these are descriptive workload strata, not matched message-by-message attribution to a carrier.

## A timed-out PATCH was resolved and delivery continued

Candidate 0 retained all six failed RPCs: three physically non-idempotent priority multipart creates, two metadata reads and one mutable PATCH. Its revision-19 PATCH timed out, then the existing readback returned the **exact prepared sealed snapshot** with the expected target identity. The source-bound diagnostic recorded `exact_readback`, and the same writer continued. No error counter was subtracted and no bare error was presented as an observed wire request.

The receiver processed every scheduled message and file exactly as required at the application boundary. Before and after all owned children were reaped, every arm's origin counters showed one GET start/completion, one echo TCP connection and 101 framed messages: the prelude plus 100 echoes. No unexpected HTTP request, failed origin completion, duplicate connection/frame count or overflow was observed. This does not count failed TCP dial attempts or establish exactly-once behavior across restarts. Priority API creates remain physically non-idempotent; logical dedupe and exact-prefix cleanup are separate requirements.

## Continued delivery after finite slot retirement

Both candidate arms logged the actual 64-PATCH refusal and 320-GET refusal for each side. All messages still completed. The following cuts use **dispatch after both sides' recorded event plus a one-second precision margin**; the raw logs have one-second timestamps.

| Cut | Candidate 0: messages / scheduled full p95 | Candidate 3: messages / scheduled full p95 |
|---|---:|---:|
| After both writer budget refusals | 53 / 3.001 s | 56 / 2.963 s |
| After both reader budget refusals | 25 / 3.001 s | 25 / 2.798 s |
| Beyond both advertised two-minute bounds, plus margin | 6 / 3.059 s | 5 / 2.694 s |

These cuts overlap and must not be summed. The final four messages in every arm also started at least 120 seconds after mixed-workload start; that is a separate overlapping cut. A configured two-minute upper bound does not establish that a slot was alive before it. These observations establish bounded-session continuation through the ordinary immutable transport after slot retirement, not indefinite slots or per-message carrier attribution.

## Provenance, cleanup and scope

All four workload reports, atomic file checkpoints, frame journals, origin proofs and native teardown proofs are retained. The JSON includes byte sizes and SHA-256 hashes for each arm's evidence files, plus exact log-derived retirement offsets and frame indices. All owned Go writers, benchmark and curl children were reaped before external Drive cleanup. Seven residual mux objects and zero slot objects were deleted; all eight exact prefixes were empty immediately and after 31 seconds. Copied profiles were removed. Cleanup completed at 23:08:11 UTC.

The candidate was clean source `8c9829dc9ccbe98d62c4357d60ab762e02ac6373`, SHA-256 `2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b`. Main was `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`, SHA-256 `34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`. Observer source was `d5437ca`; root integration passed 254 Python checks before launch. Runtime binaries were unchanged from the preceding v2 cohort.

The comparison used one Mac/network path and the approved secondary test account. No competing payload test, compiler, test suite or emulator ran during timing; source edits and ordinary desktop background activity remained. There is no regional, multi-client, browsing/video, platform VPN, maximum-user or global speed/latency ceiling proof here. The candidate is promising for this workload, with extra estimated units and a recovered error burst; the earlier incomplete and failed experiments remain part of the decision.
