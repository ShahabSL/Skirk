# Skirk Setup Guide

This is the operator flow:

1. Install `skirk` on the exit/setup machine.
2. Run `skirk setup init --out skirk-kit`.
3. Complete Google login when prompted.
4. Start `skirk serve-exit --config skirk-kit/exit.json`.
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

Easy path:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit
```

If Application Default Credentials are missing, setup runs Google Cloud CLI with
Drive access enabled:

```bash
gcloud auth login --no-launch-browser --enable-gdrive-access --update-adc --force
```

That prints a browser login flow. Open the URL, approve the Google account, and
paste the code back into the terminal. On Linux, setup can install Google Cloud
CLI under `~/google-cloud-sdk` if `gcloud` is missing.

Run setup from an interactive terminal. For SSH, use `ssh -tt -p PORT user@host`
if the server does not allocate a TTY by default.

If Google Cloud CLI accepts the pasted verification code and then appears to
hang, the most common VPS cause is broken IPv6: the server resolves Google OAuth
to IPv6 addresses but cannot actually connect over IPv6. Check it on the server:

```bash
curl -4 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
curl -6 --connect-timeout 5 --max-time 15 https://oauth2.googleapis.com/token
```

If IPv4 returns quickly but IPv6 times out, prefer IPv4 before rerunning setup:

```bash
sudo sh -c 'grep -q "^precedence ::ffff:0:0/96 100" /etc/gai.conf || echo "precedence ::ffff:0:0/96 100" >> /etc/gai.conf'
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login
```

Recommended quota-owned path:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

This uses Google's device authorization flow directly with your OAuth client and
requests only:

```text
openid email https://www.googleapis.com/auth/drive.appdata
```

Drive `appDataFolder` is the default because Skirk stores encrypted app-private
mailbox objects, not user-visible files. Official Drive docs require the
`drive.appdata` scope and `spaces=appDataFolder` for this storage area.

## Creating `oauth-client.json`

Use this when you want Drive API quota associated with your own Google Cloud
project instead of the shared Google Cloud CLI OAuth client:

1. Create or select a Google Cloud project.
2. Enable Google Drive API.
3. Configure the OAuth consent screen.
4. Add your Google account as a test user if the app is in testing mode.
5. Create an OAuth client ID for `TVs and Limited Input devices`.
6. Download the client JSON as `oauth-client.json`.

Then run:

```bash
"$HOME/.local/bin/skirk" setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

If Google blocks an OAuth client, use your own OAuth project/client and keep the
consent screen/test-user setup aligned with Google policy. Skirk cannot bypass a
Google account or OAuth enforcement decision.

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
skirk serve-exit --config skirk-kit/exit.json
```

Useful exit options:

```bash
# Send exit traffic through another local proxy.
skirk serve-exit --config skirk-kit/exit.json --exit-proxy socks5h://127.0.0.1:40000

# Override Drive worker windows for experiments.
skirk serve-exit --config skirk-kit/exit.json --upload-concurrency 16 --download-concurrency 32
```

For generated kits, prefer writing the exit proxy into `exit.json` during
setup:

```bash
skirk setup init --out skirk-kit --exit-proxy socks5h://127.0.0.1:40000
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

Revoke the OAuth refresh token embedded in the generated config:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

Then remove local generated files:

```bash
rm -rf skirk-kit
```

OAuth revocation invalidates configs generated from that token. If you no longer
have the config, revoke the app from the Google account security page.

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
