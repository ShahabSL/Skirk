# Skirk

<p align="center">
  <img src="assets/logo.png" alt="Skirk logo" width="160">
</p>

Skirk is a Go-first restricted-network transport that uses Google Drive as an encrypted mailbox. It is designed for the case where ordinary endpoints fail but Google APIs can still be reached, including through Google-fronted TLS routing.

## Current Status

- Production path: Go CLI in `cmd/skirk`.
- Transport: encrypted Drive appDataFolder mailbox with Drive-folder fallback.
- Tuning: adaptive Drive profile by default; direct routes start at full measured windows, restricted/proxied routes ramp up and back off on Google API pressure.
- Client UX: one generated `skirk:...` text config; no client-side Google login required.
- Exit UX: run `skirk serve-exit` anywhere with normal internet egress.
- Client mode: local SOCKS5 proxy on Linux, portable desktop client on Windows, and native Android proxy client with LAN sharing.

Skirk does not require a VPS for protocol reasons. It requires an exit machine with working internet egress. A VPS is the most reliable exit because it stays online, but a laptop or home server also works while it is awake and connected.

## Legal Notice

Skirk is for lawful, authorized, owned-account and owned-network use only. It is not affiliated with or endorsed by Google, Cloudflare, GitHub, Microsoft, Android, or any other provider. Read [DISCLAIMER.md](DISCLAIMER.md) before using or redistributing this project.

## Quick Start

Install on a Linux exit/client machine:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

Then open the operator menu:

```bash
skirk
```

Or build locally from a clone:

```bash
make build
./bin/skirk
```

Create a Google-backed kit:

```bash
skirk setup init --out skirk-kit
```

If Google login is needed, Skirk runs `gcloud auth login --no-launch-browser --enable-gdrive-access --update-adc --force` and prints the browser URL/code flow.
If `gcloud` is not installed, setup installs Google Cloud CLI under `~/google-cloud-sdk` first.

To switch to a different Google account, force a new login:

```bash
skirk setup init --out skirk-kit-new --google-login
```

To start from a clean local Google login state first:

```bash
skirk setup init --out skirk-kit-new --reset-google-login
```

Recommended setup: create a `TVs and Limited Input devices` OAuth client in your own Google Cloud project and run:

```bash
skirk setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

That path uses Google's device login flow and Drive `appDataFolder` with the narrow `drive.appdata` scope. The plain `skirk setup init` path remains as an easy fallback through Google Cloud CLI, but it can hit shared OAuth quota.

Run the exit on a VPS, laptop, or server with normal internet:

```bash
skirk serve-exit --config skirk-kit/exit.json
```

Run the client SOCKS5 proxy:

```bash
skirk serve-client --config skirk-kit/client.skirk --listen 127.0.0.1:18080
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

The default `profile=auto` keeps the user-facing config simple. It uses full windows on direct routes, starts safer on restricted/proxied routes, and automatically backs off when Google returns rate-limit pressure. Control polling is shared per tunnel direction so many app connections do not multiply into many independent Drive list loops, and the exit slows its new-connection polling while idle.

For sharing without file transfer, send the one-line text inside `skirk-kit/client.skirk`. The client can paste it into the menu or use it directly:

```bash
read -r SKIRK_CLIENT_CONFIG
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

For local reachability tests where the restricted network is exposed as another SOCKS proxy:

```bash
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080 \
  --route-mode google_front \
  --upstream-proxy socks5h://127.0.0.1:11093
```

Optional: run the desktop dashboard on Windows or a desktop Linux machine with a browser:

```bash
skirk client-ui --config skirk-kit/client.skirk --socks 127.0.0.1:18080 --ui 127.0.0.1:18280
```

Preferred Windows app:

```bash
cd clients/desktop
npm install
npm run tauri dev
```

Android app:

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

Install the APK from `clients/android/app/build/outputs/apk/debug/app-debug.apk`,
paste the one-line `skirk:` config, and tap Connect. Enable LAN sharing if other
devices should use the phone as a SOCKS5 proxy.

## Cleanup

Delete the visible Drive folder created by the fallback setup path:

```bash
skirk workspace delete --config skirk-kit/exit.json --delete-drive-folder
```

To invalidate all configs generated from the same OAuth login, revoke the Google app access from the account security page.

Or use Skirk's revoke command:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

## Security Model

The Google account sees encrypted chunks and control metadata. The exit sees target addresses and plaintext for non-TLS application traffic, like any proxy or VPN exit. HTTPS payloads remain protected by the target site's TLS.

Generated configs contain a Google refresh token and the Skirk tunnel secret. Treat `client.skirk`, `client.json`, and `exit.json` like passwords.

## Documentation

- [Legal Disclaimer](DISCLAIMER.md)
- [Install Guide](docs/install.md)
- [Setup Guide](docs/setup.md)
- [Client Guide](docs/clients.md)
- [Release Guide](docs/release.md)
- [Go CLI Notes](docs/go_skirk.md)
- [Drive Architecture](docs/skirk_drive_sheets_architecture.md)
- [Modes](docs/skirk_modes.md)
- [Latest Throughput Notes](docs/optimized_throughput_2026_05_02.md)
