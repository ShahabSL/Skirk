# Skirk Setup Guide

This is the operator flow:

1. Install `skirk` on the exit/setup machine.
2. Run `skirk setup init --out skirk-kit --reset-google-login`.
3. Open the Google URL printed by setup, enter the short code, and approve.
4. Setup installs/enables `skirk-exit.service` and starts the exit on Linux.
5. Send only `skirk-kit/client.skirk` or its one-line text to clients.

The exit machine needs outbound internet access. It does not need an inbound
port because both sides exchange encrypted objects through Google Drive.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
"$HOME/.local/bin/skirk" version
```

From a source checkout:

```bash
make build
./bin/skirk version
```

## Create A Kit

Reliable new-install path:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

On Linux this starts the exit as `skirk-exit.service` after Google login and
kit generation. Use `--start-exit=false` for config-only setup,
`--exit-service-name NAME` to choose a different systemd unit name,
`--exit-service-user USER` to run the service as a specific account, or
`--exit-service-enable=false` to start it now without enabling boot startup.

Google blocks the default Google Cloud SDK OAuth client when a third-party app
requests Drive scopes. That is the browser page that says "This app is blocked".
Skirk therefore does not launch the default `gcloud` Drive-scope login for new
credentials. Official release builds use Skirk's OAuth client directly. Source
or development builds can set `SKIRK_OAUTH_CLIENT_ID` and
`SKIRK_OAUTH_CLIENT_SECRET`, pass `--oauth-client-file`, or pass `--adc` with
credentials you created yourself.

Run setup from an interactive terminal. For SSH, use `ssh -tt -p PORT user@host`
if the server does not allocate a TTY by default.

If setup cannot contact Google's OAuth endpoints, one common VPS cause is broken
IPv6: the server resolves Google OAuth to IPv6 addresses but cannot actually
connect over IPv6. Check it on the server:

```bash
curl -4 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
curl -6 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
```

If IPv4 returns quickly but IPv6 times out, prefer IPv4 before rerunning setup:

```bash
sudo sh -c 'grep -q "^precedence ::ffff:0:0/96 100" /etc/gai.conf || echo "precedence ::ffff:0:0/96 100" >> /etc/gai.conf'
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

This uses Google's OAuth device authorization flow.
The terminal prints a URL and a short code. Open the URL in your browser, enter
the code there, approve Drive access, and the terminal continues without a
paste-back step.

Skirk requests only the Drive mailbox scope it needs:

```text
https://www.googleapis.com/auth/drive.file
```

The Google device-code flow accepts `drive.file`, which lets Skirk create and
manage only files and folders created by the Skirk app. Setup creates a
`skirk-mailbox-...` Drive folder and writes that folder ID into the generated
exit and client configs. This is the public easy-install path because it avoids
the blocked Google Cloud SDK OAuth client and does not require each user to
create an OAuth app.

## Personal Quota Mode

Normal users can stay on the built-in Skirk OAuth client. Heavy users, forks,
and deployments that do not want to share Skirk's Google Cloud project quota
should create their own OAuth client and pass it to setup.

With the built-in client:

- setup is easiest;
- Drive API usage is charged to Skirk's Google Cloud project quota;
- each Google account still has its own per-user-per-project quota bucket.

With a personal OAuth client:

- setup uses the user's own Google Cloud project;
- Drive API usage is charged to that project quota;
- the generated Skirk configs still use only the same `drive.file` mailbox
  scope.

To create a personal Skirk OAuth client:

1. Create or select a Google Cloud project.
2. Enable Google Drive API.
3. Configure the OAuth consent screen and publish it if you want refresh tokens
   that do not expire after the testing window.
4. Add your Google account as a test user if the app is in testing mode.
5. Create an OAuth client ID for `TVs and Limited Input devices`.
6. Download the client JSON as `oauth-client.json`.

Then run:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

The guided equivalent is:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-mode personal
```

The personal OAuth wizard prints the exact Google Cloud pages to open:

1. Create or select a Google Cloud project.
2. Enable Google Drive API.
3. Configure the OAuth consent screen and add your Google account as a test user
   if the app stays in testing mode.
4. Create an OAuth client with application type `TVs and Limited Input devices`.
5. Paste the client ID and client secret into Skirk, or download the JSON.

If you paste the client ID and secret, setup writes the ignored local
`oauth-client.json` file for you.

Or set environment variables for local builds:

```bash
SKIRK_OAUTH_CLIENT_ID='...' \
SKIRK_OAUTH_CLIENT_SECRET='...' \
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

If Google blocks your OAuth client, enable the Google Drive API, configure the
OAuth consent screen, add the signing-in Google account as a test user while the
app is in testing, or publish/verify the app according to Google policy. Skirk
cannot bypass a Google account or OAuth enforcement decision.

Google Drive API project limits can be increased for some quota types from the
Google Cloud Quotas page, but increases are not guaranteed. Some limits are not
adjustable, including Google's per-user Drive upload limit and the daily billing
threshold documented for the Drive API.

This split mirrors mature Drive tools such as rclone: default shared OAuth keeps
setup easy, while personal OAuth isolates quota for high-volume users.

## Generated Files

`skirk-kit/exit.json`:
Keep this on the exit machine. It contains credentials.

`skirk-kit/client.skirk`:
One-line profile for client devices. This is the easiest thing to paste into
Linux, Windows, or Android clients. You can import the same profile on more
than one device. Skirk gives each imported profile a local client identity, and
each connection run gets a fresh run identity, so simultaneous devices do not
claim each other's Drive responses.

