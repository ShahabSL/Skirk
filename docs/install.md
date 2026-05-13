# Install Skirk

## Linux Installer

Use this on a Linux exit machine, Linux client, VPS, laptop, or home server:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
skirk version
```

The installer puts `skirk` in `$HOME/.local/bin` by default.

## Installer Options

Install a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_VERSION=vX.Y.Z sh
```

Install to another directory:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_INSTALL_DIR=/usr/local/bin sh
```

Install from a fork:

```bash
curl -fsSL https://raw.githubusercontent.com/OWNER/Skirk/main/install.sh | SKIRK_REPO=OWNER/Skirk sh
```

Review before running:

```bash
curl -fsSLO https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh
less install.sh
sh install.sh
```

## What The Installer Does

1. Detects Linux `amd64` or `arm64`.
2. Downloads the matching GitHub release archive when available.
3. Builds from source when no release archive exists.
4. Installs one binary: `skirk`.
5. Prints the installed version and next setup command.

Release archive installs do not require Go. Source builds require Go.

## Google Cloud CLI

Client machines do not need Google Cloud CLI.

The exit/setup machine may need it only for the easy `skirk setup init` login
path. When missing on Linux, setup installs Google Cloud CLI under
`~/google-cloud-sdk` and uses it to create Application Default Credentials with
Drive access. Automatic Google Cloud CLI install is Linux-only.

The recommended quota-owned setup path uses your OAuth client file and Google's
device authorization flow directly:

```bash
skirk setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

That path does not require `gcloud` to create the Skirk token.

## Exit Machine Flow

```bash
skirk setup init --out skirk-kit
skirk serve-exit --config skirk-kit/exit.json
```

Send `skirk-kit/client.skirk` to clients. Do not send `exit.json`.

To also install Cloudflare WARP through wireproxy and point exit traffic at it:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_SERVER_SETUP=1 \
  SKIRK_INSTALL_SYSTEMD=1 \
  SKIRK_INSTALL_WIREPROXY=1 \
  SKIRK_ACCEPT_WARP_TOS=1 \
  SKIRK_ADC=/path/to/application_default_credentials.json \
  sh
```

Defaults: wireproxy listens on `127.0.0.1:40000`, Skirk writes
`tunnel.exit_proxy=socks5h://127.0.0.1:40000`, and systemd starts
`wireproxy.service` before `skirk-exit.service`. Override with
`SKIRK_WIREPROXY_BIND` or `SKIRK_EXIT_PROXY` when needed.

## Local Build

```bash
make build
./bin/skirk version
```

Run all normal checks:

```bash
make preflight
```

Include desktop and Android checks:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```
