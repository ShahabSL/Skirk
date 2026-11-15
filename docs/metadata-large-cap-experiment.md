# Negotiated 16 KiB metadata carrier experiment

Status: opt-in code experiment. The default metadata-only cap remains 8,192
sealed bytes. To enable the larger candidate, both profiles may set:

```json
{"drive":{"metadata_only_priority":true,"metadata_only_max_bytes":16384}}
```

Only 0 (default), 8,192 and 16,384 are accepted. This limit includes the mux
envelope and authentication tag, not only application bytes. A complete 16 KiB
TLS record plus framing can exceed it; this change does not claim that every
TLS record fits. The separate carrier probe established exact 16 KiB plaintext
plus envelope and Base64 retrieval in two metadata-description files on the
pinned Google front. Description's numeric maximum is undocumented. No larger
value is inferred from `contentHints.indexableText`, which is not returned by
the API.

The authenticated OPEN capability has a new 20-byte version-two form: request
kind 3 or echo kind 4, explicit version byte 2, a 16-byte nonce and a two-byte
sealed limit. The receiver echoes the smaller of its own cap and the offered
cap. Each prepared batch must fit the minimum confirmed limit across all its
frames. The original 17-byte request/echo remains an 8 KiB capability. A new
16 KiB client talking to an older binary receives no v2 echo, so it continues
ordinary media delivery. An older client talking to a new exit offers the
legacy form and receives at most 8 KiB metadata objects. No new control object
or extra negotiation round trip is added.

Only the opt-in 16 KiB Drive profile decodes larger sealed descriptions. Fresh
and adaptive list pages are clamped to 100 files and their decompressed response
body is bounded to 8 MiB before JSON parsing. Prefix control lists get the same
byte bound. Individual encoded and decoded records are checked before AEAD
processing; the enlarged static metadata parser bound cannot override the
local configured receive cap. The ordinary dual-write inline cap stays 8 KiB.

Tests cover a 12 KiB authenticated mux batch, corruption, nonce mismatch,
unknown version and invalid limits, both 8/16 KiB mixed-peer directions,
missing echo fallback, a mixed-stream batch whose peer limits differ, concurrent
echo/preparation, profile decode bounds, oversized create refusal, page-size
clamping and decompressed-response limits. The original metadata-only tests
remain part of the same targeted race suite.

No full-proxy live speed claim is attached to this code. Compare against the
same binary with 8 KiB cap, equal metadata-only and scheduler settings, new
session per arm, persistent text plus concurrent file upload/download, exact
hashes, both-peer Drive units and delayed exact cleanup. Record actual counts
of 8–16 KiB metadata objects; a workload that never emits an eligible larger
batch cannot evaluate this change. Pair this cap with asynchronous slots only
after measuring each change separately.

The A/B harness accepts `--candidate-metadata-16k --small-response-bytes 12288
--small-traffic get`. Both control and candidate enable metadata-only objects
and use 100-file list pages, with 8 KiB versus 16 KiB as the sole transport
variant. Responses use exact random bytes and SHA verification. A candidate arm
fails if no successful metadata publication exceeds 8,192 sealed bytes; a
control arm fails if one does. First-byte and complete-body p95 are recorded
separately because an early small HTTP header frame can precede the 12 KiB body.
