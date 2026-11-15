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
headers, payloads, or credentials. The opt-in flag's latency, throughput, and
quota are unmeasured until a live comparison completes; it must not be
promoted from a unit test or a single arm.
