# Drive v2 CAS priority queue: bounded research candidate

Status: component and full SOCKS/TLS fault tests pass locally. No live full
transport result exists. The earlier owned-file PATCH/PUT and rapid-revision
observations are capability evidence, not a tunnel performance result.

This candidate reuses the authenticated cumulative priority queue, processing
receipts, credits and physical/virtual replay ledger from the Firestore research
prototype. Its carrier uses two fresh known-ID Drive files, one writer per
direction, conditional v2 metadata PATCH and bounded conditional GET polling.
Normal bulk traffic and same-identity priority fallback remain ordinary Drive.
There is no Firestore resource, owner token, extra OAuth scope or product-default
change. The existing user choice remains intact; this research fixture requires
the approved pinned `www.google.com` route.

## Write fences and bootstrap

The shared document contract now explicitly distinguishes strict Firestore
timestamps from strong quoted v2 resource ETags. A timestamp can never be
substituted for an ETag or the reverse. Ambiguous writes retain the exact
prepared ciphertext and original fence. An exact readback can confirm
publication; an unchanged old document permits only the identical conditional
retry. A conflicting state, malformed identity, auth failure or unexpected API
error cannot authorize an unconditioned overwrite. A missing file cannot be
recreated by conditional PATCH.

Before either queue store starts, **both** fresh carriers must finish their
own feature probe. Each probe reads an empty owned file, challenges a fresh
random impossible fence, conditionally writes a maximum-size random opaque
body, challenges the old fence, reads the exact body/tag, then conditionally
resets and rereads empty. These seven calls require actual stale rejection,
new strong resource tags and support for the selected description capacity.
The intermediate opaque body is not an application snapshot and must never be
exposed to an already running peer store. Failed or ambiguous probing leaves
the carrier unverified. An outer caller can select ordinary immutable Drive;
it cannot construct a ready queue from that failed carrier.

The implemented outer-snapshot cap is 16 KiB, serialized as at most 21,846
Base64URL description characters. This is motivated by the preceding v3
description matrix, which accepted 16 KiB plaintext plus envelope and Base64
expansion; that result alone does not prove the v2 endpoint's capacity.
The new per-run maximum-size probe is required before claiming v2 readiness.
Queue admission checks exact encoded size before committing a sequence: an
object that would overflow this snapshot takes the original immutable path.
Some full 16 KiB TLS records therefore use immutable fallback after framing.

Claims remain bounded to one fresh five-minute process epoch, with no eviction
or cross-restart exactly-once promise. The original 4,096-claim, 8 MiB retained
payload and authenticated name/ciphertext/physical-ID rules remain. Deletion
is cleanup only; the actual mux success callback owns processing receipts.

## Polling, ownership and accounting

GET and PATCH use separate bounded route-aware HTTP/2 clients. Watch polling
has one owner, at most three sequential reconnect owners, a 100 ms active delay
and a one-second idle delay after two seconds without a change. Local successful
publication wakes the peer poll and starts the active window. Poll responses
carry cumulative current state, so coalesced revisions do not lose records.
A 304 emits no new snapshot. These are experimental scheduling choices, not
claimed optimal values. Polling delays are applied after the API response.

Close fences admissions, cancels every request and joins all operations before
return. A token provider or HTTP provider that ignores cancellation keeps that
join pending. The live driver must use tokens minted before timing, rather
than hiding refresh traffic in a timed callback. Component counters separate
token-source invocation, reservation, visible client dispatch, HTTP response,
injected/real errors and cancellation. Provider file cleanup is a separate,
independently budgeted fixture phase after writer proof.

The local component defaults are 2,048 reserved calls, 100,000 quota-unit upper
and 64 MiB encoded-request upper. Each dispatch conservatively reserves eight
opaque HTTP/2 attempts. Maximum body exposure is 32 KiB per response. These
are finite component bounds, not observed physical attempts or raw wire bytes.
They do not substitute for a shared per-arm guard across both carriers, bulk
pools, provisioning and cleanup. The live harness must charge that shared
guard exactly once per dispatch; wrapping a pool and charging its admission
hook again would double-count the same upper reservation.

