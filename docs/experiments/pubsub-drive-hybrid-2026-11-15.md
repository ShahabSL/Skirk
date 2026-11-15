# Guarded REST Pub/Sub priority / Drive bulk research transport

Status: test-only implementation and offline race/correctness tests. No live
hybrid speed, latency or capacity result is claimed in this document yet. The
standard gRPC method path was rejected through the pinned front in the
[separate retained experiment](pubsub-streaming-probe-2026-11-15.md); the earlier
[REST primitive](pubsub-front-2026-11-15.md) delivered messages. This candidate
uses documented REST methods, without guessing an alternative private gRPC path.

The three new `pubsub_hybrid_*_test.go` files are excluded from product binaries.
They exercise the actual Skirk mux and SOCKS server. Every priority (`p0`) object
up to a 1 MiB sealed bound is carried through Pub/Sub; every normal (`p1`) object
retains Drive. Larger priority records fail locally rather than silently entering
an undiscoverable fallback. Existing mux batches are bounded substantially below
that research carrier cap. Neither a new product configuration nor credentials
are exported. Pinned `www.google.com` remains the tested SNI with authority
`pubsub.googleapis.com`; the adapter follows the supplied route, preserving a
future user's explicit front/direct selection.

## Documented API and research policy

Google's [Pull reference](https://docs.cloud.google.com/pubsub/docs/reference/rest/v1/projects.subscriptions/pull)
defines bounded waiting and permits empty or smaller responses. The probe omits
the deprecated `returnImmediately` flag. One, two or four outstanding long pulls
can be compared; the full hybrid starts two per receive direction and reissues
after each response. Empty pulls back off 100 ms. A 25-second per-pull deadline
reopens an otherwise idle request. Publish, pull and ACK/lease requests use
independent same-route HTTP pools. The full test does not enable gRPC.

