# Exit generation and stream recovery proposal

Status: protocol design for a future opt-in implementation. No current Skirk
binary implements this proposal. The existing muxv4 restart behavior and its
30-second catch-up limit remain as described in
[the architecture frontier](architecture-frontier-2026.md#exit-restart-and-in-flight-streams).

## Failure to solve

The client waits for the Drive upload of `OPEN`, then starts reading local TCP
bytes. That upload acknowledgment is not an exit acknowledgment. On restart,
the exit lists objects modified no earlier than `now - 30 seconds`. An `OPEN`
left by a longer outage is not discovered. Extending the list window can
replay initial request bytes after the old exit sent them to the destination
but crashed before deleting the Drive object.

The safe goal is **bounded detection and stream reset for unanswered or
stalled active streams**, followed by an application-level retry if the
application chooses one. An entirely idle established stream can remain idle
until its next use unless the design adds a periodic liveness probe; that
extra quota cost needs separate measurement. Transparent replay of
arbitrary TCP bytes cannot provide exactly-once external effects: the exit
cannot atomically commit both a destination socket write and a Drive marker.

## Proposed negotiated protocol

1. A v5 exit generates an unpredictable 128-bit `exit_generation` before it
   starts receiving. The process keeps that generation until it stops. A
   deployment must enforce one active exit per mailbox; two live exits with
   different generations are not supported by this design.
2. A v5 client obtains the generation with an authenticated `HELLO` request
   and response, using the existing client/run response namespace. It may
   prewarm this exchange when Connect begins. It must not send a v5 `OPEN`
   until a generation is known. A cached generation may be tried, but an exit
   with a different generation rejects it before dialing or writing bytes.
3. Every v5 `OPEN` carries that generation under authenticated encryption.
   The exit sends `STALE_GENERATION` with its current generation for a
   mismatch, and never dials the destination for that `OPEN`. The client
   resets the local stream. It does **not** replay a stream that might have
   been accepted by the old generation.
4. On an uploaded `OPEN` without a reply after a configurable deadline, or on
   an established stream with pending outbound data and no forward progress,
   the client sends a fresh, authenticated
   `STATUS(stream_id, generation)` control object. The exit answers `OPENING`,
   `OPEN`, `CLOSED`, or
   `STALE_GENERATION`. This probe has no destination side effect. If no
   authenticated answer arrives after bounded retries, the client resets the
   local stream. A healthy, quiet, one-way connection can answer `OPEN` and
   remain open; silence alone is not treated as exit death. An `OPEN` status
   does not prove target delivery and cannot override an application's own
   timeout.
5. The existing Drive-upload acknowledgment, `OPEN` acceptance, and remote
   socket delivery remain distinct states in logs and UI. `OPEN` means the
   current process owns the stream in memory, not that a request reached its
   destination or will survive a crash. Do not expose it as a durable ACK.
6. Old peers continue on muxv4. Enable v5 only through an explicit
   peer-compatible profile/configuration or capability negotiation that has
   been proven before its first `OPEN`. A new client must not assume an old
   exit supports `HELLO` or close healthy old-peer streams for lack of v5
   replies.

The exit can keep the current 30-second discovery bound. A `STATUS` probe is
a new, recent control object and reaches the new exit even when the original
`OPEN` fell outside that bound. The old `OPEN` remains subject to exact-object
cleanup; a stale-generation `OPEN` encountered inside the catch-up window is
rejected and cleaned without dialing. Do not widen the startup scan to make
the protocol work.

## Crash and compatibility cases to test

| Point of failure | Required v5 result |
| --- | --- |
| Before `OPEN` upload | No exit action; local send fails or retries normally. |
| After upload, before the exit sees `OPEN` | The next generation rejects the old `OPEN`; a later `STATUS` resets the waiting client stream. |
| During destination dial | New generation rejects stale `OPEN`; old socket is closed by process death. Client resets after `STATUS` or timeout. |
| After destination accepted initial bytes, before reply or cleanup | Never replay old `OPEN` in the new generation. Client resets; the application decides whether repeating its request is safe. |
| Duplicate `OPEN` in one live generation | One dial and one initial write; duplicate object is cleaned. |
| Idle but healthy long-lived stream | `STATUS` reports `OPEN`; no idle-only reset. |
| Old client / old exit / mixed versions | Exact muxv4 behavior until both sides explicitly negotiate v5. |
| Delayed old `HELLO` or `STATUS` response | Authenticate and bind to client/run, request nonce, and generation; never let a stale response roll the cached generation backward. |
| Two simultaneous exits on one mailbox | Reject deployment or introduce a separately proven fencing lease. A random generation alone cannot fence the older live process. |

Use an authenticated request nonce and bounded replay cache for each control
exchange. Parse and length-check all new control frames before state changes.
Scope the generation to the mailbox session; never place the shared secret or
raw profile in object names or logs. A `STATUS` response must be authoritative
only for the responding exit process, and an error path must not convert it
into a claim that destination bytes were delivered.

## Performance and rollout measurements

Normal streams need no per-stream acknowledgment object: `HELLO` is amortized
across a client connection, and `STATUS` is sent only when a stream is
unanswered. This avoids adding a Drive write to every interactive message.
The first uncached connection can still pay an extra Drive round trip, so
measure both cold and warm request latency while bulk transfer is active.
Measure timeout false positives under the observed 16–30-second Drive tails,
the number of probe calls and quota units, cleanup, and behavior through
forced exit death at every table row above. A successful protocol simulation
is insufficient for default promotion; run mixed browser/file workloads and
multiple clients on the actual Google-fronted route, then compare against
clean muxv4 with exact hashes and cleanup checks.

If a future design instead requires seamless TCP resumption, it must define
durable sequence ownership and destination-side semantics. That is a separate
protocol, not an extension of the current in-memory slot ACK.
