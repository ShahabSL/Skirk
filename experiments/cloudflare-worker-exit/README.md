# Skirk Cloudflare Free Relay

This directory contains the free-tier Cloudflare build for Skirk. It uses only
Cloudflare components that have a free allowance: Workers, Durable Objects,
Cron triggers, and optional Drive push notifications.

It is a real deployable product for the subset Cloudflare Free can honestly
support: **Drive-backed HTTP fetch relay and Drive wake/pump control plane**.
It is not a VPS-equivalent SOCKS/VPN exit, because Cloudflare Free does not
provide a long-lived container/daemon runtime.

## What This Implements

- A Cloudflare Worker endpoint for Google Drive push notifications:
  `POST /drive/push`.
- A Durable Object that owns the Drive `changes.list` cursor for one Skirk
  session.
- Google OAuth refresh-token handling inside the Worker.
- `changes.watch`, `changes.getStartPageToken`, `changes.list`, and
  `channels.stop` calls.
- Cron-backed watch renewal when a stored Drive channel is near expiration.
- Durable Object alarm fallback polling while a Drive watch is active, with a
  short faster window after webhook or change activity.
- Bounded pump metrics so we can measure whether push materially reduces Drive
  polling before porting mux v4.
- A Drive-backed HTTP fetch relay:
  - upload `cfexit/v1/req/<id>` JSON request objects;
  - have the Worker fetch a bounded upstream HTTP(S) response;
  - upload `cfexit/v1/res/<id>...` response objects;
  - delete processed request objects.
- A local web proxy script that speaks to Google Drive on the client side and
  uses the Worker as the serverless fetch side. Plain HTTP works directly.
  HTTPS requires explicit local MITM mode, which decrypts HTTPS locally and
  replays HTTP requests through Worker `fetch()`.
- Low-latency response lookup for the web proxy: the local side pre-generates a
  Drive response file ID, includes it in the request, and then reads that known
  file instead of polling `files.list` for a response name. Proxy responses use
  a binary envelope so the response body is not base64-expanded in Drive.
- Large HTTP(S) responses use a hybrid completion path. Normal responses use a
  single known Drive response ID. If the response exceeds the local cap, the
  proxy first tries upstream byte ranges when the origin advertises
  `Accept-Ranges: bytes`; otherwise it retries with Worker-published Drive
  chunks. This keeps small traffic low-latency and still gives a bounded path
  for larger downloads.
- A bounded local SOCKS5 probe for opaque Drive stream testing. It waits for a
  Worker-side upstream-open acknowledgement, handles Worker error objects,
  applies local backpressure, and prints estimated Drive quota use. It is not a
  supported product mode: the existing Drive implementation has not passed
  destination coverage, opaque HTTPS, and long-running lifecycle gates.

## Capability Boundary

Supported:

- HTTP `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, and `OPTIONS` through
  the local `http-proxy` helper, with a bounded request body.
- HTTPS browser traffic through the local `http-proxy` helper only when
  `--mitm` is enabled and the generated local CA is trusted by the test
  browser/system.
- Worker-side `fetch()` of `http://` and `https://` URLs.
- Drive webhook/pump lifecycle, metrics, and bounded response objects.

Not supported by this free-tier mode:

- Opaque HTTPS `CONNECT` tunneling. HTTPS works only by local inspection/MITM.
- SOCKS5 as a production tunnel.
- VPN mode.
- UDP/QUIC.
- WebSocket/Upgrade traffic, gRPC streaming, SSE, and long-lived arbitrary TCP
  streams.

Use the normal Skirk Linux/VPS exit for those.

## HTTPS Inspection Warning

`--mitm` changes the trust boundary. Skirk terminates HTTPS locally, sees the
HTTP method, host, path, headers, and bounded request body, then serializes that
request into Google Drive objects for the Worker to fetch. Do not use this mode
with personal accounts or sensitive sessions during testing. The CA private key
must stay local and must never be uploaded, committed, logged, or shared.

The local proxy is loopback-only by default. Keep MITM on loopback. Do not use
LAN sharing with this mode unless you intentionally want other trusted devices
to trust this local CA.

## Local Validation

```bash
npm install
npm run typecheck
npm test
npm run dry-run
```

`npm run dry-run` builds the Worker bundle locally. Real deploys require
Wrangler to be logged into the intended Cloudflare account.

## Required Secrets

Set these as Wrangler secrets for deployment:

```bash
npx wrangler secret put SKIRK_ADMIN_TOKEN
npx wrangler secret put SKIRK_WEBHOOK_TOKEN
npx wrangler secret put GOOGLE_OAUTH_CLIENT_ID
npx wrangler secret put GOOGLE_OAUTH_CLIENT_SECRET
npx wrangler secret put GOOGLE_REFRESH_TOKEN
```

