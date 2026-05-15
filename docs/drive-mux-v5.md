# Drive Mux v5 Protocol Checkpoint

This checkpoint records the next architecture target for Skirk's Google Drive
transport. It exists to prevent future regressions into ad hoc tuning.

## Goal

Skirk must provide generic SOCKS and HTTP proxy transport over Google Drive for
hostile networks that only allow the configured Google route. The target is the
lowest practical latency, highest practical throughput, and stable concurrent
use across normal browsing, bulk downloads, media streams, chat apps, and
multiple clients.

The protocol must not depend on website, hostname, path, content type, or app
specific filters.

## Current v4 Boundary

v4 is stable after the 52f1c8a fixes, but it has an architectural speed ceiling.
Each mux object is both the discovery item and the data container:

- receivers discover data by repeatedly listing a prefix;
- bulk transfers create many large data objects in that listed prefix;
- priority and normal data compete in the same discovery result set;
- Drive list pagination, partial pages, and object count all become part of the
  hot path;
- larger batches improve throughput but worsen latency and reassembly pressure;
- smaller batches improve interactivity but cap bulk throughput with upload,
  list, download, and delete round trips per object.

This means v4 can be made stable, but cannot be both list-light and
throughput-heavy under sustained bulk load.

## Drive Primitives

The design uses only current Drive API primitives:

- `files.create` for immutable object creation.
- `files.get?alt=media` for whole-object download by ID.
- `Range: bytes=start-end` for partial media reads.
- `files.list` on control/ack prefixes as the first control-plane candidate.
- `changes.getStartPageToken` and `changes.list` with
  `spaces=appDataFolder` as an experimental durable cursor candidate.
- `files.delete` by ID for garbage collection.

Important constraints:

- `changes.list` page tokens are stable and produce `newStartPageToken` only at
  the end of the current change stream.
- `changes.list` is filtered by Drive space, not by Skirk object prefix. Data
  objects created in `appDataFolder` still appear in the change stream even
  when receivers ignore their names.
- `files.list` pages can be incomplete, paginated, or token-rejected; it cannot
  be treated as a reliable single-call queue under load.
- Range downloads must require HTTP 206 and validate the returned range before
  trusting the bytes.
- Multipart upload is simple and low latency for small objects. Resumable upload
  is a candidate for larger data segments, but it adds an initiation round trip
  and must be benchmarked before becoming the default.

## v5/v6 Shape

v5/v6 separates the control plane from the data plane.

Data objects carry encrypted payload bytes. Control objects carry encrypted
manifests that point to data object IDs and describe how those bytes map into
streams.

### Object Classes

Control object:

```text
<session>/<dir>/<client>/<run>/c/<epoch>/<seq>.ctrl
```

Data object:

```text
<session>/<dir>/<client>/<run>/d/<epoch>/<lane>/<seq>.data
```

Ack object:

```text
<session>/<dir>/<client>/<run>/a/<epoch>/<seq>.ack
```

Only control and ack names are downloaded through the hot discovery path. Data
objects are fetched by Drive file ID from control records.

Important: with the current `appDataFolder` scope, data object creation still
pollutes the Drive change feed. The protocol must therefore support both
discovery candidates until live gates prove the better default:

- v5a: control-only `files.list` prefixes, keeping slabs out of discovery
  results while retaining app-private scope.
- v5b: `changes.list` cursor, accepting data-change noise and proving the
  change-feed tax with measurements.
- opt-in broader-scope variant: move data objects to another Drive corpus and
  keep appDataFolder control, accepting security and setup tradeoffs.

As of the 2026-05-14 `google_front_pinned` primitive benchmark,
control-prefix `files.list` observed generated-ID control objects in one call at
roughly 200-350 ms, while `changes.list` required seconds for the same objects
because data object creations polluted the change stream. That made v5a the
right architecture to prototype first.

The later 2026-05-14 live mixed gate did not justify changing the default:

- muxv4: 128 MiB bulk at about 47.7 Mbps, TTFB about 4.5 s, small p95 about
  5.9 s.
- muxv5a: about 30.4 Mbps, TTFB about 5.1 s, small p95 about 6.0 s.
- muxv5b: about 44.2 Mbps, TTFB about 5.1 s, small p95 about 7.0 s.

v5a protected discovery but paid an extra data/control upload cost. v5b
recovered much of the bulk throughput but reintroduced data-volume discovery
pressure through a second bulk prefix. Both remain experimental until ACK/credit,
range-readable multi-record slabs, and per-client byte fairness produce a live
win over muxv4.

The 2026-05-14 muxv6a range-slab experiment made `muxv6` a real opt-in
transport with a separate `muxv6/` prefix, control-prefix discovery, and
validated Drive range reads. It proved the safety primitive but did not beat
muxv4 in live tests:

- muxv6 single-record range path: 100 MiB bulk at about 26.5 Mbps, TTFB about
  4.4 s, small request p95 about 5.8 s.
