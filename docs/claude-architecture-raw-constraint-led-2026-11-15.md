# Raw constraint-led Fable 5.2 architecture answer — November 15, 2026

This is an unedited generated design opinion, not measured performance or a
validated protocol. Returned metadata records **only `claude-fable-5-2`**, one
turn, no web searches, and no permission denials. The prompt withheld current
implementation and historical results. However, the model disclosed that the
session harness attached the repository handbook, which names the incumbent
transport and old lessons. This is **constraint-led, not perfectly blinded**.
No code or live API calls were requested.

**Read the fact checks first:** [review and corrections](claude-architecture-review-2026-11-15.md).
The design contains unverified assumptions about metadata consistency, leases,
folder deletion, comments, token endpoints, quotas, and ID lifecycle. Its
numerical examples are estimates and proposed knobs, not benchmark results.

---

**Model.** I believe I am Claude Fable 5.2 (`claude-fable-5-2`). That comes from my session configuration, so your metadata check is the authority.

**Independence disclosure.** The session harness auto-attached this repository's agent handbook, which names the incumbent transport and lists lessons from earlier experiments. I used no tools and read no code. Everything below is derived from your problem statement and my recollection of the public Drive API. Because I have seen that summary, discount my handling of list-based baselines accordingly.

**Conventions.**
- Units are yours: metadata GET 5, create/update/delete 50, list 100, media GET 200.
- `[V]` marks a Drive behaviour or cap that I recall but your supplied information does not establish. Each needs official-doc verification or a probe.
- Calls your table does not price are assumed equal to the nearest class, and are also `[V]`: 404 and 304 responses, batch sub-calls, comments, revisions, changes, `generateIds`, `watch`.
- `w` = writes/min, `p` = reader polls/min including empty ones, `k` = objects/min, `S` = MiB per object.
- Nothing below is a measurement. Numeric parameters are starting values to tune.

## Bottom line

