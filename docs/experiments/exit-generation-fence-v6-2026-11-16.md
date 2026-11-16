# Opt-in muxv6 exit generation fence

Status: isolated local prototype. The shipped `main` and current branch default
remain unchanged (`muxv4` and `muxv5`, respectively). A bounded pinned-front
comparison and one crash observation were completed, but a later local audit
found a same-generation replay gap in the measured v6 source. The gap is
addressed only in the isolated branch and has no live validation. Do not
promote `muxv6` without another correctness gate, a complete liveness design, and a
rollout proof for every client platform.

## Reproduced failure

`TestMuxV5ExitRestartReplayReproduction` uses the actual mux encoder, fake
Drive listing, receive worker, and a destination `net.Pipe`. The first exit
writes the initial bytes of an authenticated `OPEN`; its Drive object survives
deferred cleanup. A second exit with a new sender epoch and empty dedupe map
scans the 30-second startup window and writes the same bytes again. The bytes
in the test represent a non-idempotent HTTP POST. A later physical retry can
also give the same logical OPEN a newer Drive modified time, so a timestamp
filter alone cannot prove safety.

## Fenced state machine

1. Each `muxv6` exit creates a fresh random 128-bit generation before its
   receiver starts. Its object namespace and AEAD lane keys are separate from
   `muxv4` and `muxv5`.
2. A client sends an AEAD-authenticated `HELLO` with a random 128-bit request
   nonce in its own client/run namespace. It waits for a matching, authenticated
   `HELLO_ACK` containing the exit generation. The receive loop stays awake
   during this exchange. A client prewarms it on startup and resends the same
   nonce in a fresh object every 20 seconds until a reply or the 90-second
   deadline. An old reply cannot replace a later cached generation because
   the nonce must match the one pending exchange.
3. The client puts the learned generation inside every AEAD-sealed `OPEN`.
   The exit parses and compares it **before** claiming or dialing the stream.
   An invalid or stale OPEN queues a best-effort RST; it cannot reach the
   destination. When a stale-generation RST arrives, the client closes the
   local stream and clears only a matching cached generation. An ordinary
   destination RST retains the cached generation. After 60 seconds from its
   HELLO_ACK, a cached generation expires; the next OPEN waits for a fresh
   nonce-matched HELLO_ACK. Concurrent OPENs share that one exchange. A
   subsequent stream can handshake with the new exit. No
   already-uploaded OPEN or TCP byte is transparently replayed.

4. Within one exit generation, a separate OPEN ledger reserves the
   `(transport, session, client, run, stream)` key and its authenticated OPEN
   payload digest before dialing. It never evicts entries. The same key with
   any digest cannot dial again, even after the short closed-stream tombstone
   or object-seen cache expires or resets. At 100,000 unique OPENs the exit
   fails closed and must restart with a fresh generation.

The core safety invariant is: an exit process can dial only an OPEN bound to
its own generation, and only after its generation-lifetime ledger reserves
that logical stream key. A post-crash process has a distinct generation, so
even a physically re-uploaded old OPEN remains rejected. This
assumes unpredictable independent generations and the established one-active-
exit deployment rule. Two simultaneous exits on different hosts are not
coordinated by this prototype; the CLI's existing process lock only covers
one host.

The fence deliberately chooses stream reset after an uncertain crash. It
cannot establish exactly-once destination effects: the exit cannot atomically
commit both a TCP write and a Drive acknowledgment. Applications decide
whether a new request is safe. The current prototype has no `STATUS` probe,
so an unanswered OPEN outside the catch-up window or a quiet established
stream can remain open until the application or socket closes it. The existing
best-effort RST can also be lost under queue/upload failure. The lazy 60-second
cache expiry lets a later new stream recover, but it does not close an already
open quiet stream. A bounded authenticated status/liveness exchange remains
necessary before default promotion.

The first v6 prototype did **not** have the generation-lifetime OPEN ledger.
Its two-minute closed-stream tombstones and 100,000-entry reset, together with
the 200,000-entry seen-object reset, allowed a same-generation re-upload to
dial twice. A copied ciphertext under a new valid `.bN` object-name suffix
also bypassed name-keyed `seen`, because that suffix is outside the AEAD AAD.
A deterministic fake-Drive test went red on the pre-ledger source for the
alias-renamed physical copy after tombstone expiry: the same POST bytes reached
the destination twice. The four local cases (expired tombstone, `.bN` alias,
closed-table reset, and seen-table reset) then went green with the
generation-lifetime ledger. The live observations below used the earlier
source and do not validate this correction.

