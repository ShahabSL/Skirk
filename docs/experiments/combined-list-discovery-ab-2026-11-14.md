# Combined mux list discovery A/B

`drive.combined_mux_discovery` is an opt-in receive-side experiment. The
current default lists priority p0 objects separately, then lists normal p1
objects while active. With the flag, the receiver uses one legacy
`muxv4/<session>/...` prefix list and its normal list cursor. It still parses,
authenticates, orders, and processes the same encrypted muxv4 objects. This
does not require a peer capability or change the selected Direct Google or
pinned `www.google.com` front route. It does not enable metadata-only objects
or fixed-slot delivery. Old peers' untagged mux objects remain visible.

The comparison harness runs the same binary in all four arms. Control uses
the current split discovery; candidate sets only
`drive.combined_mux_discovery=true`. It clears saved slot settings in both
arms, pins the normal p1 setting to the current 1000 default in both arms,
preserves the route and other config, verifies message and file hashes,
and records both peers' quota/API errors. The caller must perform exact
`muxv4/<session>/` cleanup and a delayed zero-file check after the run.

```sh
go build -trimpath -o .skirk-runs/combined-list-skirk ./cmd/skirk
SKIRK_HTTP_TRACE=1 python3 tools/skirk_live_ab.py \
  --binary .skirk-runs/combined-list-skirk \
  --control-binary .skirk-runs/combined-list-skirk \
  --config .skirk-runs/personal-test/quota-kit/client.json \
  --out .skirk-runs/combined-list-down-001 --live --observe --probe-exit \
  --order candidate,control,control,candidate \
  --candidate-combined-mux-discovery \
  --bulk-mib 16 --bulk-direction download \
  --small-traffic message --small-count 12 --small-interval-ms 250 \
  --stall-seconds 60
```

`SKIRK_HTTP_TRACE=1` logs safe timings for slow Google requests without URLs,
headers, payloads, or credentials.

## Pinned-front live results

The isolated `shahabpre` account ran 16 MiB bulk transfers with 12 scheduled
new-stream messages per arm. Download used candidate/control/control/candidate;
upload reversed the order. The same binary hash was used in every arm of each
comparison. Every bulk SHA-256 and all 48 message bodies per direction passed;
each arm's on-demand authenticated exit proof passed; Drive API errors were
zero. Medians below summarize **two arms per mode**, not population percentiles.

| Direction | Discovery | Scheduled message p95 | Request message p95 | File goodput | Est. Drive units/arm |
| --- | --- | ---: | ---: | ---: | ---: |
| Download | Split default | 13.240 s | 7.065 s | 14.050 Mb/s | 24,480 |
| Download | Combined | 14.939 s | 4.233 s | 12.693 Mb/s | 22,755 |
| Upload | Split default | 18.530 s | 8.326 s | 9.428 Mb/s | 24,030 |
| Upload | Combined | 15.074 s | 5.217 s | 11.777 Mb/s | 24,455 |

The download result trades better request timing for worse scheduled timing
and file goodput. Upload favors combined in this pair, but individual file
arms ranged from 7.806 to 15.724 Mb/s. A broad combined list requests at
most 16 pages of 100 objects per poll and retains a continuation token.
An older priority object behind a large, newer p1 backlog can therefore be
delayed while pagination advances. The split default queries p0 separately.
These results do not justify changing the default without a backlog-safe
priority fallback and a larger mixed workload.

Raw results and exact cleanup reports are in the ignored
`.skirk-runs/combined-list-{down,upload}-16m-20261114-01/` directories.
Each run's eight exact mux/slot prefixes had zero immediate and delayed
matches after cleanup; 28 and 17 residual objects were deleted respectively.
