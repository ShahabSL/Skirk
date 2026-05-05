# Go Skirk

Skirk's production client is implemented in Go under `cmd/skirk` and `internal/skirk`.

## Current Modes

- `hybrid-send` and `hybrid-recv`: encrypted file/object round trips over Drive appData/folder storage, with legacy Sheets control support.
- `e2e`: creates a random payload, sends it through the hybrid transport, receives it back, compares bytes, and optionally cleans up data/control rows.
- `serve-client`: local SOCKS5 listener that sends CONNECT streams through the Drive mailbox.
- `serve-exit`: exit poller that reads client stream events, dials target TCP, and writes downstream events back.
- `workspace create/delete`: deletes visible Drive fallback workspaces; appData kits are disconnected by revoking OAuth access.

## Config

Generate a starter config:

```sh
go run ./cmd/skirk sample-config --out skirk.json --spreadsheet-id SHEET_ID
```

The important fields are:

- `secret`: shared AEAD secret. Use `skirk keygen`.
- `session_id`: optional fixed 32-hex session for a paired client and exit.
- `route.proxy`: restricted-network SOCKS proxy, usually `socks5h://127.0.0.1:1080`.
- `route.google_ip`: known Google edge IP for pinned routing.
- `drive.space`: set to `appDataFolder` for the recommended app-private mailbox.
- `drive.folder_id`: visible Drive folder ID for the fallback mailbox.
- `tunnel.chunk_size`: Drive object payload size. Start conservative, then benchmark.
- `tunnel.concurrency`: number of parallel Drive upload/download/delete workers.
- `tunnel.cleanup_processed`: removes Drive chunks and tombstones processed control rows.

## Why Drive appData

Drive appDataFolder keeps Skirk's encrypted mailbox private to the OAuth application and lets the runtime use one Google API and one narrow OAuth scope. Legacy Drive+Sheets configs still work, but new custom-OAuth kits use Drive-only control and data objects.

This does not make Google Drive a low-latency stream substrate; polling and API quotas still define the ceiling.

## Operational Notes

- Use a dedicated Google account or workspace for testing.
- Use a dedicated OAuth client/project per operator where practical.
- Keep `chunk_size` within a measured range; larger chunks improve bulk throughput but hurt latency and retries.
- `cleanup_processed` should stay enabled for interactive tests to avoid Drive object buildup.
- The access token can come from `SKIRK_ACCESS_TOKEN`, `auth.access_token`, or `auth.token_command`.

## Validation

Local:

```sh
go test ./...
pytest -q
```

Restricted network substrate:

```sh
go run ./cmd/skirk e2e --config skirk.json --bytes 2048 --delete-after
```

Throughput:

```sh
go run ./cmd/skirk bench \
  --config skirk.json \
  --sizes 1048576,33554432 \
  --chunk-sizes 1048576,4194304 \
  --concurrency 16
```

SOCKS path:

Run an exit:

```sh
go run ./cmd/skirk serve-exit --config skirk.json
```

Run a client:

```sh
go run ./cmd/skirk serve-client --config skirk.json --listen 127.0.0.1:18080
```

Then point an app at `socks5h://127.0.0.1:18080`.

## Learning Notes

This follows a split-lane design common in real transports: small ordered control messages are kept separate from heavier data frames. It gives the scheduler room to improve retries, ACKs, adaptive chunking, and cleanup without changing the encrypted data envelope.

## Why This Matters

The hard part is not AES or SOCKS; it is making a brittle, quota-limited substrate fail predictably. The current implementation keeps the core binary envelope independent from Google APIs so future carriers can reuse the same protocol.
