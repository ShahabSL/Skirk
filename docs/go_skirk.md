# Go CLI Notes

Skirk's core transport lives in `cmd/skirk` and `internal/skirk`.

## Commands

```bash
skirk help
skirk version
skirk keygen
skirk setup init --out skirk-kit --reset-google-login
skirk serve-exit --config skirk-kit/exit.json
skirk serve-client --config skirk-kit/client.skirk --listen 127.0.0.1:18080
skirk bench-live --config skirk-kit/client.skirk
skirk cleanup --config skirk-kit/exit.json --older-than 2h [--delete]
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

`client` and `exit` are compatibility aliases for `serve-client` and
`serve-exit`; user-facing docs should use the explicit `serve-*` names.

## Config

Generate a sample config:

```bash
go run ./cmd/skirk sample-config --out skirk.json
```

Generate a production kit:

```bash
go run ./cmd/skirk setup init --out skirk-kit --reset-google-login
```

Important fields:

- `secret`: shared tunnel secret.
- `session_id`: paired client/exit mailbox session.
- `client.id`: optional local client identity. Desktop and Android create this
  automatically per imported profile; CLI users can pass `--client-id`.
- `client.run_id`: generated on every client start. It is not stored in normal
  shared profiles.
- `auth`: Google OAuth credentials or a token command.
- `route.mode`: Google API route mode.
- `route.proxy`: optional upstream proxy for the client Google API path.
- `route.google_ip`: Google edge IP for pinned route modes.
- `drive.folder_id`: generated setup kits use a Skirk-created Drive mailbox
  folder for the public device-login flow.
- `tunnel.profile`: `auto` by default.
- `tunnel.chunk_size`: transport coalescing target. Defaults to 16 MiB; v4
  still caps an individual Drive mux object at about 4 MiB to stay under the
  measured stable Drive object size.
- `tunnel.poll_interval_ms`: baseline mailbox poll interval.
- `tunnel.upload_concurrency` / `tunnel.download_concurrency`: optional manual
  caps; leave unset for auto profile.
- `tunnel.exit_proxy`: optional proxy for target traffic from the exit.
- `tunnel.cleanup_processed`: deletes processed mux objects.

For the production transport design, see [Architecture](architecture.md). For
protocol experiments and promotion gates, see
[Transport Research](transport-research.md).

## Drive Mailbox

Skirk setup creates a Drive mailbox folder for encrypted runtime objects. The
recommended public setup path uses only `drive.file`, so Skirk can manage files
and folders created by the Skirk OAuth app without requesting broad Drive
access.

Drive is still an object API. Runtime discovery uses fresh prefix listing;
latency comes from upload, object visibility, download, and cleanup operations.
The mux design reduces object count and browser fanout overhead; it does not
make Drive a low-latency stream.

## Quota Accounting

Skirk logs an estimated Drive quota window:

```text
drive quota window=1m0s calls=42 est_units=5100 errors=0 response_bytes=123456 ops=download:12/2400u,list:18/1800u,upload:12/600u
```

The estimate follows Skirk's internal unit table:

- `list`: 100 units
- `download`: 200 units
- `upload`, `delete`, and object create operations: 50 units

Set:

```bash
SKIRK_QUOTA_LOG_INTERVAL=10s
```

to log more frequently during short tests, or:

```bash
SKIRK_QUOTA_LOG_INTERVAL=0
```

to disable the periodic quota line.

For project-level truth, use Google Cloud Console metrics for the Google Drive
API. Those charts are useful when the kit was generated with your own OAuth
client/project.

## Cleanup

Generated configs enable runtime cleanup. Processed objects are deleted after
they are consumed. Cleanup yields to foreground traffic, so active browsing gets
priority over deleting old objects.

`serve-exit` also starts a janitor:

- default age: 2 minutes;
- default interval: 1 minute;
- prefixes: mux transport, Drive benchmark, and setup marker prefixes.

Environment controls:

```bash
SKIRK_DISABLE_JANITOR=1
SKIRK_JANITOR_OLDER_THAN=6h
SKIRK_JANITOR_INTERVAL=1h
```

Manual cleanup:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
skirk cleanup --config skirk-kit/exit.json --older-than 2h --delete
skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000
```

If the mailbox folder was deleted, repair the kit and restart the service:

```bash
skirk repair-mailbox --kit skirk-kit --start-exit
```

## Validation

Local tests:

```bash
go test ./...
```

Direct live test:

```bash
skirk serve-exit --config skirk-kit/exit.json
skirk bench-live --config skirk-kit/client.skirk --samples 5
```

Restricted path:

```bash
skirk bench-live \
  --config skirk-kit/client.skirk \
  --upstream-proxy socks5h://127.0.0.1:11093 \
  --route-mode google_front \
  --samples 3
```

## Learning Notes

The production path is a bounded mux over object storage. That is the same
high-level pattern used by real transports when they separate logical streams
from physical lanes, but the underlying carrier here is Drive object creation
and discovery rather than a socket.

## Why This Matters

Most failure modes are operational: quota pressure, stale objects, leaked
profiles, token expiry, and hostile-path variance. Keeping the CLI surface small
and measurable makes those failures visible instead of hiding them behind many
unmaintained modes.
