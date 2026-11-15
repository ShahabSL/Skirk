# Sustained end-to-end recovery stress v1: incomplete comparison

The first candidate completed the exact 100 MiB file at **26.007 Mb/s**, all 100 echoes and its origin-side-effect proof, despite one real metadata timeout. Both main controls completed cleanly at **19.660 and 22.290 Mb/s**. The final candidate was interrupted after ten verified echoes by a bare upload deadline that **v1 did not permit**. There is no balanced candidate median or winner.

| Arm | Completed file | Full echo request p95 | Scheduled full echo p95 | Estimated units | Observed API result |
|---|---:|---:|---:|---:|---|
| Candidate 0 | 26.007 Mb/s | 7.357 s | 7.362 s | 132,195 | Completed with one permitted metadata timeout |
| Actual main 1 | 19.660 Mb/s | 14.487 s | 14.492 s | 105,420 | Clean; zero reported API errors |
| Actual main 2 | 22.290 Mb/s | 14.186 s | 14.193 s | 117,970 | Clean; zero reported API errors |
| Candidate 3 | Incomplete | Not reported | Not reported | 24,430 last observed | V1 rejected bare upload timeout; three pre-stop timeout logs, one stale periodic error counter |

Each arm requested a 100 MiB download plus 100 distinct 64-byte echoes, offered every 1,250 ms on one persistent TCP stream after a verified 128 KiB prelude in each direction. Bulk began one second before the text offers. The limits remained 300 seconds overall, 180 seconds for the file, 60 seconds without progress, and local estimates of 125,000 units per rolling minute, 150,000 per arm and 600,000 per cohort. Up to eight eligible timeouts per arm, four per rolling minute and sixteen per cohort were permitted; a cap-triggering event remains observed and rejected.

Request p95 covers the complete verified echo response from actual dispatch; scheduled p95 starts at the intended offer time and retains queue delay. Exact samples, file checkpoints, runtime options and per-role quota snapshots are in the [sanitized JSON](sustained-end-to-end-recovery-stress-2026-11-15.json). Units include warm-up and the observed tail, exclude the external cleanup commands, and remain local estimates rather than Google billing.

All three complete arms verified the exact file hash and all 100 echoes. Their origin counters were unchanged before and after every owned writer, benchmark and curl child was reaped: exactly one GET start/completion, one echo TCP connection, one prelude plus 100 frames, zero failed completions, unexpected HTTP or overflow. These counters observe this process epoch; they do not count failed TCP dials or establish restart exactly-once behavior.

Candidate 0 reported actual slot processing, sender ACK and asynchronous handoff/publication activity. Both sides recorded the actual finite 320-GET refusal and writer PATCH retirement; immutable fallback publications were retained. The full application workload still completed. This establishes continuation under the measured finite session, not indefinite slot life or a maximum latency guarantee.

The final candidate's original rejection was `unclassified_read_or_upload_failure`. Its client logged a six-second metadata deadline, a ten-second mutable PATCH deadline and a ten-second **bare** `op=upload` deadline. The later mux result identified a 151-byte priority object with `idempotent=false`. V1 could accept eligible observed upload URLs, but its exact bare-upload form was outside that policy. This is an observer interruption, not proof that the tunnel could never recover.

The mutable PATCH had entered its existing six-second exact-readback window when the upload rejection stopped the benchmark. Its later `invalid_readback_or_state` diagnostic followed parent cancellation during teardown; it is not evidence of a separate prior integrity failure. The report retains that pending outcome and the original trigger instead of treating shutdown consequences as the initiating fault.

The partial journal contains eleven records: an observation-start record plus **ten verified small responses**. The origin processed fifteen text frames plus the prelude; those five additional frames had no verified response receipt before interruption. No file checkpoint exists. The subsequent origin failed-completion counter reflects the interrupted file writer. There is no partial-arm percentile or inferred throughput. Last periodic counters reported one API error for this arm despite its three pre-stop timeout logs; across the cohort, four timeout logs contrast with two last-periodic errors. This publication lag remains explicit.

All four native teardown proofs showed every owned writer and child stopped before external Drive cleanup. The wrapper deleted 35 residual mux objects and zero slot objects. All eight exact mux/slot prefixes were empty immediately and after 31 seconds; copied profiles were removed. Cleanup completed at 22:38:21 UTC. These resource proofs are independent of the failed fourth workload.

The candidate executable is SHA-256 `2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b`, clean source `8c9829dc9ccbe98d62c4357d60ab762e02ac6373`; actual main is `34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`, source `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`. The observer derives from `d4471c8`, with root integration and the writer-proof wrapper checked before launch. No competing payload experiment, compiler, test or emulator ran during timing. Source editing and ordinary desktop background activity remained; this is one machine and network path.

The prior strict and read/normal-only failures remain unchanged. A future separately labelled v2 may observe a narrowly source-inferred bare upload class; it cannot reclassify this v1 attempt. Physical priority creates remain non-idempotent at the API layer, with logical duplicate handling and cleanup costs. This incomplete cohort establishes neither an architectural ceiling nor a capacity claim.
