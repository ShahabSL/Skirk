# Architecture

New Skirk kits carry TCP streams, UDP datagrams and exit DNS over Google
Drive. Setup enables IPv6 when the exit route passes its connectivity checks.
Applications use the client's SOCKS5 proxy, HTTP proxy or native VPN interface.
The exit reads encrypted traffic from the shared mailbox and opens the
corresponding Internet sockets. The return path uses the same mailbox.

```text
application
-> local SOCKS5 / HTTP proxy / native VPN
-> encrypted stream or datagram objects
-> Google Drive mailbox
-> Skirk exit
-> destination TCP or UDP socket
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
or cleanup.

## Mux v5

New kits use Mux v5 as the default transport. It retains Mux v4's batching and
Drive API operations, while a separate `muxv5/` namespace and a fresh
generation-bound encryption key on each sender start prevent the reproduced
restart key/nonce collision. Many application streams share a bounded set of
Drive lanes:

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

For each exit instance, `tunnel.exit_max_streams` counts established mux
streams and pending opens together, including DNS and UDP control streams.
Omit it or set it to `0` for automatic sizing. The default allows up to
1,024 streams. On Linux and Apple systems, automatic sizing uses at most
half the process's soft file-descriptor limit, leaving descriptors for the
Drive and UDP sockets and local listeners. Explicit limits from 1 through
4,096 override automatic sizing. At 1,024 idle TCP streams, the minimum
32 KiB read buffers account for 32 MiB in addition to sockets and shared
upload and receive queues.
Up to 128 opens can be pending, with at most 64 simultaneous dials. A new
open at capacity is refused while existing streams continue. Closing a pending
stream cancels its outbound dial and the worker releases its place when it
returns. The exit logs
`mux exit stream capacity` with a cumulative count. Raising the limit uses
more file descriptors and memory and leaves the Drive request budget intact.

## Datagram carrier

UDP uses its own queues and immutable Drive objects. An authenticated mux
control stream negotiates the destination policy and owns the carrier's
lifetime. One client carrier can serve multiple local associations. Each
retains its own flow bindings and exit source sockets.

An exit at its UDP carrier limit returns an authenticated refusal on the
existing control stream. The client records that separately from a local UDP
connection limit. Older exits can close the stream without a reason. The
client reports a remote capacity event only after verifying the reply against
its fresh challenge. Existing carriers continue serving their associations.

Small sealed batches travel in discovery metadata. Larger batches use media
objects, with bounded upload and download concurrency. Byte-based fair queuing
prevents one association from taking the entire pending queue. UDP remains
unreliable: late packets expire, duplicate batches are rejected, and uncertain
creates are not blindly retried. Publisher failures and receiver delivery
counters describe different stages and must not be added together as a loss
rate. Exact datagram sequence checks are needed to measure end-to-end loss.

The carrier uses its own same-route HTTP pool and shares authentication,
quota accounting, and provider backoff with TCP. Separate HTTP pools still use
the same underlying network and Google service, so they do not guarantee
latency isolation from bulk transfers. Deferred deletion handles processed
objects; a bounded, age-checked janitor removes orphaned UDP objects after
interrupted sessions. See [frontend parity](udp-preview-platform-parity.md) for
the opt-in profile requirements and platform limits.

## Performance Model

Drive publish and discovery times affect interactive latency. Large file throughput alone cannot predict call quality. Test file integrity, short-message delivery and UDP media together when evaluating this transport.

For upgrade compatibility, see the [migration guide](mux-v5-migration.md).

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