- muxv6 coalesced range slabs: 100 MiB bulk at about 21.4 Mbps, TTFB about
  4.8 s, small request p95 about 5.1 s.
- same-binary muxv4 comparison: 100 MiB bulk at about 32.7 Mbps, TTFB about
  4.5 s, small request p95 about 4.9 s.

The loss came from serial data-then-control uploads and the extra control
discovery step. A coalesced multi-record slab variant was removed from the
runtime path because it was slower and could make retry ownership ambiguous if
the uploader drained extra batches before a transient Drive error. The range
mechanism is useful as a primitive, but it is not a default candidate unless
sender credit and byte-fair scheduling can keep large slabs full while
preventing bulk from delaying interactive streams. muxv4 remains the best
proven default after this checkpoint.

### Control Record

Each control page contains one or more encrypted records:

```text
version
direction
client_id
run_id
epoch
control_seq
records[]
```

Each record contains:

```text
record_type          // open, data, fin, rst, ack, credit
stream_id
priority_class       // control, interactive, burst, bulk
stream_seq_min
stream_seq_max
plain_bytes
sealed_bytes
data_file_id         // empty for pure control records
data_offset
data_length
frame_count
credit_bytes
ack_stream_seq
created_unix_nano
```

The sealed data object remains self-authenticating. The control record only
authorizes which object ID and byte range should be read.

### Data Slab

Large payload objects are immutable slabs. Each slab contains independently
authenticated records, not one whole-object AEAD envelope:

```text
slab_header:
  magic="SKD5", version, direction, client_id, run_id, epoch, lane, slab_seq,
  data_file_id, object_name, record_count

record:
  magic="SKR5", version, direction, lane, slab_seq, record_index,
  priority_class, flags, stream_id, stream_seq_min, stream_seq_max,
  stream_byte_start, plain_len, sealed_len, ciphertext+tag
```

The data AEAD key is derived separately from the control key and includes the
session, direction, client, run, epoch, and `data` purpose. Nonces are
deterministic within an epoch/lane from `(direction, lane, slab_seq,
record_index)`.

The AEAD associated data includes the record header, Drive `data_file_id`, Drive
object name, byte offset, and byte length. This is required for safe range
reads: a `206 Content-Range` proves which bytes Drive returned, while AEAD
proves that the bytes are the exact manifest-authorized record.

## Traffic Classes

Classes are inferred from generic transport behavior:

- `control`: OPEN, FIN, RST, ACK, CREDIT, and transport probes.
- `interactive`: first stream bytes, tiny writes, sparse request/response
  streams, and any stream with low observed bandwidth demand.
- `burst`: medium sequential reads that need latency, such as media segment
  startup and page asset bursts.
- `bulk`: sustained streams whose queue, byte rate, and frame count exceed the
  burst window.

Classification must use only local stream measurements: bytes queued, bytes per
second, time since open, frame count, idle gaps, reassembly backlog, and inbound
writer pressure.

## Scheduling Invariants

1. Control records always bypass bulk data.
2. New streams get a startup allowance before established bulk streams.
3. Bulk can fill unused capacity but must not consume all upload, list/change,
   download, or receive-worker slots.
4. Per-stream receive credit caps bytes in flight, not just object count.
5. Sender must stop producing data records beyond receiver credit.
6. Receiver must be able to fetch the next expected range for a paused stream
   before fetching speculative later ranges.
7. ACK and credit state must be idempotent and monotonic.
8. Cleanup must be watermark-based and safe after restart.

## Data Plane

Small data:

- inline in control pages up to a small threshold when it avoids an extra Drive
  GET and does not bloat the control feed.

Normal data:

- grouped into immutable data objects by lane and stream class;
- target object size starts at 1 MiB and adapts per direction;
- bulk may grow toward 4-16 MiB only when the receiver grants enough byte credit
  and interactive pressure is low;
- burst/interactive data stays smaller to reduce head-of-line delay.

Range reads:

- used when a data object contains multiple stream records and the receiver only
  has credit for part of it;
- used for reassembly hole recovery and expected-sequence-first reads;
- whole-object GET remains preferred when the full object is immediately useful.

Range reads require independently authenticated fragments. Whole-object AEAD is
not enough for arbitrary byte ranges because a valid HTTP `Content-Range` only
proves which bytes Drive returned, not that a partial slice is a valid encrypted
Skirk record.

Generated IDs:

- reserve data file IDs before upload;
- include the ID in the upload metadata and in the sealed record associated
  data;
- on ambiguous upload retry or `409 Conflict`, fetch metadata by that same ID
  and verify name and size;
- never allocate a new file ID for the same stream range.

## Control Plane

Primary discovery candidates:

- v5a: `files.list` on control and ack prefixes only. This keeps data slabs out
  of the discovery result set while staying inside the current `appDataFolder`
  scope, but still inherits list pagination and incomplete-page behavior.
