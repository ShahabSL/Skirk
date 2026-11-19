# Skirk Setup Guide

This is the operator flow:

1. Install `skirk` on the exit/setup machine.
2. Create and publish a [personal Google OAuth app](personal-google-setup.md).
3. Run `skirk setup init --out skirk-kit`, open the Google URL, and approve Drive.
4. Setup installs/enables `skirk-exit.service` and starts the exit on Linux.
5. Send only `skirk-kit/client.skirk` or its one-line text to clients.

The exit machine needs outbound internet access. It does not need an inbound
port because both sides exchange encrypted objects through Google Drive.

New kits use `muxv5`. Upgrade the exit and every client together; older
versions only watch `muxv4/`. See the [migration guide](mux-v5-migration.md)
when replacing an existing kit.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
skirk version
```

From a source checkout:

```bash
make build
./bin/skirk version
```

## Create A Kit

Download your Desktop app OAuth JSON from Google and save it as
`oauth-client.json` on the exit. Then create a kit.

```bash
skirk setup init --out skirk-kit
```

On Linux setup starts `skirk-exit.service`. Use `--start-exit=false` for a
config-only setup, `--exit-service-name NAME` for a dedicated unit, and
`--exit-service-user USER` for a specific account. See the
[install guide](install.md) for macOS and Windows exit commands.

New kits use UDP and exit DNS. Setup tests IPv6 through the exit's route and
keeps IPv4 available. For advanced overrides, see `skirk setup init --help`.
Publish your OAuth app before signing in. If you previously signed in while the
app was in Testing, reauthorize under the same client after publishing.

## Personal Quota Mode

Setup uses a personal Desktop OAuth client by default. Each operator can share
their generated Skirk profile with the people who use that exit. They draw
from the same account and project quota.

With the built-in client:

- the operator explicitly selects shared OAuth in a build that supplies it;
- Drive API usage is charged to Skirk's shared Google Cloud project quota;
- each Google account still has its own per-user-per-project quota bucket.

With a personal OAuth client:

- setup uses the user's own Google Cloud project;
- Drive API usage is charged to that project quota;
- the generated Skirk configs still use only the same `drive.file` mailbox
  scope.

The verified publisher domain belongs to the Skirk OAuth app; it is not a
domain that every user needs for Drive transport. A user publishing a separate
personal OAuth app must still follow Google's own branding and verification
requirements for that app. A shared, published OAuth client with a separate
user-owned quota project is an experimental alternative described below;
the current short test proves API compatibility, not a 200,000-user capacity.

To create a personal Skirk OAuth client:

1. Create or select a Google Cloud project.
2. Enable Google Drive API.
3. Configure the OAuth consent screen and publish it to avoid Testing mode's
   seven-day refresh-token expiration. Other expiry and policy rules still apply.
4. Add your Google account as a test user if the app is in testing mode.
5. Create an OAuth client ID with application type `Desktop app`.
6. Download the client JSON as `oauth-client.json`, or copy the client ID and
   client secret from the client details page.

Then run:

```bash
skirk setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

The guided equivalent is:

```bash
skirk setup init --out skirk-kit --reset-google-login --oauth-mode personal
```

The personal OAuth wizard prints the exact Google Cloud pages to open:

1. Create or select a Google Cloud project.
2. Enable Google Drive API.
3. Configure the OAuth consent screen and add your Google account as a test user
   if the app stays in testing mode.
4. Create an OAuth client with application type `Desktop app`.
5. Paste the client ID and client secret into Skirk. You can also download the
   JSON.

If you paste the client ID and secret, setup writes the ignored local
`oauth-client.json` file for you. Skirk then prints a Google approval URL. On a
VPS over SSH, the browser usually redirects to a localhost URL that cannot load;
copy that full address-bar URL back into the terminal and Skirk extracts the
authorization code.

Do not choose `TVs and Limited Input devices` for personal setup unless Google
also provides a client secret and you intentionally run setup with
`--oauth-flow device`. Google's device-code token polling requires
`client_secret`; a TV client that only shows a client ID cannot complete setup.

If Google shows `Access blocked` and says the app is currently being tested,
open `https://console.cloud.google.com/auth/audience`, add the exact Google
account shown on the blocked page under Test users, then rerun setup. Publishing
the app to Production also removes the Testing allowlist block, although Google
can still show unverified-app warnings and user caps until verification. Do not
add extra scopes for this error; Skirk requests `drive.file` during Google login.

Or set environment variables for local builds:

```bash
SKIRK_OAUTH_CLIENT_ID='...' \
SKIRK_OAUTH_CLIENT_SECRET='...' \
SKIRK_OAUTH_FLOW=desktop \
skirk setup init --out skirk-kit --reset-google-login
```

