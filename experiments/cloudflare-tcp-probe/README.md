# Fixed-destination TCP capability probe

This disposable experiment tests whether `cloudflare:sockets.connect()` can
carry raw TLS bytes to port 443. The Worker accepts only Google, GitHub, and a
Cloudflare-hosted control, requires a random secret, expires after a configured
deadline, and caps each stream at 15 seconds, 64 KiB up, and 128 KiB down.
It does not receive any Google OAuth or Skirk profile credentials.

`probe.mjs` reads `PROBE_TOKEN` from a private JSON file, checks the three TCP
destinations, then wraps a WebSocket-backed Duplex in Node TLS. Certificate
verification stays enabled and TLS terminates on the local client. Only public
`HEAD /robots.txt` requests are sent; no CA installation or HTTPS inspection is
involved. Raw TLS bytes are all the Worker sees.

The November 14, 2026 deployed probe returned certificate-verified TLS 1.3 and
HTTP 200 for Google and GitHub. The Cloudflare-hosted control returned the
socket restriction error. The first probe version opened TCP successfully but
failed TLS because recent Workers deliver WebSocket binary messages as Blob by
default; setting `binaryType = "arraybuffer"` fixed the probe. Both attempts
are retained in `.skirk-runs/astra-cf-probe/`.

The successful end-to-end probe durations were about 200 and 206 ms. These are
single direct Worker requests, **not Drive latency or a Skirk benchmark**.
The account's plan was not varied, so this does not prove free-plan capacity.
The temporary Worker and its local probe secret were deleted after testing.

For reproduction, use Wrangler 4 with a private generated JSONC config pointing
to `worker.js`, a recent compatibility date, `EXPIRES_AT` in Unix milliseconds,
and a private JSON secrets file containing a fresh random `PROBE_TOKEN`.
Validate with `wrangler deploy --dry-run`, then deploy with `--secrets-file`.
Run:

```sh
node probe.mjs https://YOUR-TEMPORARY-WORKER PRIVATE-SECRETS.json PRIVATE-RESULTS.json
```

Delete that temporary Worker and secret after the test. Never put credentials
in the source or command arguments. This folder intentionally provides no
open relay, production route, DNS change, or persistent deployment config.

References: [TCP restrictions](https://developers.cloudflare.com/workers/runtime-apis/tcp-sockets/),
[binary WebSocket messages](https://developers.cloudflare.com/workers/runtime-apis/websockets/#binary-messages),
[invocation limits](https://developers.cloudflare.com/workers/platform/limits/).
