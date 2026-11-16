# Mux receive budgets: two slow streams, local reproduction

Status: **deterministic local receiver-path reproduction, no product fix or
Google call**. The fixture lives on the separate
[`codex/mux-receive-budget-20261116` research branch](https://github.com/ShahabSL/Skirk/blob/codex/mux-receive-budget-20261116/internal/skirk/mux_receive_budget_full_test.go)
because it deliberately reproduces a current product failure.
This follows a read-only Claude audit's claim that one stalled 64 MiB local
consumer plus a second slow stream can overflow both 64 MiB receive budgets.
The reset is real, but its immediate cause is more specific.

The fixture uses actual muxv5 sealed Drive objects in `MemoryStore`, parsed
object metadata, the normal receive queue, receive workers, the adaptive
download limiter, AES-GCM decoding, stream reassembly and the local handoff.
`net.Pipe` peers intentionally do not read, so the normal socket writers
block. `GetByID` gates model slow but finite in-flight Drive reads. This is
not a SOCKS/browser benchmark, Drive visibility test, or sender-side
backpressure proof; direct object enqueue bypasses prefix discovery.

| Stage | Observed receiver state |
| --- | --- |
| Prefill | 16 ordinary objects, each three 1 MiB DATA frames, are processed in order. One frame blocks in the local writer, leaving 47 MiB in stream A's primary channel, just below its 48-frame pause mark. |
| Admit | Six further ordinary objects for A and six for B are admitted by the actual normal scheduler. Each object is about 3 MiB, below the 4 MiB sealed batch cap. The auto profile starts with eight physical GET permits; the other admitted requests wait on that limiter. |
| Release A | A reaches the global 64 MiB primary byte cap plus one 1 MiB deferred frame. A remains open. |
| Release B | B's already admitted objects cannot use the full primary pool. Its own 8 MiB deferred tail fills, then a later frame resets B. A remains open. |
| Unrelated flow | A separate small priority DATA followed by FIN is delivered in order through the same receiver. |

The focused test passes under `go test -race` on the unchanged product code.
It reproduces an admitted, legitimate second stream reset under contention.
It **does not** reproduce exhaustion of the global 64 MiB deferred pool:
with only A and B, each has an 8 MiB per-stream deferred cap, so their
combined tail is at most 16 MiB. The fixture checks no more than 9 MiB
global deferred use after B resets. The issue is an admission/ownership gap:
the scheduler bounds in-flight objects per stream at six and 24 MiB, but it
does not reserve local handoff capacity for those objects before their Drive
reads. A global primary pool occupied by A leaves B only its 8 MiB tail.

A safe change would make normal-object admission account for the bytes and
frames already reserved by **other in-flight objects**, the 64 MiB primary
pool, and each stream's remaining deferred capacity. It must avoid holding
shared receive workers on a stalled local socket, keep small priority flows
available, and preserve DATA/FIN order. Merely raising the deferred cap or
reducing the fixed six-object window would shift memory or hurt bulk speed
without proving fairness. No reservation logic was promoted here: it needs a
separate bounded design, lost-response/close-race tests, and a matched mixed
interactive/bulk comparison before changing the product scheduler.
