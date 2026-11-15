# Mixed substrate experiment

`skirk_live_slot_mix.py` compares two ways to move 20 synthetic 64-byte text
frames while a 16 MiB file passes through the default Skirk mux on the same
pinned-front Drive test account:

- **Control:** a previously demoted, persistent SOCKS TCP stream sends text to
  a local echo origin. The origin records one-way arrival; the benchmark also
  verifies every echo and the file body.
- **Slot:** the separate two-file metadata ACK probe sends text through Drive
  PATCH/GET while Skirk carries the file. It verifies every HMAC-tagged frame
  and cumulative ACK. A matching 128 KiB SOCKS prelude runs before the file,
  and its TCP stream remains idle during the text run.

This is a **mixed substrate test**, not an integrated Skirk client mode. The
slot probe performs setup after the file starts; the control starts text 250
ms after its file starts. One-way arrival p95 is the comparable text metric;
the control's echo p95 is additionally reported, not substituted for one-way
latency. A completed slot arm requires the file still to be active when all
text and ACK work finishes. Successful arms require all text and file bytes,
zero origin hash errors, zero Drive API errors, and exact cleanup.

The wrapper defaults to `control,slot`, a 65-second quota cooldown, 60-second
file low-speed window, 60,000 service units per arm, 100,000 observed service
units per 60 seconds, and a 140,000-unit total bound. The slot probe separately
limits itself to less than 10,000 conservative units. The wrapper stops after
any failed arm. The slot tool's private recovery sidecar is left in place if
exact-ID cleanup cannot be verified; the wrapper never broad-deletes Drive
files.

Build the two binaries from this same commit, then validate the kit without
Drive calls:

```sh
go build -o .skirk-runs/slot-mix-skirk ./cmd/skirk
go build -o .skirk-runs/slot-mix-ack-probe ./tools/ack_slot_probe
python3 tools/skirk_live_slot_mix.py \
  --binary .skirk-runs/slot-mix-skirk \
  --slot-binary .skirk-runs/slot-mix-ack-probe \
  --kit-dir .skirk-runs/personal-test/quota-kit \
  --out .skirk-runs/slot-mix-upload-001 \
  --validate-only
```

Only the isolated personal test kit is suitable for live experiments. Run a
single fresh `--out` directory and replace `--validate-only` with `--live`
after reviewing its quota and cleanup gates. Use `--bulk-direction download`
for the separate file-download pair. Never run this simultaneously with other
live Drive tests or against a main account.
