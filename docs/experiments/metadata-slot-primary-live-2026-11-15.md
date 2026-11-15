# Primary cumulative metadata slot experiment, November 15, 2026

The primary mode keeps a successfully published cumulative snapshot as the
only carrier until processing ACK or explicit writer/link failure. It removes
the asynchronous mode's fixed 1.5-second post-publication immutable fallback.
Both peers retain the same bounded ownership and exact immutable fallback for
unpublished, failed or overflowing work. This setting remains off by default.

The first attempted comparison used the exact clean `a6297f2` source (the same
Go runtime as `2631e61`, with Python comparison-selector additions). Binary
SHA-256 was
`be47879c1325768b6f45b9f8f1bba0a120ae6dd3b4bd62bf612c83f92dce9519`.
Both modes ran that binary through the pinned `www.google.com` front on the
isolated test account. Both used asynchronous slot delivery and interaction
epochs, one exact 16 MiB download, a verified 128 KiB persistent-stream prelude,
and twelve 64-byte echoes pipelined every 250 ms after a one-second bulk lead.
Only the candidate enabled primary mode. The order was planned as
async/primary/primary/async, with equal eight-second tails and a 60-second file
stall allowance. The local API-error guard remained enabled.

The first asynchronous control completed its file, twelve echoes and exit
proof with zero API errors: request p95 **4.008 s**, scheduled p95 **4.009 s**,
file goodput **14.179 Mb/s**, and **22,425** estimated units including warmup
and tail. It logged fourteen asynchronous handoffs and publication ACKs,
fourteen receiver-processed records and nine sender processing ACKs.

The first primary arm stopped on the API-error guard. One exit normal-priority
4 MiB immutable upload waited **30.001 s** before its request deadline. Another
normal upload had waited **31.081 s** for an upload slot; the client had received
about **5 MiB** of the file before the runner canceled the arm. Final telemetry
recorded **19,725 client + 18,700 exit = 38,425** estimated units, zero client
API errors and one exit API error. No complete file or final twelve-message
summary exists for this arm. The runner did not start its remaining two arms.

This is an incomplete comparison. The failure was in the unchanged immutable
bulk path, so it does not isolate a causal primary-slot regression. It also
does not establish a primary-mode win. The candidate did exercise six client
handoffs/publications/processing ACKs and two exit handoffs, one publication,
one processing ACK and one fallback before cancellation.

No builds or tests ran from this experiment worker during timed arms. Other
agents were allowed offline development on the shared Mac, so CPU load was
not fully isolated or recorded. A final winner-versus-default measurement
must arrange shared-host CPU quiet as well as serialized Google API traffic.

Cleanup removed eleven control mux objects and the primary arm's two slot
files. The first immediate primary mux sweep was empty, but the 31-second
delayed sweep found **two late-visible mux objects**. Both exact objects were
deleted. A second delayed sweep, another 31 seconds later, found all four
exact mux/slot prefixes empty; do not describe the first delayed check as
clean. Config copies were
removed and peers stopped. The private summary, traces and cleanup report are
under ignored `.skirk-runs/slot-primary-download16m-20261115-01/`.