Under the current official [Drive quota model](https://developers.google.com/workspace/drive/api/guides/limits),
metadata GET costs five units and metadata writes cost 50. The published
limits include 1,000,000 units/minute/project, 325,000/minute/user/project,
the 400-million-units/day standard billing threshold and 1 TB/day egress.
Polling/write duty cycles therefore matter even when storage is small.

Ignoring API time, the selected one-second idle delay permits at most 60 GETs
per minute per endpoint: 600 units/minute for a two-endpoint pair, or 864,000
units/day if kept continuously idle. Active 100 ms delay permits at most
6,000 GET units/minute per pair. Four writes/second in each direction adds
24,000 units/minute before provisioning, fallback, bulk discovery and cleanup.
The eight-attempt experimental upper is additional conservative accounting,
not a revised Google price. At 200,000 simultaneous centrally hosted pairs,
even two GETs every 30 seconds cost about four million units/minute; these
experiments establish no centralized free low-latency capacity at that scale.
Per-user projects/accounts or slower idle duty cycles need separate product
and operational evaluation. No billing is enabled by this code.

## Local evidence and retained failed attempts

Eight component test functions cover unsupported/ignored/missing/static fences,
exact conditional mutation, path/method/header restrictions, a global guard
denial before dispatch, 304 polling/coalescing, lost responses with and without
commit, exact ciphertext/fence retries, wrong IDs/titles/header conflicts,
oversized data, wrong returned ciphertext, unchanged write tags, forbidden
readbacks, missing-file non-recreation, and cancellation of an unresponsive
token owner. POSIX private-directory fixtures skip on Windows; pure protocol
and opaque-fence tests remain enabled. A Windows/amd64 test cross-compile
passed, which is compile proof only.

The full test uses actual SOCKS, mux routines and a TLS origin trusted only
through its own certificate. Independent random 16 MiB GET and POST bodies
are verified exactly. Forty unique 64-byte echoes complete strictly between
actual bulk progress and completion gates. A committed snapshot response is
lost, conditional peer visibility is withheld until the exact immutable
fallback arrives, and one peer poll fails to force cumulative-state reconnect.
Final origin effects are rechecked after handlers and owned loops stop;
normal/priority cleanup removes both fresh run prefixes while preserving an
unrelated sentinel. Adapter queues, payloads and active streams are zero.

Two initial accelerated 20 ms local-poll attempts hit the unchanged
100,000-unit component upper after proving the injected recovery paths; both
failed and are not recast as success. The second, diagnostic attempt retained
735 client dispatches/99,960 reserved units. A new
local-only correctness fixture explicitly selected 150,000 upper units per
peer, within the fixed constructor limit. A subsequent attempt completed all
bytes but exposed a test assertion that incorrectly expected a response for
each intentionally lost-response dispatch; that assertion was corrected to
retain both injected transport errors. The final proof does not call those
two errors a clean run. No Google calls occurred in any of these local cases.

After the capacity-probe tightening, all nine V2 component/full test functions
passed under race on the final snapshot in 11.656 seconds; vet passed. The extracted shared fixture's
Firestore mixed/OPEN/fatal-cancellation and fence/snapshot regressions also
passed under race in 9.497 seconds before that carrier-only tightening.
These test durations are not Google latency measurements. Live performance,
idle/reconnect behavior, true mixed-workload quota and feature availability
must still be measured in a separate fresh, same-binary controlled campaign.

The official v2/v3 guide recommends v3 for new work unless a needed v2 feature
warrants otherwise. The observed v2 CAS behavior is feature-detected, bounded
and version-dependent; it is not assumed permanent. There is no absolute
latency/speed optimum or production-promotion claim in this experiment.
