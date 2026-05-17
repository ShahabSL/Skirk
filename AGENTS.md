# Skirk Agent Handbook

This file is the operating context for coding agents working in this repository.
Read it before changing code, tests, docs, packaging, or release automation.

## Product Summary

Skirk is a Google Drive backed TCP transport for hostile networks where the
available path is constrained to Google-fronted traffic. The client exposes one
or more local frontends:

- SOCKS5 proxy
- HTTP proxy
- Windows desktop proxy/system-proxy/VPN modes
- Android `VpnService` mode

The exit runs on a server with normal Internet access. Both ends share a Google
Drive mailbox, exchange encrypted mux objects, and the exit opens outbound TCP
connections on behalf of the client.

The main network constraint is intentional: preserve the Google-fronted,
pinned-Google-IP route behavior used by `google_front_pinned`. Do not replace it
with a design that assumes direct raw access from the hostile network.

## Non-Negotiable Security Rules

- Never commit files under `private/`, `.skirk-runs/`, `skirk-kit/`,
  `skirk-config/`, `bin/`, or `dist/`.
- Never commit OAuth refresh tokens, access tokens, Google service account
  material, OAuth client secrets, `.skirk` files, `skirk:` config strings,
  generated `client.json`, generated `exit.json`, keystores, or certificates.
- Treat a one-line `skirk:` profile as a password. It carries enough material
  for a client to use the mailbox.
- Treat generated exit configs as secrets. They contain Google OAuth material.
- Release builds inject the public Skirk OAuth client and Android signing
  material through GitHub repository secrets; do not hard-code those values in
  tracked source.
- Run `scripts/preflight.sh` before release. It intentionally checks for common
  tracked runtime artifacts, personal email residue, and generated credentials.

## Current Production Transport

Mux v4 is the production transport. The implementation is in
`internal/skirk/mux.go`.

Key properties:

- Four Drive lanes carry many logical streams.
- Frames are encrypted and coalesced into Drive objects.
- Priority traffic carries opens, closes, small writes, first bytes, and sparse
  interactive traffic.
- Normal traffic carries bulk data with bounded per-stream and global queues.
- Client response objects are namespaced by client ID and run ID so multiple
  devices can use a copied profile without consuming each other's responses.
- Upload/download worker windows adapt to Drive health.
- Processed objects are cleaned up by deferred cleanup so foreground traffic is
  not blocked by delete calls.

Do not resurrect old transports or experimental protocols as defaults unless a
new design beats mux v4 on mixed browser plus bulk traffic. Synthetic
single-stream download speed is not enough.

## Performance Model

Drive is an object API, not a stream API. The hot path always includes upload,
Drive visibility delay, prefix discovery, download, and cleanup. The practical
goal is to minimize avoidable objects and keep interactive traffic moving while
bulk traffic is active.

Important lessons from previous work:

- `files.list` prefix polling is the proven low-latency discovery path for this
  design.
- `changes.list` is not prefix-filtered. It can be useful for research, but a
  production design must handle mailbox-wide pollution and extra bookkeeping.
- Known-ID and range-read primitives are fast after an ID is known, but previous
  live transports lost to mux v4 when they added extra control objects or
  metadata waits.
- Bulk-only throughput can be misleading. Promotion requires small request
  latency under active downloads, browser startup behavior, and multi-client
  stability.

## Repository Map

Top-level files:

- `README.md`, `README.fa.md`: user-facing overview.
- `CHANGELOG.md`: release notes.
- `LICENSE`, `DISCLAIMER.md`, `SECURITY.md`, `CONTRIBUTING.md`: project policy.
- `install.sh`: Linux installer used by the public quick-start command.
- `Makefile`: local build, test, preflight, and packaging entry points.
- `.github/workflows/ci.yml`: CI validation.
- `.github/workflows/release.yml`: tag-triggered release build and publish.

Command package:

- `cmd/skirk/main.go`: command dispatch and CLI flags.
- `cmd/skirk/setup.go`: Google kit creation, OAuth login, mailbox setup, and
  optional exit service startup.
- `cmd/skirk/oauth_wizard.go`: personal OAuth setup guidance.
- `cmd/skirk/menu.go`: interactive operator menu.
- `cmd/skirk/service.go`: Linux systemd service lifecycle.
- `cmd/skirk/uninstall.go`: local uninstall, optional kit deletion, OAuth
  revocation, and Drive deletion flow.
- `cmd/skirk/client_ui.go`: optional desktop-style browser dashboard.
- `cmd/skirk/parent_watch_*.go`: platform-specific parent process watching.
- `cmd/skirk/signals_*.go`: platform-specific shutdown signal sets.

Core package:

- `internal/skirk/config.go`: config structs, defaults, inline `skirk:` config
  encoding/decoding, OAuth token source.
- `internal/skirk/drive.go`: Google Drive API operations, listing, upload,
  download, delete, cleanup, quota accounting, and limiter behavior.
