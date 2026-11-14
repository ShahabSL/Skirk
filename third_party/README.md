# Third-Party Source

Skirk vendors third-party source only when the client build needs native code
that is not practical to fetch at runtime. The main vendored dependency today is
`hev-socks5-tunnel`, which backs the Android and iOS VPN packet bridges.

## Policy

- Keep vendored source under `third_party/`.
- Keep license files from upstream intact.
- Document every nested third-party component in `third_party/NOTICE.md`.
- Do not mix generated build outputs into the vendored tree.
- Keep local patches minimal and document them in this file.
- When refreshing a vendored dependency, record the upstream project, upstream
  revision, refresh command, local patches, and validation result in the commit.

## Current Inventory

| Path | Upstream | Purpose | Local patches |
| --- | --- | --- | --- |
| `third_party/hev-socks5-tunnel` | https://github.com/heiher/hev-socks5-tunnel @ `9573d2dee00ccd8aee5d8fc371fe6f4affb09af7` | Android and iOS TUN-to-SOCKS bridge | Upstream CI files, Docker wrapper, demos, and tests are pruned. Skirk adds an opt-in IPv6 mapped DNS path for iOS and a focused native test. |

The local mapped DNS patch adds `mapdns.ipv6-network`, which enables AAAA
answers using a ULA `/96` prefix and converts those addresses back to SOCKS
domain names. Configurations without that key retain the upstream IPv4-only
behavior. It also preserves the mapped destination's address family for UDP
replies, validates fake IPv4 network membership before a reverse lookup, and
fixes DNS response counts and question offsets. The network membership fix was
merged upstream in [2.18.0](https://github.com/heiher/hev-socks5-tunnel/pull/335);
IPv6 mapped DNS remains an [open upstream request](https://github.com/heiher/hev-socks5-tunnel/issues/302).

## Validation

After changing vendored source, run:

```bash
make preflight
cd clients/android && ./gradlew :app:assembleDebug --console=plain
scripts/test_hev_mapped_dns.sh
clients/ios/build.sh
```

Use `make clean` to remove native build outputs before reviewing a patch.
