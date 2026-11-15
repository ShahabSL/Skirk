# Google-fronted Pub/Sub hybrid primitive

The user authorized exploring a different architecture, including other
Google APIs, while preserving the selected Google front. This probe tests
whether Pub/Sub can provide an optional small-message carrier beside Drive
bulk. It changes neither Skirk defaults nor the Drive OAuth grant.

All API calls used Skirk's existing pinned `www.google.com` TLS route with
HTTP authority `pubsub.googleapis.com`. An already authorized secondary
non-main test account and its existing `skirk-p` Cloud project supplied
Cloud-platform credentials. The user's main accounts were not used. That
project reported billing disabled; enabling Pub/Sub and creating the small
test resources succeeded. This observation is not a permanent free-service
guarantee. See [Pub/Sub pricing](https://cloud.google.com/pubsub/pricing).

The tool created one random topic/subscription, published eight random
256-byte plaintexts sealed with Skirk AEAD, validated session/sequence and
every byte after reception, acknowledged each message, deleted both
resources, and confirmed both returned404. Its private access-token file was
removed. It made30 API requests with no failed message or cleanup operation.

| Mode | Publish response median | Publish-start to verified receive median | Maximum of four deliveries |
| --- | ---: | ---: | ---: |
| Publish then unary pull | 86.9 ms | 548.6 ms | 2,138.1 ms |
| Begin unary pull200ms before publication | 72.6 ms | 396.3 ms | 786.3 ms |

The last two warm long-poll samples took101.3 and72.2ms from publish start
to reception. They demonstrate a reachable low-latency path, not a latency
guarantee, application round trip or tunnel benchmark. The initial queued
sample's2.138s tail and long-poll786ms tail are retained. Modes were
interleaved queued/long/long/queued twice; four samples per mode are too few
to rank them statistically. Resource setup is excluded from delivery timing.
Raw sanitized metrics are in ignored `.skirk-runs/pubsub-front-20261115.json`.

Google [recommends StreamingPull for low latency](https://docs.cloud.google.com/pubsub/docs/pull)
and explicitly says unary pull does not guarantee low latency or throughput.
The next probe must test a persistent HTTP/2 gRPC StreamingPull connection
through this same front, exact encrypted request/reply, reconnect/duplicate
handling and cleanup. Pub/Sub has different credentials, IAM, byte charges,
retention, subscription/resource limits and at-least-once delivery semantics;
an optional hybrid needs an explicit setup flow and cannot silently reuse a
Drive-only grant. A successful local fronted request does not prove regional
reachability for every user.

Reproduce with `go run ./tools/pubsub_probe --live --route-config <test-kit>
--token-file .skirk-runs/<private-cloud-token> --project skirk-p
--out .skirk-runs/<fresh-result>.json`. The hard-coded project is an intentional
guard for this bounded research tool, not product configuration.
