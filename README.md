# Skirk

[فارسی](README.fa.md)

<p align="center">
  <img src="assets/logo.png" alt="Skirk logo" width="160">
</p>

## Donate

If Skirk is useful to you, donations help fund testing infrastructure and
maintenance:

- USDT: `0x5d0b46d821910a5a5503de78e230f9a5e9c52c2f`
- BTC: `bc1q8qsxlp7pzgdqkhu2aj5ss3krnkrecyrh6hedpj`
- ETH: `0x5d0b46d821910a5a5503de78e230f9a5e9c52c2f`
- TON: `UQAO9dEwEVIrrTwoWzCd3Rksb6r3qFSs80Xa7yp3nkO4CPyp`

Skirk is a Go-first transport for restricted-network testing. It exposes a local
SOCKS5 proxy, optional HTTP proxy, or Android VPN frontend, then moves encrypted
TCP stream frames through a Google Drive mailbox folder to an exit
machine with normal internet egress.

Skirk is for lawful, authorized, owned-account and owned-network use only. It is
not affiliated with or endorsed by Google, Google Cloud, Google Drive,
Cloudflare, GitHub, Microsoft, Android, or any other provider. Read
[DISCLAIMER.md](DISCLAIMER.md) before using or redistributing it.

## What You Need

- One exit machine with working internet egress. A VPS is best for uptime, but a
  laptop or home server works while it stays online.
- One Google account for the Drive mailbox.
- One generated `skirk:...` client profile to share with client devices.

Clients do not need Google login, `gcloud`, or a Google Cloud project. The exit
setup creates the Google-backed kit once and prints a one-line client profile.
The same profile can be imported on multiple devices. Each client app creates a
local profile identity, and each connection run gets a fresh run identity, so
Drive replies are routed back to the correct device.

## Quick Start

Install Skirk on the exit machine:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
"$HOME/.local/bin/skirk" version
```

Create a kit and start the exit service:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

Interactive setup first lets you choose easy Skirk OAuth or a personal Google
OAuth project. Easy mode prints a Google device URL plus a short code; open the
URL, enter the code in the browser, approve Drive access, and the terminal
continues.
On Linux, setup also installs/enables `skirk-exit.service` and starts the exit
immediately. Use `--start-exit=false` if you only want the config files.

By default this uses Skirk's built-in OAuth client for the easiest install.
Heavy users can instead use their own Google Cloud OAuth client so Drive API
traffic is charged to their own project quota:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-mode personal
```

For personal OAuth, create a Google OAuth client with application type
`Desktop app`, paste its client ID and client secret, approve the printed Google
URL, then paste the redirected localhost URL back into the VPS terminal if the
browser cannot reach it locally.

This is the same operational model used by tools such as rclone: shared OAuth
is convenient, personal OAuth isolates quota.

Check the exit service:

```bash
"$HOME/.local/bin/skirk" service status
```

Or run the operator menu and choose setup, service, cleanup, or revoke actions
from one place:

```bash
"$HOME/.local/bin/skirk"
```

Uninstall from Linux:

```bash
"$HOME/.local/bin/skirk" uninstall --dry-run
"$HOME/.local/bin/skirk" uninstall --yes
# or:
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh -s -- uninstall
```

If you generated a kit with `--start-exit=false`, start the exit manually or
install the service later:

```bash
"$HOME/.local/bin/skirk" service install --config skirk-kit/exit.json
"$HOME/.local/bin/skirk" service status
```

Copy the one-line text from `skirk-kit/client.skirk` and use it on a client.
From a Linux client:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"

read -r SKIRK_CLIENT_CONFIG
# paste the skirk:... profile, press Enter, then run:
"$HOME/.local/bin/skirk" serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

Test the local SOCKS proxy:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

Use `socks5h` behavior in apps that support it so DNS resolution happens through
the Skirk exit path.

## Client Options

Linux and headless servers use the Go CLI:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

For a long-lived Linux install, set a stable local client ID once. This is not a
secret; it only separates this device from other devices using the same copied
profile:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

Windows users should use the portable desktop app from the release assets. It
imports the same one-line `skirk:` profile and starts the Skirk SOCKS sidecar.
The Windows build is proxy-first; configure the browser or app to use SOCKS5
`127.0.0.1:18080`.

Android users should use the Android app. Import the same one-line profile,
select `VPN`, and tap `Connect`. Android asks for VPN consent on first use.
`Proxy` mode is available when an app or another LAN device explicitly supports
SOCKS5.

See [docs/clients.md](docs/clients.md) for build and release details.

