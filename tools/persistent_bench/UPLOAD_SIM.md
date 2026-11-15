# Local multi-client upload experiment

This experiment exercises five client muxes and one exit over a shared `MemoryStore`. Each client warms a persistent TCP stream past the 64 KiB priority threshold, pipelines six exact 128-byte echoes at 250 ms intervals, and sends one SHA-verified 16 MiB HTTP upload. The exit verifies every upload. All runs use 80 ms small-object PUT, 60 ms GET, 30 ms list, and 250 ms poll delays. Large object PUTs additionally wait 1 second per MiB behind a shared six-slot semaphore. No Google Drive call is made.

Run the benchmark with `go run ./tools/persistent_bench` and these common options:

```
-clients=5 -bulk-direction=upload -bulk-mib=16 -bulk-requests=1
-messages=6 -interval-ms=250 -put-ms=80 -get-ms=60 -list-ms=30
-poll-ms=250 -bulk-put-ms-per-mib=1000 -bulk-put-slots=6
```

Use `-chunk-mib=16` for the existing 4 MiB normal batch cap or `-chunk-mib=2` for a 2 MiB cap. Add `-interaction-epoch` for the epoch arm. Two runs of each combination yielded:

| Mode | Normal batch | Text p95 (ms) | Bulk (Mb/s) | Estimated units | Gap repairs |
| --- | ---: | ---: | ---: | ---: | ---: |
| Default | 4 MiB | 2,226 / 8,301 | 40.8 / 40.8 | 121,700 / 120,900 | 9 / 9 |
| Epoch | 4 MiB | 1,478 / 1,831 | 37.9 / 41.2 | 137,200 / 126,200 | 23 / 9 |
| Default | 2 MiB | 4,229 / 5,013 | 43.4 / 45.7 | 119,800 / 117,000 | 3 / 8 |
| Epoch | 2 MiB | 1,728 / 1,486 | 44.0 / 43.4 | 126,300 / 127,300 | 12 / 7 |

Every run verified 30 in-order echoes and 80 MiB of uploaded data. All five bulk transfers remained active through the text burst. There were no gap timeouts, terminal failures, transport errors, hash failures, or objects left after cleanup. The raw JSON for the second run of each combination is under `.skirk-runs/epoch-upload-sim-20261114/`; this ignored directory must not be committed.

In this model, 2 MiB batches consistently improve bulk completion. Epoch text p95 at 2 MiB overlaps the 4 MiB range, so the evidence does not establish a text improvement. The unit totals include an exact one-page synthetic class list and vary with run duration; they do not predict Drive's paginated search or quota. The six-slot linear PUT delay is a sensitivity model, not a measurement of Drive. The simulator also lacks Drive's metadata-only priority implementation. This result does not justify a production batch-size change.

The next falsifiable live comparison, if authorized, is same-binary epoch-plus-metadata with 4 MiB versus 2 MiB normal batches in alternating order, identical five-client upload work, and gates on per-client text p95, verified file goodput, Drive units, errors, and cleanup. A separate normal upload HTTP pool is another existing same-route configuration worth isolating before changing scheduling code.
