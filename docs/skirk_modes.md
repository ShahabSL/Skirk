# Skirk Transport Modes

Skirk has one production transport:

```text
local SOCKS5 / HTTP proxy / Android VPN
-> encrypted Drive Mux v5 objects
-> Google Drive mailbox folder
-> Skirk exit
-> target TCP
```

Older alternate-carrier experiments are not part of the production path.

## Drive Mux v5

New kits default to Drive Mux v5. Public setup uses a
Skirk-created Drive mailbox folder and the
`https://www.googleapis.com/auth/drive.file` OAuth scope, which works with
Google's device-code login flow. V5 keeps the V4 object batching and Drive API
operations, but uses its own `muxv5/` namespace and a fresh sender generation
in the encryption key to avoid a reproduced restart key/nonce collision.

The transport groups active TCP streams into bounded mux lanes:

- many application streams share a small number of Drive lanes;
- `OPEN` can carry the first client bytes;
- each frame carries stream and sequence metadata inside the encrypted payload;
- each Drive object path includes a local client ID and per-run ID, so the same
  copied `skirk:` profile can be active on multiple devices at once;
- bulk frames are striped across lanes and reassembled in order;
- upload and download worker windows adapt to Drive health;
- processed objects are cleaned up outside the foreground byte path;
- stale leftovers are handled by the exit janitor or `skirk cleanup`.

This shape minimizes object count and avoids one Drive polling loop per browser
connection.

V5 is the default for restart safety. Exact live comparisons with clean `main`
V4 show mixed file and short-message success, but upload orders disagree on
speed and text p95. No stable performance win or universal limit is proven.
See the [current comparison](experiments/current-v5-vs-main-live-2026-11-16.md)
and [migration guide](mux-v5-migration-2026-11-16.md). Old V4 peers do not see
V5 objects; upgrade both ends together, or select V4 explicitly for a legacy
pair while migrating.

## Discovery

```text
exit:   fresh prefix list on muxv5/<session>/up/
client: fresh prefix list on muxv5/<session>/down/<client>/<run>/
```

Runtime discovery stays prefix-scoped because that keeps the hot path narrow and
predictable under mixed browsing and downloads.

## Route Modes

Client profiles default to:

```text
client route: google_front_pinned
exit route: google_front_pinned
```

Existing Linux profiles generated before v0.1.51 keep their embedded route.
Regenerate the kit or pass
`--route-mode google_front_pinned --google-ip 216.239.38.120` when starting the
client.

Available route modes:

- `direct`: normal Google API hostnames and TLS.
- `real_pinned`: connect to a configured Google edge IP while preserving the
  real Google API TLS name.
- `google_front`: use `www.google.com` TLS SNI for Google API traffic while
  sending the API hostname in the HTTP authority.
- `google_front_pinned`: the same Google front with an edge IP pinned by
  `--google-ip`.
- `google_front_h1` and `google_front_h1_pinned`: HTTP/1.1 variants used for
  route experiments.

Use fronted routes only on networks where you are authorized to test and where
normal Google API hostnames are blocked or unreliable.

## Local Frontends

- `serve-client`: SOCKS5 listener for Linux, macOS, Windows, and desktop apps.
- `serve-client --http-proxy-listen`: optional HTTP/HTTPS proxy listener using
  the same tunnel.
- Android app: whole-device VPN mode and optional SOCKS/HTTP LAN sharing.
- Windows desktop app: profile import, SOCKS/HTTP proxy control, system proxy,
  VPN mode, performance presets, and local Drive API estimate.
- macOS desktop app: profile import, SOCKS/HTTP proxy control, optional system
  proxy on the active network service, performance presets, and local Drive API
  estimate. The system proxy applies to proxy-aware apps, not all Mac traffic.

## Constraints

Google Drive is an object API, not a stream API. A new small request/response
still needs object upload, object discovery, exit processing, response upload,
and response discovery. Skirk removes avoidable extra objects and shares polling
across streams, but it cannot remove Drive object visibility latency.

The best achievable speed under these constraints has not been established.
See [Transport Research](transport-research.md) for tested alternatives and
their correctness, quota, and mixed-workload results.

UDP is not a first-class transport. Android VPN mode routes TCP through Skirk;
apps that rely heavily on UDP or QUIC may need to fall back to TCP.

## Verification

```bash
go test ./...

skirk serve-exit --config skirk-kit/exit.json
skirk serve-client --config skirk-kit/client.skirk --listen 127.0.0.1:18080
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

Hostile-path verification:

```bash
skirk bench-live \
  --config skirk-kit/client.skirk \
  --upstream-proxy socks5h://127.0.0.1:11093 \
  --route-mode google_front_pinned \
  --samples 3
```
