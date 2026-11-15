# Drive push notifications and 1 MiB upload mode probe

Run on 2026-11-14 CST using the isolated personal-test exit kit. The two probes force `google_front_pinned` for every Google API request. They use a unique generated-ID prefix, at most 4,000 estimated Drive quota units each, and no production mailbox. Raw reports are private and ignored by Git:

- `.skirk-runs/drive-watch-probe-20261114.json`
- `.skirk-runs/drive-watch-probe-retry-20261114.json`
- `.skirk-runs/drive-upload-mode-20261114.json`

## `files.watch`: blocked before Drive calls

The probe started a local receiver and two separate Cloudflare Quick Tunnels. In each attempt the ephemeral HTTPS URL failed local reachability preflight, so the tool did **not** create a Drive file or channel. The reports record `watch_status=0`, `drive_units_estimated=0`, `cleanup_verified=true`. There are no measured notification latencies. No tunnel process remained after either attempt.

The [Drive notification guide](https://developers.google.com/workspace/drive/api/guides/push) requires a publicly reachable HTTPS callback with a valid certificate. Notifications carry headers, not file metadata or content; a receiver must still call `files.get` to read the new description. `files.watch` costs 100 quota units, `channels.stop` costs 5, and notification deliveries themselves do not consume Drive quota units under the [current Drive quota schedule](https://developers.google.com/workspace/drive/api/guides/limits). A file channel lasts at most **one day**, defaults to one hour, and cannot renew automatically; it must be replaced with a new channel ID. The same user and OAuth client must stop it. The approved `drive.file` scope is supported by both [files.watch](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/watch) and [channels.stop](https://developers.google.com/workspace/drive/api/reference/rest/v3/channels/stop).

The callback is an inbound request to an arbitrary public HTTPS host. This does not preserve a client's Google-front-only accessibility by itself. An exit server might receive notifications and later write to Drive, but a restricted client still needs to poll Drive or use another reachable push route. Therefore these source-level constraints make watch a poor default for Skirk's two-ended transport even if a future webhook latency probe succeeds.

## Multipart versus resumable, one MiB

The second probe ran eight **sequential** upload arms in ABBA order: multipart, resumable, resumable, multipart, repeated. Every arm used a fresh app-generated Drive ID and exactly 1,048,576 random bytes. The multipart arm used Skirk's current `PutObjectWithID`. The resumable arm sent one metadata initiation request and one full-body PUT to the returned session URI. The returned URI had host `www.googleapis.com` and the expected `/upload/drive/v3/files?uploadType=resumable&upload_id=...` shape in all four resumable arms; sending its path through Skirk's pinned `www.google.com` front succeeded. No session URL or upload ID was logged.

| Arm | Mode | Initiation (ms) | Body (ms) | Total upload (ms) |
| ---: | --- | ---: | ---: | ---: |
| 0 | Multipart | — | — | 850.8 |
| 1 | Resumable | 348.4 | 831.3 | 1,179.7 |
| 2 | Resumable | 334.8 | 880.2 | 1,215.0 |
| 3 | Multipart | — | — | 783.7 |
| 4 | Multipart | — | — | 926.1 |
| 5 | Resumable | 326.0 | 806.0 | 1,132.0 |
| 6 | Resumable | 321.7 | 770.5 | 1,092.3 |
| 7 | Multipart | — | — | 1,038.2 |

Multipart median was **888.5 ms** (max 1,038.2 ms); resumable median was **1,155.9 ms** (max 1,215.0 ms), 30.1% longer by median. The extra initiation round trip accounted for 322–348 ms. The [Drive upload guide](https://developers.google.com/workspace/drive/api/guides/manage-uploads) describes multipart as a one-request option for small files and resumable as an initiation plus media transfer that can recover interrupted large uploads. This experiment supports keeping multipart for healthy 1 MiB transfers on this route. It does **not** rule out resumable improving the previously observed 16–28-second upload tails, interrupted mobile transfers, or larger files. Eight low-load samples cannot establish tail latency or user capacity.

All eight arms returned HTTP 200 and passed both Drive metadata SHA-256 and downloaded-byte SHA-256 checks. Every generated ID was deleted and returned 404 on two exact-ID checks. Immediate and 30-second delayed prefix sweeps each found zero objects. Store API errors and quota backoff were zero. Total estimated cost was **2,960 Drive quota units**. No live account test remains running.

The probes are at `tools/drive_watch_probe` and `tools/drive_upload_mode_probe`; both require an explicit `--live`, the exact isolated test-kit config path, and a new private output path under `.skirk-runs`. The upload-mode tool stops if a returned session URI cannot be carried by the pinned route.
