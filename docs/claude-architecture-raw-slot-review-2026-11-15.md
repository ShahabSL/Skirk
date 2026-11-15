# Raw targeted Fable 5.2 slot review — November 15, 2026

This is an unedited generated design opinion, not measured performance or a
validated patch. Returned metadata records **only `claude-fable-5-2`**, one
turn, no web searches, and no permission denials. The CLI used no built-in tools
or MCP servers. Its input was a sanitized prompt and the three tracked slot
source files from `f9a2413`; it did not receive `mux.go` and explicitly states
that limitation.

**Read the fact checks first:** [review and corrections](claude-architecture-review-2026-11-15.md).
Some suggestions are contingent on a deduplication watermark, bounded retirement
of state, and failure/reordering tests. The proposed immutable ID train still
needs credits, empty polls, deletes, and restart handling; it is not zero-cost
or proved faster. The initial substrate results are not end-to-end guarantees.

---

**Verdict:** yes. The synchronous processing-ACK wait is enough on its own to erase the substrate gain, and the supplied code adds four more serializers and two black-hole states that produce the same symptom. None of this is measured: it is code inspection plus arithmetic on your primitive timings, and I could not see `mux.go`.

**Ranked decision**
1. **Async slot integration now**, behind the existing flag. It is the decisive test of the hypothesis and reuses the codec and identity checks you already have.
2. **Known-ID immutable trains** as the promotion candidate. They have the fewest hot-path operations and need no ACK or fallback machinery, but three primitive facts need probing first.
3. **Metadata-only prefix packets** stay as the floor carrier, with no new investment.
4. **Manifest/pointer ring:** don't build.

## 1. Current integration

**Worker occupancy.** `trySlotDelivery` holds the lane's priority worker from `shadowQ` insert until ACK, error or 1.5 s.
- The shortest ACK path is: writer queue → pacing → PATCH → receiver poll phase (0–250 ms) → GET → mux processing → ACK-only PATCH → sender poll phase (0–250 ms) → GET.
- That is 2·PATCH + 2·GET + 0–0.5 s. With GET at 0.12–0.23 s it is 2·PATCH + 0.24–0.96 s.
- I don't have your PATCH RTT. Unless its p50 is under roughly 0.3 s, the 1.5 s timer sits in the body of the distribution, not the tail.
- Even a successful slot delivery holds the worker for a full round trip, longer than a baseline upload. A timeout holds it 1.5 s plus the upload.

**Same-lane cross-stream HOL.** The blocked worker serves the whole lane, so OPENs, RSTs and other streams' first bytes wait behind one stream's follow-on DATA. Cold text can end up slower than baseline.

**Completion and ACK semantics.**
- The timer starts at `shadowQ` insert, not at confirmed PATCH.
- The writer moves one record per PATCH and never drains `shadowQ`. The k-th concurrent record starts its PATCH after k−1 PATCH round trips, so with four lanes records 3–4 will rarely be ACKed in 1.5 s.
- ACK-only PATCHes share that writer, its 250 ms pacing and its budget. An ACK-only PATCH in flight delays the response DATA behind it by up to one PATCH.
- `queueSlotDeliveryHead` offers only the staged head, and only from the reader loop. Unless `mux.go` re-arms it after `markProcessed`, drain is one record per ≥250 ms iteration.
- The head is skipped while its stream is not `ready`. DATA that outruns its immutable OPEN stalls every later record of every stream on that run until the OPEN or the fallback lands.
- If mux receive ordering is contiguous per lane sequence, slot records also wait for slower immutable predecessors. That would cap the gain regardless of ACK design.

**Fallback timer.**
- A timed-out record is not withdrawn. It stays in `pending`, is republished in every PATCH, and occupies the cumulative window (about 6 KiB raw), so later admissions overflow.
- Each timeout creates a second copy. When the slot copy wins, the physical one costs a write, a list hit, a verification GET (only two verify slots) and a delete.
- The eligibility check compares raw `len(sealed)` with 8192, but 8192 is the base64 budget. A lone record fits only up to about 6064 − len(name) bytes, so batches between ~5.9 and 8 KiB queue for the writer just to be rejected.

**Process-level caps.** The link allows 64 PATCHes (ACK-only included), 320 GETs (about 80 s) and a 2 min lifetime, with one attempt per run. At one data PATCH plus one ACK-only PATCH per exchange, a link covers roughly 32 request/response exchanges. Two black holes follow:
- **Sender side:** budget exhaustion returns from the writer without setting `sendClosed`. `canSend()` stays true with no writer, so the next ≤16 records each burn the full 1.5 s.
- **Receiver side:** when the receiver's writer exhausts its budget, its reader still stages and delivers but can never ACK. Every sender record then times out until the sender's own budget dies. This is exactly "timeout despite quick receive".

