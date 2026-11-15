# Opt-in metadata slot shadow slice

`drive.metadata_slot_v1` is **off by default**. Enabling it on both a client
and exit adds a bounded observation lane. The existing muxv4 immutable p0
object remains the only application delivery path. The slot's encrypted
records are authenticated and counted, then discarded; the slot ACK means
only “shadow receipt validated,” **not** “TCP bytes delivered.” This mode
cannot improve user latency and must not be promoted as a transport result.

The user still chooses Direct or the pinned `www.google.com` front in the
route setting. This feature does not alter either route or their defaults.

## Lifecycle and limits

When the client mux starts, the opt-in runtime asynchronously prewarms two
metadata-only Drive files with generated IDs: one uplink writer owned by the
client, one downlink writer owned by the exit. A later OPEN advertises IDs, a
random generation, version, and 8 KiB description cap inside its authenticated
batch. An OPEN sent before setup completes proceeds immediately without the
offer, so a one-connection workload might not exercise any slot. The exit
validates each exact ID, expected name, and mailbox parent asynchronously so
validation does not delay processing the immutable OPEN.
It then sends an authenticated acceptance. An old peer ignores the extended
capability and continues to exchange ordinary muxv4 objects.

Only p0 DATA/FIN batches whose complete sealed mux object fits in 8 KiB are
mirrored. Initial request bytes embedded in OPEN, RST, p1, and larger batches
are outside the shadow sample. The immutable upload is attempted separately,
including when the p0 object uses the existing metadata-only priority format.
An observed shadow record alone does not prove that its immutable counterpart
persisted or that identical bytes were delivered. Readers validate the slot AEAD
and the original mux envelope and frames; they do not call `handleFrame` or
mark the object as delivered. The slot codec's `markProcessed` and ACK are
used solely to record a validated shadow receipt.

Each run is capped at 2 minutes, 64 PATCH attempts per peer, 320 known-ID
GETs per peer, 64 distinct mirrored records per peer, and a 16-record pending
queue. PATCHes are serialized and at least 250 ms apart. At most 16 client
runs can have active or pending slots on one exit. A lost PATCH response is
resolved by exact-ID readback before another PATCH; unresolved writes close
the shadow writer while the immutable path continues. The two client-owned
slot IDs are deleted exactly on client stop or shadow expiry, with 404
readback. A client makes only one setup attempt per process/run, so reopening
after expiry cannot reset its budget. If no peer accepts, an inactive link
expires after 45 seconds and deletes its files. Ambiguous creates use a fresh
exact-ID readback and bounded delayed exact-ID cleanup retries. A process
crash or a commit later than the recovery window can still leave files until
the opt-in `slotv1/` janitor runs. Manual cleanup
also includes that prefix. Slot records never enter ordinary mux object cleanup.

This adds quota cost and storage churn. A fully active pair can use up to 640
known-ID GETs and 128 PATCH attempts in its two-minute shadow lifetime,
plus create, validation, and cleanup calls. The standalone roundtrip probe
used separate HTTP pools for up/down GET and PATCH. Current DriveStore slot
GET and PATCH share its control HTTP pool, while quota/backoff is shared.
Do not compare timing from these two substrates as though they were identical.

## Validation

Local safety checks, without Drive traffic:

```sh
go test ./internal/skirk ./cmd/skirk
go test -race ./internal/skirk -run 'TestMuxSlot|TestMetadataSlot'
python3 tools/test_skirk_benchmarks.py
```

For a bounded live interference comparison, use the same reviewed binary for
control and candidate and a private isolated test config. The harness creates
fresh session IDs, uses an ABBA order, validates every text/file response,
records both peer quotas, and preserves `google_front_pinned` from the config.
Only the candidate arms set `drive.metadata_slot_v1=true`. The harness requires
both peers to log `shadow active` and at least one authenticated `shadow
observed` event before it marks a candidate arm valid:

```sh
go build -o .skirk-runs/slot-shadow-skirk ./cmd/skirk
python3 tools/skirk_live_ab.py \
  --binary .skirk-runs/slot-shadow-skirk \
  --control-binary .skirk-runs/slot-shadow-skirk \
  --config .skirk-runs/personal-test/quota-kit/client.json \
  --out .skirk-runs/slot-shadow-download-001 --live --observe \
  --order control,candidate,candidate,control \
  --candidate-metadata-slot-v1 --bulk-mib 16 --bulk-direction download \
  --small-traffic message --small-count 12 --small-interval-ms 250
```

Repeat with a fresh output directory and `--bulk-direction upload`. Inspect
each arm's `client.log`, `exit.log`, `client-metrics.json`, `exit-metrics.json`,
and the summary before claiming any timing result. Look for authenticated
slot offer/accept, `shadow active`, `shadow observed`, or `shadow stopped`
logs; verify every scheduled small request and the 16 MiB SHA-256 file check,
zero Drive API errors, all slot IDs removed, and no remaining `slotv1/` or
session `muxv4/` objects after the cleanup window. These live runs measure
shadow overhead and compatibility, not slot-delivery latency. Real delivery
still needs the claim, deduplication, ACK, fallback, and exact cleanup gates
in the parent design document.
