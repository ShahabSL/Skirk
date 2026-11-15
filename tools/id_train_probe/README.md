# Immutable known-ID train probe

This tool tests a specific missing primitive: reserve an ordered vector of IDs
once, then publish each encrypted packet once and poll its known ID. Its control
creates the same metadata objects but reads through a prefix list. The vector is
pre-shared in the test process; the authenticated transmission and renewal of
that vector are **not** measured. It is a Drive substrate test, not a TCP proxy
or a production protocol.

The ID-train reader polls only its next expected ID, immediately reads the next
ID after making progress, and backs off for the configured poll interval after
a precreate 404. The list reader polls its unique prefix at that interval and
buffers later packets until the missing sequence arrives. The writer schedules
packets independently with a bounded maximum of two outstanding creates by
default; a window of one reproduces serialized publication. `--writer-concurrency`
accepts 1–4. `--id-train-catchup=false` reproduces the initial fixed cadence.
Drive may return literal-name false matches for a slash-separated search value;
the probe counts and locally filters those names. Only exact expected IDs,
names and authenticated records can advance delivery, and only the reserved
ID ledger supplies deletion targets.

Run from the repository root:

```sh
go test -race ./tools/id_train_probe
go build -o .skirk-runs/id-train-probe ./tools/id_train_probe
.skirk-runs/id-train-probe --live \
  --config /absolute/repo/.skirk-runs/personal-test/kit/exit.json \
  --out .skirk-runs/id-train-new.json \
  --count 8 --payload 256 --poll-ms 200 --interval-ms 250 \
  --writer-concurrency 2
```

The config must resolve to that isolated test kit suffix and name a visible
Skirk mailbox plus pinned Google IP. The tool forces `google_front_pinned` and
uses independent writer/reader HTTP pools with the same grant. It does not print
the token, generated IDs, plaintext or private config. Each arm reserves fresh
IDs; a private exact-ID recovery ledger is written **before** creating files.
The hard limits are 12 frames per arm, 2,048 payload bytes, 12,000 active units,
18,000 total estimated units, and a four-minute active deadline. One run uses
ID-train/list/list/ID-train order. No retry sends a second create at a new ID.

Packets use Skirk AES-GCM envelopes with a random session/key and unique
sequence numbers across arms. Reads validate exact ID, name, parent, Drive
space, session, direction, sequence and payload bytes. Reordered creation never
changes delivery order. A failed reader cancels and joins every outstanding
writer before cleanup starts. Deterministic race tests cover this cancellation
ordering and a slow first create overtaken by later creates.

Cleanup deletes every reserved ID, checks each ID is missing twice, and performs
immediate plus 31-second delayed exact-prefix sweeps. Any incomplete query,
remaining file, request error, corrupt packet or guard failure fails the run.
Only a completely verified cleanup removes the recovery ledger. Generated IDs
and reports stay ignored under `.skirk-runs/`.

The report includes all scheduled receive times, post-create-response receive
times, create durations, operation counts, expected precreate 404s, request/response
bytes and quota estimates. A negative post-create-response time means a reader
observed a file just before the publisher received its own create response; it
does not mean negative network latency. Scheduled times include sender queue
delay, which is essential when a serialized create takes longer than the offered
packet interval. The p95 of eight samples is their maximum; this small burst
cannot establish population latency, throughput or user capacity.
