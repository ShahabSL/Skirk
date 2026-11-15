# MasterHttpRelayVPN-RUST: transferable architecture review

Read-only source review on 2026-11-15 of
`therealaleph/MasterHttpRelayVPN-RUST` at
`b32f65f0cdec0602c9ffe4db0ed24b81d974cd6f`. No build, certificate installation,
public relay request or third-party origin traffic was performed. Source was
retained only in the ignored experiment directory. This is feasibility
analysis, not measured performance or a claim that its published presets work
on this network.

## A real alternative critical path

The repository includes Full Tunnel mode, beyond its separate HTTP/MITM relay.
Its [proxy dispatch](https://github.com/therealaleph/MasterHttpRelayVPN-RUST/blob/b32f65f0cdec0602c9ffe4db0ed24b81d974cd6f/src/proxy_server.rs#L1760)
passes raw streams to a batch mux. No destination certificate interception is
required for that path. The [Apps Script bridge](https://github.com/therealaleph/MasterHttpRelayVPN-RUST/blob/b32f65f0cdec0602c9ffe4db0ed24b81d974cd6f/assets/apps_script/CodeFull.gs#L137)
forwards operations to a user-operated HTTP tunnel-node, which owns persistent
TCP sockets. Compressed batches can pass through the script opaquely.

An equivalent Skirk design could carry authenticated encrypted frames through
one synchronous Google-fronted POST → owned HTTP relay → response, eliminating
Drive create, visibility, discovery, read and delete from that exchange. This
is a materially different transport worth investigating, not evidence that
Drive itself can achieve the same critical path. It adds Apps Script
deployment, an HTTPS exit endpoint and separate quotas. The client's existing
user-selected `www.google.com` front must also carry every redirect; its
reachability remains untested here. Google documents that Content Service
responses redirect to a one-time `script.googleusercontent.com` URL.
[Content Service](https://developers.google.com/apps-script/guides/content#redirects).

## Useful ideas and traps

The [client](https://github.com/therealaleph/MasterHttpRelayVPN-RUST/blob/b32f65f0cdec0602c9ffe4db0ed24b81d974cd6f/src/tunnel_client.rs)
bundles connection setup with first payload bytes, adapts coalescing using an
RTT window and hysteresis, and increases per-session pipeline depth only while
data arrives. A global elevated-session cap limits that acceleration. Skirk
already embeds first bytes in stream opens and has fairness/interaction
scheduling; these ideas are not all missing. RTT-based coalescing and narrowly
scoped unreachable-target caching could be separate A/B candidates, with hard
interactive deadlines and bounded memory rather than copying its constants.

The same client explicitly warns that the tunnel-node drains received bytes
before successful client delivery. Losing a response therefore loses those
bytes; replay can also duplicate writes. Its response sequence ordering does
not replace retained-response acknowledgements. A reliable Skirk relay must
retain bounded encrypted response chunks until cumulative acknowledgement,
deduplicate request/write offsets and retry identical operations safely.
Timeout/drop/reorder fault tests must prove both directions before speed tests.
The source also uses an unbounded mux channel; Skirk should retain its explicit
queue bounds.

The [node](https://github.com/therealaleph/MasterHttpRelayVPN-RUST/blob/b32f65f0cdec0602c9ffe4db0ed24b81d974cd6f/tunnel-node/src/main.rs#L1140)
uses notifications to wake an empty long poll on upstream data instead of
sleeping on a fixed cadence. Separate active and idle drain deadlines, bounded
straggler settling and a total batch-response byte budget are useful. They
apply naturally to an owned streaming relay, not to Drive's object API: Drive
does not expose the node's socket notifications. A poll must not block outbound
messages, and one bulk session must not fill the whole response budget.

The script has optional DNS caching/DoH near the relay. Compression is not
encryption: a Skirk variant must keep its AEAD envelope opaque through Apps
Script, including destination/control metadata. Raw encrypted TLS data usually
will not compress materially. Direct DNS or host-bypass branches in the other
project must not silently change Skirk's selected route. Separately, Skirk's
current raw SOCKS UDP DNS A helper calls local `net.LookupIP`. This is distinct
from the normal Android and iOS VPN path: both configure HEV mapped DNS at
`198.18.0.2`, synthesize addresses from `240.0.0.0/4`, then recover domain names
for SOCKS forwarding; iOS also configures mapped IPv6. HEV intercepts those DNS
packets before the Go UDP helper. Thus the helper observation alone does not
establish that normal VPN DNS uses local resolution. Relevant local source is
`clients/android/.../SkirkVpnService.kt`,
`clients/ios/TunnelExtension/PacketBridge.swift`,
`third_party/hev-socks5-tunnel/src/hev-socks5-tunnel.c`, and
`third_party/hev-socks5-tunnel/src/misc/hev-utils.c`.

A falsifiable platform check should block or poison external DNS only inside
an isolated emulator/test network, preserve the pinned Google route, then
query a fresh hostname through VPN mapped DNS and connect to the returned
synthetic address. Require a SOCKS domain target at the exit, a successful
connection, and no external local DNS packet for that hostname. Test raw SOCKS
UDP DNS separately, plus literal IPv4/IPv6, expired mappings and an application
using its own DoH. Record Android and iOS independently. No host-wide DNS or
firewall change, runtime leak claim or cache implementation was made here.

## Documented limits, not multiplied by deployment IDs

Google currently lists 20,000 URL Fetch calls/day for consumer accounts and
100,000 for Workspace, 30 simultaneous executions per user, 1,000 per script,
six minutes per execution and 50 MB URL Fetch request/response limits. Quotas
are per user and can change. Multiple deployments on the same account do not
multiply the per-user quota; the reviewed client keys its 30-slot pool by
deployment ID, so a Skirk adaptation needs account-aware admission control.
[Official Apps Script quotas](https://developers.google.com/apps-script/guides/services/quotas).

At one fetch per batch, 20,000/day is an average 0.231 batches/second per
consumer account, or one every 4.32 seconds continuously. A continuously empty
four-second poll would already consume about 21,600 calls/day before active
messages. This arithmetic is a planning bound, not a capacity benchmark.
Batching amortizes calls over users; it does not remove the limit. No 200,000
user capacity follows from the source or this analysis.

Google's [URL Fetch API](https://developers.google.com/apps-script/reference/url-fetch/url-fetch-app)
is HTTP/HTTPS request/response with complete response objects and an external
request authorization scope. Its `fetchAll` can issue multiple fetches, but
that is not an unlimited raw TCP socket or an exemption from fetch accounting.
The user-operated node supplies the TCP capability.

## Next falsifiable experiment

First measure a minimal authenticated, AEAD-opaque owned echo through the
chosen Google front, including redirect hops, body bounds, cold/warm paths and
idle long-poll interruption. Then prove retained-response ACKs, replay safety,
cleanup/session expiry and account-level quota limits with fault injection.
Only then compare the same persistent-message plus bulk workload with Drive
and include Apps Script fetch use, extra exit requirements and all failures.
Until those steps run, this remains a plausible alternative, not a rejected or
winning benchmark.
