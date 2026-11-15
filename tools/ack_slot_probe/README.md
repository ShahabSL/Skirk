# Two-slot metadata mailbox probe

This is a bounded research tool, not a Skirk transport. It tests whether two
known Drive file IDs can carry an ordered message log and cumulative ACK using
metadata-only `files.update` and cheap `files.get` calls. The data writer owns
one file; the ACK writer owns the other. The reader pauses for one second after
the tenth scheduled message. Each data snapshot retains all frames that the
writer has not yet seen acknowledged, so skipped Drive versions need not imply
lost messages.

The probe uses random synthetic 64-byte frames and HMAC tags to check sequence
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