The `.dev.vars.example` file documents local variable names but does not contain
real credentials.

Set `SKIRK_DRIVE_FOLDER_ID` as a non-secret variable or secret when the Worker
should write responses into a specific Skirk mailbox folder.

## Polling Defaults

Drive push notifications only wake the Worker; the Worker still has to call
`changes.list` to discover which objects changed. The default fallback policy is:

- idle watch active: poll every 5 seconds;
- recent webhook/change activity: poll every 1 second;
- active window: 30 seconds.

Set `SKIRK_FALLBACK_IDLE_POLL_MS`, `SKIRK_FALLBACK_ACTIVE_POLL_MS`, or
`SKIRK_FALLBACK_ACTIVE_WINDOW_MS` to tune this. A value of `0` disables that
fallback interval. Lower values can reduce discovery latency but burn Worker
requests, Durable Object storage writes, and Drive API quota faster.

## Admin API

All admin routes require:

```text
Authorization: Bearer <SKIRK_ADMIN_TOKEN>
```

Routes:

- `GET /health`
- `GET /admin/metrics`
- `POST /admin/pump`
- `POST /admin/watch`
- `POST /admin/renew`
- `POST /admin/stop`
- `POST /drive/push`

## Local Web Relay

After deployment, run the local proxy:

```bash
npm run http-proxy -- \
  --config /path/to/exit.json \
  --listen 127.0.0.1:18088
```

Then test an HTTP URL through it:

```bash
curl -v -x http://127.0.0.1:18088 http://example.com/
```

For HTTPS browser/curl testing, enable inspected HTTPS mode:

```bash
npm run web-proxy -- \
  --config /path/to/exit.json \
  --listen 127.0.0.1:18088 \
  --ca-dir ~/.skirk/cf-free-mitm-ca
```

The command prints the generated CA path and SHA-256 fingerprint. For a quick
curl smoke without installing the CA system-wide:

```bash
curl -v --proxy http://127.0.0.1:18088 \
  --cacert ~/.skirk/cf-free-mitm-ca/skirk-cf-free-ca.crt \
  https://example.com/
```

For browser testing, use a disposable browser profile and trust only
`skirk-cf-free-ca.crt` for that profile where possible. Remove the CA after the
test. Without `--mitm`, HTTPS `CONNECT` returns a clear `501`.

The local proxy is Drive-only by default. It writes request objects to Drive and
waits for known response file IDs. It does not call the Worker. The Worker must
therefore be woken by Drive push notifications or its fallback alarm. Register
the Drive watch once after deployment:

```bash
WORKER_URL=https://<worker>.workers.dev \
SKIRK_ADMIN_TOKEN=<admin-token> \
curl -fsS -X POST "$WORKER_URL/admin/watch" \
  -H "authorization: Bearer $SKIRK_ADMIN_TOKEN" \
  -H "content-type: application/json" \
  -d '{}'
```

For lab-only latency comparisons, direct Worker wake can be enabled explicitly:

```bash
SKIRK_CF_HTTP_DRIVE_ONLY=false \
WORKER_URL=https://<worker>.workers.dev \
SKIRK_ADMIN_TOKEN=<admin-token> \
npm run web-proxy -- --config /path/to/exit.json --listen 127.0.0.1:18088
```

Do not use that lab mode for product claims; it bypasses the Drive-only client
transport constraint.

Useful tuning knobs for the local web proxy:

- `SKIRK_CF_HTTP_MAX_BYTES`: first response object cap. Default: 32 MiB.
- `SKIRK_CF_HTTP_STREAMING_CHUNK_BYTES`: chunk size for range-less large
  fallback. Default: 8 MiB.
- `SKIRK_CF_HTTP_MAX_STREAMING_CHUNKS`: pre-generated chunk IDs for one
  fallback request. Default: 64.
- `SKIRK_CF_HTTP_RANGE_FOLLOW`: use upstream byte ranges when advertised.
  Default: true.
- `SKIRK_CF_HTTP_TIMEOUT_MS`: local wait timeout. Default: 300000.

## Opaque SOCKS Probe

The `npm run socks` helper is an experimental Drive stream client, not a
supported client mode:

```bash
WORKER_URL=https://<worker>.workers.dev \
SKIRK_ADMIN_TOKEN=<admin-token> \
npm run socks -- \
  --config /path/to/exit.json \
  --listen 127.0.0.1:18180
```

