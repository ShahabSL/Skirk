# Changelog

## Unreleased

## v0.1.50 - 2026-05-18

- Restart the interactive Linux menu from the newly installed binary after a
  menu-driven Skirk update, so freshly added menu actions are visible
  immediately instead of requiring users to quit and rerun `skirk`.

## v0.1.49 - 2026-05-18

- Added an outbound-proxy menu action to uninstall Skirk-managed WARP
  wireproxy, remove its systemd dependency from the exit service, and switch
  the exit back to direct mode.
- Hardened WARP wireproxy removal so leftover config and helper binaries are
  cleaned even if the wireproxy systemd unit was already missing.
- Hardened WARP setup and updates with loopback-only managed SOCKS binds,
  preflight checks before root writes, an unprivileged `skirk-wireproxy`
  service user, Skirk-owned artifact manifests, and stricter menu-update
  environment sanitization.

## v0.1.48 - 2026-05-18

- Added pinned installer version arguments.
- Kept Android release downloads on the stable `skirk-android-arm64.apk` asset
  name because the GitHub release tag already carries the version.
- Added menu actions for updating Skirk and configuring exit outbound proxy,
  including optional WARP wireproxy setup.
- Simplified Android status hierarchy so the app shows one primary connection
  state and keeps the version visible without repeating status labels.
- Tightened Windows desktop status wording and version visibility.

## v0.1.47 - 2026-05-17

- Added Drive-wide quota backoff so 403/429 rate-limit responses stop poll,
  upload, download, and cleanup retry storms instead of multiplying them.
- Reworked mux v4 priority so small streams stay ordered in the priority lane
  while bulk streams demote cleanly to normal traffic, preventing priority and
  normal frames from overtaking each other.
- Raised auto-profile polling defaults to reduce Drive list pressure across
  Android, Windows, and generated setup profiles.
- Added targeted mux gap repair and regression tests for missing Drive objects,
  ordered small-stream priority, bulk fairness, and Drive rate-limit handling.
- Suppressed expected Android non-DNS UDP refusal noise while keeping the VPN
  IPv4/TCP fallback policy for QUIC-heavy apps.
- Clarified setup docs that easy OAuth shares Skirk project quota and personal
  OAuth only isolates quota when users create their own Google Cloud project.

## v0.1.46 - 2026-05-17

- Reduced Drive mailbox quota pressure in VPN mode by disabling burst polling
  for Android and Windows VPN sidecars, lowering VPN worker concurrency, and
  pacing normal active polling while keeping proxy mode aggressive.
- Hardened mux v4 under long-running and multi-client traffic with bounded
  bootstrap priority data, idempotent reserved-ID normal uploads when available,
  stale Drive object handling, and receive-gap timeout/repair behavior.
- Made Drive cleanup less disruptive by tightening janitor defaults and
  avoiding foreground stalls from cleanup pressure.
- Fixed Android VPN connect/disconnect lifecycle races so repeated connect
  taps are idempotent and disconnect closes the Android TUN descriptor before
  stopping tun2socks and the Skirk sidecar.
- Added regression coverage for stale Drive objects, reserved upload IDs, and
  Drive ID-generation fallback.

## v0.1.45 - 2026-05-17

- Switched personal Google OAuth setup to the Desktop app authorization-code
  flow with PKCE and VPS paste-back support for redirected localhost URLs.
- Kept easy Skirk OAuth on Google's device-code flow and restored the built-in
  release requirement for both OAuth client ID and client secret.
- Updated setup docs and wizard text to stop recommending TV/Limited Input
  clients for personal OAuth unless a client secret is available and
  `--oauth-flow device` is explicitly selected.

## v0.1.44 - 2026-05-17

- Allowed personal Google OAuth clients to use a client ID without a client
  secret, matching Google's public-client behavior.
- Updated setup docs, wizard prompts, and release build checks so client
  secrets are used when available but are not required.

## v0.1.43 - 2026-05-17

- Removed the release publish job's `actions/download-artifact` dependency and
  switched to GitHub CLI artifact download to avoid a Node deprecation warning
  emitted by that action.
- Suppressed Git checkout initialization hints by configuring the default branch
  before checkout in CI and release jobs.

## v0.1.42 - 2026-05-17

- Pinned GitHub Actions CI and release runners to explicit stable images
  (`ubuntu-24.04` and `windows-2022`) to avoid floating-runner migration
  notices in release builds.

## v0.1.41 - 2026-05-17

- Refreshed the release workflow onto GitHub's Node 24 artifact actions so
  the latest release is produced without Node 20 deprecation warnings.
- Kept the Android release path on `assembleRelease` with required keystore
  secrets, signature verification, checksums, and artifact attestations.

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
