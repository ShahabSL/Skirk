# Third-Party Notices

This directory contains source code redistributed with Skirk for Android VPN
support plus notes for external binaries staged by release builds. Keep this
notice in sync with vendored source changes.

## hev-socks5-tunnel

- Project: `hev-socks5-tunnel`
- Upstream: https://github.com/heiher/hev-socks5-tunnel
- License: MIT
- License file: `third_party/hev-socks5-tunnel/License`
- Local path: `third_party/hev-socks5-tunnel`

Skirk builds this project into `libhev-socks5-tunnel.so` for Android `VpnService`
and into a static framework for the iOS packet tunnel.
The vendored build preserves complete supported UDP payloads after IP
reassembly and uses the configured tunnel MTU for lwIP output.
It also preserves empty UDP datagrams in both directions.

## sing-box

Skirk runs the unmodified [sing-box 1.13.12](https://github.com/SagerNet/sing-box/releases/tag/v1.13.12)
as a separate VPN process in the Windows and Linux portable packages.
The Windows helper is named `skirk-tunnel.exe` and the Linux helper is named
`skirk-tunnel`. Both packages include the GPL-3.0-or-later license and the
matching source archive under `third_party/`. The
[upstream source archive](https://github.com/SagerNet/sing-box/archive/refs/tags/v1.13.12.tar.gz)
contains the Go module definitions and release build scripts.

The Windows helper embeds the [Wintun 0.14.1](https://www.wintun.net/)
driver from WireGuard LLC. The portable package includes the upstream
prebuilt binaries license at `third_party/wintun-LICENSE.txt`.

| Upstream archive | SHA-256 |
| --- | --- |
| sing-box-1.13.12-windows-amd64.zip | `e93fc531134eb1beb4efa3c74990a24e48456098a31c03b60d5ddf17f223cf98` |
| sing-box-1.13.12-linux-amd64.tar.gz | `1540533adb3df24f5ad5f14b5c7ca3dbc2401b10a1c1eb278fcadcada47ec6c4` |
| sing-box v1.13.12 source tarball | `08f3f7295130b76a60c1cb6565b89da6c4ce98d7bdea8852973a5d2e3c6de3b7` |

## Nested Components

The vendored tree also includes these nested components and license files:

- `third_party/hev-socks5-tunnel/src/core/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/hev-task-system/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/yaml/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/lwip/License` - BSD-style lwIP license
- `third_party/hev-socks5-tunnel/third-part/wintun/LICENSE.txt` - Wintun prebuilt binaries license

Review these license files before redistributing Android VPN artifacts, and
refresh this notice whenever the vendored source is updated.