The [ACK reference](https://docs.cloud.google.com/pubsub/docs/reference/rest/v1/projects.subscriptions/acknowledge)
allows redelivery after an expired ACK deadline, even following an accepted ACK.
The adapter therefore merges every delivery's ACK ID into one prepared-object
identity. Broker ACK waits until the mux calls deferred `DeleteID`, after mux
processing or an explicit terminal discard; an authenticated stale record is
also discarded without replaying OPEN. This does not guarantee exactly-once
TCP destination effects. A 30-second lease is extended every ten seconds while
an admitted object awaits processing. Retained processed identities never expire
within this bounded process: the 4,096-entry claim limit fails closed. It cannot
forget an OPEN when the mux's shorter closed-stream map ages out. Production
reconnection/restart semantics still require a separate design.

An outer AES-GCM record binds the exact mux name and sealed bytes to session,
client, run and direction. Fresh outer random nonces avoid nonce reuse across
publication retries. The stable cache ID hashes name plus the original inner
sealed batch, so republishing prepared bytes still deduplicates. Admission also
checks the mux name namespace, priority class, inner AEAD, session, direction,
sequence and batch framing. Large sealed records are read from the authenticated
ID cache, bypassing the existing 8 KiB discovery-inline limit. The original first
broker `publishTime` is preserved across redelivery/republish; later delivery
cannot refresh it. Startup uses the existing 30-second mux catch-up, and messages
older than the configured 600-second retention plus clock tolerance are dropped.
Before starting subscribers, successful setup responses must pass the existing
three-second Google Date/header interval clock check. A process restart inside
the original 30-second catch-up can still replay OPEN effects; this test does
not solve or conceal that existing distributed-systems boundary.

The cache holds at most 32 MiB and 4,096 entries, with at most 64 outstanding ACK
IDs per entry and 4 MiB retained ACK-ID bytes across the entire receiver. HTTP responses are limited to 4 MiB before JSON decoding; pulls
request at most eight messages. API calls and request/response bytes share one
locked global budget across all HTTP pools (2,000 calls, 128 MiB). Thirty-two
calls and 1 MiB are reserved for cloud cleanup. Each active request atomically
reserves its bounded maximum response size before HTTP begins; partial/error
response bytes are counted and unused reservation is released. Cleanup responses
are capped at 16 KiB, so active response exhaustion cannot consume that reserve. The Drive HTTP transport separately reserves
per-attempt units, including independent pools and retries: active requests stop
at 40,000 reserved units, total cleanup ceiling 50,000, at most 2,000 attempts
with 32 attempts reserved for cleanup.
These are conservative experiment guards, not claims about Google project quota.
Per-peer actual Drive accounting is reported independently. Gate shutdown waits
for response-body closure before cleanup and rejects later mux calls locally.

## Workloads and evidence

The offline race suite includes a real loopback SOCKS → mux → HTTP origin run,
exact 4 MiB download and upload, twenty warm mixed text requests, both priority
directions, and forced broker duplicate delivery. It rejects any priority write
that reaches the memory-backed bulk store. Additional tests cover outer/inner
authentication corruption, cross-session/direction rejection, payload/HTTP bounds,
stale and expired broker records, duplicate ACK merge before/after processing,
ACK retry, an actual authenticated OPEN retaining exactly one destination dial/initial-byte
write after manually expiring the mux closed-stream lifetime, queue-admission
retry, exact pull concurrency, cancellation/join, clock skew, atomic global
budgets and Drive gate body-lifetime shutdown.

The live harness has three explicit modes:

- `pools`: eighteen synthetic authenticated 256-byte records, pool sizes
  `1, 2, 4, 4, 2, 1`, three records each. All original samples are retained.
  These are one-way primitive times, not messenger round trips.
- `hybrid`: real SOCKS HTTP, exact 16 MiB download and upload, one warm text
  request and ten 64-byte text responses during each bulk phase. Reports TTFB,
  complete-body time, length and SHA-256 separately. The bulk and text use
  separate concurrent TCP connections when the origin connection is busy.
- `drive`: the same binary/config/workload with the ordinary Drive store for
  both classes. Metadata-only/slots are disabled in both modes, page size is
  100, bulk/upload pools and interaction/sparse classification are enabled.
  This is a controlled same-binary baseline, not a measurement of `main`.

No mode uses external origin/user traffic. Each run uses a fresh random session,
client/run namespace and at most two topics plus two subscriptions in the
existing isolated Cloud project. A private recovery ledger is synced before
creating any cloud/Drive object. Cleanup cancels and joins subscriber workers,
closes mux streams, stops all foreground Drive HTTP gates, removes only exact
own-prefix Drive objects and exact generated Cloud resources, verifies immediate
and 31-second delayed absence, then removes the ledger and borrowed token.
Ambiguous creates remain covered by the prewritten ledger and prefix scan.

```sh
go test -race ./internal/skirk -run '^TestHybrid' -count=1 -timeout=45s
SKIRK_PUBSUB_HYBRID_LIVE=1 \
SKIRK_PUBSUB_HYBRID_ACCOUNT=isolated-secondary \
SKIRK_PUBSUB_HYBRID_MODE=pools \
SKIRK_PUBSUB_HYBRID_TOKEN_FILE=/absolute/.skirk-runs/secondary-token \
SKIRK_PUBSUB_HYBRID_ROUTE_CONFIG=/absolute/.skirk-runs/personal-test/quota-kit/exit.json \
SKIRK_PUBSUB_HYBRID_OUT=/absolute/.skirk-runs/fresh-hybrid-report.json \
go test ./internal/skirk -run '^TestLivePinnedPubSubHybrid$' -count=1 -timeout=9m
```

The generic account label is an operator assertion; token provenance must come
from an explicitly selected authorized secondary account. The test needs a
separate Pub/Sub-capable OAuth token and resource IAM; Drive's `drive.file` grant
alone is insufficient. Public deployment would need narrowly scoped per-operator
publish/subscribe IAM, isolated resources per peer, provisioning/revocation,
renewable least-privilege credentials, quotas and cost/billing review. No owner
or `cloud-platform` token is exported in a Skirk product profile. The current
single-client test does not establish 200,000-user capacity or durable recovery.

## Retained live pool experiment

On 2026-11-15, source `f2109da`, binary SHA-256
`f7a3d059b9b785b4ceeb6dd167492f4825357e6984c44277cd766a7bdd300330`,
mode `pools` passed all eighteen authenticated synthetic records in 203.48 s.
There were 97 REST calls, 18,507 request-body bytes, 23,353 response-body bytes,
18 ACK IDs, thirteen empty pull responses, zero duplicate deliveries and zero
Drive units. All four resources returned 404 immediately and after 31 seconds;
the private recovery ledger and borrowed token were removed. The private original
report is `.skirk-runs/hybrid-pools-f2109da-20261115.json`.

All publish-start → authenticated-receive samples are retained below. Pool size
was changed by stopping/joining the previous receiver workers and starting the
next condition on the same subscription. No 30-second tail was excluded.

| Pool | Sequence | Publish response ms | Authenticated one-way ms |
| ---: | ---: | ---: | ---: |
| 1 | 1 | 250.364 | 2470.755 |
| 1 | 2 | 73.529 | 732.313 |
| 1 | 3 | 73.254 | 836.566 |
| 2 | 4 | 66.902 | 30812.658 |
| 2 | 5 | 74.878 | 705.738 |
| 2 | 6 | 63.955 | 213.133 |
| 4 | 7 | 64.562 | 30902.708 |
| 4 | 8 | 76.421 | 212.348 |
| 4 | 9 | 68.309 | 170.618 |
| 4 | 10 | 87.319 | 30756.158 |
| 4 | 11 | 132.739 | 170.495 |
| 4 | 12 | 86.491 | 170.553 |
| 2 | 13 | 65.958 | 30913.051 |
| 2 | 14 | 79.726 | 180.852 |
| 2 | 15 | 70.079 | 169.941 |
| 1 | 16 | 80.071 | 30929.909 |
| 1 | 17 | 69.355 | 170.222 |
| 1 | 18 | 67.298 | 167.916 |

Pool medians (six samples each) were 784.440, 459.436 and 191.483 ms for
one/two/four workers, but each pool also had a roughly 30.9-second maximum.
These conditions do not establish a clean worker-count speed ranking. Every
post-restart condition's first sample incurred the large delay, including the
last one-worker condition. One plausible inference is that an outstanding pull
canceled at the client can still claim a subsequent message before server-side
cancellation takes effect, leaving it unavailable until the 30-second ACK lease
expires. This has not been isolated experimentally. The results do not prove
that `maxMessages=8` causes batching delay. A deliberate cancel/reconnect test
with different ACK deadlines can discriminate the lease hypothesis.

The next full hybrid test uses fresh subscriptions and a stable two-worker
receiver, keeping the carrier unchanged. New runs append and sync a private
`.progress.jsonl` after every sample plus five-second counters and categorized
empty/timeout/cancel/error worker events, without payloads, ACK IDs or tokens.
The report remains a separate final artifact; a journal failure cancels active
work and is reported after cleanup. This makes a failed or slow run inspectable
before its final deadline.

## Scale and cost require separate evidence

Google's [quota document](https://docs.cloud.google.com/pubsub/quotas) lists fixed
limits of 10,000 topics and 10,000 subscriptions per project. Two of each per
client limits this simple isolation scheme to 5,000 clients per project before
other constraints. It also warns that normal-user gcloud credentials are rate
limited for manual usage and recommends service accounts for automation; this
test token is not a production throughput credential. No 200,000-user capacity
has been established. Quota attribution and resource-owner billing differ.

[Pub/Sub pricing](https://cloud.google.com/pubsub/pricing) currently provides
10 GiB monthly free throughput per billing account, then $40/TiB for ordinary
combined publishing/delivery, with a minimum 1 KiB per request and possible
transfer/storage charges. Batching changes those costs but can affect latency.
The [IAM reference](https://docs.cloud.google.com/pubsub/docs/access-control)
supports resource-level publish and consume permissions; future setup must give
a client only its own topic/subscription rights. A user's separate Drive OAuth
grant does not supply these rights. Cloud owner credentials remain test-only.
