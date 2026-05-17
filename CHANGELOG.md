# Changelog

## Unreleased

## v0.1.40 - 2026-05-17

- Added `skirk service` and expanded the operator menu for setup, systemd
  service lifecycle, Drive cleanup, OAuth revocation, and local kit deletion.
- Stopped new setup runs from launching the blocked default Google Cloud SDK
  OAuth client for Drive scopes; release builds can now use Skirk's built-in
  device OAuth client through Google's URL/code device flow.
- Switched the public device-code setup scope to `drive.file` and a
  Skirk-created Drive mailbox folder, because Google rejects `drive.appdata`
  during the tested device-code request.
- Clarified Windows release packaging so the portable desktop zip is the GUI
  app and `skirk-windows-amd64.zip` is documented as CLI-only.
- Clarified install commands to use the absolute installed binary path when
  shell `PATH` propagation is unreliable.
- Added donation placeholders to the README.
- Fixed Android sidecar startup validation so stale local listeners are not
  accepted as a healthy new engine, without using an Android parent-death signal
  that can terminate valid app-launched sidecars.
- Added Drive Mux v4 client/run namespacing so the same copied `skirk:` profile
  can run on multiple devices at the same time without response races.
- Updated Android and Windows clients to pass stable per-profile client IDs to
  the Skirk sidecar.
- Added Drive Mux v4 documentation as the single production transport.
- Added docs for exit-side proxy forwarding, mailbox janitor cleanup, live
  benchmarks, quota telemetry, and Drive Changes based discovery.
- Hardened Android VPN mode around the proven Drive transport flags, IPv4-only
  routing, TCP fallback for app media traffic, and real-device Reels plus bulk
  download validation.
- Added SOCKS DNS/UDP tests covering AAAA suppression and non-DNS UDP refusal.
- Switched Android release assets from debug APKs to release-signed APKs and
  added GitHub artifact attestations for published release archives/APK.
- Updated setup docs around one-line `skirk:` profiles, `serve-client`,
  `serve-exit`, custom OAuth setup, and Drive mailbox folders.
- Removed stale references to alternate runtime control lanes and visible Drive
  folder cleanup from user-facing docs.

## v0.1.3 - 2026-05-02

- Replaced noisy Google Cloud CLI setup with quiet archive installation.
- Fixed Ctrl-C handling for Skirk menu prompts and long-running commands.

## v0.1.2 - 2026-05-02

- Added automatic Google Cloud CLI install/check during server setup.
- Added one-line `.skirk` client configs for paste-friendly sharing.
- Added config export/decode commands while keeping JSON compatibility.

## v0.1.1 - 2026-05-02

- Added official Skirk logo assets.
- Added the Skirk terminal banner.
- Updated desktop and Android launcher icons.

## v0.1.0 - 2026-05-02

- Added Go Skirk CLI with a Google Drive mailbox transport.
- Added one-command Google kit setup and config generation.
- Added Linux SOCKS5 client mode and exit mode.
- Added optional browser dashboard and Windows desktop wrapper.
- Added Android VpnService scaffold.
- Added Linux installer, release packaging, CI, and preflight checks.
