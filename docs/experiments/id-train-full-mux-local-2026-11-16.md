# Immutable known-ID train through the full mux — local negative gate

Status: **isolated test-only prototype; do not promote or run live.** Source is
the separate [research branch](https://github.com/ShahabSL/Skirk/tree/codex/skirk-id-train-full-mux-20261116)
at `a723b1c`. The two test files have
SHA-256 `912dfe346eb235b9af947c84b79bdb6f36eb7f83168f607c4f3e829e797bdf66`
(carrier) and `b490c9fe7b89779b7e4f0a3904016835f2aba84f3a15dd4678fb4fbae18696c8`
(fixture). No OAuth token, Google object, or external request was used.

## Mechanism and local proof

The sender generates a first priority object ID and publishes an authenticated
immutable bootstrap for **all four mux lanes** in its direction and epoch.
Each priority object's authenticated name points to the next generated ID.
The receiver discovers boots through a prefix list, probes a known ID's
metadata, validates its name and HMAC, then hands that immutable object to the
ordinary mux receiver. Bulk stays on the existing immutable media path. A
simulated lost create response retries the same ID and sealed bytes. If ID
reservation fails before publication, the sender uses the ordinary immutable
name/list path and pins the decision for that name.

One deterministic fixture ran the actual v5 mux and SOCKS5 frontend through
verified end-to-end TLS: exact 1 MiB download, exact 1 MiB upload, one warm
64-byte text reply, and eight concurrent short TCP text streams while each
bulk direction was held at the origin. All 17 text replies matched; the origin
observed one upload effect; owned workers joined. The fixture explicitly
swept only its local session prefix and found it empty. Local tests also passed
the same-ID lost-response retry, ID-reserve fallback, and a restart after a
deleted chain head. The restart test proves a list fallback can *find* a
surviving later object; it does **not** prove application delivery or
exactly-once side effects after restart. Five race-enabled repeats of the
fixture and four tests passed, as did `go vet ./internal/skirk`.

This particular fake-store run used a 5 ms polling interval and observed:

| Peer | Bootstrap prefix lists | Known-ID metadata probes | Priority fallback lists | Normal bulk lists |
| --- | ---: | ---: | ---: | ---: |
| Client | 8 | 1,016 | 46 | 3 |
| Exit | 7 | 1,052 | 45 | 3 |

These are **local method counts**, not Google latency, provider billing, or
throughput. At published method weights, the metadata probes and prefix lists
alone would nominally be at least 21,540 units in that short fake-store run,
before content downloads, creates, deletes, retries, and any real RPC delay.
The 5 ms fake poll is intentionally unsuitable as a production quota policy.
Early variants exposed two real design traps: bootstrapping only a lane used
by the first stream stranded later streams on another lane, and advancing the
known-ID head only after deferred physical deletion serialized independent
text behind cleanup. The local prototype now boots four lanes and advances
after authenticated metadata observation, while the mux owns its in-memory
claim/retry. The latter change still lacks a durable restart proof.

## Why it is not a scale or speed winner

The earlier [one-way primitive](id-train-live-2026-11-15.md) saved about 58%
of core discovery units by putting small sealed records **in metadata**. This
full-mux prototype instead creates media files: it uses a 5-unit metadata
probe to discover each known ID and a file-content GET, whose published weight
is 200 units, to read it. Its local result cannot inherit the primitive's
quota saving or p95. A real metadata-only known-ID protocol would need a new
ID-specific metadata create/read path, versioned peer negotiation, and the
same recovery and ownership proof. [Drive method weights](https://developers.google.com/workspace/drive/api/guides/limits).

The quiet-exit arithmetic depends on *how the first ID arrives*:

| Hypothetical one-second idle policy | Nominal discovery units/min | Cold-client consequence |
| --- | ---: | --- |
| Current one `files.list`/s | 6,000 | New clients discovered on that poll. |
| Four preissued per-lane metadata heads in **both kits**, no new-client list | 1,200 | Existing static pair only; a new device needs a server-side ID update or a fallback list. |
| Dynamic clients with one bootstrap list/s **plus** four metadata heads/s | 7,200 | Similar cold-open cadence, but 20% more idle units than current list alone. |
| Dynamic clients with one bootstrap list/5 s plus four heads/s | 2,400 | A new client can wait up to about five seconds before its first OPEN is found. |

These are quota-model examples using metadata GET at 5 units and list at 100,
**not** measurements of this fake wrapper or of a viable product. They exclude
active media reads, uploads, deletes, account/project limits and backoff. A
single preissued head could use only 300 units/min but would serialize all
priority traffic or require another fair fanout mechanism. Google documents
pre-generated IDs and same-ID `409` retry behavior, but these docs do not
establish indefinite usability of an uncreated ID from initial kit setup.
[Generate IDs](https://developers.google.com/workspace/drive/api/guides/create-file).

The test wrapper stops scanning for new boots once it knows any boot, so it
cannot discover a newly added client or epoch. It has unbounded issued,
sender and receiver maps, no real Drive pagination or modified-time cursor,
and no runtime bootstrap cleanup; the fixture's exact-prefix sweep is a
test-harness cleanup, not carrier-owned crash cleanup. A sender can advance
the chain before a create commits, leaving a missing head and pushing later
records onto expensive list reconciliation. The fallback after ten local
missing probes is a test convenience, not a reviewed latency/quota policy.
Per-client preissued heads move this problem to owner-driven client
provisioning and a durable per-peer cursor journal; they do not make rotation
or restart automatic. A deleted head's successor can be found by a list, but
the full mux has no proof that replaying it cannot repeat a destination
side effect.

**Decision:** keep the shipping immutable list carrier. This prototype
establishes full-tunnel local feasibility and identifies a sharp bootstrap
trade-off, but it does not improve both free scale and cold text latency under
the required dynamic-client model. No live Google comparison, universal
performance limit, or 200,000-user claim follows.
