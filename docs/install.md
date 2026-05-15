# Install Skirk

## Linux Installer

Use this on a Linux exit machine, Linux client, VPS, laptop, or home server:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
"$HOME/.local/bin/skirk" version
```

The installer puts `skirk` in `$HOME/.local/bin` by default. The `export PATH`
line makes `skirk` available in the current shell, but scripts and fresh SSH
sessions can always use the absolute path: `$HOME/.local/bin/skirk`.

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
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

That path does not require `gcloud` to create the Skirk token.

### Headless SSH And Broken IPv6

Run setup from an interactive terminal. For SSH, force a TTY when needed:

```bash
ssh -tt -p PORT user@host
```

If Google Cloud CLI prints a browser URL, accepts the pasted verification code,
and then appears to do nothing, check for broken IPv6 on the server:

```bash
curl -4 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
curl -6 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
```

If IPv4 returns quickly but IPv6 times out, make the host prefer IPv4 before
rerunning setup:

```bash
sudo sh -c 'grep -q "^precedence ::ffff:0:0/96 100" /etc/gai.conf || echo "precedence ::ffff:0:0/96 100" >> /etc/gai.conf'
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

This is a host networking fix, not a Skirk protocol setting. It prevents Python
tools such as `gcloud` from choosing a blackholed IPv6 route for Google OAuth.

## Exit Machine Flow

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit
"$HOME/.local/bin/skirk" serve-exit --config skirk-kit/exit.json
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
