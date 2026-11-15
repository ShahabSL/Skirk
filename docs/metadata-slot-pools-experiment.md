# Isolated metadata-slot HTTP pools

`drive.separate_metadata_slot_pools` is an opt-in experiment. It has an effect
only when `drive.metadata_slot_v1` is also enabled. The slot's known-ID GETs
use one dedicated Google HTTP connection pool and its PATCHes use another.
Create, delete, list, and ordinary mux traffic remain on their existing pools.
Both new clients copy the user's selected Direct or pinned `www.google.com`
route; the default route and default transport are unchanged.

This separates connection scheduling, not Drive quota or identity. Both pools
use the same `AccessTokenSource`, `X-Goog-User-Project` header, request logger,
quota accounting, and rate-limit backoff through `DriveStore.requestWithHTTPMode`.
PATCH remains a single attempt because an ambiguous write must be read back
before another revision is published. A canceled read waiting on shared
backoff does not issue an HTTP request. The extra pools can add TLS handshakes
and sockets, so local routing tests alone do not show a latency benefit.

The bounded comparison mode uses the **same binary and slot delivery protocol
in both arms**. Only candidate arms enable the pool split. Both arms must log
two active slot peers plus a receiver processing event and sender ACK, and
must verify every small message and bulk-file hash before comparing timings:

```sh
go build -o .skirk-runs/slot-pools-skirk ./cmd/skirk
python3 tools/skirk_live_ab.py \
  --binary .skirk-runs/slot-pools-skirk \
  --control-binary .skirk-runs/slot-pools-skirk \
  --config .skirk-runs/personal-test/quota-kit/client.json \
  --out .skirk-runs/slot-pools-download-001 --live --observe --probe-exit \
  --order control,candidate,candidate,control \
  --candidate-separate-metadata-slot-pools \
  --bulk-mib 16 --bulk-direction download \
  --small-traffic message --small-count 12 --small-interval-ms 250
```

The harness stops after a failed arm, caps estimated quota and API errors,
and writes raw results only under the ignored `.skirk-runs/` directory. The
operator must still check and clean the exact generated `muxv4/<session>/`
and `slotv1/<session>/` prefixes after each run. A second upload ABBA and
repeated larger mixed workloads are necessary before changing any default.

Local checks passed: full Go tests, vet, focused race tests for slot delivery
and pool routing, and Python benchmark-harness tests. There is no live Drive
performance result for this pool split in this document.
