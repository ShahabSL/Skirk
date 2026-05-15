# Skirk Transport Modes

Skirk has one production transport:

```text
local SOCKS5 / HTTP proxy / Android VPN
-> encrypted Drive Mux v4 objects
-> Google Drive appDataFolder mailbox
-> Skirk exit
-> target TCP
```

Older alternate-carrier experiments are not part of the production path.

## Drive Mux v4

Drive Mux v4 is the default live tunnel transport. It uses Drive
`appDataFolder`, so setup needs only Drive API access and the
`https://www.googleapis.com/auth/drive.appdata` OAuth scope when using the
recommended custom OAuth path.

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

This is the current best shape for Drive because it minimizes object count and
avoids one Drive polling loop per browser connection.

Current protocol lab results show muxv4 as the best proven default under the
current appDataFolder, single-mailbox, Google-fronted/pinned-route constraints.
That conclusion is workload-based: candidates must preserve browsing and media
behavior during bulk transfers, not only improve one bulk download.

## Discovery

```text
exit:   fresh prefix list on muxv4/<session>/up/
client: fresh prefix list on muxv4/<session>/down/<client>/<run>/
```

Runtime discovery stays prefix-scoped because that keeps the hot path narrow and
predictable under mixed browsing and downloads.

## Route Modes

Client profiles default to:

```text
client route: google_front
exit route: direct
```

Available route modes:

- `direct`: normal Google API hostnames and TLS.
- `real_pinned`: connect to a configured Google edge IP while preserving the
  real Google API TLS name.
- `google_front`: use a Google-looking TLS/SNI path for Google API traffic.
- `google_front_pinned`: same idea pinned to `--google-ip`.
- `google_front_h1` and `google_front_h1_pinned`: HTTP/1.1 variants used for
  route experiments.

Use fronted routes only on networks where you are authorized to test and where
normal Google API hostnames are blocked or unreliable.

## Local Frontends

- `serve-client`: SOCKS5 listener for Linux, macOS, Windows, and desktop apps.
- `serve-client --http-proxy-listen`: optional HTTP/HTTPS proxy listener using
  the same tunnel.
- Android app: whole-device VPN mode and optional SOCKS/LAN sharing.
- Windows desktop app: profile import and local SOCKS proxy control.

## Constraints

Google Drive is an object API, not a stream API. A new small request/response
still needs object upload, object discovery, exit processing, response upload,
and response discovery. Skirk removes avoidable extra objects and shares polling
across streams, but it cannot remove Drive object visibility latency.

Large muxv4-only speedups are therefore unlikely without changing a core
constraint such as using multiple mailboxes, a broader OAuth/data-plane split,
or a non-Drive carrier. See [Transport Research](transport-research.md).

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
  --route-mode google_front \
  --samples 3
```