### Same-host v5 journal option

A local append-only journal could reduce v5 replay on one exit host without
adding a Drive request. Before the first destination dial, the exit would
write the authenticated session/client/run/stream claim, fsync the record,
and fail closed on any write, fsync, checksum, or capacity error. Initial
creation also needs an atomic file install and parent-directory fsync. A crash
between fsync and the TCP write would discard a request that never took
effect; this is the unavoidable at-most-once tradeoff. The extra cold path is
local disk latency, whose tail still needs measurement.

This cannot safely be enabled for an already-active v5 mailbox: an OPEN left
by a pre-journal exit has no durable claim and could be replayed on the first
journaled start. Losing or recreating the journal creates the same gap. A
second host does not share the claims; a shared transactional store or
single-writer lease would add coordination and failure modes. Since old v5
ciphertext can be physically re-uploaded at any future time, age-based
garbage collection is unsafe. Claims must live for the whole session or be
retired behind a cryptographic namespace/epoch barrier. A full journal must
fail closed. These constraints make it a narrowly deployable mitigation, not
a replacement for the v6 generation fence; no production v5 journal was
implemented in this prototype.

## Request cost and latency path

These are **Skirk's local estimated units**, using 50 per Drive create or
delete, 100 per list, and 200 per media download. They are per successful
HELLO exchange, not measured provider billing or live latency.

| Exchange | Additional Drive calls | Estimated units |
| --- | ---: | ---: |
| Two control objects: HELLO and HELLO_ACK, each created, downloaded, and later deleted | 6 | 600 |
| One discovery list per direction, if additional rather than shared with active polling | 2 | 200 |
| Minimum healthy exchange without inline metadata reads and two additional lists | 8 | 800 |
| Minimum healthy exchange with both sealed objects served inline and two additional lists | 6 | 400 |
| One delayed idle-client cleanup list, if no application stream supersedes it | 1 | 100 |

The first uncached OPEN waits for client upload, exit discovery/download,
exit reply upload, and client discovery/download **before** its own OPEN
upload. Cleanup is deferred off that critical path. Prewarming may hide the
exchange from the first application request, but a truly cold connection adds
one full Drive control round trip. Warm streams add no control object per
OPEN. The table is a structural floor, **not an upper bound**: the client
polls its reply prefix for the entire exchange, and each extra list is another
100 estimated units. At the default one-second polling interval, an unanswered
90-second handshake can request roughly 90 client lists (about 9,000 estimated
units), plus up to five 50-unit HELLO uploads and later cleanup. The exit's
ordinary polling continues separately. A stale OPEN causes an extra RST
object and a new HELLO for a later stream. An idle client performs one delayed
list 35 seconds after the exchange to remove late ACK copies. Measure the
actual call counts and false timeouts before considering default rollout.

### Bounded client cache refresh

The isolated source expires a cached generation 60 seconds after its last
authenticated HELLO_ACK. The next OPEN blocks for a fresh nonce-matched ACK;
concurrent waiters share a single exchange. An expired cache never authorizes
another OPEN, and a previous OPEN is never rewritten. A fake-Drive restart test
suppresses the stale-generation RST and shows a later new stream learning the
new exit generation without restarting the client. A second test checks that
12 concurrent callers upload one additional HELLO, that a fresh cache uploads
none, and that both exchanges' control objects can be reclaimed.

On a continuously opening client, the 60-second expiry can add at least one
successful exchange per minute: roughly 800 local estimated units per minute
at the structural floor, plus polling, late cleanup, and any failed exchange.
An idle client does no periodic refresh after the startup prewarm, so its next
OPEN can incur a full control round trip. If the exit is absent, each
90-second attempt can cost roughly 9,000 list units before timing out. A
background prewarm could hide some expired-cache latency but would need an
activity gate and backoff to avoid quota burn while idle; none is implemented.
There is still no STATUS exchange, so a quiet established stream can remain
stuck after an unobserved exit restart.

### One-way beacon option

