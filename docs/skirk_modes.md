# Skirk Modes

Date: 2026-05-11

Skirk has one production transport for browsing:

```text
local SOCKS/HTTP proxy or Android VPN
-> encrypted mux batches
-> Google Drive appDataFolder mailbox
-> Skirk exit
-> target TCP
```

## Production Transport

### Drive Mux v3

Drive Mux v3 is the default and only live tunnel transport. It uses Drive
`appDataFolder` for both control and data, so setup needs only Drive API access
and the `drive.appdata` OAuth scope.

The runtime no longer creates independent Drive objects for every TCP connection
event. It groups active TCP streams into four mux lanes:

- `OPEN` stays on a stable lane so new streams are cheap to discover;
- data and `FIN` frames carry per-stream sequence numbers;
- bulk frames are striped across lanes and reassembled in order at the receiver;
- each lane has bounded upload workers, so Drive create latency is overlapped
  without making unbounded local queues;
- object downloads are processed concurrently, so one slow Drive object does not
  block every other ready object;
- `OPEN` can carry the first client bytes, avoiding a separate object for the
  first request payload;
- all mux objects are AEAD-encrypted with lane-specific keys.

This is the best Drive shape for browsing because it minimizes object count
without turning every browser connection into head-of-line blocking.

## Supported Local Frontends

- `serve-client`: SOCKS5 listener for Linux, macOS, Windows, and desktop apps.
- `serve-client --http-proxy-listen`: optional HTTP proxy listener sharing the
  same mux instance as SOCKS.
- Android app: VPN mode for whole-device routing and optional SOCKS/LAN sharing.
- Windows desktop app: profile import and local proxy control.

## Constraints

Drive is an object API, not a stream API. Even in the best protocol shape, a new
small request/response needs object upload, object discovery, exit processing,
response upload, and response discovery. Skirk reduces avoidable extra objects
and avoids per-stream polling, but it cannot remove Drive's object visibility
latency.

## Verification

```bash
go test ./...
skirk serve-exit --config skirk-kit/exit.json
skirk serve-client --config skirk-kit/client.skirk --listen 127.0.0.1:18080
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```
