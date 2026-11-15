# Guarded Google-fronted Pub/Sub StreamingPull primitive

Status: implemented primitive with local tests; no live result recorded yet.
The [earlier unary probe](pubsub-front-2026-11-15.md) established encrypted
messages through the pinned Google front. This experiment compares one persistent
HTTP/2 StreamingPull receiver with prestarted unary long polls. It carries no
user traffic and changes no Skirk default or production code.

The guarded test is
`internal/skirk/pubsub_streaming_probe_test.go:TestLivePinnedPubSubStreamingPull`.
It uses the same private `GoogleHTTPClient.client` transport as Skirk, URL host
`www.google.com`, HTTP/2 authority `pubsub.googleapis.com`, and gRPC method
`/google.pubsub.v1.Subscriber/StreamingPull`. The API's generated request and
response types come from official
`cloud.google.com/go/pubsub/v2/apiv1/pubsubpb` version 2.9.0 and Google's
`google.golang.org/protobuf` decoder. Only gRPC length framing is local code.
No unchecked custom protobuf field parser is used.

The reader refuses compressed frames, truncated headers/bodies, frames larger
than 64 KiB, more than 128 frames or 1 MiB total response, more than 32 messages
per frame, oversized ACK IDs and message bytes. The first request sets a 30-second
stream ACK deadline, 32 outstanding messages and 64 KiB outstanding bytes. ACK
requests travel on the same duplex stream after AEAD verification. Context
cancellation closes both pipe ends and joins the stream reader and writer;
unary workers are also joined before cloud resource deletion.
Each ACK call waits for its protobuf frame to be written to the HTTP request
stream before reporting success. That confirms local stream publication, not
a server-side exactly-once ACK confirmation; this subscription does not enable
exactly-once delivery. The report records actual completed request-frame writes.

The probe creates two randomly named topics and two subscriptions only in the
authorized test project `skirk-p`. Its twenty 256-byte encrypted one-way payloads
alternate unary/stream/stream/unary five times, ten per carrier. Unique session,
direction, sequence and exact bytes must authenticate. Duplicate deliveries are
counted, acknowledged and not reported as new payloads. The run has a 90-second
active deadline and 160 unary control/publication/pull/ack call ceiling. A
private resource ledger exists before the first create. Cleanup stops every
worker, deletes exact generated names (including ambiguous creations), and
checks all four resources return 404 immediately and after 31 seconds.

The token must be freshly minted by the caller for the explicitly authorized
secondary account and stored with 0600 permissions in an ignored `.skirk-runs`
path. The guarded test requires the exact account label, reads the token without
printing it, and removes the file after use. The account label is a provenance
assertion by the runner, not independent introspection of the access token.
The previous Drive-only grant does not acquire Pub/Sub scopes by importing this
test. The caller supplies a separate authorized `cloud-platform` or Pub/Sub
grant; no main Gmail account should be used.

```sh
go test -race ./internal/skirk -run '^TestStreamingProbe' -count=1
SKIRK_PUBSUB_STREAM_LIVE=1 \
SKIRK_PUBSUB_STREAM_ACCOUNT=tekkenchanse3@gmail.com \
SKIRK_PUBSUB_STREAM_TOKEN_FILE=/absolute/.skirk-runs/secondary-token \
SKIRK_PUBSUB_STREAM_ROUTE_CONFIG=/absolute/.skirk-runs/personal-test/kit/exit.json \
SKIRK_PUBSUB_STREAM_OUT=/absolute/.skirk-runs/streaming-new.json \
go test ./internal/skirk -run '^TestLivePinnedPubSubStreamingPull$' -count=1 -timeout=3m
```

The report separates publish-response time from publish-start to authenticated
receive time and retains all twenty samples. A received record can precede the
publisher's own response. These one-way primitive times are not messenger
round trips, app VPN measurements, long-running cost or a 200,000-user proof.
The official [pull guide](https://docs.cloud.google.com/pubsub/docs/pull)
recommends StreamingPull for latency/throughput and notes periodic server-side
stream closure; a production transport must reconnect, dedupe, extend leases
and handle quota backpressure. The probe does not claim those lifecycle features.

## Possible full transport, contingent on live evidence

A hybrid could keep mux bulk objects on Drive and place the existing sealed
priority batch in Pub/Sub. It would negotiate that carrier inside an authenticated
OPEN exchange, bind every record to session/client/run and stream sequence, and
deduplicate a late identical Drive fallback with the same prepared batch identity.
Publish acceptance, subscriber processing ACK and destination socket delivery
must stay distinct. Pub/Sub alone does not supply exactly-once external TCP
effects. A missing IAM grant or unconfirmed peer must take the existing Drive
route without emitting incompatible control objects. A client still selects
pinned Google front or Direct explicitly.

This adds a separate Cloud product and resource lifecycle. Each operator needs
a billing-enabled project with Pub/Sub enabled and narrowly scoped publish/subscribe
IAM on their own resources. Client topics/subscriptions need isolation so
subscribers cannot consume another user's ciphertext or spend unbounded project
quota. Avoid sharing owner credentials or project-wide administration in an
exported profile. A setup/renewal path must create, rotate and delete resources,
revoke credentials, meter bytes/requests/connections and bound retained messages.
Drive's `drive.file` OAuth grant alone supplies none of that IAM authority.
Per-operator projects distribute quotas; they do not establish large-scale
capacity or eliminate billing and authentication work.
