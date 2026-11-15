# macOS system proxy: bounded native test, November 15, 2026

## Change and safety boundary

The desktop app now offers **System proxy** on macOS. It selects the network
service associated with the current default-route interface, saves that
service's HTTP and HTTPS proxy settings in a private atomic backup, and points
proxy-aware apps at the local Skirk HTTP listener. It refuses to replace an
enabled manual proxy, PAC, WPAD, SOCKS proxy, or stored authenticated HTTP
proxy settings. Disconnect and next launch restore only fields whose server
and port still match Skirk; a user or other app's later proxy change is left
alone. A second read immediately before applying reduces the chance of
overwriting a concurrent edit, though macOS `networksetup` has no atomic
compare-and-swap operation. A hard crash can leave a dead local proxy until
the app launches again or the user disables it in Network settings. This mode
does not capture traffic from apps that ignore macOS proxy settings.

## Verification

- Rust desktop suite: 26 passed, 2 explicitly ignored live tests, 0 failed.
  Frontend TypeScript/Vite build and final macOS Tauri app bundle build passed.
- On a temporary **duplicate** of the active network service, an isolated
  live test enabled HTTP/HTTPS, checked both exact local endpoints, recovered
  them after simulated app restart, and preserved a subsequent external web
  proxy edit while restoring Skirk's secure-web field. The duplicate service
  was removed.
- On the active Wi-Fi service, a separate bounded live test set HTTP/HTTPS to
  `127.0.0.1:18991`. Native Swift `URLSession` requested
  `http://example.com/<unique-probe>` without an explicit proxy. A local
  listener received the absolute HTTP proxy request and returned the exact
  response. Disconnect restored the original disabled/empty/port-zero
  settings. An independent 45-second watchdog later found both fields already
  restored and made no changes.
- The native Tauri app then imported a fresh disposable `shahabpre` Drive
  session, selected the pinned `www.google.com` front, and connected in
  **System proxy** mode. The GUI **Test exit** returned an authenticated proof
  in **3.927 s**. For deterministic origin bytes, a test-only exit-side SOCKS
  mapper sent `example.com:19090` to a local origin. Native `URLSession`
  supplied no proxy argument. The exact text GET returned HTTP 200 in
  **2.232 s**; a SHA-verified 1 MiB GET returned HTTP 200 in **3.731 s**;
  and a 1 MiB POST returned the matching SHA-256 in **5.530 s**. Over these
  transfers, cumulative Drive calls rose by **283 client / 280 exit**, with
  client upload +87 and exit download +86, and **zero API errors** on both.
  The matching origin response at `example.com:19090` could only be reached
  through this test mapper and the Skirk exit. An initial URLSession probe to
  a local LAN IP bypassed the system proxy and is excluded from these figures.
- The GUI disconnected and removed its saved test profile. Wi-Fi HTTP and
  HTTPS both returned to disabled, empty server, port zero. The exit and local
  test servers stopped. Exact cleanup deleted **59** residual objects and
  found zero objects under both generated `muxv4/` and `slotv1/` prefixes
  immediately and after 30 seconds, with no failed or truncated sweep. The
  independent 120-second watchdog later found settings restored and did
  nothing. Disposable OAuth config copies were removed.

The full Drive run used the app bundle immediately before the final
pre-apply reread and generic **System proxy** metric label were added. The
final source passed the Rust unit and duplicate-service tests and rebuilt as
a native app. A second full Drive run on that exact final bundle was not
performed while another controlled Drive comparison was running. This is one
proxy-aware native client test, not Safari/Chrome coverage, whole-device VPN,
a signed/notarized public package, or a sustained speed benchmark.

Private raw results, metrics snapshots, watchdog results, and exact cleanup
are under `.skirk-runs/mac-system-proxy-live/` in the isolated worktree.