An authenticated exit-published generation beacon could be created once per
exit process and discovered by all clients. It would replace the per-client
HELLO/ACK pair with one exit upload and one client discovery/download, saving
roughly one control transit on a cold first OPEN and amortizing its upload
across clients. A sealed beacon served inline in the list could avoid the
200-unit media GET. The challenge is authority: an old beacon can survive an
exit restart or be physically re-uploaded later. Selecting the newest
`modifiedTime` cannot prove which exit currently owns the mailbox. The OPEN
fence would still prevent a duplicate destination effect, but cold requests
could fail repeatedly against stale beacons. A fixed known-ID, authenticated
current-generation record might need only a 5-unit metadata GET per client,
but requires setup-time ID distribution and a proven single-writer update
rule across restart and Drive visibility races. No such claim is made here.

## Compatibility and local evidence

`muxv6` is explicit opt-in. Both client and exit must select it. The three
transports use different object prefixes; v6 also uses a distinct AEAD key
domain. A v5 peer cannot process v6 control or OPEN objects, and v6 does not
read v5 OPENs. Mixed versions fail without a destination side effect rather
than silently falling back. The v5 replay remains present until clients and
exits migrate together.

Local tests cover the v5 duplicate, a cold OPEN before the exit starts,
two simultaneous client namespaces, a HELLO whose first copy falls outside
the 30-second exit catch-up, malformed/mixed-version OPENs, stale reply nonce
handling, a retained OPEN after restart, a later physical re-upload, and a
fresh OPEN after the client learns the new generation. The newer local tests
also cover the `.bN` alias replay, failed-closed ledger capacity, a lost
restart RST followed by cache expiry, concurrent refresh cost and control
cleanup, and no OPEN when an expired cache cannot get a fresh ACK. `go test -race
./internal/skirk -run 'TestMuxV6|TestMuxV5ExitRestartReplayReproduction' -count=2`
passed with the final source. `make preflight` also passed on the final source,
including the full Go suite, Linux/386 build check, and `go vet`. These tests used fake
Drive; the bounded pinned-front observations below used the earlier v6 source.
No Android, iOS, or desktop app exercised this v6 branch.

Any later live gate should use one isolated mailbox, both peers on the pinned
`www.google.com` front, and a same-binary v5/v6 ABBA workload. Record cold and
prewarmed first-byte latency, mixed interactive/bulk p95, both-peer estimated
units, HELLO/ACK object counts, cleanup after 31 seconds, and safe resets at
each crash boundary. Stop on any duplicate origin side effect or unknown
owner process. Keep the transport opt-in until that evidence exists.

Archived command for the earlier **performance** gate; do not rerun before
coordination and confirming
the private kit selects `google_front_pinned` and no exit owns its session:

```bash
mkdir -p .skirk-runs/exit-generation-v6/bin
go build -o .skirk-runs/exit-generation-v6/bin/skirk ./cmd/skirk
python3 tools/skirk_live_ab.py \
  --binary .skirk-runs/exit-generation-v6/bin/skirk \
  --control-binary .skirk-runs/exit-generation-v6/bin/skirk \
  --control-transport muxv5 --candidate-transport muxv6 \
  --config /Users/shahabsl/dev/hobby/skirk/repocomp/.skirk-runs/personal-test/quota-kit/exit.json \
  --out .skirk-runs/exit-generation-v6/abba-down \
  --live --observe --probe-exit \
  --order control,candidate,candidate,control \
  --bulk-mib 16 --bulk-direction download \
  --small-traffic persistent --small-count 20 \
  --small-interval-ms 250 --persistent-pipeline \
  --persistent-bulk-lead-ms 1000 \
  --max-units-per-minute 100000 --max-arm-units 50000 \
  --max-total-units 200000
```

The command starts and stops its own peers and reports exact payload checks.
It does **not** inject an exit crash or establish first-request latency by
itself. The earlier flawed-source ABBA ran once, as reported below; the
corrected ledger source has not been rerun live.

### Archived live crash/re-upload gate

Read-only config inspection on 2026-11-16 found that the active secondary
`personal-test/quota-kit/client.json` and `exit.json` share a nonempty session
ID and secret, Drive folder and quota project, auth, and pinned-front route.
The experiment copies those **role-specific** files into a private run
directory and assigns a fresh random session, client ID, and run ID.

