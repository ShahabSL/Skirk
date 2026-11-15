# Owned Firestore Listen feasibility

This is an opt-in research probe, outside product binaries. The preceding
document create/read test does not establish that notifications work through
the Google front. This probe addresses that separate question with one owned
document, six authenticated states (initial plus five serial updates), and
exact deletion checks. It is not a TCP or messenger benchmark.

Current status: the first live run verified all six ciphertext states through
the pinned Google front but **failed overall during observer teardown**. Its
samples and failed outcome remain unchanged. A local shutdown repair is being
validated before a fresh database/run can retry; no production transport is
promoted by this probe.

The [official RPC reference](https://firebase.google.com/docs/firestore/reference/rpc/google.firestore.v1#firestore)
limits Listen to gRPC or WebChannel. The [browser SDK implementation](https://github.com/firebase/firebase-js-sdk/blob/main/packages/firestore/src/platform/browser/webchannel_connection.ts)
provides the exact `/google.firestore.v1.Firestore/Listen/channel` endpoint,
database routing parameter and authorization encoding. The [watch implementation](https://github.com/firebase/firebase-js-sdk/blob/main/packages/firestore/src/remote/persistent_stream.ts)
supplies the add/remove target schema. No speculative REST Listen endpoint is
used. [Long-poll timeout options](https://github.com/firebase/firebase-js-sdk/blob/main/packages/firestore/src/api/long_polling_options.ts)
support five through thirty seconds; this test selects five.

`observer.mjs` executes the unchanged, Apache-2.0 Google Closure WebChannel
implementation at commit `6f48f578d3e80fe7a85e530a5d95b9351433d135`, the dependency
pinned by the Firebase wrapper package inspected during this audit. Obtain its
[official source](https://raw.githubusercontent.com/google/closure-net/6f48f578d3e80fe7a85e530a5d95b9351433d135/firebase/webchannel_blob_es2018.js)
before the timing window. It must be exactly 40,502 bytes and SHA-256
`c820e65a0254c9765ab8564cc01faf2778dc87dfd72425421029f82aa8087467`.
Keep the downloaded file under the ignored private run directory, not in a
product package. This uses the browser transport in a Node VM, not the public
Node Firestore SDK, whose long-poll settings are not supported. The VM contains
the network adapter of this trusted, hash-pinned SDK; it is not a security
sandbox for adversarial JavaScript.

The observer receives only one network function. It forwards allowed logical
Firestore requests to a secret-authenticated loopback Go bridge. The bridge
independently restricts the service path and owned database, then calls Skirk's
Google HTTP client with the same pinned IP and `www.google.com` front. Native
XHR, image probes, sendBeacon and arbitrary fetch are unavailable; local fetch
redirects are rejected. Authorization stays in headers or the WebChannel init
body and is rejected in URLs. Owner OAuth credentials are research-only and
must never become a client profile or production authorization design.

The Go test accepts an absolute `SKIRK_FIRESTORE_LISTEN_DIR` under `.skirk-runs`
(0700). It requires regular 0600 files:

- `recovery-ledger.json`: root-provisioned proof of the exact new, empty,
  free-eligible `projects/skirk-p/databases/skirk-front-probe-<12 hex>` database.
- `access-token`: the authorized secondary account's existing owner token.
- `webchannel_blob_es2018.js`: the pinned source above.

Invocation requires `SKIRK_LIVE_FIRESTORE_LISTEN=1` and selecting only
`TestLiveOwnedFirestoreListenPinnedFront`. It consumes an exclusive run marker
before activity. Every dispatch is journaled before and after completion.
Active work is limited to 36 visible dispatches; four additional slots are
reserved for cleanup, for 40 total and at most 320 HTTP/2 retry attempts by the
conservative eight-attempt model. Encoded request upper budget is 16 MiB;
each response is capped at 1 MiB. The revised local bridge has four forwarding
slots, one independent local event-ACK slot, and a 64-event bounded buffer.
Separating event admission adds no Google request concurrency or API budget.
Partial samples remain when the experiment fails.

The initial sample includes observer startup. Later samples separately report
the PATCH acknowledgement and the listener callback relative to write start;
the callback can precede the HTTP acknowledgement. The observer has no local
Firestore persistence, so a cached SDK write cannot count as a notification.
Every delivered ciphertext must match the prepared version and pass AES-GCM
authentication and exact plaintext comparison. Duplicate prior versions are
counted as replay, never a new state.

Shutdown requests target removal and separately records whether it was
acknowledged; a closed channel is not a remove acknowledgement. It closes
the channel, aborts and joins bridge calls while draining local event ACKs,
disposes owned SDK timers, then deletes the exact known
document and verifies immediate and 31-second 404s. The external root runner
must still delete the entire exact test database and restore the preceding API
state in its finally block, including after process failure. No data outside
the newly created test database is part of this probe.

Offline validation to perform before any live lease:

```sh
SKIRK_WEBCHANNEL_BLOB_PATH=/absolute/ignored/webchannel_blob_es2018.js node --test experiments/firestore-listen/observer.test.mjs
go test ./internal/skirk -run '^TestFirestoreListen' -count=1
go test -race ./internal/skirk -run '^TestFirestoreListen' -count=1
```

Completed offline validation at runtime source `e41b368`:

- Two Node tests passed, including evaluating the unmodified official blob and
  checking its actual initial request. The first attempt exposed missing VM
  browser globals; that was fixed before any Google request.
- Three Go guard/fake HTTP tests passed (0.698 s), targeted race tests passed
  (1.858 s), and package `go vet` passed.
- The test binary compiled successfully. Its ignored basename is
  `firestore-listen-e41b368.test`, SHA-256
  `5bb3ffafc58998ad0c7b46c317caa7e70093e20308b0358aa39ea64b94ff4983`.

These checks establish local containment and request formation, not Google
backend compatibility, notification latency, production credentials, or an
improvement to Skirk. A separate authorized live lease and exact database/API
cleanup remain required.

The first live source was `577f67a`, binary SHA-256
`411e40dcc0e67f96a40a28afeef10aca429f2c9c63e6f9148757d3eedc785c2d`.
It verified these serial 2 KiB plaintext states; times are milliseconds from
each PATCH start on the same Mac. Listen is the API callback time, not TCP RTT.

| State | PATCH acknowledgement | Listen callback |
| --- | ---: | ---: |
| Initial, including observer startup | 663.411 | 969.608 |
| Update 1 | 317.195 | 326.311 |
| Update 2 | 353.831 | 371.637 |
| Update 3 | 159.281 | 233.385 |
| Update 4 | 233.179 | 230.786 |
| Update 5 | 204.258 | 214.228 |

The run used 21 visible pinned HTTP/2 dispatches (168 conservative attempt
upper bound). All Google responses were 200 except expected cleanup 404s;
one canceled GET had no HTTP response. Observer output reported
`EVENT_DELIVERY`, eight parsed target events but only seven journaled target
events, and no target-remove acknowledgement. Go required observer termination
after its five-second wait. The exact document and database subsequently
returned 404 twice, and the previously disabled Firestore API was restored to
disabled. Copied owner-token and observer-config files were removed.

Connected offline test `e67091c` reproduced both `EVENT_DELIVERY` and live
1.5-second/45-second timers with the actual pinned SDK. Source inspection found
that ordinary shutdown aborted local event ACKs with the transport requests,
and native Node timer objects broke browser SDK numeric timer disposal. The
repair separates event draining from request cancellation and exposes numeric
timer IDs with bounded owned disposal. It retains real local HTTP 400/429
failures, rather than treating them as successful cancellation. The original
run lacked local HTTP status diagnostics, so these mechanisms do not exclude
an additional local admission rejection in that run. New local result records
contain only constant route/reason enums, status, cancellation state and timing.

Completed repair validation (all synthetic/local, with no Google calls):

- Re-executing the retained `e67091c` connected regression failed as expected
  in 136 ms with `EVENT_DELIVERY` and live 1,500/45,000 ms timers; the test then
  disposed its owned handles.
- All nine repaired Node tests passed in 7.058 s. These evaluate the actual
  pinned SDK, preserve real 400/429/network/body failures even during close,
  cover the pre-dispatch body-read ownership window, and drain an event ACK
  following the final document. Numeric SDK timers and all request/event
  promises are joined before `stopped` can be true.
- A real observer subprocess using native loopback HTTP exited successfully in
  518 ms, with zero pending requests/events/SDK timers, no forced termination,
  and an acknowledged target removal. This is a local lifecycle test, not a
  new Google measurement.
- Five Go fake/guard tests passed under the race detector in 2.008 s; `go vet`
  passed. They include four simultaneous blocked forwarders followed by eight
  successful local event ACKs, and verify diagnostic status/phase retention
  without credentials, URLs or raw error text.
- An independent read-only review found no remaining blocking lifecycle issue
  after whole-fetch ownership, exact abort-error attribution and parallel
  request/event drains were fixed. The JS shutdown bound is approximately
  4.1 s; Go permits five seconds before its separately recorded forced-stop
  fallback and a further bounded join.

A new frozen binary and a fresh owned database/run are required for another
live probe. The six successful payload samples above and the original failed
teardown remain the first run's result.