- `internal/skirk/httpclient.go`: route-aware HTTP transport and Google-fronted
  dialing behavior.
- `internal/skirk/tunnel.go`: client and exit tunnel orchestration.
- `internal/skirk/mux.go`: mux v4 framing, queues, lanes, fairness, receive
  ordering, cleanup scheduling, and observability logs.
- `internal/skirk/socksserver.go`: local SOCKS server, UDP DNS handling, and
  VPN-facing UDP policy.
- `internal/skirk/socksdial.go`: outbound SOCKS/HTTP proxy dialing helpers.
- `internal/skirk/httpproxy.go`: local HTTP proxy frontend.
- `internal/skirk/store.go`, `stores.go`, `memory.go`: store abstractions and
  memory-backed tests.
- `internal/skirk/protocol.go`: encryption and object protocol helpers.

Clients:

- `clients/android/`: Android app, Go sidecar build, and HEV TUN-to-SOCKS
  bridge.
- `clients/android/app/src/main/java/app/skirk/client/AndroidSkirkEngine.kt`:
  starts the Go sidecar and passes production mux flags.
- `clients/android/app/src/main/java/app/skirk/client/SkirkVpnService.kt`:
  Android VPN frontend and HEV config generation.
- `clients/desktop/`: Tauri Windows desktop UI.
- `clients/desktop/src-tauri/src/lib.rs`: desktop commands, sidecar lifecycle,
  system proxy integration, and Windows VPN sidecar orchestration.
- `clients/desktop/scripts/package_windows_portable.py`: Windows portable zip
  staging.

Docs and tooling:

- `docs/architecture.md`: current architecture and performance model.
- `docs/transport-research.md`: transport experiments and promotion gates.
- `docs/setup.md`, `docs/install.md`, `docs/go_skirk.md`: setup and CLI docs.
- `docs/release.md`: release checklist.
- `scripts/preflight.sh`: local release gate.
- `scripts/package_release.sh`: Linux and Windows CLI archive builder.
- `tools/`: local probes and benchmarks. These are not the production runtime.
- `third_party/`: notices and vendored native tunnel source.

## Android Rules

Android VPN mode is deliberately IPv4-only today:

- `SkirkVpnService.kt` adds only an IPv4 TUN address and `0.0.0.0/0`.
- The local SOCKS DNS handler returns NOERROR/NODATA for AAAA queries.
- Non-DNS UDP is refused so apps fall back from QUIC/UDP to TCP through Skirk.
- The Skirk app package must be excluded from its own VPN. Failure to exclude it
  is fatal, because routing Skirk through itself can deadlock.
- `builder.setMetered(true)` is intentional. A/B evidence showed `false`
  encouraged more aggressive app behavior and produced visible Reels stalls.

If you change Android VPN behavior, test on a real device with:

- APK install and app launch.
- VPN connect.
- `adb shell ip addr show tun0`.
- Browser traffic through VPN.
- Instagram Reels scrolling.
- A simultaneous real bulk download through VPN.
- Sidecar logs checked for `transport_error`, `urgent_queue_full`,
  `remote_rst_before_open`, `slot_wait`, and repeated stalls.

Keep large screen recordings and debug logs under `.skirk-runs/`, then remove
large files from the phone after testing.

## Windows Desktop Rules

The normal Windows artifact is `Skirk_windows_x64_portable.zip`. The
`skirk-windows-amd64.zip` artifact is CLI-only.

Windows VPN mode uses the packaged `sing-box` sidecar. The release workflow
downloads `sing-box` and verifies its SHA-256 before packaging. Keep the
sing-box config aligned with the current sing-box schema; older legacy inbound
fields broke on sing-box 1.13.

Avoid self-elevation tricks from the GUI. If VPN mode needs administrator
rights, the UI should clearly tell the user to run the portable app as
administrator rather than spawning suspicious elevation commands.

Windows release archives are covered by SHA-256 checksums and GitHub artifact
attestations. Do not claim Authenticode signing unless a real code-signing
certificate is configured and the workflow verifies the signed executable.

## OAuth and Setup Rules

The public easy setup flow uses Google's device-code page and Skirk's built-in
OAuth client from release-time secrets.

The current public scope is `drive.file`. When `appDataFolder` is rejected,
setup falls back to a Skirk-created Drive mailbox folder. This is expected and
does not imply a broken setup.

Personal OAuth mode exists so advanced users can use their own Google Cloud
quota. While a personal OAuth app is in Testing, the exact Google account used
at `google.com/device` must be added under OAuth Audience/Test users or Google
will block access.

Generated kit outputs:

- `exit.json`: exit-side secret config.
- `client.json`: client-side JSON config.
- `client.skirk`: one-line client profile.
- `client-command.txt`: ready `serve-client` command.

Do not print or commit real generated config values in docs, tests, or release
notes.

## Drive Cleanup Rules

