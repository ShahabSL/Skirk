# Native Linux client and exit traffic, November 15, 2026

An isolated worktree at `d310ce9` built the Go CLI for Linux ARM64 and ran it
inside the Apple Silicon Docker Desktop Linux VM. Separate disposable client
and exit containers used a fresh session from only the `shahabpre` test grant,
the pinned `www.google.com` front at `216.239.38.120`, and separate network
namespaces. The exit's deterministic HTTP origin listened on its own
`127.0.0.1`; the client could reach it only through Skirk's Drive transport.
No host route was changed, and no OAuth material entered the image or Git.
The tested Go binary's SHA-256 was
`0b05aa2ebe033f76da8426129072501952ddb2d4cf50075133043c41e2dce346`.

The Linux client ran a SOCKS5 listener on its container loopback. `curl`
used `--socks5-hostname` to fetch the exit-only origin, and the native
`probe-exit` checked the authenticated exit response. The origin returned a
20-byte fixed text body and deterministic 1 MiB bodies; the GET was compared
byte for byte and the POST acknowledgment was compared against the source
length and SHA-256.

| Check | Observed time | Result |
| --- | ---: | --- |
| Authenticated exit proof | 2.275 s | Verified |
| Exact text GET | 3.206 s | HTTP 200; `skirk-linux-text-ok\n` |
| Exact 1 MiB GET | 3.442 s | HTTP 200; SHA-256 `fbbab289f7f94b25736c58be46a994c441fd02552cc6022352e3d86d2fab7c83` |
| Exact 1 MiB POST | 4.018 s | HTTP 200; length 1,048,576; acknowledged SHA-256 `eaeaa7acca0afcaee85d7abae4d8e5033652991ea19df161cc90ceec2803342c` |

The client container also ran the official Linux ARM64 sing-box **1.13.12**
image pinned by digest
`sha256:da0e2331395c9025a85fa58892772b4cdbe5f2e530e93defeec3968175d06c6d`.
Its TUN configuration mirrored the desktop backend's Linux fields: automatic
route, mixed stack, pinned Google and private-range exclusions, the `skirk`
process-name bypass, local SOCKS outbound, and DNS handling. The test's
process-path bypass named a different path than the container binary, but the
`skirk` process-name rule covered that binary. This was a manual native
sidecar invocation, not a Tauri GUI Connect test.

The container's original public route was via `eth0` and `172.28.0.1`.
While the sidecar ran, `ip route get 1.1.1.1` selected `skirk-tun0` in table
2022, and unproxied `curl` fetched `https://api.ipify.org` with HTTP 200 in
**5.299 s**. The observed public IP matched the exit container's direct
request. Those containers share one host NAT, so the IP match alone does not
prove the exit path; the route selection plus the authenticated exit-only
origin and Drive operations are the stronger evidence. After terminating
sing-box, `skirk-tun0` and all added rules disappeared, and the original
`eth0` route returned. The fallback was removal of only the disposable
client container.

At shutdown after about 4.5 minutes, the client recorded 73 Drive calls,
7,005 estimated units and zero errors; the exit recorded 333 calls, 33,405
estimated units and zero errors. Both showed no active backoff. The exit's
300 list calls accounted for 30,000 units, showing that idle polling dominates
this short session's measured API cost. These are local process estimates,
not project-wide quota usage or a sustainable user-capacity measurement.

After both containers stopped, exact-prefix cleanup of this session's
`muxv4/` and `slotv1/` objects found zero residual objects. Both prefixes
were empty on immediate and more-than-30-second delayed scans, with zero
cleanup failures. The runtime had already removed its own objects. The
temporary local credentials, test origin, and image were removed after the
run. This validates one native Linux ARM64 CLI SOCKS and container-isolated
TUN TCP path; it does not validate a Linux desktop GUI click, a physical
Linux machine, sustained mixed traffic, or a maximum-throughput claim.