If the A/B used these constants, any run longer than about a minute is mostly baseline plus these stalls.

**Stale reads.** `receive` validates the ACK before the replay check, so a stale read carrying a lower ack closes the link instead of being ignored.

**Timestamps that separate the causes** (monotonic, keyed by record fingerprint + SlotSeq):

| Side | Stamps |
|---|---|
| Sender | s0 sealed · s1 worker dequeue · s2 `shadowQ` accept (depth, pending bytes) · s3 writer dequeue · s4 PATCH prepared (after `sendMu` + pacing) · s5 limiter/connection acquired, on wire · s6 response headers · s7 GET that first carried ack ≥ seq · s8 caller return + outcome · s9 fallback start/end · s10 late-ACK time for timed-out records |
| Receiver | r1/r2 start/end of the first GET containing the record · r3 staged · r4 offered to mux, or skip reason (`not_head`, `not_ready`, `stream_missing`) · r5 process start, or `blocked_on_predecessor` + its carrier · r6 `markProcessed` · r7 `ackWake` consumed · r8 ACK PATCH prepared · r9/r10 on wire/response |

- **Scheduler terms:** s3−s2, s4−s3, r4−r3, r8−r6, and both poll phases.
- **Queue terms:** s5−s4, r9−r8, and GET limiter and connection waits.
- **Backend terms:** s6−s5, r10−r9, GET TTFB, and visibility lag.
- **Confirmed** if timed-out records show r6−s2 < 1.5 s while s7−s2 > 1.5 s, and scheduler + queue terms exceed backend terms.
- **Refuted** if PATCH or GET TTFB p95 alone approaches the budget.
- **Counters:** records per PATCH; timeouts before vs after the peer's last published revision; budget-exhaustion events; overflow rejections; per-lane blocked time and batches queued behind it.
- **Cross-host alignment:** log Drive's `modifiedTime` for each PATCH on both sides instead of trusting host clocks. I have not verified that a description-only PATCH bumps it.

A zero-risk discriminator exists today. In shadow mode, compare `slot shadow observed` with `mux process` time for the same record fingerprint; that lead is the in-situ gain available with no ACK gating.

## 2. Asynchronous integration

**Sender**
1. **Admission** (worker, synchronous, microseconds). Enqueue directly under the peer mutex, with no channel hop and an exact capacity check.
   - Eligible: single-stream DATA/FIN, link active, and per-stream share ≤50% of the window.
   - For client→exit, admit only after the client has received any frame from the exit on that stream. This removes the ready-gate stall.
   - Success creates a ledger entry and releases the worker; failure goes straight to the ordinary immutable upload.
   - Release at admission, not at PATCH: releasing at PATCH would still queue four lanes behind one writer.
2. **Writer.** Level-triggered group commit, one PATCH in flight per slot. Each PATCH carries all un-ACKed records plus the current cumulative ACK. The first record after idle goes immediately; arrivals during a PATCH ride the next one. Send an ACK-only PATCH only when no data is pending, after a delayed-ACK timer ≤200 ms.
3. **Completion callback.** Keep it. The original frame ACK keeps its meaning (durably in Drive) and fires at confirmed publish or fallback success. Firing it at admission is what would let `readLoop` outrun the transport.
4. **Fallback scheduler.** A separate deadline heap with at most window-size entries, concurrency 2, inside the Drive limiter and quota accounting.
   - Deadline = confirmed publish + RTO, where RTO = clamp(SRTT + 4·RTTVAR, 2 s, 8 s), initial 3 s, sampled only from records with no fallback.
   - Immediate triggers: unresolved PATCH, 404, link close, budget.
   - Two consecutive expiries with no ACK progress close the link.
   - If more than 5% of records in 30 s are hedged, raise RTO or close.
5. **Cold wake.** After about 10 s without ACK progress, send the next record on both carriers at once.

**Receiver**
- Stage every record and offer all of them to mux at once. Re-offer on stream-ready and predecessor-processed events, not ticks.
- Poll at 250 ms while the run is active, 1 s after 3 s quiet, and park after about 15 s. The ordinary list poll is the wake path.
- Put slot PATCH/GET on its own connection and limiter class so bulk bodies cannot queue ahead; still count them in quota.
- Replace lifetime caps with token-bucket rates. Allow a new generation (new IDs) after a close, with backoff.

