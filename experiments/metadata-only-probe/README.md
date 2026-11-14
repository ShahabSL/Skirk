# Metadata-only priority object probe

This is a bounded **live experiment**, not an application transport mode.
It only accepts the isolated `personal-test/quota-kit/exit.json` profile under
`.skirk-runs`, with a visible test folder and a Google-front route. It creates
at most 27 disposable files (3 methods × 3 sizes × 3 repetitions), stops the
create/discovery phase after five minutes or approximately 25,000 locally
estimated Drive units, then does bounded cleanup. It writes
one private `0600` JSON result. File IDs are pre-generated in one batch.

```sh
go test ./experiments/metadata-only-probe
go build -o .skirk-runs/metadata-only-probe ./experiments/metadata-only-probe
.skirk-runs/metadata-only-probe --live \
  --config .skirk-runs/personal-test/quota-kit/exit.json \
  --out .skirk-runs/metadata-only-probe-unique.json
```

The tool tests 256, 2,048, and 8,192 random sealed-byte stand-ins. It shuffles
size order and rotates the three methods over each size's three repetitions:

- `metadata_only`: one JSON `files.create` with the sealed bytes in
  `description`, no uploaded media.
- `dual_inline`: the current opt-in idea, one multipart create with the same
  ciphertext in both `description` and media.
- `media_multipart`: Skirk's production-style one-request multipart create,
  with the reserved ID, name, and folder metadata plus media bytes, and no
  ciphertext copy in `description`.

For each file it checks returned ID/name, waits for exact-ID list visibility,
checks full decoded description bytes, reads metadata and media behavior, then
deletes that exact ID. A three-second final list sweep deletes only the run's
reserved IDs if any create committed after its immediate cleanup. The private
report records API statuses, measured times, raw request estimates, and a
core-path estimate excluding diagnostic reads. A rejected metadata-only field
(HTTP 400/413) is retained and the other methods continue; other verification
or cleanup failures stop the matrix.

The [Drive files.create reference](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create)
documents the metadata-only endpoint and `drive.file` scope, but describes
`description` only as a *short* field; it does not publish a numeric maximum.
Current 8 KiB description success in a multipart upload does not prove a
metadata-only object will behave the same way. This probe tests the actual
folder/account/route. It does not change user traffic, claim regional results,
or guarantee that a canceled create cannot appear after the final sweep; run
the normal delayed mailbox cleanup afterward.