`SKIRK_OAUTH_FLOW=desktop` is the personal-project path that works cleanly on a
VPS by pasting back the redirected localhost URL.

If Google blocks your OAuth client, enable the Google Drive API, configure the
OAuth consent screen, add the signing-in Google account as a test user while the
app is in testing, or publish/verify the app according to Google policy. Skirk
cannot bypass a Google account or OAuth enforcement decision.

### Renew an expiring personal OAuth grant

Google says that an external OAuth app in **Testing** expires its Drive refresh
tokens seven days after consent. For a long-running personal kit, publish its
OAuth app **In production** in the Google Auth Platform before signing in. See
[Google's app audience rules](https://support.google.com/cloud/answer/15549945).
Refresh tokens can also stop working after revocation, long inactivity, or
Google account or administrator changes; see [Google's OAuth guidance](https://developers.google.com/identity/protocols/oauth2#expiration).
Publishing does not guarantee indefinite access or approval for every use case.
Google may require app verification, and operators must review the
[Google Workspace user data and developer policy](https://developers.google.com/workspace/workspace-api-user-data-developer-policy)
before using Drive as a transport. Renewal cannot override a Google policy or
verification decision.

To renew the Google grant for an existing kit without changing its tunnel secret,
session ID, or Drive folder:

```bash
skirk setup renew-oauth --kit skirk-kit --restart-exit
```

The command signs in with the **same Google OAuth client ID**, checks that the
new grant can use the existing mailbox, backs up the original kit files, and
updates credentials in `exit.json` and any client profiles in the kit. Renewal also
migrates the command generated by older kits to read the private `./client.skirk`
file. Custom file-based commands keep their original contents. For
a personal client without a stored secret, pass its original downloaded JSON as
`--oauth-client-file`; a different OAuth client ID is refused because it may not
have access to the original Drive folder. Use `--config /path/to/exit.json` for
a kit in a custom location. Add `--oauth-flow desktop` when needed.

Re-import the updated `client.skirk` on a device if its own Google grant has
expired or you want it to use the newly issued grant. Existing clients with a
still-valid grant and the same mailbox can continue after the exit is renewed.
An imported profile retains its old refresh token, so restarting the exit alone
does not restore a client whose grant also expired.
Without `--restart-exit`, restart the exit service yourself after renewal. The
backup directory printed by the command contains old refresh tokens and must be
kept private or deleted after recovery is confirmed.

When Google returns a terminal OAuth error such as `invalid_grant`, either at
startup or while a process is running, `skirk serve-exit` and `skirk
serve-client` exit with status 78. The current Skirk-managed **exit** systemd
unit does not restart on that status,
avoiding a repeated failed refresh loop. If an older unit is already installed,
run `skirk service install --config /path/to/exit.json` once to update its
restart policy. After renewal, `--restart-exit` starts the service with the new
grant. If an external supervisor runs the exit, configure it to treat status
78 as a stop rather than an automatic retry.

Google Drive API project limits can be increased for some quota types from the
Google Cloud Quotas page, but increases are not guaranteed. Some limits are not
adjustable, including Google's per-user Drive upload limit and the daily billing
threshold documented for the Drive API.

For a self-hosted exit, a personal OAuth project lets its operator monitor
Drive usage and handle authorization directly.

### Optional separate Drive quota project

An advanced operator can experiment with a separate Google Cloud project for
Drive API quota while retaining the same OAuth client and mailbox. Create the
project, enable Google Drive API there, and ensure the signing-in account has
`serviceusage.services.use` permission on it. Then include the project ID at
setup time:

```bash
skirk setup init --out skirk-kit --reset-google-login \
  --oauth-client-file ./oauth-client.json \
  --quota-project your-cloud-project-id
```

Skirk stores that ID in both kit configs and sends `X-Goog-User-Project` on
Drive calls only. It does not put the header on OAuth token requests. Invalid
IDs and Google permission or disabled-API errors fail explicitly; Skirk does
not silently fall back to another project's quota. Leave this flag unset for the usual personal OAuth setup.

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
Run this command on the client from the directory that contains its private `client.skirk`.

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

The exit proxy handles connections from the exit to websites and other
destinations. It accepts SOCKS5 and HTTP(S) CONNECT URLs. UDP and QUIC need a
SOCKS5 proxy that supports UDP ASSOCIATE and can return relayed datagrams. Setup
checks for a real IPv4 UDP DNS reply over SOCKS5 before generating a UDP kit.
An HTTP(S) exit or a SOCKS5 proxy that explicitly rejects UDP ASSOCIATE gets a
TCP kit when UDP is left on automatic. Use `--udp=false` to choose TCP explicitly
if the test cannot verify UDP, for example when the proxy filters UDP port 53.

The client's `route.proxy` setting is separate. Use `--client-proxy` during kit
setup or `--upstream-proxy` with `serve-client` when the client itself needs a
SOCKS5 proxy to reach Google. Keep authenticated proxy URLs in private configs.

### Choose the exit DNS resolver

The exit uses Google DNS over TLS by default. A UDP kit can use a different
DNS-over-TLS resolver by setting both `exit_dns_address` and `exit_dns_tls_name`
inside the `tunnel` object in `exit.json`. Use a numeric IP address with its
port for the address and the resolver's certificate hostname for the TLS name.
Put brackets around IPv6 addresses. Restart the exit after editing the file.
Skirk uses the configured exit proxy to reach the resolver when one is enabled.
Certificate verification is required, and an authentication failure returns a
DNS error. Clients keep their existing VPN DNS address.

### Run the exit without root

The Go exit itself needs no root privileges. If your Linux host provides a
systemd user manager, keep the binary and secret kit in your home directory and
install a user service. Run these commands as the account that owns the kit:

```bash
install -d -m 700 "$HOME/.config/skirk" "$HOME/.config/systemd/user" "$HOME/.local/bin"
install -m 600 skirk-kit/exit.json "$HOME/.config/skirk/exit.json"
install -m 755 ./skirk "$HOME/.local/bin/skirk"
cat >"$HOME/.config/systemd/user/skirk-exit.service" <<'EOF'
[Unit]
Description=Skirk exit

[Service]
ExecStart=%h/.local/bin/skirk serve-exit --config %h/.config/skirk/exit.json
Restart=on-failure
RestartPreventExitStatus=78
RestartSec=30s

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now skirk-exit.service
systemctl --user status skirk-exit.service
```

Use the path to your actual `skirk` binary in the `install` command. Check logs
with `journalctl --user -u skirk-exit.service -f`. A user service normally stops
after the last logout; if your host permits it, `loginctl enable-linger "$USER"`
keeps the user manager running after logout and starts it at boot. Some hosts
require an administrator to enable lingering. The generated kit and profile
remain credentials; keep the config mode `0600`. This is a manual user-service
path for issue [#22](https://github.com/ShahabSL/Skirk/issues/22); the `skirk
service` CLI manages system services instead. See systemd's
[user unit paths](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit)
and [lingering behavior](https://www.freedesktop.org/software/systemd/man/latest/loginctl).

## Multiple Exit Instances

One Linux exit machine can run more than one Skirk instance when you want to
split traffic across multiple OAuth projects or Drive mailboxes. Run `skirk`
and choose `Manage exit services and instances`:

- `Create another Skirk exit instance` generates a separate kit under
  `~/.config/skirk/instances/<id>/` and installs a service named
  `skirk-exit-<id>` by default.
- `Manage an existing exit instance` shows status, start/stop/restart, outbound
  proxy/WARP configuration, Drive cleanup, and per-instance performance tuning.
- `Delete this instance` has separate prompts for service removal, Drive object
  deletion, OAuth revocation, and local kit deletion. It does not remove the
  Skirk binary.

The original `skirk-kit/exit.json` plus `skirk-exit.service` flow remains the
default simple setup.

When using `install.sh`, `SKIRK_INSTALL_WIREPROXY=1` installs Skirk-managed
wgcf/wireproxy under fixed system paths, starts `wireproxy.service` as the
unprivileged `skirk-wireproxy` user, and defaults `SKIRK_EXIT_PROXY` to the
local loopback SOCKS listener. Managed WARP binds only to `127.0.0.1:PORT`;
use an explicit `--exit-proxy` or `SKIRK_EXIT_PROXY` for a different
already-secured outbound proxy. `SKIRK_ACCEPT_WARP_TOS=1` makes WARP
registration noninteractive.

`serve-exit` starts a mailbox janitor automatically. It runs at startup and then
every 2 minutes, removing stale mux transport, benchmark, and setup-marker
objects older than 24 hours. Normal processed mux objects are deleted by the
foreground-aware runtime cleanup path; the janitor uses a long age window and
low delete concurrency so very slow long-running downloads are not treated as
stale data, while crashed-session leftovers are still cleaned eventually.
Override with:

```bash
SKIRK_JANITOR_OLDER_THAN=48h SKIRK_JANITOR_INTERVAL=2h skirk serve-exit --config skirk-kit/exit.json
SKIRK_DISABLE_JANITOR=1 skirk serve-exit --config skirk-kit/exit.json
```

## Run A Linux Client

For a supervised router client, see the [OpenWrt service guide](openwrt.md).

With a file:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

For repeated CLI use on the same machine, set a stable client ID. Desktop and
Android apps do this automatically when importing a profile:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

If you received the profile as text, paste it into the Android or desktop app.
For CLI use, save the text in a private `client.skirk` file and run the file
command above. On Linux and macOS, `chmod 600 ./client.skirk` limits file
access to your account.

Test:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

Use `socks5h` semantics in apps that support it so DNS resolution happens
through the exit.

Optional HTTP/HTTPS proxy listener:

```bash
skirk serve-client \
  --config ./client.skirk \
  --listen 127.0.0.1:18080 \
  --http-proxy-listen 127.0.0.1:18081
```

For trusted-LAN sharing, bind to the client's LAN address. Only allow your
intended clients through the host firewall. Replace `192.168.1.20` with the
address of the machine running Skirk.

```bash
skirk serve-client \
  --config ./client.skirk \
  --listen 192.168.1.20:18080 \
  --http-proxy-listen 192.168.1.20:18081
```

SOCKS5 carries TCP and, with a UDP-enabled profile, standard UDP ASSOCIATE
traffic from loopback and private-network peers. The client app must support
SOCKS5 UDP. The HTTP listener carries HTTP and TCP CONNECT. Desktop and Android
offer LAN sharing in proxy mode. One shared Skirk client uses one exit carrier
for its UDP sessions.

Clients can also override Drive runtime behavior:

```bash
skirk serve-client \
  --config ./client.skirk \
  --listen 127.0.0.1:18080 \
  --poll-ms 1000 \
  --upload-concurrency 8 \
  --download-concurrency 16 \
  --metrics-path /tmp/skirk-client.metrics.json
```

Desktop and Android clients expose this through presets and show the same local
metrics estimate in the UI. Treat it as local burn rate only, not remaining
project quota.

## Restricted Networks

Generated client profiles default to `google_front_pinned`. That route uses
`www.google.com` TLS SNI, sends the request to Google with the Drive API host
in the HTTP authority, and pins the configured Google edge IP. Newly generated
exit configs use that front too; pass `--exit-route direct` at setup only when
you explicitly choose it. Android and desktop apps show **Google front** and
**Direct Google** as explicit per-profile choices while disconnected; they do
not silently switch routes based on a speed test.

Hand-written configs with no `route.mode` also default to
`google_front_pinned`. Existing configs with an explicit `direct` or
`real_pinned` mode retain that value; choose **Google front** in the app or
set `route.mode` to `google_front_pinned` to switch them.

The CLI follows the route saved in the profile. Users who can reach direct
Google can select it when creating a kit with `skirk setup init --client-route
direct`, or override one run with `skirk serve-client --route-mode direct`.

A hand-written `auth.token_url` overrides Google-front routing for token
refreshes and sends the refresh request to that custom endpoint. An explicit
`route.proxy` still carries it through SOCKS5. Skirk does not follow token
endpoint redirects, so a redirect cannot resend OAuth credentials elsewhere.

Existing Linux profiles generated before v0.1.51 are not rewritten by an
update. Regenerate the kit or pass
`--route-mode google_front_pinned --google-ip 216.239.38.120` when starting the
client.

When the hostile network is available through a local SOCKS proxy:

```bash
skirk serve-client \
  --config ./client.skirk \
  --listen 127.0.0.1:18080 \
  --route-mode google_front_pinned \
  --upstream-proxy socks5h://127.0.0.1:11093
```

For direct Google API routing on a normal network:

```bash
skirk serve-client --config ./client.skirk --listen 127.0.0.1:18080 --route-mode direct
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
  --route-mode google_front_pinned \
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

Dry-run stale object cleanup. Without `--prefix`, cleanup scans Skirk's runtime
object prefixes (`muxv5/`, `muxv4/`, `bench-drive/`, and `setup/`):

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
```

Delete stale objects:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h --delete
```

Clean a specific prefix:

```bash
skirk cleanup --config skirk-kit/exit.json --prefix muxv5/ --older-than 1s --delete
```

Emergency-only cleanup: stop Skirk clients and the exit first, then empty every
object in this Skirk mailbox. Running this while traffic is active can delete
live mux objects and disconnect clients.

```bash
skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000
```

If the configured Drive mailbox folder was deleted or the logs show
`drive_not_found`, create a replacement folder, rewrite the local kit, and
restart the exit service:

```bash
skirk repair-mailbox --kit skirk-kit --start-exit
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
skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000
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
