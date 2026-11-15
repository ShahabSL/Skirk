# Two-slot metadata mailbox probe

This is a bounded research tool, not a Skirk transport. It tests whether two
known Drive file IDs can carry an ordered message log and cumulative ACK using
metadata-only `files.update` and cheap `files.get` calls. The data writer owns
one file; the ACK writer owns the other. The reader pauses for one second after
the tenth scheduled message. Each data snapshot retains all frames that the
writer has not yet seen acknowledged, so skipped Drive versions need not imply
lost messages.

The default probe uses random synthetic 64-byte frames and HMAC tags to check sequence
integrity. A production protocol would use Skirk's authenticated encryption,
replay protection, restart recovery, and separately validated device ownership.
This probe makes no claim about bulk traffic, sustained throughput, or service
capacity.

Run only with the isolated personal test kit, after other live tests finish:

```sh
go run ./tools/ack_slot_probe --live \
  --config .skirk-runs/personal-test/quota-kit/exit.json \
  --out .skirk-runs/ack-slot-live-20261114.json
```

The tool accepts only a pinned Google front and an explicit test folder. It
schedules 20 frames at 250 ms intervals; caps data PATCHes at 40, ACK PATCHes
at 20, and all GET calls at 200; and caps hot traffic at 6,500 estimated quota
units. It deletes both generated IDs and requires
two separate 404 checks per ID. The JSON result contains only sanitized counts
and timing. A mode-0600 `.recovery-ids` sidecar is created before either test
file and removed only after verified cleanup; if cleanup fails, use it for an
exact-ID recovery sweep. Both files are ignored by Git under `.skirk-runs/`.

For a matched mixed-traffic latency arm, pass `--reader-pause-ms 0`. The default
one-second pause remains the version-skip correctness stress.

## Bidirectional request/reply mode

`--roundtrip` uses the same two file IDs with one serialized writer per file.
Each metadata snapshot contains a cumulative unacknowledged suffix plus the
latest ACK for the opposite direction. It schedules 20 random 64-byte requests
at 250 ms intervals. The reader generates a 64-byte echo only after receiving
each request; the client records scheduled request-to-reply latency. Both
directions must acknowledge all 20 records. The default one-second server
reader pause after request ten tests recovery when Drive versions are skipped.

Before the workload, this mode PATCHes an exactly 8,192-byte description and
GETs it back byte for byte. The round-trip snapshots are capped at that same
encoded size. The mode allows at most 70 PATCHes per direction and 260 GETs
total, with a 12,000-unit hot-traffic budget and 16,000-unit complete-arm
bound including conservative setup and cleanup reserves. The probe deletes and
twice verifies both exact IDs as above. It is still a synthetic Drive substrate
test, not a Skirk app or TCP round trip.

```sh
go run ./tools/ack_slot_probe --live --roundtrip \
  --config .skirk-runs/personal-test/quota-kit/exit.json \
  --out .skirk-runs/ack-slot-roundtrip-20261114.json
```

Two single pinned-front arms on the isolated personal test folder completed on
November 14, 2026. These are synthetic Drive-substrate results; neither arm
included a Skirk TCP stream or bulk transfer. Scheduled round-trip timing starts
at each intended 250 ms dispatch time and ends when the echo is read.

| Arm | Verified request/reply and ACK | Scheduled RTT p50 / p95 | Skipped Drive versions | Conservative units, including cleanup |
| --- | ---: | ---: | ---: | ---: |
| Forced 1 s server-reader pause | 20/20 each direction | 1.299 / 2.145 s | 4 up, 1 down | 3,780 |
| No injected pause | 20/20 each direction | 1.044 / 1.265 s | 0 up, 2 down | 3,950 |

Both arms read back an exact 8,192-byte description, returned zero API errors,
deleted both exact IDs, confirmed 404 twice per ID, and removed their recovery
record. The private raw summaries are
`.skirk-runs/ack-slot-roundtrip-live-20261114.json` and
`.skirk-runs/ack-slot-roundtrip-nopause-live-20261114.json`. The tool does not
measure separate one-way times or prove steady-state user capacity.

### Polling and concurrent-pair sensitivity

An order-reversed four-arm no-pause comparison used the same isolated account
and 20 request/reply exchanges per arm, with a 65-second cooldown:

| Known-ID GET interval | Two scheduled RTT p95 values | Median p95 | Median GETs | Median complete-arm unit bound |
| --- | --- | ---: | ---: | ---: |
| 100 ms | 1.436 / 1.743 s | 1.589 s | 56 | 4,055 |
| 200 ms | 1.483 / 1.879 s | 1.681 s | 40 | 3,925 |

All 80 round trips and ACKs verified; each arm read back 8,192 bytes and
deleted both exact IDs. The 92 ms median-p95 gap is small beside run variation;
100 ms used 40% more GETs and was not promoted as the probe default. Private
summary: `.skirk-runs/ack-slot-poll-sensitivity-20261114.json`.

Separate bursts started five and ten independent two-file probes concurrently
on the same account, each at 200 ms polling and with no reader pause. The
ten-pair burst used a stricter 5,000-unit per-pair hot cap so even its
worst-case complete-arm bound stayed below 90,000 units.

| Concurrent pairs | Verified request/reply and ACKs | Combined scheduled RTT p50 / p95 | Conservative total units | Final slot files |
| ---: | ---: | ---: | ---: | ---: |
| 5 | 100/100 | 1.397 / 2.382 s | 17,755 | 0 |
| 10 | 200/200 | 1.118 / 1.515 s | 39,565 | 0 |

Every pair completed its exact 8,192-byte readback and twice verified both
deleted IDs as 404. There were no probe/API errors or recovery ledgers. The
ten-pair sample was faster than the earlier five-pair sample under changing
conditions; these are **not** matched capacity curves or Skirk app workloads.
Private summaries: `.skirk-runs/ack-slot-{five,ten}-pair-20261114.json`.