**Invariants**
- **I1 Identity.** A batch is sealed once. Name and sealed bytes are immutable; slot record, fallback and retries are byte-identical. Never re-seal, because a new nonce is an identity conflict. A pre-generated fallback file ID is worth adding (409 makes retry idempotent and gives an exact cleanup target); keep it in the ledger, not in the AEAD.
- **I2 Single admission.** A mux name gets at most one SlotSeq per generation, and none once its immutable upload has started.
- **I3 Window.** Un-ACKed records ≤ 64 and their encoded snapshot ≤ 8192 characters. This one bound is the send window, the retransmit buffer and the receiver's staging bound. A full window routes to immutable; it never blocks.
- **I4 Frame ACK.** Fires exactly once, at the first of confirmed publish (200 or exact readback) or fallback upload success. It fails only as a failed immutable upload would.
- **I5 Retention.** Sealed bytes and the stream's byte credit are held until cumulative ACK ≥ SlotSeq or fallback success. Memory is at most one description per direction per link.
- **I6 Fallback obligation.** Every un-ACKed ledger record is uploaded exactly as prepared on deadline, unresolved PATCH, or any close. There is no state where `canSend()` is true without a live writer. Slot files are deleted only after this drain.
- **I7 Acceptance order.** Authenticate → generation/direction → revision replay (ignore the whole snapshot, including its ack) → ACK validation → staging.
- **I8 Ordering.** Mux sequence governs delivery; SlotSeq governs only the ACK.
- **I9 Terminal disposition.** Every staged record ends as exactly one of: processed via slot, processed via immutable with the same digest, rejected as duplicate, or discarded because the stream closed. Each advances the cumulative ACK. A dedup hit inside mux must report back, because a silent drop wedges the head.
- **I10 Exactly-once.** Keyed by mux identity, not carrier. Single-flight per name; the first authenticated copy wins. A copy at or below the lane watermark is never delivered.
  - If its digest is retained, it must match; otherwise terminal-close the affected streams.
  - If the digest was evicted, reject by watermark, delete by exact ID, and log it as unverified.
- **I11 ACK meaning.** Processed, not socket-delivered. It is monotone and releases only ledger and window.
- **I12 PATCH discipline.** One in flight; nothing new while one is ambiguous; resolve only by exact readback or a covering ACK; otherwise close and apply I6.
- **I13 Cleanup.** Slot records are never deletion targets. The creator deletes slot files by exact generated ID, and the janitor covers crashes. Physical fallbacks belong to the receiver's deferred cleanup, by exact ID.
- **I14 Epoch.** A generation is valid only within the two process lifetimes that negotiated it. After a crash nothing is resumed and no application bytes are replayed; streams reset.

**De-dup after eviction and lost ACK.** The authority is the per-lane sequence watermark, which is O(lanes) state that lives for the whole run. The digest maps (64 entries, or 30 min) only add identity verification. A lost ACK makes the sender upload the identical fallback (I1, I6), and the receiver rejects it by watermark whether or not the digest survives. This assumes mux keeps a contiguous per-lane watermark; if ordering is per stream, substitute the stream's next-expected sequence.

## 3. Alternatives

| Carrier | Hot-path operations | Off-path cost | Main hazard |
|---|---|---|---|
| Metadata-only prefix packet | create → list poll (payload inline) | delete | list lag; no new state |
| Async slot | PATCH → known-ID GET | ACK PATCH, hedged duplicates, ledger | mutable overwrite; 6 KiB ACK-gated window |
| Ring + manifest | entry PATCH + manifest PATCH → manifest GET → entry GET | ACK before wrap | two serialized writes; overwrite ambiguity ×N |
| Known-ID train | create at pre-shared ID → known-ID GET | grants, delete | 404 ambiguity; map loss |

**The train removes the most.** It has the same two hot-path operations as the slot, but no per-record ACK and no mutable state. Window turnover is a batched credit grant, and running out of credit just means an ordinary create.

Its decisive property is that a train object is still an ordinary named immutable mux object, so the list poll remains a second discovery path for the same object. That means no fallback upload, no dual-identity dedup, no ledger, and 409-idempotent retries.

The ring repeats the previous failure mode (an extra control write or metadata wait). Prefix packets keep list discovery on the path. The caveat on trains is evidence: the measured gain belongs to the slot substrate, and the train is unmeasured end to end.