By default the helper refuses normal web ports (`80` and `443`) so a browser does
not appear to connect and then fail inside TLS. To intentionally reproduce the
archived failure, add `--allow-web-ports true`.

The live validation on 2026-05-25 proved that the Drive object path and Worker
TCP socket path are real for non-web TCP: a SOCKS request to `github.com:22`
connected and returned the SSH banner through Drive. Ordinary web ports failed
before TLS because `cloudflare:sockets.connect()` returned:

```text
proxy request failed, cannot connect to the specified address. It looks like you might be trying to connect to a HTTP-based service - consider using fetch instead
```

The original interpretation of that error was too broad. On November 14,
`../cloudflare-tcp-probe/` carried opaque TLS 1.3 to Google and GitHub on port
443, with certificates verified by the local client and HTTP 200 responses.
A Cloudflare-hosted control returned the quoted error. [Cloudflare's socket
documentation](https://developers.cloudflare.com/workers/runtime-apis/tcp-sockets/)
blocks Cloudflare IP ranges; it does not establish a blanket web-port ban.
The new probe bypasses Drive and does not validate this older Drive stream
implementation or free-plan capacity. This implementation still needs
destination-coverage, lifetime, quota, and stream correctness work before it
can be offered as an opaque exit. MITM is a limitation of the existing HTTP
helper, not a universal requirement of the Workers platform.

## Drive Request API

The benchmark path accepts Drive request objects named with
`SKIRK_EXIT_REQUEST_PREFIX`, default `cfexit/v1/req/`.

Request body:

```json
{
  "kind": "http_fetch_v1",
  "requestId": "bench-1",
  "url": "https://example.com/file.bin",
  "maxBytes": 8388608
}
```

The Worker writes a raw response object named under
`SKIRK_EXIT_RESPONSE_PREFIX`, default `cfexit/v1/res/`, and deletes the request
object after the response has been uploaded.

For local HTTP proxy use, the request kind is `http_proxy_v1`. The response is
a compact binary envelope with status, headers, truncation, upstream duration,
and the raw body. Older clients can still request the JSON/base64 response
format, but it costs more CPU and Drive bytes.

`http_proxy_v1` rejects private/local destinations, URLs with embedded
credentials, WebSocket/Upgrade traffic, and request bodies over the configured
cap. Redirects are returned to the browser instead of being followed inside the
Worker, so redirect targets are re-evaluated through the proxy path.

Run a live bench after deployment:

```bash
WORKER_URL=https://<worker>.workers.dev \
SKIRK_ADMIN_TOKEN=<admin-token> \
npm run bench:drive-exit -- \
  --config /path/to/exit.json \
  --bytes 8388608 \
  --url https://speed.cloudflare.com/__down?bytes=8388608
```

The script prints elapsed time, Mbps, Drive list/pump counts, and estimated
Drive units for the request/response cycle.

By default the benchmark and local web proxy use generated response file IDs.
That replaces expensive response-name list polling with cheap file metadata
checks. Set `SKIRK_BENCH_KNOWN_RESPONSE_ID=false` or
`SKIRK_CF_HTTP_LEGACY_LIST_FALLBACK=true` only when debugging older Workers.

Live validation on 2026-05-25 against the free Worker account showed:

- 8 MiB direct Drive+Worker bench:
  14.81 Mbps, 810 estimated Drive units, 101.3 units/MiB.
- HTTPS MITM through the local proxy, 8 MiB:
  3.86 seconds, 17.37 Mbps.
- HTTPS MITM through the local proxy, 32 MiB:
  4.45 seconds, 60.27 Mbps.
- HTTPS MITM through the local proxy, 100 MiB from a range-capable origin:
  62.44 seconds, 13.44 Mbps.

Range-less origins can still hit a practical Cloudflare/Drive ceiling above the
first 32 MiB object. In validation, `speed.cloudflare.com/__down?bytes=67108864`
did not honor byte ranges and stalled after the first 32 MiB despite chunked
fallback. Treat large range-less downloads as unsupported in this free mode.

`POST /admin/watch` registers a Google Drive changes webhook. The default
webhook URL is `<worker origin>/drive/push`, or you can pass an explicit address:

```json
{
  "address": "https://example.workers.dev/drive/push"
}
```

## Design Boundary

This stays under `experiments/` because it is a separate Cloudflare Free mode,
not the production Skirk exit. Promotion into the normal release should only
happen if the product UI clearly labels the limitation and tests prove:

- lower idle Drive quota use than polling;
- reliable webhook delivery and cursor advancement;
- bounded Durable Object CPU/subrequests;
- cleanup behavior equal to or better than the VPS exit;
- useful HTTP fetch behavior without presenting it as SOCKS/VPN.
