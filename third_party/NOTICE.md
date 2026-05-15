# Third-Party Notices

This directory contains source code redistributed with Skirk for Android VPN
support. Keep this notice in sync with vendored source changes.

## hev-socks5-tunnel

- Project: `hev-socks5-tunnel`
- Upstream: https://github.com/heiher/hev-socks5-tunnel
- License: MIT
- License file: `third_party/hev-socks5-tunnel/License`
- Local path: `third_party/hev-socks5-tunnel`

Skirk builds this project into `libhev-socks5-tunnel.so` and uses it as the
Android TUN-to-SOCKS packet bridge behind `VpnService`.

## Nested Components

The vendored tree also includes these nested components and license files:

- `third_party/hev-socks5-tunnel/src/core/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/hev-task-system/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/yaml/License` - MIT
- `third_party/hev-socks5-tunnel/third-part/lwip/License` - BSD-style lwIP license
- `third_party/hev-socks5-tunnel/third-part/wintun/LICENSE.txt` - Wintun prebuilt binaries license

Review these license files before redistributing Android VPN artifacts, and
refresh this notice whenever the vendored source is updated.
