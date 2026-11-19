# UDP support by frontend

New kits enable UDP when the exit supports it. Existing profiles retain their
original capabilities. Both peers must agree on the UDP settings, including
shared leases. The default Google route is `google_front_pinned`.

| Frontend | Legacy TCP-only profile | UDP-enabled profile |
| --- | --- | --- |
| Android VPN | IPv4, HEV mapped DNS | Existing exit DNS, IPv6 route and HEV UDP support |
| Windows/Linux desktop TUN | Local DNS, IPv4, UDP rejected | Dual-stack capture; public UDP uses RFC 1928 SOCKS; exit DNS uses TCP through SOCKS; IPv6 UDP follows `udp_preview_ipv6` |
| iOS packet tunnel | Existing dual-stack capture and HEV mapped A/AAAA | Exit DNS disables HEV mapdns; UDP uses existing HEV TCP framing; authenticated Go policy controls IPv6 UDP |
| macOS desktop | SOCKS/HTTP/system proxy | The Go core accepts the same flags; no native desktop TUN mode is added |

Desktop reads the profile's `udp_preview_public_ipv4`,
`udp_preview_remote_dns`, and `udp_preview_ipv6` flags before starting TUN.
Exit DNS has a single numeric TCP server at `198.18.0.2:53` with explicit
`detour: proxy`. Skirk intercepts that address and sends the query over its
authenticated exit DNS stream. There is no local resolver fallback in this
mode. The existing pinned Google route, sidecar process exclusions and private
LAN exclusions remain unchanged. SOCKS UDP-over-TCP is deliberately absent:
sing-box's optional protocol is not HEV framing.
Public UDP preview profiles capture IPv6 even when IPv6 UDP is disabled, then
explicitly reject IPv6 UDP in that case. This prevents an absent IPv6 route
from becoming a bypass for literal IPv6 destinations; IPv6 TCP still uses
the existing SOCKS transport. Legacy profiles keep their existing routes.

iOS obtains only three validated non-secret booleans from Go, never a decoded
profile. It keeps both IPv4 and IPv6 default routes, including with legacy
profiles, to preserve existing TCP behavior and prevent unsupported IPv6 from
escaping via an omitted route. HEV's cache size is explicitly set on every
start: zero for exit DNS and 10000 for legacy mapped DNS. This also handles
switching profiles in a process whose native parser retains configuration.
Pinned IPv6 Google addresses receive an exact /128 exclusion, matching the
existing IPv4 /32 exclusion. UDP preview profiles advertise MTU 1280 on both
new frontend paths; legacy MTUs remain unchanged.

The vendored HEV bridge preserves the full supported payload when IP reassembly
returns a chain of buffers, including a fragmented legacy mapped-DNS query.
Its virtual interface uses the configured MTU for downlink fragmentation.
Transparent IPv6 input accepts supported extension-header chains before the
existing lwIP parser validates and reassembles them. Oversized native UDP
records are discarded whole rather than forwarding a truncated prefix.

The Go carrier still limits each UDP payload to 1,452 bytes. This is a transport
limit, not a promise of unrestricted IP tunneling: larger UDP datagrams need a
separate negotiated fragmentation design. The native bridge's 1,500-byte
buffer does not increase that carrier limit. Exit DNS uses its separate reply
size and TCP-fallback policy. `scripts/test_hev_udp_fragments.sh` tests real
IPv4/IPv6 fragments through the native bridge using owned local sockets,
including exact 1,300/1,452-byte payloads, reordered fragments, IPv6 options,
downlink MTU enforcement, and oversized-record rejection. It does not use Drive
or a physical device.

The iOS IPv6 flag gates public UDP at the authenticated Go boundary, not
whether the OS captures IPv6. Android's VpnService family-blocking semantics
and iOS's existing dual-stack capture are intentionally different.

Local preview frontends reserve admission before allocating an RFC UDP socket
or HEV reply writer, including unused and DNS-only associations. The bound
follows the configured shared association limit (16 by default). RFC sockets
expire after 120 seconds without accepted traffic in either direction;
successful DNS responses refresh this local lifetime without extending a
shared media lease. Carrier shutdown cancels inline DNS, closes local controls,
and waits for frontend queue cleanup before retiring packet counters.

For shared UDP, an exit operator can set `tunnel.udp_preview_exit_carriers`
in the exit JSON to bound the number of concurrent authenticated carriers.
Omit the setting or use `0` for the default of four. Explicit limits range
from one to eight. Clients keep their existing profiles and local limits.
The runtime metrics file reports `exit_carrier_limit` and
`exit_carrier_limit_hits` separately from `local_frontend_limit_hits`, which
covers local frontend and shared association admissions. The previous
`admission_limit_hits` total also includes `client_carrier_limit_hits`.

Configuration references: vendored HEV `src/hev-config.c` and
`src/hev-socks5-tunnel.c`; the installed Apple SDK's `NEIPv6Settings.h`;
[sing-box TCP DNS](https://github.com/SagerNet/sing-box/blob/v1.13.14/docs/configuration/dns/server/tcp.md),
[dial fields](https://github.com/SagerNet/sing-box/blob/v1.13.14/docs/configuration/shared/dial.md),
and [SOCKS outbound](https://github.com/SagerNet/sing-box/blob/v1.13.14/docs/configuration/outbound/socks.md).
The desktop release currently packages 1.13.12; the fields used here are the
1.12+ DNS and 1.13 TUN schema already used by this branch.

Offline validation covers legacy/preview configuration, route preservation,
invalid flags, secret-free bridge output and profile switching. These checks
do not establish runtime parity: Windows/Linux TUN traffic, real iPhone packet
flow, sleep/wake, switching networks, calls, QUIC, and reconnect still require
platform runs. No physical device, VPN connection or live service is involved
in these checks. The initial build is unsigned; the isolated Simulator UI test
runner uses local ad-hoc signing, without a developer certificate. The cached
official sing-box 1.13.12 container validated eight generated configurations
with `check`, `--network none`, a read-only filesystem and all capabilities
dropped. The matrix covers legacy, public IPv4 UDP, exit DNS with IPv4 UDP,
and dual-stack exit DNS profiles, each with IPv4 and IPv6 Google pins. No TUN
was started.

Validation on the available Apple silicon Mac:

- Go iOS engine tests, race detector, and vet passed with module networking disabled.
- Full `go test ./...` and `go vet ./...` passed offline.
- Swift package tests passed, including legacy → exit DNS → legacy configuration.
- Desktop Rust tests compiled for macOS arm64: 28 passed, 2 tests that mutate
  system networking remained ignored.
- `cargo build --offline` produced the macOS arm64 desktop executable.
- Fresh Go and HEV arm64 iOS Simulator libraries built offline; all extension
  Swift sources type-checked against that generated Go API.
- The full arm64 iOS Simulator app and extension linked successfully with
  `CODE_SIGNING_ALLOWED=NO`.
- Four existing UI tests passed on a fresh iOS 26.5 Simulator: first-run
  guidance, synthetic profile import/replace/remove, legacy route selection,
  and route persistence through relaunch. The app launched successfully after
  the tests. No private profile was imported and no Connect action was taken.

A separate isolated Linux test also started the generated sing-box TUN and
sent IPv4/IPv6 TCP and UDP, UDP DNS A, and TCP DNS AAAA queries from a distinct
application process. It used the product mux/shared-UDP core with a mocked
Drive store and owned origin fixtures. It checked the Google-pin exclusion and
route restoration after stopping. This exercises actual Linux TUN forwarding;
its fixture timings are not live Drive performance, and it does not substitute
for Windows or physical iOS validation.
