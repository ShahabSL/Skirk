# Architecture

Skirk is a two-ended TCP transport over Google Drive. The client exposes a local
SOCKS5 proxy, optional HTTP proxy, or Android VPN frontend. The exit machine
watches the same Drive mailbox, opens target TCP connections, and returns
encrypted response frames through Drive.

```text
application
-> local SOCKS5 / HTTP proxy / Android VPN
-> encrypted Drive mux objects
-> Google Drive mailbox
-> Skirk exit
-> target TCP service
```

## Trust Boundary

The generated Skirk profile contains the tunnel secret and Google OAuth refresh
token. Treat it like a password.

Payload bytes are encrypted before they are written to Drive. Drive object names
still contain routing metadata such as session, direction, local client ID, run
ID, lane, and sequence. The exit machine can see target addresses and can see
plaintext for non-TLS application protocols, like any proxy or VPN exit.

## Google Drive Mailbox

The production setup uses Drive `appDataFolder`. That keeps runtime objects out
of the user's visible Drive files and allows the recommended setup flow to use
the narrow `https://www.googleapis.com/auth/drive.appdata` OAuth scope.

Drive is an object API, not a stream API. Every request/response path contains
at least these carrier operations:

1. upload an encrypted object;
2. wait for Drive object visibility;
3. discover the object by listing the direction prefix;
4. download by Drive file ID;
5. delete the processed object after foreground traffic is quiet.

This is the hard latency floor. Skirk can reduce avoidable objects and polling
fanout, but it cannot make Drive behave like a socket.

## Mux v4

Mux v4 is the production transport. Many application streams share a bounded set
of Drive lanes:

- each frame carries stream, sequence, and close metadata inside the encrypted
  payload;
- each Drive object contains one or more frames;
- client downlink objects are namespaced by local client ID and per-run ID, so
  multiple devices can use one copied profile without consuming each other's
  responses;
- priority frames carry opens, closes, small writes, and sparse traffic;
- normal frames carry bulk data and are coalesced to reduce Drive object count;
- upload and download worker windows adapt to Drive health;
- processed objects are deleted by a deferred cleanup loop that yields to
  foreground traffic.

The important design choice is that a mux object is both the discovery item and
the data container. That keeps the hot path simple and minimizes extra control
objects, but it also means sustained bulk transfers increase the number and size
of objects visible to the same prefix-listing path.

## Performance Model

Under the current constraints, large improvements are bounded by Drive publish
and discovery behavior, not local CPU. Known-ID and range-read primitives can
download quickly once the file ID exists, but live transports repeatedly lost
when they added extra control objects, metadata polling, change-feed work, or
large whole-file update tails.

The current evidence says muxv4 is the best proven default for the
`google_front_pinned` hostile-path profile. Future work should be promoted only
when it beats muxv4 on mixed browser and bulk traffic, not just a synthetic
single-stream download.

See [Transport Research](transport-research.md) for the experiment record and
promotion gates.

## Operations

Generated configs enable runtime cleanup. The exit also runs a stale-object
janitor for interrupted clients and crashed exits.

Operators should monitor:

- Drive operation counts and estimated quota units;
- upload, list, download, and delete latency;
- mux object size distribution;
- tiny-object ratio;
- small-request p95 and max latency under bulk load;
- cleanup backlog and delete failures.
