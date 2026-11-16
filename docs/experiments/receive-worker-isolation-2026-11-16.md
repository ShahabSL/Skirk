# Isolate stalled local readers from shared Drive receive workers

Status: product receive-path correction, validated locally. No Google call or
live latency measurement was made for this change.

`runReceiveWorker` authenticates a Drive object, then delivers its frames into
each local TCP stream's 64-item inbound channel. If the application's socket
stops reading and that channel fills, `acceptFrame` waited on the channel
while still occupying a shared receive worker. The normal scheduler checks
inbound depth **before** scheduling an object, but one admitted object can
contain up to 512 frames; priority objects had no equivalent precheck. A few
stalled local streams could therefore delay unrelated messages.

The deterministic baseline regression filled two streams' inbound channels,
started two shared priority workers, and then published a third independently
authenticated message. Both workers reached the blocked delivery; the third
message failed its 500 ms local deadline. This was a local worker-starvation
proof, **not** a claim that Google adds exactly 500 ms in a real connection.

The stream's existing writer goroutine now drains a finite ordered deferred
tail after older inbound frames. Receive workers only append; they do not wait
for a local socket. The added tail is capped at 512 frames and 8 MiB per
stream. Normal scheduling pauses while any deferred frame remains. If a local
application stays stalled past that hard cap, only its stream closes and a
bounded best-effort RST is queued. Existing per-stream reassembly and global
normal-receive limits still apply. Mux processing receipts continue to mean
accepted into the bounded local handoff, not confirmed application-level
consumption; that is the same delivery boundary as the original inbound
channel.

The regression then passed 20 race-enabled repetitions. Separate tests passed
ordered channel-plus-deferred delivery, actual local-socket byte order with a
blocked writer, normal-scheduler pause/resume, and per-stream overflow without
closing an unrelated stream. The broader receive/memory transport subset,
`go vet ./...`, and the full `go test ./...` suite passed; the long
`internal/skirk` package took 281.563 s. The final added local-socket test
passed 20 race-enabled repetitions after the full suite. No cross-platform
runtime or new live mixed-workload performance result follows from these
tests; the root branch's combined preflight must still run after integration.
