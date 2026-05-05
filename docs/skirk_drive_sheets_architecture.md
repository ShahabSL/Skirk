# Skirk Drive + Sheets Architecture

Date: 2026-05-01

Status: historical design note. Current recommended Google transport is Drive `appDataFolder` for both control and data. Legacy Drive+Sheets support remains for old configs and comparison.

Reference checked: `sources/twoman` at `0d4227b4ddcbe947fd34f609d9e6089f517ef565`.

## Why This Shape

Twoman's important lesson is lane separation:

- external lanes: `ctl`, `data`
- internal scheduling classes: `ctl`, `pri`, `bulk`
- the public substrate should follow measured backend behavior instead of assuming one universal transport profile

For Skirk, the closest working equivalent is:

- **Sheets = control lane**
- **Drive = data lane**

This is not a low-latency VPN. It is a restricted-network-safe encrypted mailbox/object transport.

## Lane Mapping

| Twoman concept | Skirk Drive + Sheets equivalent |
|---|---|
| `ctl` lane | Sheets rows for sessions, chunk index, ACKs, errors, heartbeats |
| `data` lane | Drive files containing encrypted `.skb` chunk envelopes |
| `pri` class | small command/result chunks with tiny Drive objects and high-priority control rows |
| `bulk` class | larger Drive chunks for file/data transfer |
| transport profiles | adaptive chunk/poll profiles based on restricted-network measurements |

## Data Lane

Drive object names:

```text
{session_id}/{direction}/{sequence:016x}.skb
```

Drive object body:

```text
Skirk header + AES-GCM ciphertext + auth tag
```

The current implementation already uses this layout.

## Control Lane

Recommended Sheets columns:

```text
session_id
direction
sequence
event
drive_object
bytes
sha256
priority
timestamp_ns
ack_base
ack_bitmap
error
```

Events:

```text
SESSION_OPEN
CHUNK_READY
CHUNK_ACK
CHUNK_MISSING
SESSION_DONE
SESSION_ERROR
HEARTBEAT
```

## Sender Flow

1. Split plaintext into chunks.
2. Encrypt each chunk into an `.skb` envelope.
3. Upload `.skb` to Drive.
4. Append `CHUNK_READY` row to Sheets.
5. Watch Sheets for `CHUNK_ACK` or `CHUNK_MISSING`.
6. Retry missing chunks with backoff.

## Receiver Flow

1. Poll Sheets for `CHUNK_READY`.
2. Download the named Drive object.
3. Verify/decrypt the envelope.
4. Reassemble in sequence order.
5. Append `CHUNK_ACK` rows to Sheets.
6. Optionally delete acknowledged Drive objects.

## Adaptive Profile

Start conservative in the restricted network:

```text
chunk_size = 2 KiB or 4 KiB
parallel_uploads = 1
parallel_downloads = 1-2
sheets_poll = 1-3 seconds
drive_request_timeout = 30-60 seconds
```

Then increase only after successful windows:

```text
4 KiB -> 8 KiB -> 16 KiB -> 32 KiB -> 64 KiB
```

The live test showed:

- Drive tiny multipart upload works.
- Skirk Drive E2E works at ~2 KiB.
- A ~96 KiB run timed out.

So the first real profile should prefer reliability over throughput.

## Answer

Drive + Sheets can do the job for:

- reliable encrypted command exchange,
- delayed data transfer,
- file/message transport,
- store-and-forward tasks,
- a fallback data lane in restricted Google-reachable networks.

Drive + Sheets should not be treated as the final answer for:

- live browsing,
- SSH-like interactive sessions,
- high-throughput continuous proxy traffic,
- low-latency VPN behavior.

For interactive traffic, the next candidate remains Apps Script + controlled exit, using the same lane lessons: control should be cheap and explicit, data should be separately scheduled and quota-governed.
