# Final async slots versus actual default and clean main

The asynchronous package improved completed-run text tails, but it is not a universal winner. Against clean main it traded lower text latency for lower file goodput and about 9% more estimated Drive units. One async upload arm failed on the unchanged immutable bulk path. Keep this mode opt-in; these results do not establish a minimum possible latency, maximum speed or capacity for 200,000 users.

## Frozen scope

All current/default/candidate arms used clean source `42e6742e93d3376a03d4354a4b172236ff1b1fc4`, CLI SHA-256 `c6c42930544e03694fdaaaa1fd1e14e538ceb47e598250c5cc7e26e52503c9d9`. Clean main was `2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a`, binary `34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`. No binary was rebuilt or substituted during this matrix. The current binary includes the stale-snapshot, stopped-writer and late-ACK fixes; primary mode and state-aware upload hedging stayed disabled.

The candidate enabled slot negotiation/delivery, asynchronous ownership and interaction epochs. Controls cleared those and every other experiment. The source kit retained its saved `auto` profile and 8 MiB chunk setting; runtime mux batching may use smaller objects. Both sides used the approved isolated secondary account, pinned `www.google.com` front and the same credentials/route within each comparison. No main account or external user traffic was tested.

Every arm attempted one SHA-256-verified 16 MiB file, a byte-exact 128 KiB prelude in each direction on one persistent TCP connection, then twelve distinct length-framed 64 B echoes pipelined every 250 ms after a 1 s bulk lead. Prelude traffic demoted the persistent stream before the mixed test. Download order was control/candidate/candidate/control; upload was candidate/control/control/candidate. Equal 8 s tails were retained. The same-binary pairs included an equal exit probe; both clean-main arms omitted it because main lacks the command. Compare costs within pairs.

Request first-byte latency starts at frame send and ends at the first echo-header byte. Full-echo latency ends after all 64 bytes match. Scheduled variants start at intended dispatch, including local queuing. The table uses full-echo values. Nearest-rank p95 on 12 messages is the maximum; a median of two arm p95s is descriptive, not a population p95. File goodput is verified payload bits/transfer time in decimal Mb/s.

Agent compilers/tests were stopped throughout the final arms. Normal desktop/UI background was uncontrolled; start/end load averages are retained. The guarded harness stopped a comparison on any API error or quota backoff. Local request counters include warmup/tail and Skirk-visible retries, but can omit opaque retries inside HTTP2; they are estimates, not Google wire/billing totals. Explicit final cleanup is reported separately.

## Results

All 16 planned arms were attempted. Fifteen completed: fifteen exact 16 MiB files, 180 exact echoes and fifteen full prelude reports. The failed sixteenth arm remains in the evidence; no latency/file-rate zero or successful average replaces its missing report.

| Baseline / direction | Completed control/candidate | Full-echo request p95 s | Full-echo scheduled p95 s | File Mb/s | Estimated units/arm |
| --- | ---: | ---: | ---: | ---: | ---: |
| default / download | 2/2 | 10.103 → 4.623 | 10.105 → 4.628 | 11.175 → 15.748 | 23135 → 23370 |
| default / upload | 2/1 | incomplete | incomplete | incomplete | incomplete |
| main / download | 2/2 | 6.695 → 5.509 | 6.701 → 5.512 | 14.415 → 13.203 | 18810 → 20495 |
| main / upload | 2/2 | 7.952 → 4.634 | 7.955 → 4.635 | 12.653 → 12.448 | 18485 → 20120 |

Actual-default download scheduled full-echo p95 fell 54.2%, with file median +40.9% and units +1.0%. Its controls varied substantially, so this is not a causal estimate or evidence that default is uniformly slower than main. Clean-main download text fell 17.7%, file fell 8.4%, units rose 9.0%; clean-main upload text fell 41.7%, file fell 1.6%, units rose 8.8%. Those are separate comparisons, not pooled samples.