- v5b: `changes.list` on `appDataFolder`. This gives a durable cursor, but the
  change stream includes data slab creations and must be benchmarked under bulk
  load.

If `changes.list` is used:

1. On startup, get or recover the saved start page token for this session side.
2. Poll `changes.list` with `spaces=appDataFolder`.
3. Filter changes to the current session, direction, client, and run.
4. Download only new control/ack objects by file ID.
5. Advance the saved token only when the page is fully drained and
   `newStartPageToken` is present.

Fallback recovery:

- on rejected or missing change token, list the control prefix with
  `files.list`, rebuild the highest control and ack watermarks, then request a
  fresh start token;
- on restart, keep enough lookback to tolerate delayed Drive visibility.

## ACK, Credit, and GC

Receivers emit ack/credit records containing:

- per-stream highest contiguous sequence delivered to the local socket;
- per-stream cumulative byte offset delivered to the local socket;
- per-stream cumulative byte credit granted to the sender;
- highest control sequence processed per peer epoch;
- data file IDs eligible for deletion after all referenced bytes are delivered.

Senders use ACKs to:

- release local pending accounting;
- reduce retransmit/recovery state;
- schedule cleanup of sent data/control objects after both sides have safe
  watermarks.

GC must delete by file ID and tolerate duplicate or already-deleted files.

## Resumable Upload Policy

Multipart upload remains the initial path for control and small data because it
is a single request.

Resumable upload is evaluated for larger bulk data objects only if benchmarks
show that session initiation plus upload beats multipart latency or improves
failure recovery. Google documents resumable upload as the preferred path for
larger files and unreliable links, but the initiation request is still expensive
for control and small interactive data. A resumable implementation must:

- persist upload session state only inside a single object upload attempt;
- query upload status after ambiguous failures;
- never create duplicate data records for the same stream range;
- fall back to multipart below the measured crossover size.

## Compatibility

v4 stays available as the stable fallback until v5 passes live gates.

A new protocol version must be explicit in config or negotiated through an
initial control object. v5 receivers must not parse v4 data objects as control
records.

## Required Observability

Before claiming a speed or stability improvement, logs or metrics must expose:

- per-direction Drive call counts, quota estimates, latency percentiles, and
  error counts by operation;
- control poll latency and drained control records per poll;
- data upload size, upload latency, and object ID publication latency;
- data download size, whole/range mode, latency, and reassembly delay;
- per-class queued bytes, admitted bytes, dropped/retried objects, and credit;
- per-stream first-byte latency, delivered bytes, socket write blocking, and
  close reason;
- cleanup backlog and delete latency.

## Required Test Gates

Unit:

- control record encode/decode/authentication;
- data slab encode/decode/authentication;
- single-record range open rejects wrong manifest metadata and tampered
  ciphertext;
- monotonic ACK and credit handling;
- receiver credit blocks bulk but not control;
- range validation rejects wrong status or wrong `Content-Range`;
- change-token pagination and rejected-token recovery;
- GC watermark idempotency.

Integration:

- raw Drive known-ID whole-object and range throughput matrix;
- raw Drive generated-ID upload and `409 Conflict` idempotency matrix;
- change-feed pollution matrix for appDataFolder slabs under v5b;
- live SOCKS download throughput;
- deterministic synthetic page load with many small assets;
- deterministic synthetic media stream with periodic segment bursts while bulk
  downloads run;
- chat/WebSocket/SSE-style small bidirectional stream during bulk;
- five or more clients sharing one exit;
- restart recovery with orphan data/control objects;
- cleanup under foreground load.

Regression gate:

- v5 must beat v4 on at least one of latency or throughput without regressing
  the other beyond the accepted SLO.
- v5 must not increase stuck-download rate, stream reset rate, or Drive errors.
- If v5 fails these gates, keep v4 as default and record why.

## Rejected Shortcuts

- Domain-specific prioritization. It will optimize the demo and fail general
  applications.
- YouTube as the deterministic gate. Keep it as exploratory smoke only; ads,
  codecs, experiments, login state, and autoplay policy change independently of
  the tunnel.
- Infinite concurrency. It raises Drive latency and creates self-inflicted
  queueing.
- Larger objects only. It can recover bulk throughput but increases startup
  latency and head-of-line blocking.
- Listing faster only. It cannot remove the coupling between data volume and
  discovery work.
- Disabling cleanup. It hides foreground cost but creates quota, storage, and
  recovery problems.
- Assuming browser-reported download speed equals tunnel throughput. Browser
  buffering, socket backpressure, and reassembly gaps must be measured
  separately.

## Implementation Phases

1. Add Drive change-feed and validated range primitives behind interfaces.
2. Add metrics needed to compare v4 and v5 objectively.
3. Implement v5 control records with inline small data and data-by-ID records.
4. Add ACK/credit records and byte-level receive windows.
5. Add adaptive data object sizing and range reads.
6. Add watermark GC.
7. Run the full live gate matrix before changing defaults.
