# Go Skirk

Skirk's production client is implemented in Go under `cmd/skirk` and `internal/skirk`.

## Current Modes

- `serve-client`: local SOCKS5 listener that sends CONNECT streams through Drive Mux v3.
- `serve-exit`: exit poller that reads muxed client stream frames, dials target TCP, and writes downstream frames back.
- `revoke`: revokes the OAuth refresh token embedded in a generated kit.

## Config

Generate a starter config:

```sh
go run ./cmd/skirk sample-config --out skirk.json
```

The important fields are:

- `secret`: shared AEAD secret. Use `skirk keygen`.
- `session_id`: optional fixed 32-hex session for a paired client and exit.
- `route.proxy`: restricted-network SOCKS proxy, usually `socks5h://127.0.0.1:1080`.
- `route.google_ip`: known Google edge IP for pinned routing. The default setup path uses hostname fronting (`google_front`) because some SOCKS relays allow `www.google.com` but reject IP-literal Google edge targets; use `google_front_pinned` only when a specific Google edge IP is measured to work.
- `drive.space`: set to `appDataFolder` for the recommended app-private mailbox.
- `drive.folder_id`: visible Drive folder ID for the fallback mailbox.
- `tunnel.profile`: `auto` by default. Direct routes start at the full measured Drive windows. Restricted/proxied routes start lower, then grow and back off based on Google API success or rate-limit pressure.
- `tunnel.chunk_size`: Drive object payload size. Start conservative, then benchmark.
- `tunnel.concurrency`: legacy shared cap for Drive workers.
- `tunnel.upload_concurrency` / `tunnel.download_concurrency`: optional manual caps for experiments. Leave unset for `profile=auto`.
- `tunnel.cleanup_processed`: removes processed Drive mux objects.

## Why Drive appData

Drive appDataFolder keeps Skirk's encrypted mailbox private to the OAuth application and lets the runtime use one Google API and one narrow OAuth scope. New setup kits use Drive-only mux objects for control and data.

This does not make Google Drive a low-latency stream substrate; polling and API quotas still define the ceiling.

## Operational Notes

- Use a dedicated Google account for testing.
- Use a dedicated OAuth client/project per operator where practical.
- Keep `chunk_size` within a measured range; larger chunks improve bulk throughput but hurt latency and retries.
- Runtime polling is shared per tunnel direction. Active TCP streams are batched into four mux lanes, and bulk frames are striped with per-stream ordered reassembly, so browser fanout does not create one Drive list loop per TCP stream.
- `cleanup_processed` should stay enabled. Runtime cleanup is delayed out of the foreground byte path so active streams get priority.
- The access token can come from `SKIRK_ACCESS_TOKEN`, `auth.access_token`, or `auth.token_command`.

## Quota Accounting

Skirk logs an estimated Drive quota window every minute while traffic is active:

```text
drive quota window=60s calls=42 est_units=5100 errors=0 response_bytes=123456 ops=download:12/2400u,list:18/1800u,upload:12/600u
```

Set `SKIRK_QUOTA_LOG_INTERVAL=10s` for short tests, or `SKIRK_QUOTA_LOG_INTERVAL=0` to disable the periodic line. The estimate follows the current Drive API quota-unit table: list requests are 100 units, downloads are 200 units, and upload/delete/create requests are counted as 50 units.

For project-level truth, use Google Cloud Console: APIs & Services -> Enabled APIs & services -> Google Drive API -> Metrics. Those charts are project-scoped, so they are useful only when the kit was generated with your own OAuth client/project.

## Validation

Local:

```sh
go test ./...
```

Restricted network substrate:

```sh
go run ./cmd/skirk serve-client \
  --config skirk.json \
  --listen 127.0.0.1:18080 \
  --route-mode google_front \
  --upstream-proxy socks5h://127.0.0.1:11093

curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

SOCKS path:

Run an exit:

```sh
go run ./cmd/skirk serve-exit --config skirk.json
```

Run a client:

```sh
go run ./cmd/skirk serve-client --config skirk.json --listen 127.0.0.1:18080
```

Then point an app at `socks5h://127.0.0.1:18080`.

## Learning Notes

This follows a mux-lane design common in real transports: independent streams share a bounded number of ordered physical lanes. It preserves per-stream ordering while avoiding the pathological one-object-per-stream-event behavior that makes browsers unusable over Drive.

## Why This Matters

The hard part is not AES or SOCKS; it is making a brittle, quota-limited substrate fail predictably. The current implementation keeps the core binary envelope independent from Google APIs so future carriers can reuse the same protocol.