| Pair / arm | Mode | File Mb/s | Echo request/body p95 s | Echo scheduled/body p95 s | Units | Errors |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| default download / 0-control | control | 14.196 | 7.225 | 7.228 | 21710 | 0 |
| default download / 1-candidate | async | 14.850 | 4.722 | 4.727 | 24785 | 0 |
| default download / 2-candidate | async | 16.646 | 4.524 | 4.528 | 21955 | 0 |
| default download / 3-control | control | 8.153 | 12.981 | 12.982 | 24560 | 0 |
| default upload / 0-candidate | async | 12.284 | 4.143 | 4.145 | 23780 | 0 |
| default upload / 1-control | control | 12.499 | 7.440 | 7.442 | 20410 | 0 |
| default upload / 2-control | control | 9.875 | 10.323 | 10.331 | 23460 | 0 |
| default upload / 3-candidate | async | incomplete | incomplete | incomplete | 24890 | 1 |
| main download / 0-control | control | 14.073 | 6.421 | 6.424 | 19260 | 0 |
| main download / 1-candidate | async | 12.726 | 5.339 | 5.342 | 20350 | 0 |
| main download / 2-candidate | async | 13.679 | 5.680 | 5.681 | 20640 | 0 |
| main download / 3-control | control | 14.758 | 6.968 | 6.977 | 18360 | 0 |
| main upload / 0-candidate | async | 11.822 | 5.118 | 5.119 | 19855 | 0 |
| main upload / 1-control | control | 12.687 | 8.546 | 8.548 | 18110 | 0 |
| main upload / 2-control | control | 12.619 | 7.357 | 7.362 | 18860 | 0 |
| main upload / 3-candidate | async | 13.075 | 4.149 | 4.151 | 20385 | 0 |

## Retained failure and carrier activity

The final same-binary upload candidate stopped after a normal immutable 1,048,631-byte upload timed out in 28.002 s with zero upload-slot wait. Final counters were 11,855 client units/one timeout and 13,035 exit units/zero errors; neither peer reported active quota backoff. The mixed report was not completed. This is a real incomplete arm, but it does not isolate a causal slot regression: the failed operation used the unchanged bulk carrier. The first candidate and both controls completed; they are not sufficient for an upload candidate median.

All seven successful candidate arms logged actual async handoff, storage publication ACK, receiver processing and sender processing ACK. Merely enabling a flag did not count as an active carrier result. The 1.5 s processing deadline still caused some exact immutable fallbacks; publication is not destination/application delivery. Full per-role activity and call/unit/operation counters are retained in the JSON.

## Cleanup and provenance

All 32 exact generated mux/slot prefixes were verified empty and every generated config copy removed. Explicit cleanup deleted 117 residual objects, including one slot file. Same-binary upload required an honest exception: its first 31 s delayed sweep found one late mux object; that exact object was deleted, and a second 31 s sweep found all eight prefixes empty. The other three comparisons were empty at the first immediate and 31 s delayed checks. These checks establish final cleanup for these sessions, not indefinite automatic garbage collection.

Private raw directories under `slot-async-wt/.skirk-runs/`:

- `final-async-vs-default-download16m-20261115/`: summary, all peer logs/metrics, completed or interrupted workload, host condition, exact cleanup.
- `final-async-vs-default-upload16m-20261115/`: summary, all peer logs/metrics, completed or interrupted workload, host condition, exact cleanup.
- `final-async-vs-main-download16m-20261115/`: summary, all peer logs/metrics, completed or interrupted workload, host condition, exact cleanup.
- `final-async-vs-main-upload16m-20261115/`: summary, all peer logs/metrics, completed or interrupted workload, host condition, exact cleanup.

[Sanitized numeric evidence](final-async-default-main-2026-11-15.json) includes every attempted arm and both latency definitions. The dependency-only HTTP2 diagnostic is separate: main uses `x/net 0.54`, current uses `0.61`; the latter changed flow-control internals. No dependency cause or connection-local cause is inferred here.
