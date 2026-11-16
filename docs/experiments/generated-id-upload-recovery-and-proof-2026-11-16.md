# Generated-ID upload recovery and exact conflict proof — November 16, 2026

The bounded Drive v2 selective-receipt campaign H stopped in its third,
ordinary-receipt control arm. Four concurrent 1,048,631-byte exit bulk uploads
hit Skirk's own 28-second per-attempt deadline within 18 ms of one another.
The mux scheduled retries with the same generated file IDs. A later
Google-front POST returned HTTP 502; the **research guard** cancels an arm on
any unexpected status, so the file request was canceled at 51.4 seconds. The
first twenty download text echoes completed, but the 16 MiB download did not.
The arm is a failure, not a speed sample. Both writers joined, exact owned
Drive cleanup passed, and the copied account tokens were removed. Its guard
recorded no quota denials or 429s. The evidence does not identify whether the
front, network, or Drive caused the initial stalls, nor prove the normal mux
retry would have failed without the research guard's cancellation.

## Exact generated-ID recovery

Inspection found that a normal generated-ID upload receiving 409 previously
accepted an object after checking only ID, name and byte count. The optional
state-aware upload-stall hedge already used Drive's `sha256Checksum`, but the
ordinary path did not. The ordinary and hedge paths now require the same
exact ID, name, size and SHA-256 content proof before treating 409 as an
idempotent success. The normal path computes its checksum only after a 409;
successful uploads retain their prior HTTP retry policy and make no new API
call. If Drive returns 404 or a temporarily absent checksum, the conflict
reader makes at most three known-ID GET attempts with 100/200 ms pauses, then
fails closed. A mismatched identity or checksum fails immediately. Mux's
existing finite retry policy may try the prepared generated-ID upload again;
an absent or mismatched checksum is never accepted as delivery.

The local full-tunnel fixture committed one generated-ID bulk object and
returned a synthetic 502 instead of its successful response. The normal
Google HTTP client retried the same ID; the fake returned 409; DriveStore read
back and verified the exact
SHA-256. The fixture leaves upload hedging off, matching the ordinary
generated-ID path used in the failed live arm; the existing upload-stall
tests cover the optional hedge path.
Through actual loopback SOCKS and TLS, independent exact 16 MiB files
completed in both directions while 40 short echoes completed during bulk.
The fixture joined both muxes, checked processing receipts and physical
fallback, removed the exact owned run objects, and retained an unrelated
sentinel. Its injected path took **two POSTs and one known-ID GET**, 105
estimated Drive units, and 840 conservative eight-attempt reserved units;
the synthetic 502 and expected 409 remain visible as two error statuses.
Separate tests reject a same-size wrong-content 409, recover an exact
committed lost response, and allow three bounded readbacks when 404/empty
checksum precedes the exact checksum. These are fake-route correctness tests,
not a Google latency, throughput, or production-capacity result.

The Drive v2 priority carrier remains a research fixture. This repair applies
to the ordinary generated-ID bulk upload path shared by the current mux.
