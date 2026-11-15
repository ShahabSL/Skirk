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

The public setup flow uses a Skirk-created Drive mailbox folder with
`https://www.googleapis.com/auth/drive.file`. That scope lets Skirk manage only
files and folders created by the Skirk app, while keeping the Google device-code
login flow available for normal users.

Drive is an object API, not a stream API. The default media path contains
these carrier operations:

1. upload an encrypted object;
2. wait for Drive object visibility;
3. discover the object by listing the direction prefix;
4. download by Drive file ID;
5. delete the processed object after foreground traffic is quiet.

The experimental `drive.inline_small_objects` path can carry eligible sealed
priority payloads in discovery metadata, omitting step 4 for those objects while
retaining their media body for compatibility. It remains off by default. This
removes one API round trip; it does not remove upload, visibility, discovery,
or cleanup. See the [audit experiments](audit-experiments-2026.md).

## Mux v4

Mux v4 is the production transport. Many application streams share a bounded set
of Drive lanes:

- each frame carries stream, sequence, and close metadata inside the encrypted
  payload;
- each Drive object contains one or more frames;
- client downlink objects are namespaced by local client ID and per-run ID, so
  multiple devices can use one copied profile without consuming each other's
  responses;
- priority frames carry stream opens, resets, first bytes embedded in opens,
  and ordered small-stream follow-on data/FIN while the stream remains under
  the small-stream threshold;
- normal frames carry demoted and bulk data, preserving stream sequence order
  while coalescing bulk data to reduce Drive object count;
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

Muxv4's initial priority allowance is per TCP stream. A long-lived messenger
connection that has sent more than 64 KiB stays in the normal class, even for
later short messages. A bounded interaction-priority and metadata-only
experiment improved short paired text-plus-file workloads, but some 100 MiB
file arms stalled. Its per-process priority budget also cannot enforce one
account-wide budget across devices. These experiments have not changed the
default; see the [architecture frontier](architecture-frontier-2026.md) for
the measurements and remaining correctness and quota gates.

See [Transport Research](transport-research.md) for the experiment record and
promotion gates.

## Operations

Generated configs enable runtime cleanup. The exit also runs a conservative
stale-object janitor at startup and then every 2 minutes for interrupted
clients and crashed exits. The janitor is not the primary hot-path cleanup:
consumed mux objects are deleted by foreground-aware runtime cleanup, while the
janitor uses a 24 hour age window and low delete concurrency so slow
long-running downloads are not mistaken for stale crash leftovers.

Operators should monitor:

- Drive operation counts and estimated quota units;
- upload, list, download, and delete latency;
- mux object size distribution;
- tiny-object ratio;
- small-request p95 and max latency under bulk load;
- cleanup backlog and delete failures.