**Train design**
- **Credits.** The receiver of each direction mints the IDs and grants them, so credits are the receive window. The client's OPEN offer carries down-train IDs, so the first response already rides the train. The exit's accept carries up-train IDs from a pool minted at startup, so there is no setup on the critical path.
- **Window and replenishment.** 32 IDs per direction per run. Grant 16 more when 16 remain, as a small control frame on the reverse priority path. On exhaustion, do an ordinary create without a preset ID; never wait for credit.
- **Consumption.** Strictly in order, with no skipping: a larger object is created as media at the same ID. Per-lane trains keep one create in flight per train.
- **Idle polls.** Poll only the head ID: 250 ms hot, 1 s warm, parked after about 15 s at zero cost. The list poll finds the next object because it is a normal named object, and that re-enters hot. No wake protocol or duplicate write is needed.
- **404.** Always means pending, never lost. Loss is never inferred from 404; the list path stays authoritative for existence.
- **Multi-client.** Trains are keyed by session, direction, client ID, run ID and generation, with grants carried in that run's own authenticated frames. The exit polls only hot runs, under a shared GET token bucket.
- **Lost map or restart.** If the receiver restarts, unconsumed objects are still list-discoverable and follow existing stale-run rules, and the sender's credits die with the generation. If the sender restarts, the receiver reaps with one GET per outstanding granted ID after the retry horizon.
- **Exact-ID cleanup.** The receiver deletes each consumed object by the ID it fetched. A late re-created duplicate is caught by list + watermark and deleted by exact ID. On orderly close the sender sends its next-unused index, so nothing is probed.

**Probe before investing** (none of these verified against Drive docs):
- 404→200 latency when the ID is polled before creation. Negative caching would kill this design.
- Create-at-ID latency versus PATCH.
- Whether a deleted ID can be re-created, whether unused generated IDs expire, and what `generateIds` costs in units. On re-creation the design is safe either way, because the watermark is the authority.

## 4. A/B and failure tests

**Phase 0: diagnosis, no behaviour change.** Add the stamps to the current sync path and shadow mode. Run warm text with an overlapping 100 MiB download, one client, 10 minutes.

**Phase 1: A/B.** Arms are baseline mux v4, current sync slot (first two cells only, for attribution), and async slot. Interleave in blocks, alternate order, and use the same account, route and hour, with fresh run IDs and a mailbox inventory between blocks.

| Cell | Workload |
|---|---|
| C1 | Cold text: 200 requests, new connection each |
| C2 | Warm text: 200 requests on one keep-alive connection, one per 2 s |
| C3 | One long-lived connection: text, then 16 MiB up and down, 100 MiB up and down, then text again on the same connection |
| C4 | C2 on one connection while 100 MiB down, then up, runs on another (the promotion cell) |
| C5 | Five clients running C2; one also downloads 100 MiB; one is a copied profile |
| C6 | 90 s idle, then five requests |

Verify bytes with a sequence-stamped echo sink and SHA-256 on files. Two hundred samples support p95, not p99.

**Gates, fixed before running** (thresholds are my proposals):
- **G1 Hypothesis:** async admission holds the worker for milliseconds, and the sync arm's timeouts decompose into scheduler/queue terms.
- **G2 Benefit:** warm text p95 at least 40% below baseline in C2 and C4 (the primitive showed about 70%); cold text not worse.
- **G3 No harm:** file throughput ≥ 97% of baseline in both directions; units per minute ≤ baseline + 5%; hedged fallbacks < 5%; zero duplicate or reordered bytes; zero residue.
- **G4 Scale:** each client's p95 within 1.25× of single-client; no cross-client consumption; exit GET units scale with hot clients only.

**Phase 2: fault injection on the async arm** (store-level interposer):

| Fault | Injection | Must hold |
|---|---|---|
| Delayed ACK | ACK PATCH held 0.5, 2, 5 s | Delivery latency unchanged; window overflows to immutable; no stall |
| Lost PATCH response | Applied and unapplied variants | Readback resolves or link closes; every un-ACKed record falls back once; frame ACK fires once |
| Late physical fallback | 5 s, 60 s, and after digest eviction (shorten retention) | Rejected by watermark; exact-ID delete; no stream impact |
| Fallback first | Slot copy arrives after 64+ other fallbacks | Suppressed; head advances; no wedge |
| Process crash | `kill -9` client, then exit, with un-ACKed records | Streams reset; no replayed bytes at the sink; slot files and objects cleaned; old generation never resumed |
| Sequence gap | Drop one slot record; then drop both copies | Hedge fills the gap in order; persistent gap follows existing mux policy; reorder buffer bounded |
| Quota pressure | Injected 403/429 at 10–50%, low unit budget | Slot sheds first; immutable keeps priority; no retry storm |
| Stale read | Older description served after newer | Ignored; link survives |
| Budget or close mid-flight | Force close with records pending | I6 drain; no 1.5 s stalls |
| Slow consumer | One stream's receive buffer full | Other streams still delivered |

**Reading the outcome**
- If G2 fails but publish→staged p95 is good, the substrate is fine and the loss is coupling (immutable OPENs, lane predecessors). Move to trains, which carry every priority object.
- If PATCH/GET TTFB degrades under bulk even on a dedicated connection, park the slot with that evidence.
- Passing this matrix earns only a wider trial. Promotion still needs the device, Reels, restart and cleanup-pressure gates in the handbook.
