# Guarded REST Pub/Sub priority / Drive bulk research transport

Status: research only. The first full hybrid failed its download; successful
text/reverse-upload samples and every failure are retained below. Pub/Sub is
not a dependency for the requested free Skirk product. The
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

The cache holds at most 32 MiB and 4,096 entries, with at most 64 outstanding
ACK IDs per entry and 4 MiB retained ACK-ID bytes across the receiver. Exposed
HTTP response bodies are limited to 4 MiB before JSON decoding; pulls request
at most eight messages. The latest research guard reserves eight possible
internal HTTP/2 attempts and eight copies of each outgoing request body before
transport dispatch. The separate returned-body cap is not labeled exact wire
traffic. The Pub/Sub budget is 2,000 upper attempts / 128 MiB, including 256
attempts and 1 MiB reserved for cleanup; cleanup responses are capped at 16 KiB.
The later conservative-retry section records source/version provenance.

For the next Drive-only control, the approved guard is 240,000 active / 280,000
total worst-case reserved units and 4,000 upper attempts, including 40,000 units
and 256 attempts for cleanup. Guard exhaustion cancels active work immediately.
Older results below retain their original 40,000/50,000 visible-unit guard and
are not retroactively presented as having used the new bound. These are
experiment guards, not Google quota or exact billing claims. Visible per-peer
Drive accounting, upper reservations, local denials and write callbacks are
reported separately. Gate shutdown waits for response-body closure before
cleanup and rejects later mux calls locally.

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

## First full hybrid: failed download, retained positives

The unchanged stable-two-worker full test used source `75bab64`, binary SHA-256
`5ba77dfe0866c93246b0da512babb6b6e0ca74c4bddb00e5246ceec7fed0f29d`.
It failed after 248.28 s. This is **not a working full transport or a promotion
result**, despite the successful text and reverse-direction measurements:

| Workload | Result |
| --- | --- |
| Fresh warm-up text | Exact 64 B in 3517.922 ms |
| Ten texts during download | 10/10 exact; median 443.381 ms, maximum 1664.068 ms |
| 16 MiB download | Failed after 180003.689 ms; only 10,485,358 of 16,777,216 B; first byte 58846.156 ms |
| Ten texts during upload | 10/10 exact; median 704.592 ms, maximum 1332.820 ms |
| 16 MiB upload | Exact length/SHA-256 accepted by origin; 23468.691 ms, about 0.682 MiB/s including setup |

Pub/Sub carried 46/46 priority publications/deliveries with zero duplicates,
197 API calls, 50,025 request-body bytes, 55,425 response-body bytes, 45 observed
ACK IDs and 36 empty pulls. The journal observed no pull-deadline or worker-error
event. This does not prove reconnect safety or absence of every possible tail.

The exit's ordinary Drive upload path recorded 23 upload calls and **17 timeouts**
(roughly 28–30 seconds each); its complete active accounting was 117 calls,
10,905 estimated units. The client's active accounting was 143 calls,
14,255 estimated units and zero errors. Visible transport reservations totaled
25,570 units including cleanup, below the 40,000 active guard. Local guard
exhaustion therefore does not explain this attempt. Opaque retries inside the
HTTP/2 transport are not independently counted by this older guard or the
DriveStore counters; these are visible request/estimated-unit statistics, not
exact Google wire acceptance/billing measurements. The test kept the saved
`auto` profile and 8 MiB chunk configuration while enabling separate upload/bulk
pools. An identical same-binary ordinary Drive control is needed before
attributing those upload stalls to the hybrid architecture.

All four Cloud resources were deleted and returned 404 immediately and after
31 seconds. Both Drive prefix scans were empty; two residual exact own IDs were
deleted and their absence checked. The recovery ledger and borrowed token were
removed. The private report and synced progress journal remain at
`.skirk-runs/hybrid-full-75bab64-20261115.json` and its `.progress.jsonl` suffix.
The failed partial download is never converted into a successful throughput
number, and none of the successful text samples are discarded.

Follow-up instrumentation adds immediate global cancellation on local guard
denial, visible dispatch/denial counts and standard Go `WroteRequest` callback
counts (including internal write attempts/errors, without claiming server
acceptance). Heartbeats include per-peer Drive accounting and the guard state.
A private 8 MiB bounded mux/Drive log, sanitized failed-request sample rows and
per-MiB plaintext application/request-body progress identify the failing phase.
Request-body progress means bytes read by the HTTP transport, not destination
acceptance; only the origin's length/hash response establishes upload success.
These diagnostic changes are applied equally to the next Drive/hybrid pair.
The priority carrier, deadlines, ACK behavior and normal Drive pools stay fixed.

## Free-product boundary and conservative retry guard revision

Skirk's requested product route remains `www.google.com` and must be free to
use. Pub/Sub's capped free tier is conditional, **not unlimited free transport**:
10 GiB/month per billing account can be exhausted, and paid usage follows when
billing is enabled. Test-project billing remains disabled; this candidate is
optional bounded research, not a dependency or default for the free Skirk
product. Neither 200,000 free users nor unbounded free Pub/Sub traffic is claimed.
The separate Drive OAuth grant and Drive-backed product remain the priority.

The diagnostic source guard now accounts conservatively for the pinned
`golang.org/x/net/http2` v0.61 retry loop's maximum eight internal stream attempts
per outer RoundTripper dispatch. Before entering the transport, it reserves
**eight calls and eight copies of request-body bytes**, or eight method-unit
charges for bodyless Drive GETs as well. These upper reservations are not
refunded as if unused attempts were proven absent. Visible calls, actual exposed
body bytes, upper reservations, WroteRequest callbacks and local denials have
separate counters. Incoming decoded-body memory/exposed-body caps remain separate;
TLS/headers and opaque transport traffic are not falsely labeled exact wire bytes.
A dependency/transport change requires re-auditing this bound.

For the next identical 16 MiB Drive control, the parent authorized a routine
experiment guard of 240,000 active / 280,000 total worst-case reserved Drive
units, including a 40,000 cleanup reserve. At eightfold reservation this admits
at most 30,000 visible active unit estimates, rather than silently treating
40,000 old visible units as a physical bound. The Drive attempt cap is 4,000 with 256
reserved for cleanup (the separate Pub/Sub research cap remains 2,000). Run only after a 60-second quiet interval and preserve a
cap-hit failure without blindly enlarging it. This guard revision changes
accounting/diagnostic cancellation, not the priority carrier or Drive pools.
Any failed foreground HTTP measurement now records its sanitized failure row,
cancels the scenario and proceeds directly to bounded independent cleanup.

The `drive` control now requires only the isolated Drive kit and private output
path: no Cloud token, Cloud account assertion, Pub/Sub HTTP client or Pub/Sub
resource is created. Its report explicitly has `uses_pubsub=false` and no Cloud
project field. Accidental calls through a Cloud callback fail locally in this
mode. The same workload's optional hybrid mode still requires its separate
authorized secondary-account token and cannot reuse the Drive grant.
