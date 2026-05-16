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

After install, run `skirk` for the operator menu or run setup directly:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

On Linux, setup installs/enables `skirk-exit.service` and starts it after Google
approval. Pass `--start-exit=false` when you only want to generate configs.

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

## Google OAuth

Client machines do not need Google Cloud CLI. The exit/setup machine also does
not need Google Cloud CLI for the normal release flow. Google blocks the default
Google Cloud SDK OAuth client when Drive scopes are requested, so Skirk uses
Google's device-code OAuth flow with Skirk's own OAuth client instead:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

In an interactive terminal this opens the setup picker for easy Skirk OAuth or a
personal Google OAuth project. Non-interactive runs default to easy mode unless
`--oauth-mode personal` or `--oauth-client-file` is passed.

Source builds and forks can use an OAuth override when needed:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-mode personal
```

## OAuth And Drive Quota Modes

Skirk supports two OAuth modes:

Default easy mode:

- uses Skirk's built-in OAuth client;
- gives users the one-command device-code setup flow;
- charges Drive API usage to Skirk's Google Cloud project quota;
- still keeps each Google account under Google's per-user-per-project quota.

Personal quota mode:

- uses a Google OAuth client created in the user's own Google Cloud project;
- charges Drive API usage to that user's project quota instead of Skirk's shared
  project quota;
- guides the user through creating a Google Cloud project, enabling Drive API,
  configuring consent, and creating a `TVs and Limited Input devices` OAuth
  client:

```bash
"$HOME/.local/bin/skirk" setup init \
  --out skirk-kit \
  --reset-google-login \
  --oauth-mode personal
```

This is the same pattern used by mature Drive tools such as rclone: a shared
client is convenient for new users, while serious or high-volume users should
bring their own OAuth client to avoid shared-project contention.

Google Drive API project limits can be increased for some quota types from the
Google Cloud Quotas page, but approval is not guaranteed. Google also enforces
non-adjustable constraints such as the per-user Drive upload limit and the daily
billing threshold described in the Drive API limits documentation.

### Headless SSH And Broken IPv6

Run setup from an interactive terminal. For SSH, force a TTY when needed:

```bash
ssh -tt -p PORT user@host
```

If setup cannot contact Google's OAuth endpoints, check for broken IPv6 on the
server:

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

This is a host networking fix, not a Skirk protocol setting. It prevents OAuth
tools from choosing a blackholed IPv6 route for Google OAuth.

## Exit Machine Flow

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
"$HOME/.local/bin/skirk" service status
```

Send `skirk-kit/client.skirk` to clients. Do not send `exit.json`.

The same operations are available in the interactive operator menu:

```bash
"$HOME/.local/bin/skirk"
```

If you used `--start-exit=false`, install the persistent Linux exit service
later:

```bash
"$HOME/.local/bin/skirk" service install --config skirk-kit/exit.json
"$HOME/.local/bin/skirk" service status
```

Use `service stop`, `service restart`, or `service uninstall` with
`--name NAME` if you changed the service name.

To also install Cloudflare WARP through wireproxy and point exit traffic at it:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_SERVER_SETUP=1 \
  SKIRK_INSTALL_SYSTEMD=1 \
  SKIRK_INSTALL_WIREPROXY=1 \
  SKIRK_ACCEPT_WARP_TOS=1 \
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