```bash
umask 077
mkdir -p .skirk-runs/exit-generation-v6/bin
chmod 700 .skirk-runs/exit-generation-v6
go build -o .skirk-runs/exit-generation-v6/bin/skirk ./cmd/skirk
go build -o .skirk-runs/exit-generation-v6/bin/exit-generation-probe ./tools/exit_generation_live_probe
python3 tools/skirk_exit_generation_live.py \
  --live \
  --binary .skirk-runs/exit-generation-v6/bin/skirk \
  --probe-binary .skirk-runs/exit-generation-v6/bin/exit-generation-probe \
  --client-config /Users/shahabsl/dev/hobby/skirk/repocomp/.skirk-runs/personal-test/quota-kit/client.json \
  --exit-config /Users/shahabsl/dev/hobby/skirk/repocomp/.skirk-runs/personal-test/quota-kit/exit.json \
  --output-root .skirk-runs/exit-generation-v6
```

Run this only after the current mailbox campaign and its cleanup finish.
The helper rejects a missing `--live`, nonprivate configs, non-v6 transport,
nonpinned route, or a session mismatch. It authenticates the retained OPEN
ciphertext and checks its exact initial marker before re-uploading the same
sealed bytes under a new physical Drive ID. It verifies each restarted exit
logged rejection of that stream and the local origin saw no second connection
or marker. A fresh stream must reach the origin after the first restart.
Only original Popen children with captured native identities are eligible for
the injected hard stop. The finalizer verifies all registered writers stopped
before deleting the exact `muxv6/<fresh-session>/` prefix, then counts it
immediately and after 31 seconds. Failures leave private logs and a final
result for review; unknown owner state suppresses Drive deletion. This is a
single controlled observation, not proof across every provider race.

### One pinned-front crash observation

The first live harness invocation stopped before any Google call because its
output directory was not private. Its first actual workload then delivered
the marker once but could not locate the retained OPEN: `serve-client` rotates
the configured run ID on startup, while the locator queried the configured
run namespace. All owned processes stopped. An exact-session second cleanup
removed one late-visible 83-byte object, and the immediate and later-than-31-
second exact-prefix scans were empty. The harness now reads the runtime run
ID from the exact client startup log, writes a separate private probe config,
and performs bounded exact-session cleanup rounds.

With that offline-tested correction, **one** pinned-front muxv6 crash gate
completed on the secondary quota kit. The first exit delivered one 40-byte
initial marker to the local TCP origin. After a native-identity-verified hard
stop, the second exit logged stale-generation rejection for the retained
stream and the origin remained at one accepted connection. A fresh stream
reached the origin exactly once. The original authenticated OPEN ciphertext
was then re-uploaded under a different physical Drive ID with the same hash.
After another verified hard stop, the third exit logged stale-generation
rejection for that stream; the origin remained at two accepted connections
and both markers had count one. The finalizer verified every owned writer and
process stopped. Exact-session cleanup took two rounds (intermediate counts
three, then zero), and the 31-second recheck was zero. No project live process
remained after the run. The local metric files recorded about 11,300 estimated
units including helper calls; very short-lived exit metrics can miss calls
before their five-second sample, so this is not an exact quota count or a
latency result. The observation supports this crash/re-upload scenario, not
general provider visibility or concurrent-exit safety.

### Exploratory pinned-front ABBA, not a product ranking

The same-binary `muxv5, muxv6, muxv6, muxv5` 16 MiB download and 20
pipelined persistent-text comparison finished before the same-generation
replay gap was reported. All four arms verified the exact file hash and all
20 text echoes, with no recorded Drive API errors and verified owner teardown.
The two-arm medians were:

| Metric | muxv5 control | measured muxv6 |
| --- | ---: | ---: |
| Scheduled text first-byte p95 | 6.886 s | 7.174 s |
| File goodput | 25.460 Mb/s | 21.011 Mb/s |
| Local estimated units including warm-up/tail | 23,960 | 24,260 |

The result is **not** evidence to promote or rank v6: the measured candidate
still had the same-generation replay hole, only two arms per condition were
run, and the harness did not separately measure a truly cold versus
prewarmed first OPEN. It used a common warm-up before the measured workload.
Each arm had a fresh session. All four exact `muxv5/<session>/` or
`muxv6/<session>/` namespaces passed a bounded delete and immediate empty
scan, followed by an empty scan after 31 seconds. The private
`abba-down/exact-cleanup.json` records each prefix and count. No project live
process remained. No other Google calls are planned for this branch before
the corrected ledger and liveness behavior receive another review.
