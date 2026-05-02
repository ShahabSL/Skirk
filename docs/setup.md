# Skirk Setup Guide

This is the intended user flow:

1. The operator runs Skirk on a machine with Google login available.
2. Skirk creates a dedicated Google Sheet and Drive folder.
3. Skirk writes `exit.json`, `client.json`, and one-line `client.skirk`.
4. The operator runs the exit on a VPS, laptop, or home server.
5. Clients paste/import `client.skirk` and start a local SOCKS5 proxy.

## Does Skirk Need A VPS?

No. Skirk does not need an inbound server port because both sides exchange encrypted messages through Google Drive and Google Sheets.

It does need an exit machine. The exit is the machine that dials the real internet targets. A VPS is best for uptime and stable egress, but a laptop works while it is awake and online.

## First-Time Setup

Install Skirk on Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

Or build the binary from a clone:

```bash
make build
```

Create the Google-backed kit:

```bash
./bin/skirk setup init --out skirk-kit
```

You can also run the interactive operator menu:

```bash
./bin/skirk
```

If Application Default Credentials are missing, setup runs:

```bash
gcloud auth application-default login --no-launch-browser --scopes=https://www.googleapis.com/auth/cloud-platform,https://www.googleapis.com/auth/drive,https://www.googleapis.com/auth/spreadsheets,https://www.googleapis.com/auth/userinfo.email,openid
```

That command prints a browser URL and code. Open the URL, approve the Google account, paste the code back into the terminal, then setup continues.

If `gcloud` is not installed, setup installs Google Cloud CLI under `~/google-cloud-sdk` before starting the login flow.

## Switch Google Accounts

By default, setup reuses the existing local Application Default Credentials. To create a kit with a different Google account, force a new login:

```bash
skirk setup init --out skirk-kit-new --google-login
```

If the old account is blocked, banned, expired, or just wrong, reset local Google credentials first:

```bash
skirk setup init --out skirk-kit-new --reset-google-login
```

That reset runs the documented local credential cleanup commands before opening the login flow:

```bash
gcloud auth application-default revoke --quiet
gcloud auth revoke --all --quiet
```

This only changes credentials on the machine running setup. Existing generated Skirk configs keep using the refresh token embedded in those config files until you delete the workspace or revoke the Google app access for that account.

## Generated Files

`skirk-kit/exit.json`:
Use this on the exit machine.

`skirk-kit/client.json`:
JSON form of the client config.

`skirk-kit/client.skirk`:
One-line text form of the same client config. This is the easiest thing to send or paste. Clients do not need Google login, OAuth, or `gcloud`.

`skirk-kit/client-command.txt`:
A ready-to-copy client command containing the one-line config.

`skirk-kit/README.md`:
Per-kit run and cleanup commands.

All generated config files contain a Google refresh token and the Skirk tunnel secret. Do not commit them.

## Run The Exit

On the VPS, laptop, or server:

```bash
./bin/skirk serve-exit --config skirk-kit/exit.json
```

## Run A Linux Client

On the client:

```bash
./bin/skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

This is the default Linux path. No GUI is required.

Point apps at SOCKS5:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

Use `socks5h` semantics in apps that support it so DNS resolution happens through the exit path.

Without copying any file, paste the one-line text config:

```bash
read -r SKIRK_CLIENT_CONFIG
./bin/skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

## Restricted Networks

The default generated client route is `google_front_pinned`, which connects with Google-looking SNI while sending the real Google API Host header after TLS. The default exit route is `direct`, because the exit normally has ordinary internet.

For normal-network clients where speed matters more than reachability, generate direct configs:

```bash
./bin/skirk setup init --out skirk-kit-direct --client-route direct
```

## Disconnect A Config

To clean up the workspace:

```bash
./bin/skirk workspace delete --config skirk-kit/exit.json --delete-drive-folder
```

Or use the higher-level revoke command:

```bash
./bin/skirk revoke --config skirk-kit/exit.json
```

To also revoke the Google OAuth refresh token in that config:

```bash
./bin/skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

To revoke every config generated from the same OAuth login, remove the app access from Google Account security settings. Workspace deletion removes Skirk's current mailbox; OAuth revocation prevents old configs from creating or using another mailbox.

## Operational Notes

- One Google account can create multiple kits, but each kit should use its own Sheet, Drive folder, secret, and session.
- The current protocol is TCP-over-mailbox. It is reliable enough for proof and selected use, but latency is higher than a streaming endpoint.
- Drive and Sheets rate limits still apply. Use this as an owned-user transport, not as an anonymous public relay.
- If a client config leaks, revoke OAuth access and generate a new kit.
