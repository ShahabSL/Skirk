# Exact current v5 versus clean main — pinned-front mixed tunnel

This is a bounded **product-build comparison**, separate from the test-only
Drive v2 priority and Firestore carrier studies. The current executable was
built from root branch source `4a9cc98` after full preflight, SHA-256
`34fabb39de22012d2fa6e8df61f47a58593fd021ecc7e64b841738cfd522621f`.
After the aggregate inbound-memory fixes, a second exact-current executable
was built from source through `ed46a9f`, SHA-256
`45d75a60e7ba9d0af5486f6aa21c8a0b11420e034075e39e04d22e1fe4c30c69`.
The clean `main` executable is from `2c52ff8`, SHA-256
`34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3`.
Both used the same secondary-account kit, pinned `www.google.com` front, one
Mac, one exit, a fresh 128-bit session per arm, and default transport/settings
for their respective binaries. `main` uses muxv4; current defaults to muxv5.
This compares complete builds, not one isolated scheduler setting.

Each arm performed one exactly hashed 16 MiB file transfer in the named
direction and 20 independently verified 64-byte frames on one persistent
SOCKS/TLS echo stream, pipelined every 250 ms beginning one second after bulk
started, plus a warm frame. The table uses the p95 of all **20 scheduled-to-
complete** frames for a fixed denominator; the number whose completion
overlapped the file varied as the file rate changed. File rate is exact file
bits divided by elapsed completion time, decimal Mb/s. Quota values are
**local estimated reservations including warmup/tail**, not provider billing.
Each cohort used local ceilings of 100,000 units/rolling minute, 50,000/arm,
and 200,000/cohort. No successful arm recorded an API error or guard denial.

| Four-arm cohort/order | `main` median text p95 | v5 median text p95 | `main` median file | v5 median file | `main` median units | v5 median units |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Download `current/main/main/current`, 4/4 clean | 6.610 s | 6.057 s (8.4% lower) | 18.774 | 20.884 Mb/s (11.2% higher) | 23,285 | 21,635 (7.1% lower) |
| Upload `main/current/current/main`, 4/4 clean | 9.214 s | 6.469 s (29.8% lower) | 11.955 | 15.264 Mb/s (27.7% higher) | 22,985 | 21,235 (7.6% lower) |
| Upload `current/main/main/current`, 4/4 clean | 6.772 s | 6.806 s (0.5% higher) | 17.558 | 16.175 Mb/s (7.9% lower) | 22,135 | 21,360 (3.5% lower) |
| **Final inbound-cap build**, download `main/current/current/main`, 4/4 clean | 6.785 s | 6.135 s (9.6% lower) | 20.779 | 26.744 Mb/s (28.7% higher) | 24,185 | 24,985 (3.3% higher) |
| **Final inbound-cap build**, upload `current/main/main/current`, 4/4 clean | 8.469 s | 7.628 s (9.9% lower) | 16.363 | 19.465 Mb/s (19.0% higher) | 23,335 | 22,235 (4.7% lower) |

The **first download** cohort in `main/current/current/main` order is
incomplete: its first three arms completed exact file/text checks (main
21.237 Mb/s, 5.559 s p95; v5 28.380/27.345 Mb/s and 7.199/7.109 s p95).
The fourth `main` arm had four 28-second pinned-front multipart-upload
timeouts while the exit was publishing file chunks. The harness stopped on
its API error guard and did not publish a file/text result for that arm. The
failure is retained in the ledger, not averaged into a four-arm comparison.
The finalizer verified the exit and client stopped; its benchmark child PID
identity changed before reap verification, and a subsequent local process
check found that PID absent.

Individual clean-arm file rates ranged from **15.780 to 28.701 Mb/s** in
the completed download arms and **6.467 to 20.161 Mb/s** in the upload arms;
this path was highly variable. The two earlier upload orders gave opposite
signs for file rate and no repeatable p95 benefit. The incomplete first
download cohort also showed v5 **worse** text p95 in its complete pair. The
final inbound-cap build improved both metrics in its two four-arm cohorts,
but these two cohorts do not erase the earlier contradictory results or
isolate the effect of the memory cap. The correct conclusion is that the
current build passes full mixed transfers and sometimes improves both
metrics, but **no stable product speed or latency win against `main` is
established**. These numbers cannot establish a provider/network limit or
a 200,000-user capacity.

The private raw ledgers are `.skirk-runs/v5-main-persistent-{down,up}-20261116-{01,02}/`.
The final inbound-cap ledgers are
`.skirk-runs/v5-inbound-main-{down,up}-20261116-01/`.
Across the six cohorts, **23 of 24** arms completed; each successful arm
verified its file and all 20 frames. After each cohort, a separate exact
session-prefix cleanup removed residual muxv4/muxv5 objects only for its own
four sessions. Every prefix was empty immediately and after 31 seconds.
The cleanup calls are outside the performance arm's local unit column.
No Firestore, Pub/Sub, Apps Script, physical iPhone, or unrelated Drive file
was used.
