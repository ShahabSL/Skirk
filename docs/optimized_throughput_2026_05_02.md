# Optimized Throughput 2026-05-02

Skirk was optimized for bulk throughput by changing the Drive+Sheets path from sequential per-chunk operations to:

- parallel Drive uploads;
- one batched Sheets append for all control rows in a transfer;
- one Sheets read for receive;
- parallel Drive downloads;
- cleanup by returned Drive file IDs instead of name lookups;
- batched Sheets tombstones for cleanup;
- larger default bulk chunk size.

The client still uses one config file. The relevant config fields are:

```json
{
  "tunnel": {
    "chunk_size": 1048576,
    "concurrency": 8
  }
}
```

## Direct Google APIs

Route:

```text
direct
```

Best result:

```text
32 MiB payload, 1 MiB chunks, concurrency 16
send       60.227 Mbps
receive    99.358 Mbps
round trip 37.498 Mbps
```

Full direct results:

| Payload | Chunk | Concurrency | Chunks | Send | Receive | Cleanup | Send Mbps | Receive Mbps | Round Trip Mbps |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 MiB | 1 MiB | 8 | 1 | 1.489s | 0.585s | 0.571s | 5.632 | 14.316 | 4.042 |
| 1 MiB | 4 MiB | 8 | 1 | 1.113s | 0.466s | 0.829s | 7.532 | 17.973 | 5.308 |
| 8 MiB | 1 MiB | 8 | 8 | 1.890s | 1.345s | 0.787s | 35.493 | 49.892 | 20.739 |
| 8 MiB | 4 MiB | 8 | 2 | 2.349s | 1.515s | 0.911s | 28.559 | 44.271 | 17.360 |
| 32 MiB | 1 MiB | 16 | 32 | 4.457s | 2.701s | 0.988s | 60.227 | 99.358 | 37.498 |
| 32 MiB | 4 MiB | 16 | 8 | 4.572s | 2.950s | 0.519s | 58.701 | 90.990 | 35.682 |

Previous best before optimization:

```text
1 MiB payload, 256 KiB chunks
round trip 1.264 Mbps
```

The optimized direct path is roughly 29.7x better by round-trip Mbps in the best measured case.

## Direct Google-Fronted APIs

Route:

```text
google_front_pinned without SOCKS proxy
TLS/SNI host: www.google.com
HTTP Host: Google API hosts
Pinned IP: 216.239.38.120
```

Best result:

```text
32 MiB payload, 1 MiB chunks, concurrency 16
send       61.221 Mbps
receive    91.444 Mbps
round trip 36.670 Mbps
```

Direct fronting is essentially the same order of throughput as direct non-fronted API access.

## Restricted Google-Fronted Path

Route:

```text
socks5h://127.0.0.1:1080
google_front_pinned
```

Stable results:

| Payload | Chunk | Concurrency | Chunks | Send | Receive | Cleanup | Send Mbps | Receive Mbps | Round Trip Mbps |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 MiB | 1 MiB | 1 | 1 | 53.465s | 7.203s | 1.540s | 0.157 | 1.164 | 0.138 |
| 4 MiB | 4 MiB | 1 | 1 | 202.456s | 20.559s | 2.286s | 0.166 | 1.632 | 0.150 |

Higher concurrency on the restricted path caused EOFs during large Drive uploads through the SOCKS proxy, so the practical restricted setting is conservative upload concurrency.

## Cleanup

Temporary benchmark spreadsheets were deleted. Drive cleanup queries found no leftover Skirk-shaped chunk objects after cleanup; one unrelated `data.z11` file was intentionally left untouched.

## Interpretation

The confirmed maximum from this machine to Google APIs is now about:

```text
37 Mbps round trip
60 Mbps send
99 Mbps receive
```

The confirmed restricted Google-fronted path is still limited mostly by the user-provided SOCKS path and upload behavior, not by the Skirk protocol.

## SOCKS tunnel throughput update: 2026-05-10

The SOCKS tunnel path was optimized after reviewing the current Drive API docs for appData files, file IDs, custom property limits, changes feeds, partial response fields, and batching behavior.

Changes applied:

- removed long `appProperties` values from Drive uploads because Drive limits each custom property key+value string to 124 UTF-8 bytes;
- encoded Drive file IDs into large DATA control filenames so receivers can download by ID without a second metadata lookup;
- inlined small encrypted payloads directly into control objects to avoid a separate Drive media file for TLS handshakes and small writes;
- batched multiple DATA events into manifest control objects, reducing control object churn for bulk streams;
- added runtime overrides for `serve-client` and `serve-exit`: `--chunk-size`, `--poll-ms`, and `--concurrency`;
- evaluated `changes.list`; it is retained only for appData-compatible future use and is not used for normal folder-backed kits because folder-scoped `files.list` was faster in the measured path.
- added split upload/download concurrency and `profile=auto`, so the client can keep restricted uploads conservative while allowing higher download fanout and higher exit response upload fanout;
- moved cleanup out of the foreground byte path. Processed Drive objects are now queued for delayed background cleanup so active streams do not compete with their own garbage collection.

Controlled test target:

```text
client -> Skirk SOCKS -> Drive mailbox -> exit -> 127.0.0.1:8000 static file on exit
chunk size: 1 MiB
poll interval: 250 ms
Drive concurrency: 32
```

Measured results:

| Mode | Payload | Result |
|---|---:|---:|
| Single stream before DATA filename IDs | 25 MiB | ~2.8 Mbps |
| Single stream after DATA filename IDs | 25 MiB | ~4.3 Mbps |
| Single stream after batched manifests | 25 MiB | **~5.85 Mbps** |
| 16 parallel streams after batched manifests | 16 x 25 MiB | **~48.74 Mbps aggregate** |
| 32 parallel streams after batched manifests | 32 x 10 MiB | ~43.98 Mbps aggregate |
| Single stream with split concurrency and delayed cleanup | 25 MiB | best sample **~13.1 Mbps**, high-variance samples around 10 Mbps |
| 16 parallel streams with split concurrency and delayed cleanup | 16 x 25 MiB | best stable sample **~37.9 Mbps aggregate**, later confirmation ~36.3 Mbps |

The current sweet spot on this machine is still about 16 concurrent application streams. Higher stream counts create more Drive API contention and tail latency without improving aggregate throughput.

`profile=auto` is not a promise that Drive behaves like a streaming CDN. It is an adaptive profile with caps: restricted client uploads are kept low because that path previously produced EOFs under upload pressure; client downloads and exit response uploads are allowed higher windows because those are the paths that benefit from fanout.

## Learning Notes

This is the expected object-store pattern: batching the control plane and parallelizing the data plane changes the scaling curve. Before optimization, every chunk paid independent Drive and Sheets latency. After optimization, Sheets is one append/read per transfer and Drive requests run concurrently.

## Why This Matters

The one-config model stays intact, but Skirk now has a real bulk mode. For restricted networks, the next step is adaptive mode selection: low upload concurrency for fragile SOCKS paths, higher download concurrency where stable, and large chunk sizes for bulk transfers.
