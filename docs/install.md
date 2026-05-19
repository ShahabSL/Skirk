# Install Skirk

## Linux Installer

Use this on a Linux exit machine, Linux client, VPS, laptop, or home server:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
"$HOME/.local/bin/skirk" version
```

The installer puts `skirk` in `$HOME/.local/bin` by default. The `export PATH`
line makes that install available in the current shell, but scripts and fresh
SSH sessions can always use an absolute path such as `$HOME/.local/bin/skirk`.

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

Equivalent pinned install using an argument:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh -s -- --version vX.Y.Z
```

Install to another directory:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_INSTALL_DIR=/usr/local/bin sh
```

Development-only installs from a fork or local asset mirror require the explicit
`--dev-install` flag. Normal release installs intentionally ignore inherited
`SKIRK_REPO` and `SKIRK_ASSET_BASE` so old update commands cannot be redirected
to an unintended binary.

```bash
curl -fsSL https://raw.githubusercontent.com/OWNER/Skirk/main/install.sh | \
  SKIRK_REPO=OWNER/Skirk sh -s -- --dev-install
```

Review before running:

```bash
curl -fsSLO https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh
less install.sh
sh install.sh
```

## What The Installer Does

1. Detects Linux `amd64` or `arm64`.
2. Downloads the matching GitHub release archive.
3. Verifies an explicitly pinned `vX.Y.Z` archive reports the requested version.
4. Installs one binary: `skirk`.
5. Prints the installed version and next setup command.

Release archive installs do not require Go. Source builds are a development-only
path behind `--dev-install`.

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
- charges Drive API usage to Skirk's shared Google Cloud project quota;
- still keeps each Google account under Google's per-user-per-project quota.

Personal quota mode:

- uses a Google OAuth client created in the user's own Google Cloud project;
- charges Drive API usage to that user's project quota instead of Skirk's shared
  project quota;
- guides the user through creating a Google Cloud project, enabling Drive API,
  configuring consent, and creating a `Desktop app` OAuth
  client:

```bash
"$HOME/.local/bin/skirk" setup init \
  --out skirk-kit \
  --reset-google-login \
  --oauth-mode personal
```

Easy mode is best for trials and low-volume personal use. Personal quota mode is
recommended for sustained video, multiple clients, or public/shared deployments,
because it avoids shared-project contention when many easy-mode users are active
at the same time.

Personal mode uses Google's desktop/native-app authorization flow. On a VPS,
Skirk prints a Google approval URL; after approval the browser may land on a
localhost URL that cannot load. Copy that full URL back into the terminal so
Skirk can finish the token exchange. The Google `TVs and Limited Input devices`
flow is reserved for easy built-in setup because Google's token polling requires
a `client_secret`.

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

## Uninstall

From the installed binary:

```bash
"$HOME/.local/bin/skirk" uninstall --dry-run
"$HOME/.local/bin/skirk" uninstall --yes
```

From the installer script:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh -s -- uninstall
```

Default uninstall behavior is intentionally conservative: it removes the
`skirk-exit.service` systemd unit when systemd is available and removes the
installed `skirk` binary. It does not delete generated kits, revoke Google
OAuth, delete Drive mailbox data, or remove WARP wireproxy unless you explicitly
ask for those actions.

Common complete cleanup:

```bash
"$HOME/.local/bin/skirk" uninstall --yes \
  --delete-drive \
  --revoke-oauth \
  --delete-kit \
  --kit skirk-kit
```

If you installed Skirk to a custom directory or used a custom service name:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_INSTALL_DIR=/usr/local/bin \
  SKIRK_SERVICE_NAME=my-skirk-exit \
  sh -s -- uninstall
```

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
`wireproxy.service` before `skirk-exit.service`. `SKIRK_WIREPROXY_BIND` must
stay loopback-only; use `SKIRK_EXIT_PROXY` when pointing Skirk at a different
already-secured outbound proxy. The interactive
`skirk` menu can also configure a custom outbound proxy, install WARP wireproxy,
unset the proxy, or update the installed binary while keeping the existing kit
and proxy settings.

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
