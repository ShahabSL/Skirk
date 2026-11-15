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
and `slotv1/<session>/` prefixes after each run.

Local checks passed: full Go tests, vet, focused race tests for slot delivery
and pool routing, and Python benchmark-harness tests.

## Pinned-front full-app comparison

The 16 MiB download and upload ABBA runs used the same binary and enabled
slot delivery in both modes. The candidate alone split slot GET/PATCH pools.
Each direction verified all four file SHA-256 hashes, 48/48 small messages,
four authenticated exit proofs, two active slot peers and processing/ACK
activity in each arm, with zero Drive API errors. Values are two-arm medians
per mode; request p95 starts when the request begins, and scheduled p95 starts
at the intended send time.

| Direction and mode | Scheduled text p95 | Request p95 | File goodput | Estimated units, both peers |
| --- | ---: | ---: | ---: | ---: |
| Download, shared pools | 11.911 s | 4.787 s | 12.362 Mb/s | 22,653 |
| Download, split pools | 11.375 s | 3.227 s | 10.770 Mb/s | 21,400 |
| Upload, shared pools | 11.119 s | 2.786 s | 10.342 Mb/s | 24,013 |
| Upload, split pools | 10.624 s | 2.675 s | 10.543 Mb/s | 21,520 |

The download split lowered request p95 by about 33%, but file goodput fell
about 13%. The upload difference was small relative to between-arm bulk
variation. Exact `muxv4/` and `slotv1/` sweeps covered all 16 generated
session prefixes; immediate and 30-second delayed checks found zero files.
The raw private summaries and cleanup records are under
`.skirk-runs/slot-pools-{download,upload}-20261114-01/`. This is a promising
interactive tradeoff, not a proven joint win or a default recommendation.
The [final evidence record](experiments/final-engineering-evidence-2026-11-14.md)
places it alongside the clean-main and other architecture comparisons.

The later **32 MiB download** ABBA used the final-code normal-list page size
in both arms and otherwise kept the same pool-only treatment. It verified all
four file hashes, 48/48 messages, four exit proofs, and zero API errors.
Shared-pool medians were scheduled text p95 14.240 s, request p95 4.997 s,
file 21.758 Mb/s, and 23,443 estimated units. Split-pool medians were
10.758 s, 2.788 s, **12.622 Mb/s**, and 25,998 units. The text improvement
coincided with a 42% file-rate reduction and 11% more estimated units. Both
control file arms were faster than both split arms. This rejects a default
pool split under the stated joint text/file objective. Private raw summary:
`.skirk-runs/slot-pools-download-32m-20261114-01/`. Exact sweeps deleted 19
residual mux/slot objects; immediate and delayed checks found zero across all
eight generated prefixes.
