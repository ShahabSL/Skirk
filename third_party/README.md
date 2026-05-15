# Third-Party Source

Skirk vendors third-party source only when the client build needs native code
that is not practical to fetch at runtime. The main vendored dependency today is
`hev-socks5-tunnel`, which backs the Android VPN packet bridge.

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
| `third_party/hev-socks5-tunnel` | https://github.com/heiher/hev-socks5-tunnel @ `9573d2dee00ccd8aee5d8fc371fe6f4affb09af7` | Android TUN-to-SOCKS bridge | Upstream CI files, Docker wrapper, demos, and tests not used by the Android build are pruned. |

## Validation

After changing vendored source, run:

```bash
make preflight
cd clients/android && ./gradlew :app:assembleDebug --console=plain
```

Use `make clean` to remove native build outputs before reviewing a patch.
