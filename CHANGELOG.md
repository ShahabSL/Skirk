# Changelog

## Unreleased

- Added a setup fallback from Drive `appDataFolder` to a normal Drive mailbox
  folder when Google Cloud CLI ADC is missing the `drive.appdata` grant.
- Updated setup login to request both full Drive and `drive.appdata` scopes for
  ADC so mailbox validation can recover cleanly.
- Clarified Windows release packaging so the portable desktop zip is the GUI
  app and `skirk-windows-amd64.zip` is documented as CLI-only.
- Fixed setup on VPS hosts with broken IPv6 connectivity to Google OAuth by
  automatically preferring IPv4 for `gcloud` login when possible.
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
- Updated setup docs around one-line `skirk:` profiles, `serve-client`,
  `serve-exit`, custom OAuth device-flow setup, and Drive `appDataFolder`.
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