`skirk-kit/client.json`:
JSON form of the same client profile.

`skirk-kit/client-command.txt`:
Ready-to-copy Linux client command with the one-line profile embedded.

`skirk-kit/README.md`:
Per-kit commands generated at setup time.

All generated profiles contain a Google refresh token and the Skirk tunnel
secret. Do not commit them or paste them into public logs.

## Run The Exit

```bash
skirk service status
```

Useful exit options:

```bash
# If setup was run with --start-exit=false, start manually.
skirk serve-exit --config skirk-kit/exit.json

# Send exit traffic through another local proxy.
skirk serve-exit --config skirk-kit/exit.json --exit-proxy socks5h://127.0.0.1:40000

# Override Drive worker windows for experiments.
skirk serve-exit --config skirk-kit/exit.json --upload-concurrency 16 --download-concurrency 32
```

For generated kits, prefer writing the exit proxy into `exit.json` during
setup:

```bash
skirk setup init --out skirk-kit --reset-google-login --exit-proxy socks5h://127.0.0.1:40000
```

When using `install.sh`, `SKIRK_INSTALL_WIREPROXY=1` installs wgcf/wireproxy,
starts `wireproxy.service`, and defaults `SKIRK_EXIT_PROXY` to that local SOCKS
listener. `SKIRK_ACCEPT_WARP_TOS=1` makes the WARP registration noninteractive.

`serve-exit` starts a mailbox janitor automatically. It removes stale mux
transport objects older than 24 hours. Override with:

```bash
SKIRK_JANITOR_OLDER_THAN=6h skirk serve-exit --config skirk-kit/exit.json
SKIRK_DISABLE_JANITOR=1 skirk serve-exit --config skirk-kit/exit.json
```

## Run A Linux Client

With a file:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

For repeated CLI use on the same machine, set a stable client ID. Desktop and
Android apps do this automatically when importing a profile:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

With pasted text:

```bash
read -r SKIRK_CLIENT_CONFIG
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

Test:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

Use `socks5h` semantics in apps that support it so DNS resolution happens
through the exit.

Optional HTTP/HTTPS proxy listener:

```bash
skirk serve-client \
  --config "$SKIRK_CLIENT_CONFIG" \
  --listen 127.0.0.1:18080 \
  --http-proxy-listen 127.0.0.1:18081
```

## Restricted Networks

Generated client profiles default to `google_front`. That route uses a
Google-looking TLS/SNI path for Google API traffic, which is the current default
for the tested hostile network. The exit route defaults to `direct`.

When the hostile network is available through a local SOCKS proxy:

```bash
skirk serve-client \
  --config "$SKIRK_CLIENT_CONFIG" \
  --listen 127.0.0.1:18080 \
  --route-mode google_front \
  --upstream-proxy socks5h://127.0.0.1:11093
```

For direct Google API routing on a normal network:

```bash
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080 --route-mode direct
```

Available route modes:

- `direct`
- `real_pinned`
- `google_front`
- `google_front_pinned`
- `google_front_h1`
- `google_front_h1_pinned`

Pinned modes use the configured `--google-ip` value.

## Benchmarks

The exit must be running before client-side benchmarks.

```bash
skirk bench-live --config skirk-kit/client.skirk --samples 5
```

Hostile path:

```bash
skirk bench-live \
  --config skirk-kit/client.skirk \
  --upstream-proxy socks5h://127.0.0.1:11093 \
  --route-mode google_front \
  --samples 3
```

Bulk throughput:

```bash
skirk bench-live --config skirk-kit/client.skirk --bulk-url http://example.com/big.bin --timeout 5m
```

The JSON result includes p50/p95 latency, Mbps, Drive operation timings, and
estimated quota calls/units per request and per minute.

## Cleanup

Runtime cleanup is enabled in generated configs. Processed mux objects are
deleted after use, with foreground traffic prioritized over cleanup. The exit
janitor removes stale leftovers from crashed clients or interrupted exits.

Dry-run stale object cleanup:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
```

Delete stale objects:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h --delete
```

Clean a specific prefix:

```bash
skirk cleanup --config skirk-kit/exit.json --prefix muxv4/ --older-than 1s --delete
```

## Disconnect Or Revoke

Stop the client and exit processes first.

If the exit is running as a systemd service:

```bash
skirk service stop --name skirk-exit
```

Revoke the OAuth refresh token embedded in the generated config:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

Delete stale Drive mailbox objects and then remove local generated files:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 0s --delete
```

```bash
rm -rf skirk-kit
```

OAuth revocation invalidates configs generated from that token. If you no longer
have the config, revoke the app from the Google account security page.

The interactive menu (`skirk` with no arguments) exposes the same setup,
service, cleanup, revoke, and local delete actions.

## Learning Notes

Skirk uses Google Drive as an object mailbox, not as a stream. The production
shape is a bounded mux: many TCP streams are encoded into encrypted Drive mux
objects, then reassembled by the exit. Mux v4 also namespaces objects by local
client ID and per-run ID, which is the normal distributed-systems pattern for
sharing one credential/profile across multiple independent devices without
cross-delivery. This avoids one polling loop per browser connection, which is
the failure mode that makes naive Drive proxy designs collapse under real
browsing.

## Why This Matters

The operational risk is not just speed. Leftover mailbox objects consume Google
Drive storage, leaked profiles are credentials, and excessive API calls can hit
Drive rate limits. The setup, cleanup, janitor, and benchmark commands are part
of the production surface, not side utilities.