Drive storage can fill quickly under VPN/bulk tests if cleanup regresses.
Cleanup is part of correctness, not a cosmetic task.

Relevant paths:

- `internal/skirk/drive.go`: cleanup implementation and Drive quota logs.
- `cmd/skirk/main.go`: `cleanup` and `repair-mailbox` commands.
- `cmd/skirk/setup.go`: generated config defaults and service startup.

Use cleanup commands from `docs/release.md` for manual validation. For severe
test pollution, use the generated exit config and `cleanup --all --delete` only
after confirming the configured Drive folder is Skirk-owned.

## Observability

Use `--observe` for deep local debugging. Important log families:

- `drive quota`: per-minute Drive call counts, estimated units, response bytes,
  and operation mix.
- `drive limiter`: adaptive upload/download window changes.
- `mux upload`, `mux poll`, `mux process`: object flow and latency.
- `mux terminal close`, `mux rst drop`, `transport_error`: failure signals.
- `slot_wait` and queue delay metrics: congestion and fairness clues.

Store experiment evidence in `.skirk-runs/`. Do not commit that directory.

## Test Gates

Before a normal release:

```bash
make preflight
```

Before a client/platform release:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```

Run targeted commands when touching the corresponding area:

- Go core or CLI: `go test ./...` and `go vet ./...`.
- Android debug smoke: `cd clients/android && ./gradlew :app:assembleDebug --console=plain`.
- Android release asset: build `:app:assembleRelease` with
  `SKIRK_ANDROID_KEYSTORE_FILE`, `SKIRK_ANDROID_KEYSTORE_PASSWORD`,
  `SKIRK_ANDROID_KEY_ALIAS`, and `SKIRK_ANDROID_KEY_PASSWORD` set, then verify
  the APK with `apksigner verify --print-certs`.
- Desktop UI: `cd clients/desktop && npm ci && npm run build`.
- Desktop Tauri changes: also run `npm run tauri build -- --no-bundle` on a
  platform that can build the target.
- Release packaging: `VERSION=vX.Y.Z make package-release`.

`scripts/preflight.sh` already runs `git diff --check`, `go test ./...`, and
`go vet ./...`. With `SKIRK_FULL_PREFLIGHT=1`, it also runs desktop and Android
builds when the required SDK environment is present.

## Release Process

1. Update `CHANGELOG.md`.
2. Bump Android version in `clients/android/app/build.gradle.kts`.
3. Bump desktop versions in:
   - `clients/desktop/package.json`
   - `clients/desktop/package-lock.json`
   - `clients/desktop/src-tauri/Cargo.toml`
   - `clients/desktop/src-tauri/Cargo.lock`
   - `clients/desktop/src-tauri/tauri.conf.json`
4. Run the test gates.
5. Commit with a Conventional Commit message.
6. Push `main`.
7. Tag `vX.Y.Z` and push the tag.
8. Watch the `Release` GitHub Actions workflow.
9. Verify release assets with `gh release view vX.Y.Z`.
10. Verify artifact attestations for at least one downloaded asset with
    `gh attestation verify <asset> -R ShahabSL/Skirk`.

The release workflow publishes:

- `skirk-linux-amd64.tar.gz`
- `skirk-linux-arm64.tar.gz`
- `skirk-windows-amd64.zip` (CLI-only)
- `Skirk_windows_x64_portable.zip` (Windows GUI)
- `skirk-android-arm64.apk`
- checksums

The Android APK is built with `assembleRelease`, signed with the configured
release keystore, and verified with `apksigner`. The archives/APK are also
covered by GitHub artifact attestations.

Current Android release signing certificate SHA-256:
`45c73cd055ad189ff421e4bd84facbc2512ab26e505aed4b0d867ee6e9c347cf`.

## Coding Standards

- Prefer existing local patterns over new abstractions.
- Keep changes narrowly scoped to the request and relevant subsystem.
- Use `rg` for searching.
- Use `apply_patch` for manual edits.
- Do not revert unrelated user changes.
- Keep comments short and focused on why behavior exists.
- Keep user-facing docs in sync with workflow behavior.
- Use Conventional Commits.

## Docs-First Rule

For external SDKs, CLIs, framework APIs, release tool flags, Android APIs,
Tauri APIs, sing-box config schema, and GitHub Actions behavior, consult
Context7 when available or official/local tool docs when Context7 is
unavailable. Record assumptions in chat or docs when they affect security,
architecture, or release behavior.

## Promotion Rule

A change is not "better" just because it improves one benchmark. Promote a
transport or platform behavior only after it survives:

- normal browsing;
- video startup;
- Instagram/Reels-style scrolling;
- active bulk download plus interactive traffic;
- multiple clients where applicable;
- cleanup pressure;
- quota pressure;
- restart/reconnect behavior.

Claims in final responses and release notes must be backed by tests, logs,
workflow results, or explicit code inspection.
