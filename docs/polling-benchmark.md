# Bounded burst polling experiment

Tested locally on 2026-11-14 with a Mac17,6 (18 logical CPUs, 64 GiB) and Go
1.26.4. The reproducible harness is `tools/skirk_memory_bench.go`. It runs the
production mux over an in-memory mailbox, with configurable artificial delays
for upload, list, and download operations. It never calls Google Drive.

The experiment used 20 ms upload, 12 ms list, and 8 ms download delays. These
are deliberately smaller than the saved live Drive measurements, so the Mbps
figures below are **local simulation results**, not expected Internet speeds.
The quota estimates multiply operation counts by [Drive's published quota
costs](https://developers.google.com/workspace/drive/api/guides/limits); they
are not Cloud Console usage measurements.

## Short transfer and idle tail

One 1 MiB response and one small response were requested together. Each run
then waited 6.5 seconds to count extra mailbox operations. Three runs per
variant gave:

| Burst behavior | Small response time | List calls after settle | Estimated units after settle |
| --- | ---: | ---: | ---: |
| Previous 5-second burst tail | 157–160 ms | 138–140 | 15,555–16,255 |
| Bounded burst and corrected ticker | 193–195 ms | 43–44 | 6,305–6,655 |

The revised behavior stopped fast polling when the last stream closed. The
receive loop now updates its ticker whenever the desired interval changes,
including the transition back to normal polling after a wake event. Together,
these changes cut total list calls by about 69% in this test. The calls added
after the workload fell from roughly 116 to 16. The small response took about
35 ms longer. The result is sensitive to synthetic timing.

## Mixed traffic

Twenty-four small HTTP responses competed with a 32 MiB response. The tunnel
used a 16 MiB configured chunk size. Three runs per variant gave:

| Mode | Small response p95 | Simulated bulk rate | List calls during workload | Estimated units during workload |
| --- | ---: | ---: | ---: | ---: |
| Auto, burst off | 1,975–1,994 ms | 132 Mbps | 21–23 | 8,255–8,755 |
| Previous burst | 170–191 ms | 977–1,000 Mbps | 31–33 | 9,555–9,755 |
| Bounded burst | 200–220 ms | 1,054–1,060 Mbps | 31–32 | 9,555–9,655 |

The current burst setting still beats the no-burst setting in this simulation,
but it uses more calls while traffic is active. The bulk rates mainly reflect
memory and local scheduling. They cannot be used to estimate Drive throughput
or supported user count.

## Live Drive cross-check

The [November 2026 live comparison](research-2026.md#new-bounded-live-drive-comparison)
used the same `google_front` route and 32 MiB random HTTP body with burst
enabled on both client and exit, versus burst off. The two no-burst runs
delivered 35.793 and 36.434 Mb/s with overlapping small-request p95 first
byte of 3.025 and 2.682 seconds. The two burst runs delivered 22.939 and
26.981 Mb/s with p95 of 3.452 and 3.596 seconds. All HTTP requests succeeded
and both ends recorded zero Drive API errors. The burst process also reported
a higher recent estimated Drive unit rate. These sequential, short runs are
not a general proof, but they do not justify changing the no-burst default.

## Reproduce

```bash
go run ./tools/skirk_memory_bench.go -small=1 -bulk-mib=1 \
  -put-ms=20 -list-ms=12 -get-ms=8 -burst -settle-ms=6500

go run ./tools/skirk_memory_bench.go -small=24 -bulk-mib=32 \
  -put-ms=20 -list-ms=12 -get-ms=8 -burst
```

The JSON includes response time, bulk rate, operation counts, estimated quota
units, and optional half-second post-traffic snapshots. The regression tests
for stream-close burst behavior and ticker transitions are in
`internal/skirk/burst_poll_test.go`.