## Restricted-Network Testing

Generated client profiles default to `google_front`, which uses a
Google-looking TLS route for Google API traffic. The exit defaults to `direct`
because it normally has ordinary internet access.

If the restricted network is exposed locally as another SOCKS proxy:

```bash
skirk serve-client \
  --config "$SKIRK_CLIENT_CONFIG" \
  --listen 127.0.0.1:18080 \
  --route-mode google_front \
  --upstream-proxy socks5h://127.0.0.1:11093
```

For normal-network throughput checks, omit `--upstream-proxy`. You can also
force direct Google API routing:

```bash
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080 --route-mode direct
```

## Benchmark And Logs

With the exit running, measure live latency, throughput, and estimated Drive API
use:

```bash
skirk bench-live --config skirk-kit/client.skirk --samples 5
```

Measure a hostile path:

```bash
skirk bench-live \
  --config skirk-kit/client.skirk \
  --upstream-proxy socks5h://127.0.0.1:11093 \
  --route-mode google_front \
  --samples 3
```

Add a bulk URL when you want throughput:

```bash
skirk bench-live --config skirk-kit/client.skirk --bulk-url http://example.com/big.bin
```

Runtime logs include per-minute Drive operation counts, estimated quota units,
errors, response bytes, and operation timing. Google Cloud Console metrics are
the project-level source of truth when using your own OAuth client/project.

## Runtime Shape

Skirk's production runtime uses a prefix-scoped Drive mailbox with fresh object
listing. That path is intentionally simple: upload encrypted mux objects, poll the
matching direction prefix, download by Drive file ID, and delete processed
objects after foreground traffic is quiet.

Mux v4 is the current default for Skirk's Drive-only transport. In local
same-day mixed-workload tests, it has been more reliable than the experimental
candidates tried so far, especially when browser or media traffic overlaps bulk
downloads. It is still bounded by Drive upload, object visibility, prefix
listing, download, cleanup, quota, and route conditions, so benchmark results are
environment-specific rather than guaranteed speed claims.

The Linux installer can perform VPS setup non-interactively:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_SERVER_SETUP=1 \
  sh
```

Clients still use the Google-fronted Drive path and do not need inbound
connectivity.

## Cleanup And Disconnect

Normal runtime deletes processed mailbox objects. `serve-exit` also starts an
automatic janitor every minute that deletes stale Skirk transport objects older
than 2 minutes.

Manual cleanup is dry-run by default:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
```

Actually delete matching stale objects:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h --delete
```

Empty every object in the configured Skirk mailbox:

```bash
skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000
```

If the Drive mailbox folder was deleted or the exit reports `drive_not_found`,
repair the kit and restart the exit service:

```bash
skirk repair-mailbox --kit skirk-kit --start-exit
```

Revoke the OAuth token embedded in a generated config:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

Then delete local generated files:

```bash
rm -rf skirk-kit
```

If a client profile leaks, revoke OAuth access and generate a new kit. Treat
`client.skirk`, `client.json`, and `exit.json` like passwords.

## Advanced

Forward exit traffic through another proxy, such as a local WARP/wireproxy
SOCKS listener:

```bash
skirk serve-exit --config skirk-kit/exit.json --exit-proxy socks5h://127.0.0.1:40000
```

For a clean VPS install that should create the WARP wireproxy service, write the
exit proxy into the generated config and start the exit service:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_SERVER_SETUP=1 \
  SKIRK_INSTALL_SYSTEMD=1 \
  SKIRK_INSTALL_WIREPROXY=1 \
  SKIRK_ACCEPT_WARP_TOS=1 \
  sh
```

Expose an HTTP/HTTPS proxy on the client in addition to SOCKS5:

```bash
skirk serve-client \
  --config skirk-kit/client.skirk \
  --listen 127.0.0.1:18080 \
  --http-proxy-listen 127.0.0.1:18081
```

Skirk uses prefix-scoped fresh listing for runtime object discovery. The main
latency knob exposed to clients is `--poll-ms`; lower values trade more Drive API
calls for faster wakeups.

## Documentation

- [Install Guide](docs/install.md)
- [Setup Guide](docs/setup.md)
- [Client Guide](docs/clients.md)
- [Architecture](docs/architecture.md)
- [Transport Modes](docs/skirk_modes.md)
- [Transport Research](docs/transport-research.md)
- [Go CLI Notes](docs/go_skirk.md)
- [Development Guide](docs/development.md)
- [Release Guide](docs/release.md)
- [Security Policy](SECURITY.md)
- [Legal Disclaimer](DISCLAIMER.md)
