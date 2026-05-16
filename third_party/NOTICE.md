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

Skirk builds this project into `libhev-socks5-tunnel.so` and uses it as the
Android TUN-to-SOCKS packet bridge behind `VpnService`.

## sing-box

- Project: `sing-box`
- Upstream: https://github.com/SagerNet/sing-box
- Release artifact used by Skirk Windows releases: `sing-box-1.13.12-windows-amd64.zip`
- Release SHA-256: `e93fc531134eb1beb4efa3c74990a24e48456098a31c03b60d5ddf17f223cf98`
- License: GPL-3.0-or-later
- License file included in Windows portable releases: `third_party/sing-box-LICENSE.txt`
- Corresponding source: https://github.com/SagerNet/sing-box/tree/v1.13.12

Skirk Windows portable releases stage the `sing-box.exe` release binary as
`skirk-tunnel.exe` and run it as a separate VPN/TUN sidecar process for Windows
VPN mode. The Skirk repository does not vendor sing-box source code.

## Nested Components

The vendored tree also includes these nested components and license files:

- `third_party/hev-socks5-tunnel/src/core/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/hev-task-system/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/yaml/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/lwip/License` - BSD-style lwIP license
- `third_party/hev-socks5-tunnel/third-part/wintun/LICENSE.txt` - Wintun prebuilt binaries license

Review these license files before redistributing Android VPN artifacts, and
refresh this notice whenever the vendored source is updated.
