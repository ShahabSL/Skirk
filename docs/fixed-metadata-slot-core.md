# Fixed metadata slot protocol core (candidate)

`internal/skirk/metadata_slot.go` is an opt-in, in-memory protocol core. It does
not make Drive calls, create slot files, alter muxv4, or enable the fixed lane.
The production transport remains muxv4. The broader bootstrap and promotion
plan is in [fixed-metadata-lane-design-2026.md](fixed-metadata-lane-design-2026.md).

## State and wire contract

- One `metadataSlotPeer` owns an outbound writer and inbound reader for one
  `(session, client ID, run ID, generation)`. The two directions use different
  keys. A restart requires a fresh random generation and run ID.
- The writer queues exact prepared mux `name` and `sealed` bytes. The slot
  sequence is separate from the mux object's own sequence. Only priority mux
  names in the matching client/run namespace are accepted.
- Each base64url description is independently AES-256-GCM sealed with a random
  96-bit nonce. HKDF uses a slot-specific salt and info label, separate from
  muxv4. The authenticated plaintext includes generation, direction, strictly
  increasing snapshot revision, reverse-direction contiguous ACK, and all
  unacknowledged records. Drive `version` is only a read hint.
- A description is capped at 8 KiB, with at most 64 queued records. Enqueue
  failure consumes no slot sequence; the caller can publish that original mux
  record through the immutable path. The reader retains only a bounded recent
  identity window. The existing mux receive path supplies longer-lived name
  deduplication when this core is integrated.
- `prepare` permits one outstanding PATCH. Definite success calls `published`.
  A timeout calls `ambiguous`; no later PATCH is permitted until the original
  HTTP request is drained and exact description readback calls
  `resolveAmbiguous`, or a processed authenticated reverse ACK proves delivery.
  Mismatched readback requires `fallbackAndClose`; it cannot prove a late PATCH
  will not overwrite a newer version. Fallback returns independent copies of
  **the exact mux name and ciphertext** for every unacknowledged record and
  closes the slot generation.
- `receive` authenticates and validates the full snapshot and reverse ACK
  before changing state. It offers new records once. The caller must pass each
  successful mux processing result to `markProcessed` in slot sequence order.
  Only then can the ACK advance. `noteFallbackProcessed` is called only after
  the original immutable mux record has passed the mux receive path; it
  suppresses the matching slot copy whether fallback or slot arrives first.

## Integration conditions

The future Drive adapter must validate slot file ID, name, parent folder,
generation, and writer ownership during the authenticated muxv4 capability
exchange. It must serialize PATCH calls, treat an unknown result as ambiguous,
publish fallback bytes under the original mux name, and never enqueue virtual
slot records for Drive object deletion. It must keep legacy p0 listing active
for peers without the capability and while the fixed lane is unhealthy. This
core alone does not establish any live latency, throughput, quota, or platform
improvement.
