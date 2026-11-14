# Bounded upload diagnosis

This is an opt-in live experiment, not a load generator or application default.
It requires an existing private config for an isolated Drive folder. It creates
at most 32 random files, reserves their IDs first, verifies successful uploads
byte for byte, and deletes those exact IDs. Do not run beside an active exit.

```sh
go build -o .skirk-runs/drive-upload-probe ./tools/drive_upload_probe
.skirk-runs/drive-upload-probe --live \
  --config .skirk-runs/personal-test/quota-kit/exit.json \
  --out .skirk-runs/upload-probe-unique.json --hedge-after 3s
```

Without `--hedge-after`, a still-pending upload gets a metadata visibility check
at three seconds. With it, a second HTTP pool sends the identical name, ID and
payload. Either successful leg may win; the other is canceled and joined before
verification and deletion. There are at most two logical upload legs, each with
the client's existing bounded HTTP retry policy. Each round uses four files and
new connection pools. Both stores' estimated quota is counted.

The probe limits uploads to 20 seconds, the run to five minutes, and checks a
20,000-unit guard after each round. It stops after a verification or cleanup
failure, active quota backoff, or elapsed deadline. A failed upload remains in
the results. Output contains no tokens or profile data. `winner_ms` measures
first reported success; `upload_ms` also includes joining the canceled loser.
Earlier saved runs predate the separate winner timer and integrity stop gate.

`WroteRequest` means the local HTTP transport finished writing, not that Google
committed the file. Likewise, a delete returning 404 immediately after a canceled
upload does not rule out a late server commit. After the run, perform a delayed
cleanup and dry-run check for the exact reported prefix in that same isolated
folder. Keep failed runs when comparing results; these short probes do not
establish regional reachability, production capacity, or a statistically proven
latency bound.
