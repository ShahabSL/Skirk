# Android VPN integrity smoke — 14 November 2026

This is a bounded runtime check of the current experiment branch on a Pixel 7
API 36 Android emulator. The current debug APK used a fresh session cloned
from the isolated `shahabpre` test kit, with Google front pinned routing and
the published test OAuth grant. The emulator had no Google account signed in.
No main account or existing Drive mailbox session was used.

## Method

1. Install the current Android debug APK and a disposable hash-probe APK. Pipe
   the fresh client configuration into Skirk's app-private debug import path;
   confirm the private test profile is selected. Start the cloned macOS exit.
2. Start a local loopback origin containing deterministic synthetic bytes and
   expose it over a temporary Cloudflare Quick Tunnel. Android VPN excludes
   private LAN ranges, so a public HTTPS hostname is required to prove that
   this traffic traverses the VPN. The origin caps `/blob` at three downloads
   and three uploads and verifies the incoming body hash.
3. Connect Skirk VPN. Verify Android `tun0` at `198.18.0.1/30`. Load the small
   HTTPS origin page in Chrome without signing in. Run one 8 MiB GET and one
   8 MiB POST from the separate probe app, checking the full SHA-256 digest on
   both ends. Limit each transfer to 120 seconds and the live run to five
   minutes.
4. Stop the probe, browser, VPN, exit, origin, and temporary tunnel. Delete
   only the private debug profile. Clean only the newly generated
   `muxv4/<session>/` Drive object prefix, verify zero matches immediately
   and again after 30 seconds, then remove the cloned local credentials.

## Result

| Check | Observation |
| --- | --- |
| VPN | Connected through local SOCKS at 20:08:40 CST; `tun0` present during traffic and absent after disconnect at 20:10:59. |
| Chrome | Public HTTPS synthetic page rendered through the active VPN. |
| GET | 8,388,608 bytes, SHA-256 match, **7,706 ms** (**1.04 MiB/s** payload rate). |
| POST | 8,388,608 bytes, origin SHA-256 match, **14,497 ms** (**0.55 MiB/s** payload rate). |
| Digest | `0ff4d6c068be24637e84ea9f481c3c29f7afcdef1e06e1f40a68e5de85dcbb5b` for both bodies. |
| Origin | One page, two health checks (one Mac setup check and one Android probe), one download, one upload, **zero failures**. |
| Exit quota sample | The 20:10:06 log reported a 1m01s window of **221 calls**, approximately **21,805 Drive units**, **zero errors**, and 8,631,251 response bytes. Its operation counts were 117 lists, 39 uploads, 33 downloads, 31 deletes, and one generate IDs call. |
| Cleanup | Exact fresh prefix: **11 objects deleted** in one page; immediate and delayed 30-second dry-run scans each matched **zero**. No `--all` cleanup. |

The Android log showed VPN startup, connection, and targeted stop without a
Skirk quota or backoff error. The live VPN interval was about 2m19s; the exit
ran about 2m56s. The profile was deleted and the fresh cloned credential
files were removed after cleanup.

The ignored local evidence is
`.skirk-runs/android-vpn-session-20261114-27a183b6/result.md`, with the
Chrome screenshot, probe log, and VPN log beside it. The temporary hostname,
OAuth credentials, and configuration contents are excluded from this note.

These rates include the Android emulator, Drive transport, and the extra
Cloudflare tunnel hop. This check proves one successful integrity-checked
transfer in each direction and a browser page load. It does not measure a
transport ceiling, pure message latency, sustained mixed traffic, reconnect
behavior, battery use, or multi-user capacity.