- **Top design:** small and control traffic rides in *registers*, which are mutable metadata-only files at known IDs that the reader polls with 5-unit GETs. Bulk rides in immutable media objects created at pre-announced IDs (an *ID tape*). Nothing on the hot path lists.
- **Make-or-break:** a GET by file ID must reflect a just-acknowledged write promptly from both vantage points (probe 1). If it does not, fall back to the list family (#7–#10).
- **Floor:** one write plus one read per direction (55 units for a small message), four calls per request/response, zero calls when no stream is open.
- **Scale:** 1M units/min over 200,000 users is 5 units per user per minute, which is one metadata GET. No protocol fits that; it needs granted quota or per-user projects (section 2.9).

Three facts drive the ranking:

1. **Reads are priced 5 / 100 / 200.** An empty poll by known ID costs a twentieth of an empty list. A payload read from metadata costs a fortieth of a media read.
2. **The project budget binds, not the per-user one.** One user may take 325k of the 1M, so units per message and idle cost matter more than peak speed.
3. **Drive shows nothing before a write commits, and nothing can push to a client that reaches only `www.googleapis.com`.** Each direction therefore costs at least one committed write and one read.

## 1. Twenty candidates, ranked

**Shared machinery, unless a candidate says otherwise:**
- **Frames:** each carries (stream, byte offset, length, type). The receiver reassembles by offset, so duplicates are dropped deterministically.
- **ACK and flow:** the reader sends a cumulative ACK, plus a selective bitmap for objects, on its next reverse write. Credit windows apply per stream and per session, and a local socket is not read while its queue is full.
- **Retries:** creates use pre-generated IDs, so a retry reuses the ID and a 409 means the first attempt landed `[V]`. Deletes treat 404 as success.
- **Crash cleanup:** every artifact sits in a per-device folder under an isolated root. A janitor authenticates sealed names, deletes by exact ID, and re-checks later.
- **Network:** only `www.googleapis.com` paths `/drive/v3`, `/upload/drive/v3`, `/batch/drive/v3`, with token refresh at `/oauth2/v4/token` `[V]`, all on the user-selected route.

**1. Register bank — mutable known-ID metadata files holding a cumulative snapshot** (small traffic, ACKs, control)
- **Calls:** once per session, `files.create` (metadata only) per register. To send, `files.update` (PATCH, metadata only) writes a sealed snapshot into `appProperties`. The snapshot holds every frame the peer has not acknowledged, plus ACK and credit fields. To receive, `files.get?fields=appProperties` on the known ID.
- **Discovery and ownership:** none needed, because IDs are exchanged in the handshake. One writer per register.
- **Critical path:** one-way 2 calls (PATCH, GET); request/response 4.
- **Caps:** `appProperties` is 124 bytes per key+value, 30 per app, 100 per file `[V]`. That gives about 2.7 KB sealed per file after base64url, or about 5.5 KB with public `properties` `[V]`. `description` and `name` caps are unknown `[V]`. Several files ("pages") multiply capacity.
- **Units/min:** `50·w + 5·p`, with no media GET and no delete. Open-but-idle at one poll per T seconds costs `300/T`. No open streams costs 0.
- **ACK and retry:** the snapshot is state, not a message, so a retry rewrites the newest state. A stale PATCH landing late can roll the file back. The reader therefore accepts only a strictly higher write counter, and the writer re-asserts when no ACK arrives. Overflow spills to #2.
- **Crash cleanup:** a fixed, enumerated set of files. Nothing accumulates, provided metadata updates create no revisions `[V]`.
- **Restricted/mobile:** tiny bodies and cheap polls. It depends on prompt read-after-write by ID (probe 1) and on Drive tolerating sustained PATCHes to one file (probe 2).

**2. ID-tape media objects — immutable objects at pre-announced IDs** (bulk)
- **Calls:** `files.generateIds` (up to 1000). The writer announces the ordered ID list inside the encrypted stream before use. Object n is `files.create?uploadType=multipart` with `id = tape[n]`, after which the writer marks n committed in its register. The reader calls `files.get(tape[n])?alt=media`, using `Range` to resume. The exit calls `files.delete` after the ACK.
- **Discovery and ownership:** known ID plus the register's commit mark. The writer creates. The exit deletes in both directions, so mobile clients issue no deletes.
- **Critical path:** one-way 2 calls, with the commit mark riding a register write; request/response 4.
- **Caps:** multipart is advised up to 5 MB `[V]`; larger objects use #4.
- **Units:** 300 per object (50 + 200 + 50), so `300/S` per MiB. To hold a budget of U units/min at θ MiB/min, `S ≥ 300·θ/U`.
- **Queues:** at most W objects beyond the cumulative cursor, plus a session cap on unacknowledged bytes, which bounds Drive storage.
- **Crash cleanup:** IDs are announced before use, so the reader can enumerate every possible object. Deleting the device folder catches strays.
- **Restricted/mobile:** bodies are streamed record by record, so memory is one record per lane.

**3. ID-tape micro-objects — payload in the metadata of a create** (substitute for #1)
- **Calls:** `files.create` metadata only at `tape[n]` with payload in `appProperties`. The reader calls `files.get(tape[n])?fields=appProperties`, which returns 404 until the object exists. Delete later.
- **Critical path:** 2 calls one-way; 4 request/response.
- **Units:** per message `50 + 5·(1 + misses) + 50`. Idle polls are 404s at 5 each, if 404s are billed like hits `[V]`.
- **Versus #1:** immutable, so no rollback and unlimited pipelining. Against that, it produces a steady 404 stream when idle, file churn, a delete per message, and polling a window of W IDs costs `5·W` per tick.
- **Verdict:** use if PATCH proves slow or rate-limited per file.

**4. Streamed large objects with range-progressive reads** (bulk upload mode)
- **Calls:** `POST /upload/drive/v3/files?uploadType=resumable` with the tape ID, then `PUT` chunks in 256 KiB multiples `[V]` as bytes arrive. Resume uses `PUT` with `Content-Range: bytes */N`. The reader does ranged `alt=media` GETs and decrypts records as they arrive.
- **Critical path:** at least 3 calls one-way.
- **Units:** lowest per byte: `(50 + 50·chunks + 200·ranges + 50)/S`, if chunk PUTs are billed as writes `[V]`.
- **Limit:** nothing is readable until the last chunk commits, so first-byte delay equals object fill time. Size objects by fill time: tens of MiB for exit-to-Drive, 1–2 seconds of uplink for a client.
- **Crash cleanup:** no file exists before commit; abandoned sessions expire server-side `[V]`.
- **Verdict:** the upload mode for #2 above about 4 MiB or on flaky links, not a separate scheme.

**5. Exit push-wake via `files.watch` / `changes.watch`** (discovery accelerator, exit only)
- **Calls:** the exit registers a webhook channel on the bell or uplink registers and renews before expiry (about 1 day for files, 1 week for changes `[V]`). Each notification triggers a `files.get`.
- **Critical path:** uplink is PATCH, then notification delay (unknown and not guaranteed), then GET.
- **Units:** exit empty polls drop to about 0; 50 per renewal; 5 per notification.
- **Needs:** a public HTTPS endpoint with a valid certificate at the exit, and possibly domain verification `[V]`. Clients cannot receive webhooks.
- **Verdict:** optional. It removes the largest idle term at scale, and a slow safety poll stays.

**6. Epoch folders — containers as cumulative cleanup**
- **Calls:** the writer creates folder E at a known ID and creates objects with `parents=[E]`. When all are acknowledged, one `files.delete(E)` removes the descendants `[V]`. Recovery discovery is `files.list` with `'E' in parents`.
- **Units:** cleanup falls from 50 per object to `50/objects-per-epoch`.
- **Variant, delete-as-ACK:** the reader deletes each consumed object and the writer learns of it from a 404. This merges ACK and cleanup but costs one write per object on the reader, so it loses to cumulative ACKs.
- **Risk:** folder deletion may be asynchronous, and a create may race the delete (probe 3).

**7. Prefix or parent listing of media objects** (baseline and fallback)
- **Calls:** `files.create` multipart with a sealed name carrying lane and sequence. The reader calls `files.list` with `'F' in parents and name contains 'prefix'`, then `alt=media`, then `files.delete`.
- **Critical path:** one-way 3 calls; request/response 6.
- **Units/min:** `100·p + 300·k`. Empty polls dominate: one list per second is 6,000/min while merely idle.
- **Caps:** `contains` on name is prefix-only and listing is index-backed and eventually consistent `[V]`.
- **Crash cleanup:** the listing is its own inventory, which is the strongest recovery story of any candidate.
- **Verdict:** needs no ID announcement and no unverified read-after-write property. It is the fallback if probe 1 fails.

**8. Listing with inline payload — data in name, description or `appProperties` of zero-byte files**
- **Calls:** `files.create` metadata only. The reader calls `files.list` with `fields=files(id,name,description,appProperties)`. Delete after.
- **Critical path:** 2 calls one-way; one list returns many messages.
- **Units/min:** `100·p + 100·m` for m messages/min.
- **Actual advantage:** it removes the media GET (one round, 200 units) per small message. It saves nothing on writes and nothing for bulk.
- **Name versus other fields:** the name's only unique property is server-side prefix filtering and ordering. Put routing in the name and payload in the description.

**9. `changes.list` cursor with inline payload**
- **Calls:** `changes.getStartPageToken` once, then `changes.list` with `fields=changes(fileId,removed,file(name,appProperties))`.
- **Discovery:** one incremental cursor over the whole mailbox, so one exit poll covers all devices.
- **Units/min:** `100·p` plus writes.
- **Trade-off:** responses contain only new entries even if deletes lag. It is not filterable per client, so each of 10 devices pays for all devices' entries and every delete. Feed latency is unknown `[V]`.
- **Verdict:** the best list-family choice for the exit, and poor for clients.

**10. Synthetic `modifiedTime` sequence cursor**
- **Calls:** the writer sets `modifiedTime` on create to a fixed epoch plus the sequence number in milliseconds (the field is writable `[V]`). The reader lists with `modifiedTime > cursor`, ordered by `modifiedTime`.
- **Advantage:** a server-side "newer than cursor" filter that is immune to wall-clock skew and to delete lag.
- **Limits:** still 100 per poll and still index-backed. An item that becomes visible late behind the cursor needs an overlap re-list. Precision and clamping are unknown `[V]`.
- **Verdict:** a refinement of #7 and #8, not a new cost class.

**11. Mutable media slot, version-polled**
- **Calls:** `files.update` with media on a known ID. The reader polls `files.get?fields=version,headRevisionId,md5Checksum`, then `alt=media` on change.
- **Critical path:** 3 calls one-way. Units: `50 + 5·p + 200` per update.
- **Caps:** object-sized. Each update creates a binary revision, kept roughly 30 days or 100 revisions `[V]`, so storage can reach 100 times the slot.
- **Flow:** stop-and-wait per slot, or re-upload of the whole unacknowledged window.
- **Verdict:** the mid-size carrier only if metadata caps prove tiny.

**12. Shared downlink bus — one writer, N readers**
- **Calls:** as #1 and #2, but the exit writes one register bank for all devices, with frames sealed per session.
- **Units/min:** writes `50·w` regardless of N; reads `5·p·N`.
- **Trade-off:** it wins only if an account-level write ceiling binds. I recall guidance of a few writes per second per account `[V]`. A suspended device pins shared capacity, so it needs per-device quotas.
- **Verdict:** contingency mode, decided by probe 2.

**13. Batch op-train (`/batch/drive/v3`)**
- **Calls:** one HTTP request carrying up to 100 metadata calls, with no media and no ordering guarantee `[V]`.
- **Effect:** it collapses k metadata calls into one HTTP round. Units are unchanged if each sub-call is billed `[V]`.
- **Verdict:** concurrent HTTP/2 streams give the same round saving, so batch matters only on HTTP/1.1-only paths and for delete trains.

**14. Revision log**
- **Calls:** the writer repeatedly calls `files.update` with media on one file per direction, each making a revision. The reader checks `files.get?fields=headRevisionId`, then `revisions.list`, then `revisions.get?alt=media`. Cleanup is `revisions.delete`, which cannot remove the head.
- **Units:** about `50 + 5·p + 100 + 200` per message, plus 50 to delete.
- **Risks, all `[V]`:** coalescing of rapid revisions, purge at 100 revisions, storage accounting.
- **Verdict:** an append log without an ID tape, but every step costs at least as much as #2. Research only.

**15. Comment and reply log**
- **Calls:** `comments.create(fileId, content)` with sealed base64url text. The reader calls `comments.list` with `startModifiedTime`. `replies.create` gives per-stream threads. Cleanup deletes the host file, because deleted comments leave tombstones `[V]`.
- **Critical path:** 2 calls. Units assumed `50·w + 100·p` `[V]`.
- **Unique property:** a true multi-writer append with no lost updates.
- **Unknowns:** content cap, ordering, rate limits and text sanitisation.
- **Verdict:** only as a multi-writer doorbell if bell collisions (section 2.3) prove painful.

**16. Workspace Events to Pub/Sub, and Google-hosted relays**
- **Calls:** an event subscription on the mailbox publishes to a Pub/Sub topic. The exit pulls; a client would need `pubsub.googleapis.com`.
- **Blockers:** credentials beyond `drive.file`, a billed Cloud project, and a client authority other than `www.googleapis.com`. Drive coverage in Workspace Events is `[V]`.
- **Verdict:** outside the supplied grants. Record it as an explicit alternative. The same gate applies to a Cloud Run or Apps Script relay, which would bypass Drive entirely.

**17. Conditional-request register (ETag, `If-Match`, `If-None-Match`)**
- **Idea:** compare-and-set for multi-writer files, and 304 polls.
- **Status:** I cannot establish that v3 honours these on metadata, or how a 304 is billed.
- **Verdict:** cheap to probe, but the top design must not depend on it.

**18. `indexableText` and thumbnail carriers — rejected**
- `indexableText` is write-only `[V]`. It can be read only as a search predicate, costing a 100-unit list per extracted token.
- A thumbnail must be a valid image, may be transcoded, and is served from another host. It is not byte-exact and breaks the authority constraint.

**19. Reading before upload completion — rejected**
- This covers tail-reading a resumable upload, or one giant object per stream.
- Drive exposes no content until the final byte commits, and a session reveals only an offset.
- A 100 MiB stream would deliver nothing until done, which breaks destination and application timeouts.

**20. Flag and rename symbol channels — rejected**
- This covers `trashed` or `starred` toggles, parent moves, shortcut targets and renames as symbols.
- Each symbol costs a 50-unit write and carries a few bytes. The same write to `appProperties` carries about 2.7 KB, so these are strictly dominated.

**Ranking logic.** The order reflects critical-path calls for a small message, units per message and per empty poll, cleanup debt, and how many unverified Drive behaviours each needs. #1–#6 form one design. #7–#10 are the fallback family if read-by-ID fails. #11–#17 are contingencies or research.

## 2. Best mixed architecture: registers plus tape

### 2.1 Layout and identifiers

```
root/                      isolated, app-created
  DIR        register, written only by the exit: generation, instance, parameters
  BELL       register, any client may overwrite it to wake the exit
  LEASE[g]   empty files at pre-agreed IDs; existence means generation g is claimed
  device/    one folder per enrolled device
    U pages  uplink register bank    (client writes, exit reads)
    D pages  downlink register bank  (client creates empty, exit writes)
    objects  tape objects, both directions
```

| Identifier | Scope |
|---|---|
| `mailbox_id` | set at setup; domain separation |
| `client_id` | per install, persistent |
| `run_id` | per client process start, never persisted as current |
| `g` (generation) | index of the lease file the exit claimed |
| `inst` | random nonce of one exit process |
| `session_id` | hash of client, run, g, inst and both ephemeral keys |
| `stream_id` | client-allocated, strictly increasing, never reused |
| `fseq`, `n` | register frame sequence; tape object index |

### 2.2 Sealing

```
blob       = nonce ‖ AEAD(key, nonce, AD, plaintext)      base64url in text fields
AD         = version ‖ mailbox_id ‖ g ‖ inst ‖ session_id ‖ direction ‖ carrier ‖ file_id ‖ counter
page       = header(write counter, max_fseq, poll hint)
             ack(R_cum; T_cum + 64-bit SACK)
             tape(committed_through; bootstrap IDs)
             budget grants (exit to client)
             frames[fseq, stream, type, offset, len, bytes]
object     = records of at most 64 KiB, nonce = n ‖ record ‖ final-flag
```

- File names are sealed descriptors. Property keys are fixed constants.
- Session keys come from the profile secret plus an ephemeral X25519 exchange, split per direction and carrier.
- Binding the file ID stops ciphertext being moved between files.
- Records are individually authenticated, so a reader delivers them while the download is still running and resumes with `Range`.

### 2.3 Lease and handshake

**Duplicate side effects are prevented by addressing, not by the lease.** Every frame is sealed to one `inst`, and an exit executes only frames sealed to itself. Two live exits therefore cannot both execute a frame.

**The lease gives a total order for takeover and janitor rights.** It is obtained as follows:

1. Setup pre-generates a table of lease IDs.
2. A starting exit reads `DIR`. If the last holder did not mark a clean release and was not a dead process on this same host, it writes a liveness challenge to `BELL` and waits two bell intervals for the holder to answer in `DIR`.
3. If there is no answer, it calls `files.create` on `LEASE[g+1]`. Exactly one creator succeeds and others get 409 `[V]`, verified by probe 3.
4. The winner publishes `DIR`.
5. An exit that sees a higher generation in `DIR` or a ring addressed to a newer one stops reading uplinks, resets its destination sockets and exits. It will not restart while a live holder answers.

If atomic create fails probe 3, safety still holds through `inst` addressing. Only the single-exit guarantee degrades to an operator rule.

**Client session:**

| State | Event | Action | Next |
|---|---|---|---|
| DORMANT | first local stream | in parallel: create U with HELLO, create empty D, PATCH `BELL`; poll D | HELLO_SENT |
| HELLO_SENT | D holds WELCOME sealed by the named `inst` | derive keys | ESTABLISHED |
| HELLO_SENT | NACK or newer `g` | reset streams carried in the HELLO; re-read `DIR` | DORMANT |
| HELLO_SENT | timeout | re-ring up to 3 times, then report error | same |
| ESTABLISHED | GOAWAY, newer `g`, D gone, or SESSION_CLOSED | reset every local stream; replay nothing | DORMANT |
| ESTABLISHED | no streams for the linger time | send SESSION_CLOSE | DORMANT |

- A HELLO may carry OPEN frames only if it names a `g` and `inst` the client validated recently. Otherwise it is a plain hello.
- `BELL` is last-writer-wins. A ring lost to a collision is retried after the advertised bell interval.

**Exit session:**

| State | Event | Action | Next |
|---|---|---|---|
| NONE | ring; U holds a valid HELLO for my `inst`; run unseen | PATCH D with WELCOME | ESTABLISHED |
| NONE | HELLO for another `inst` | PATCH D with NACK; execute nothing | NONE |
| ESTABLISHED | no client progress for 30 s with unacknowledged downlink | stop reading destination sockets | STALLED |
| STALLED | client progress | resume | ESTABLISHED |
| any | SESSION_CLOSE, no keepalive for 15 min, or a HELLO naming this run as `prev_run` | reset destinations, write SESSION_CLOSED, delete artifacts | NONE |

**Acceptance rule for every blob:** the AEAD verifies under the exact AD. A register page must carry a strictly higher write counter than the last accepted one. A tape index must be inside the window and unseen. Anything else is dropped and counted.

**Streams:** IDLE, OPEN_SENT, OPEN, half-closed each way, CLOSED.
- OPEN carries the destination hostname and the first bytes. The client answers the local SOCKS connect optimistically.
- The exit replies OPEN_OK or OPEN_ERR.
- FIN carries the final offset and becomes a socket shutdown once all earlier bytes are delivered. RST closes immediately.
- VPN-mode DNS is a small request/response frame.

### 2.4 ACK definitions

| Level | Exact meaning | What it permits |
|---|---|---|
| Storage acceptance | Drive returned success (or 409 on an ID retry). Local event, never on the wire | start the next write; publish the commit mark |
| Peer acceptance | peer downloaded, authenticated and queued the frames: `R_cum` for register frames, `T_cum` plus SACK for objects | drop the frames from the snapshot; delete the object; advance the window |
| Destination delivery | `WINDOW(stream, delivered_offset, limit)`: the far side's socket write returned for bytes up to that offset | release per-stream credit |

Destination delivery says the kernel took the bytes, not that the remote application read them. No ACK level ever triggers a replay. A stream that loses its peer process is reset, and the application decides whether to retry.

### 2.5 Small versus bulk

Classification is behavioural, since the plaintext is opaque.

- **Interactive allowance:** each stream has a token bucket (24 KB depth, 4 KB/s refill). Bytes within it go to the register. A backlog beyond it goes to tape.
- **Ordering across carriers:** a stream with unacknowledged tape bytes sends everything by tape, so its small tail never arrives ahead of its bulk.
- **Promotion:** a stream returns to the register when its tape bytes are acknowledged and the bucket has refilled.
- **Inherent limit:** a chat message sharing one TCP stream with an attachment waits behind it. No transport can fix that.

Bulk cannot starve small traffic because:
- Registers use their own HTTP/2 connection, so no TCP head-of-line or send buffer is shared with uploads.
- Bulk uploads are paced as a scavenger. If register-call round-trip time rises more than about 25% above baseline, the bulk rate drops.
- Bulk sockets keep small unsent buffers. Bulk downloads are read at about 90% of estimated capacity while interactive streams are active.
- Bulk encryption runs on separate workers.

Small traffic cannot block bulk because:
- The interactive class has priority for write slots but is capped, by the per-stream allowance and by a ceiling of 4 register writes/s per direction.
- Bulk is guaranteed at least half the write and unit budget under contention, and unused share flows either way.
- Within each class, streams are served by deficit round-robin. The exit also round-robins across devices and grants each a write-rate and unit budget in its D page.

### 2.6 Polling and idle bounds

| Reader state | Cadence |
|---|---|
| Hot: reply expected or tape active | 100 ms for 2 s after sending |
| Warm | doubling to 1 s |
| Quiet for over 1 min | 2 s |
| Quiet for over 5 min | 5 s |
| Backgrounded | 30 s |
| No open stream | no polls |

- The exit polls `BELL` on its own ladder: 1 s, then 3 s, then 10 s.
- The exit polls each idle uplink register every 15 s as a safety net.
- A client waking an idle session rings `BELL` in parallel with its register write.
- HTTP/2 pings, which cost no quota, keep the interactive connection warm while streams are open.

### 2.7 Duplicate rejection and ledger

Rejection is deterministic at four layers:
- Register frame: `fseq ≤ R_cum`.
- Tape object: index below `T_cum` or already set in the bitmap. It is not even downloaded.
- Stream bytes: offset below the delivered cursor.
- OPEN: `stream_id` not above the highest seen.

The ledger is in memory only:
- Two cursors and a bitmap per session.
- Delivered offsets for open streams.
- A closed-stream watermark with a bounded exception set.
- The set of run IDs seen within a freshness epoch published in `DIR`, which defeats HELLO replay.

It is retained for the life of the session and discarded with it. Nothing durable is needed, because a process death changes `run_id` or `inst`, and that makes every earlier frame unaddressable.

### 2.8 Cleanup ownership

- **Hot path:** the lease-holding exit deletes everything. Uplink objects go after processing, downlink objects after the client's peer-acceptance ACK. Deletes are deferred and batched.
- **Clients:** they issue no deletes during a session. On graceful exit a client sends SESSION_CLOSE.
- **After an exit crash:** the next generation lists the root's direct children, authenticates each sealed name, deletes stale artifacts by exact ID, and verifies with GETs immediately and after a delay.
- **If the exit is gone for good:** the same janitor runs from a client, on explicit user confirmation.
- **Storage bound:** the session cap on unacknowledged bytes (64 MiB; 16 MiB on iOS).
- **Optional:** epoch folders (#6) replace per-object deletes once probe 3 confirms folder-delete semantics.

### 2.9 Suspension, revocation, route, quota

- **Suspension:** a suspended client stops polling. The exit stalls, holds destination sockets with bounded buffers, and expires the session after 15 minutes without a keepalive. On resume the client finds SESSION_CLOSED and resets local streams.
- **Revocation:** `invalid_grant` is terminal and shown to the user. Publishing removes the testing-mode expiry but does not make the grant permanent.
- **Route:** the route is fixed per session. A failed pinned front is an error, never an automatic switch to Direct.

Arithmetic on your table, for one device:

| Mode | Units/min |
|---|---|
| No open stream | 0 at the client; 30–300 for the exit's bell poll, about 0 with #5 |
| Open but idle | about 230 (D every 5 s, safety poll, bell at 2 s); about 60 in background |
| Hot interactive (30 writes each way, 120 polls each side) | about 4,200 |
| Bulk | 300 per object: about 8,750 per 100 MiB in 4 MiB objects, including commit marks |

- **Default capacity:** 1M units/min supports roughly 240 hot devices or 4,300 idle-open ones.
- **Example mix for 200,000 users:** 2% hot, 10% idle-open and the rest dormant needs about 23M units/min.
- **Legitimate routes:** a quota grant sized by `N·(f_hot·4200 + f_idle·230 + f_dormant·10)`; users bringing their own Cloud project; and a console-enforced per-user cap far below 325k `[V]`, so three users cannot drain the project.
- **Not legitimate:** sharding users across publisher-owned projects is bypass.
- **On exhaustion:** clients lengthen ladders, grow object size and tell the user.

## 3. Probes and ABBA plan

Every probe writes each ID to a local manifest before the create is issued. Cleanup deletes exactly those IDs and expects 404 at +0, +60 s and +10 min. It then lists the probe root expecting nothing, and confirms storage is back to baseline.

**Probe 1: read-after-write by ID versus by listing, from both vantage points**
- **Method:** the writer is at one end and the reader at the other, then swapped. Three writes per trial type: PATCH `appProperties` on an existing file; a metadata-only create at a pre-generated ID; a 64 KiB multipart create at an ID generated by the other peer.
- **Reader:** it polls `files.get` by ID every 50 ms starting before the write, which exposes negative caching. In a third of trials it also polls `files.list` and `changes.list` every 200 ms.
- **Recorded:** write start, write response, the first poll returning the new state, and any later poll returning older state.
- **Checks:** payloads are random and compared byte-for-byte.
- **Bound:** 240 writes, 160 deletes, at most 150k units at no more than 15k/min.
- **Decides:** #1–#3 versus #7–#10, and #1 versus #3 by write latency.

**Probe 2: register capacity, exactness and write ceiling**
- **Capacity:** binary-search the accepted size of `appProperties`, `properties`, `description` and `name`.
- **Exactness:** read 20 random payloads at each cap back byte-for-byte.
- **Atomicity:** read at 20/s while 30 keys are rewritten per PATCH. Any response mixing two snapshots is a torn read.
- **Rate:** 0.5, 1, 2, 4, 8 PATCH/s for 30 s on one file, then the same aggregate over 4 files. A step stops at the third 403 or 429 in 10 s.
- **Side effects:** confirm no revisions or storage growth.
- **Bound:** at most 600 writes and 45k units.
- **Decides:** page size and count, whether an account write ceiling forces #12, and whether #1 survives.

**Probe 3: atomic create, ambiguous retry, deletion**
- **Race:** two processes create the same ID, 20 rounds. Expect one success and one 409, with the stored bytes equal to the winner's.
- **Ambiguous retry:** abort after the request body is sent, then retry, 20 rounds each for create and PATCH. Expect never two files and no lasting rollback.
- **Pre-commit visibility:** GET the ID during 5 slow 8 MiB uploads. Expect 404 until commit. Stitched ranged GETs must equal the original.
- **Deletion:** re-create a deleted ID. Delete folders holding 10 and 100 children and check each child at the delayed times. Start a create into a parent deleted mid-flight.
- **Bound:** at most 300 writes and 35k units.
- **Decides:** the lease, the 409 rule and #6.

**ABBA plan**

*Arms.* A is your incumbent if one exists, otherwise #7. B is the design above. Both use the same account, project and exit host, with separate roots. Clients run on two networks, pinned front and Direct, never mixed within a block.

*Destination.* A controlled server beside the exit logs receive and send times and can add a fixed 0 or 50 ms of processing.

*Order.* Each workload cell runs A, B, B, A, and the next session runs B, A, A, B. Run at least 6 cells per workload over 3 days and two times of day. Between blocks: a 3-minute cool-down, exact-ID verification that the mailbox is empty, and a log of quota counters.

*Workloads.*
1. Cold 64 B, with no session and the exit on its slowest bell cadence.
2. Cold 64 B on a new stream in a warm session.
3. 64 B on a long-lived TLS stream: open-loop schedule, mean gap 5 s plus gaps of 2 and 10 minutes, in both directions including unsolicited server-to-client.
4. Bulk alone: 16 MiB and 100 MiB, upload and download, hash-verified.
5. Workload 3 during workload 4, in all four direction pairings.
6. Browser cold start: 6 parallel new TLS connections, 30 repetitions.
7. Multi-client: 5 and 10 devices each running workload 3, with 1 then 3 of them running 100 MiB transfers.
8. Faults:
   - kill the client mid-upload;
   - kill and restart the exit mid-transfer;
   - start two exits at once;
   - freeze the client for 30 s, 5 min and 20 min;
   - cut the connection after a request body is sent;
   - delay, duplicate and reorder responses inside the HTTP layer, leaving the route untouched;
   - fail half of deletes for 2 minutes;
   - cut the unit budget to 25%.

*Clocks, kept separate.*
- **Latency from intended send** is the headline: application read time minus the scheduled send time. It is immune to coordinated omission.
- **Latency from socket write:** application read time minus the return of the local `write()`.
- **Server processing:** server send minus server receive, subtracted to give tunnel-only latency. The injected 50 ms validates the subtraction.
- **Per-leg times:** storage acceptance, peer discovery, peer acceptance, destination write.
- **Bulk goodput:** verified bytes divided by the time from first byte intended to last byte delivered. Time to first byte and stalls over 2 s are reported beside it and never folded into latency.
- **Cost:** calls by type times your table, empty-poll share, rate-limit errors, peak Drive bytes, peak memory.

*Statistics.* Decisions use paired within-cell differences at p50, p95 and p99 with bootstrap intervals. Thresholds are fixed before the first run.

*Integrity.* The destination logs a unique token per connection and message, and a token seen twice is a duplicated side effect. After a fault, every stream completes intact or is reset.

*Findings that reject the top design.*
1. Probe 1 shows read-by-ID is no fresher than listing (I would set p99 above 300 ms), or returns older state after newer.
2. Probe 2 shows under about 1 KB of usable metadata per file, or sustained PATCH failing below about 1/s.
3. In workload 5, B's p95 small-message latency is not at least 25% below A's, or its 100 MiB goodput is more than 10% below A's, or it spends more units per message.
4. In the 10-device cell, rate-limit errors make B's p95 worse than A's.
5. Any byte mismatch, duplicated token, or object surviving the +10 min check rejects B regardless of speed.

## 4. Lower bound and frontier

| Event | Minimum calls | Units |
|---|---|---|
| Small message, one way | 1 write + 1 read | 55 |
| Request/response | 4 | 110 |
| Bulk object | 1 create + 1 media read; delete amortised | 250–300, so about `300/S` per MiB |
| ACK | 0 if piggybacked | 0 or 50 |
| Empty poll | 0 when dormant; one metadata read per interval when a stream is open | 5 each |
| Cold start | client: 1 parallel round; exit: 2 reads + 1 write; client: 1 read | about 215 |

- **Why not lower:** a direction needs one committed write, because nothing is visible earlier, and one peer read, because nothing pushes payload to a client. Coalescing amortises many frames over one pair of calls but cannot go below one pair per delivery.
- **Latency floor:** one-way latency is write time plus visibility lag plus read time. Polling adds about half a read interval on top.

**Unexplored frontier:**
- **A two-writer register.** A PATCH with `fields` returns the peer's keys, so every write doubles as a poll. It depends on per-key merge atomicity `[V]`.
- **Open-ended upload bodies** of unknown length, so an object uploads while it fills.
- **Conditional GETs and 304 billing** (#17).
- **HTTP/3 to the pinned front.**
- **Hedged or staggered polls** on two connections.
- **Client push** through any long-poll endpoint reachable under the same front.
- **Real prices** of 404s and batch sub-calls, and the real account write ceiling.
- **Traffic-analysis padding** and its quota cost.

**Evidence that would justify shipping:**
- All three probes pass from at least two networks, repeated across days.
- The ABBA result clears the thresholds above, with zero integrity, duplication or cleanup failures across the fault matrix.
- A 48–72 hour soak whose quota logs match the idle formulas.
- A staged rollout with a negotiated fallback to the list family.

**What that would not prove:**
- That the design is optimal. Untested parameters and designs remain, and a finite benchmark cannot rule them out.
- That undocumented Drive behaviour stays stable. Google can change consistency, limits or pricing at any time.
- Behaviour at 200,000 users on shared quota, on other networks and filters, or under Google's abuse heuristics.
- The absence of rare races. Model-checking the register, tape and lease state machines addresses that better than more benchmark runs.
