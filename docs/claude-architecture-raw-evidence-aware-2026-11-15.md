# Raw evidence-aware Claude architecture answer — November 15, 2026

This is an unedited generated design opinion, not a validated technical report.
The CLI requested `claude-fable-5-2`; returned token metadata indicates an
automatic `claude-opus-4-8` fallback produced the visible answer. The request
disabled built-in tools and explicitly prohibited tool/API calls and code
changes. It authorized no live measurements; reported web searches were zero.
The input was sanitized tracked research evidence. The separate targeted pass
also disabled all configured MCP servers.

**Read the fact checks first:** [review and corrections](claude-architecture-review-2026-11-15.md).
The response overstates causal evidence about connection stalls, omits slot
lifecycle cleanup, misreads already implemented metadata-only support, assumes
comment ordering and description revisions, and treats untested Pub/Sub front
reachability as impossible. These assertions are preserved below so they can be
audited; they are not adopted as engineering facts.

---

# Skirk Drive‑transport audit — architecture research only (no code, no tests, no API calls)

I read the five historical records as *observations*, not as decisions to preserve. Three reframings change the conclusions the attachments reached, so I put them first; they drive everything below.

## Three reframings the evidence actually supports

**R1 — The metadata carrier was probably vindicated; its *integration* was indicted. The attachments conflated the two.**
The bare substrate probe (fixed‑ID description PATCH/GET running beside a real bulk transfer) is the single strongest signal in the corpus: text p95 **4.705→1.347 s** (upload) and **4.789→1.283 s** (download), *and bulk goodput rose* 13–16%, *and* units fell 15–23%. When the same carrier was wired into the mux as "slot delivery," request p95 got **worse** (3.3→4.5 s down, 3.2→4.1 s up) while units improved. That pattern is not "the carrier is slow" — it is "the coordination wrapper is slow." The wrapper added a 1.5 s fallback *timer*, dual‑writes (slot **and** immutable), a digest cache, duplicate reconciliation, and an in‑band ACK before the receive worker advanced. The probe had none of that. The honest reading: a known‑ID description ring is fast; the delivery protocol built around it in that experiment was heavy. The decision record leaned on the integrated (negative) number and under‑credited the substrate (positive) one.

**R2 — The dominant failure is a *connection‑localized Google response stall*, not a scheduler bug and not something the carrier choice fixes.** Every stalled arm shares one fingerprint from the HTTP trace: *connection acquired, request body fully written, then no response headers for 25–30 s.* The decisive datum is that a **second connection with the same generated ID completed in ~4 s while the original sat at 20 s+** (12/12 extra‑leg wins in the astra probe). A fresh connection to the same backend for the same object succeeding quickly means the stall is **local to one connection/stream** (TCP head‑of‑line from loss on the hostile path, or a wedged h2 stream / slow frontend instance), **not** global account/project throttling (which would stall the hedge too). This is *survivable* from the client (connection diversity + idempotent, stall‑**state**‑triggered hedge) but not *removable* — and it is orthogonal to whether bytes ride media or metadata.

**R3 — Direction asymmetry is unexploited. The exit has normal Internet; only the client is confined to the Google front.** Therefore: push discovery (`files.watch`/`changes.watch`/Workspace Events) is legitimate **on the exit side** for the client→exit path — the webhook terminates on the exit's own server and never touches client reachability. HTTP/3/QUIC is likewise safe **exit‑side**, where UDP is not policed (and QUIC is exactly what a hostile network tends to block, which also explains the H3 client‑mux stalls). The attachments evaluated push and H3 as symmetric and mostly dismissed them; half of each is actually available for free.

A compact per‑message cost view (using the project's stated unit figures — *verify against the Drive limits doc*: list 100, metadata GET 5, create/delete 50, media GET 200):

| Path (one small message) | On‑path Drive ops | Discovery | Data fetch | Cleanup | ~Units |
|---|---|---|---|---|---|
| Baseline immutable media | create → list → media GET | list 100 | media 200 | delete 50 | ~400 |
| Immutable metadata‑only, inline in list | create → list(+description) | list 100 | inline 0 | delete 50 | ~200 |
| Known‑ID description ring | PATCH → get | get 5 | inline 0 | **none (overwrite)** | ~55 |

The ring removes the 100‑unit list (→5‑unit get), the 200‑unit media GET (→inline), **and** the 50‑unit delete (→overwrite reuses a fixed object). That last point is under‑stated in the records: **ring‑carried traffic has no per‑message object and therefore no cleanup problem at all** — the stated cleanup/stale‑debt risk simply vanishes for that class.

---

## Deliverable 1 — 20 materially distinct candidate designs, ranked

Ranked by expected value on the mixed (interactive‑during‑bulk) workload under the fixed constraints: Google‑front‑only client, poll‑only client, byte‑order preserved, AEAD‑sealed payloads only in any visible field. "RT" = sequential Google operations on the *delivery* critical path (cleanup off‑path). Caps/units marked **(doc)**, **(obs)**, **(inf)**, **(unk)**.

| # | Design (carrier + discovery) | Deliver RT (1‑way) | Small‑msg units | Reachability | Verdict |
|---|---|---|---|---|---|
| 1 | **Hybrid: known‑ID description ring (interactive) + immutable media (bulk), class‑split connection pools** | 2 | ~55 | front‑only ✓ | **Top candidate** — synthesis; promote only if integration is thin (R1) |
| 2 | **Immutable metadata‑only, sealed bytes in `description`, read inline from `files.list`** (small) + media (bulk) | 2 | ~200 | ✓ | **Lowest‑risk win**; keeps all mux‑v4 invariants; already tested positive |
| 3 | mux v4 baseline: immutable media + `files.list` prefix poll | 3 | ~400 | ✓ | Proven floor / compatibility default |
| 4 | Baseline + **isolated bulk pool + stall‑state idempotent hedge** (no carrier change) | 3 | ~400 | ✓ | Attacks the real failure (R2); independently shippable |
| 5 | Pure known‑ID description ring, both directions, all small traffic | 2 | ~55 | ✓ | Best chat/quota; interactive‑only product mode |
| 6 | Immutable media + **custom‑property class query** discovery (`properties.skirkClass`) | 3 | ~400 | ✓ | Cuts list overmatch; keeps media |
| 7 | **Exit‑side push** (`files.watch`/`changes.watch`) upstream + poll downstream, media | 3↓ | ~300 | ✓ (exit callback) | Quota/latency saver on upstream only (R3) |
| 8 | Ring + **exit‑side push wake** on the client ring file ID | 2 | ~30 idle | ✓ | Ring with near‑zero idle exit polling |
| 9 | Immutable media + **appProperties cursors** (seq/ACK) read inline from list | 3 | ~400 | ✓ | Control‑plane complement, not a data carrier |
| 10 | **Encrypted filename** carrier: sealed bytes in `name`, read from list response | 2 | ~200 | ✓ | Works but inferior to `description` (search/index friction) |
| 11 | **HTTP/3 front, exit‑side only**, media (client stays H2) | 3 | ~400 | ✓ (exit) | Asymmetric transport experiment |
| 12 | Comments/replies **append‑log** on a fixed file (interactive) | 2–3 | unk | ✓ | Append order without cumulative retention; API/caps unknown |
| 13 | **Revisions append‑log**: successive description revisions, read by revision ID | 2–3 | unk | ✓ | Log semantics; write/revision tails and visibility unknown |
| 14 | **Batched** immutable creates + batched deletes | 3 | ~400 (no discount) | ✓ | HTTP‑overhead only; no quota help; verify Drive batch currency |
| 15 | `changes.list` durable‑cursor feed discovery + media | 3 | ~400+ | ✓ | Loses prefix locality; mailbox‑wide pollution |
| 16 | Workspace Events + Pub/Sub folder subscription (exit‑side) + media | 3↓ | ~300 | ✓ (exit) | GA but **billing‑blocked** in test; event delay unproven |
| 17 | **Range‑read slab**: one large pre‑written object, readers range‑GET fragments | 2–3 | ~200/range | ✓ | Primitive proven, control overhead lost before |
| 18 | `files.update` whole‑file **media update ring** (bulk via mutable file) | 2 | ~250 | ✓ | **Reject for bulk** — revision/write tails (obs 7.85–13.87 Mb/s, 74 s p95) |
| 19 | Resumable‑upload **append + range‑tail** (bulk streaming) | — | — | ✓ | **Reject (likely infeasible)** — in‑progress resumable bytes are not readable by a separate reader **(inf; verify)** |
| 20 | Non‑Drive Pub/Sub **StreamingPull** control plane (interactive) + Drive media (bulk) | — | — | **✗ client** | **Reject as drop‑in** — needs client reachability outside Google; different product |

Per‑candidate fields (cap/overhead, ownership/order/ACK/retry/overwrite, cleanup, multi‑client) — grouped to stay readable:

**#1 Hybrid ring+media (top).** *Cap:* ring plaintext ≈ `8 KiB description` **(obs ceiling tested; Google doesn't publish the number — unk, verify)** → after base64 (×4/3) and AEAD nonce+tag (~28 B) ≈ **6.1 KiB plaintext per snapshot**; overflow demotes that stream to a media object. *Ownership/order:* single writer per direction per **client‑ID+run‑ID** ring (copied profiles get distinct ring pairs); cumulative snapshot retains all unacked frames so a reader that skips Drive versions still reconstructs order (obs: skipped 5–9 versions, zero loss). *ACK:* reverse ring carries a cumulative accepted‑cursor; media bulk keeps its own per‑object ack. *Retry/overwrite:* PATCH is single‑attempt, read‑back on ambiguity; media uses generated‑ID idempotent retry. *Cleanup:* ring = none (overwrite); media = deferred delete + age janitor. *Multi‑client:* exit must track one ring pair **per active device** — O(clients) known IDs, each discovered from the list‑carried OPEN, then polled or **watched** (ties to #7/#8).

**#2 Inline‑description immutable (lowest risk).** Same 2‑op delivery as the ring but keeps **list discovery** and multi‑version safety (no bootstrap, no single‑writer, no overwrite race). *Cap/overhead:* same ~6 KiB sealed payload per object; above it, use media. *Order/ACK:* identical to today's immutable path (list‑discovered, sequence‑checked). *Retry:* generated‑ID idempotent create. *Cleanup:* per‑message delete (unchanged). *Multi‑client:* unchanged from mux v4. This is the deployment the metadata‑only + inline‑list positives actually support, and it was left half‑promoted.

**#3 Baseline.** Compatibility floor and the only thing every peer version understands; must remain the negotiation fallback.

**#4 Bulk isolation + stall‑state hedge.** No carrier change. *Ownership/retry:* generated‑ID so a hedge cannot create two objects; first 200 cancels/join the other leg. *Overwrite:* n/a. *Cleanup:* delayed sweep must confirm exactly one object per ID (the astra sweep found 4 uncommitted bodies after cancel — proof immediate cleanup was insufficient). *Multi‑client:* isolated pool must be budgeted so N clients don't exhaust sockets.

**#5 Pure ring.** As #1's interactive half, standalone. Hard cap on throughput (description‑bound); unsuitable whenever a stream exceeds the small threshold. Best‑in‑class chat/quota/no‑cleanup; worst for any bulk.

**#6 Property‑class discovery.** `name contains` is **prefix‑only (doc)** so the current interior `/p0//p1/` marker overmatches; a `properties.skirkClass` term returned the exact class in one page (obs). *Caveat:* property‑only query misses untagged older peers → needs capability handshake or broad‑prefix fallback; adds nothing to data carriage, only to discovery precision under backlog.

**#7 Exit‑side push upstream.** Exit replaces steady list polling with a watch channel on the client‑upload prefix/IDs; on notice it does a known‑ID GET. *Latency:* push notice ~2.5 s median (obs, 6 samples) is **too slow to be a hot‑path win** — use it as an **idle quota saver** while active traffic still polls. *Caps:* channel renewal + duplicate notices must be handled; `files.watch` needs the ID already known (fine post‑OPEN), `changes.watch` needs a reconciling list. *Reachability:* callback on the exit only — client untouched.

**#8 Ring + exit push‑wake.** Combines #1 and #7: exit watches each client ring file, polls only when woken → exit‑side idle polling ≈ 0. Downstream (exit→client) still client‑polled.

**#9 appProperties cursors.** `properties`/`appProperties` are tiny **(inf ~124 B/entry, ~30–100 entries; verify)** and returned in list/get → ideal for sequence numbers, ACK cursors, class tags, capability bits riding alongside another carrier. Not a data carrier. Note they are *searchable*, so only sealed ciphertext may appear.

**#10 Encrypted filename.** Data in `name` arrives in the list response like `description`, but `name` is indexed and governed by prefix‑match search, inviting the same overmatch and possible write‑indexing latency; strictly worse than `description`. Length cap **unk** (practically large). Keep `name` for the discovery prefix only.

**#11 H3 exit‑side.** Native H3 read worked (obs TTFB 158–198 ms vs H2 173–260 ms) but full H3 mux stalled/EOF’d. Confine QUIC to the exit's uploads/downloads where UDP is open; never on the client path (UDP is the first thing a hostile network drops, and Skirk already forces TCP fallback elsewhere).

**#12 Comments/replies.** Each reply is an append under a fixed file → native ordered log without cumulative retention (the ring's weakness). *But:* comment size cap, `comments.list` pagination, and visibility latency are **unk**; comments also generate activity semantics. Worth a primitive probe, not a commitment.

**#13 Revisions log.** Revisions accumulate on a fixed file; read by revision ID. Visibility and per‑revision write cost **unk**; media revisions inherit #18's tails. Low priority.

**#14 Batched ops.** Batch reduces HTTP setup under backlog but, per the records, **does not discount quota and cannot carry media**; also verify the Drive batch endpoint's current support (Google retired the global HTTP batch endpoint for several APIs). Use for cleanup‑delete bursts only.

**#15 `changes.list`.** Durable cursor, but not prefix‑filtered → mailbox‑wide pollution and extra bookkeeping; research/reconciliation tool, not a hot path.

**#16 Workspace Events/PubSub.** GA for Drive folder subscriptions; could turn many `files.list` into `files.get`‑by‑ID. Needs a **billed** project (test projects had no billing → pilot blocked), Pub/Sub permissions, renewal, dedup, list reconciliation; event delay is advertised as possibly minutes → treat as exit‑side idle wake, not hot text.

**#17 Range slab.** `206 Content‑Range` with independently sealed fragments is safe (obs) but prior slab transports paid too much control overhead. Keep as a bulk micro‑optimization only if a single huge object ever beats many medium objects on HOL — unproven.

**#18 Update ring (reject bulk).** Whole‑file media update = expensive revision tails (obs 74 s p95 for 16 MiB). Not a byte‑queue primitive.

**#19 Resumable append + range‑tail (reject, verify).** The attractive "stream into one object, tail‑read it" fails because a reader cannot read an **in‑progress** resumable session's bytes **(inf — verify in the manage‑uploads doc)**; without readable partial state there is no streaming. Resumable also adds an init round trip that lost for small frames.

**#20 Non‑Drive control plane (reject as drop‑in).** A Pub/Sub StreamingPull or direct relay for interactive would need the **client** to reach something other than the Google front — forbidden by the stated constraint. Viable only as an explicitly chosen *different route*, never an automatic default.

---

## Deliverable 2 — The three strongest missing experiments

Each can flip a current "rejected/incomplete" verdict.

**Experiment A — Thin‑integration ring, isolating carrier from coordination (tests R1).**
*Primitive first:* drive the existing two‑slot probe from the **real mux's** interactive frames for one demoted stream, ring as **sole** carrier, fallback **only** on PATCH error or 6 KiB overflow — **no 1.5 s timer, no dual‑write**. Measure one‑way text p95 and zero‑loss across forced version‑skips.
*End‑to‑end:* 16 and 100 MiB up/down, one persistent demoted text stream on the ring, 5 and 10 clients. ABBA, same binary, pinned front, same day, order control/cand/cand/control **and** reversed; control = mux‑immutable text, candidate = ring‑primary text. *Stop* on any hash mismatch, lost frame, >100 k rolling units, or Drive API error.
*What changes the decision:* if ring‑primary recovers the substrate's ~70% text‑p95 cut **without** the bulk‑rate loss that slot‑delivery showed → promote to opt‑in hybrid default‑candidate. If bulk still drops, the loss is **connection contention** (the ring's extra pool stealing from bulk), i.e. a connection‑budget problem, not a carrier problem — redirect to pool sizing, don't reject the carrier.

**Experiment B — Stall‑state idempotent hedge + bulk connection isolation (tests R2).**
*Primitive first:* N concurrent 4 MiB generated‑ID uploads on the pinned front; using the existing `SKIRK_HTTP_TRACE` "write‑done, no‑headers" signal, when a leg crosses T∈{1.5, 2.5, 4} s in that exact state, launch **one** same‑ID hedge on a fresh connection. Record which leg commits and, via a **delayed** sweep, that exactly one object exists per ID.
*End‑to‑end:* 16/100 MiB mixed up/down, 5/10 clients; bulk media on a pool isolated from interactive; control = no hedge, candidate = stall‑state hedge. *Stop* on any duplicate commit, hash mismatch, >1.15× baseline units, or >100 k rolling.
*What changes the decision:* if stall‑triggered hedging converts the ~30% stall‑failure arms into completed arms at ≤1.15× units with **zero** duplicate commits → the 100 MiB bulk gate (currently failing) can pass and hedging graduates from "rescue" to "bulk‑reliability default." If any duplicate ever commits despite identical generated ID, Google's retry idempotency for this path is insufficient **(verify in the manage‑uploads doc)** → fall back to single‑writer leader election instead of hedging. The earlier fixed‑3 s hedge was rejected for amplifying work (28 legs, 42 k units); this triggers on the *observed state*, not a clock, which is the fair test that was never run.

**Experiment C — Exit‑side push + adaptive client polling, measured by direction (tests R3 and the scale ceiling).**
*Primitive first:* on the exit, `files.watch`/`changes.watch` the client‑upload prefix/IDs (these need **no billing**, unlike Workspace Events); measure notice latency **and** exit‑side units/min when the exit does **zero** steady polling and relies on push, across an active→idle cycle, vs current list polling.
*End‑to‑end:* 5/10 clients, bursty interactive + one bulk each; exit = push + on‑demand get, client = adaptive poll (fast during its own send‑epoch, backing off when idle). Report **units/min per pair, split by direction**, and text p95. ABBA control = symmetric list polling.
*What changes the decision:* if exit‑push cuts exit‑side discovery units >5× without pushing text p95 past the ~1 s Drive floor → the per‑project active‑pair ceiling roughly doubles and the capacity story improves (still bounded, still needs per‑user projects). If push latency/variance or duplicate notices dominate, push stays idle‑only. This is the only experiment that measures the two directions separately, which is what the 200 k‑user arithmetic actually hinges on.

---

## Deliverable 3 — Why priority/metadata helped text yet bulk sometimes stalled

Separating the candidate causes against the evidence:

- **Google backend response delay (root cause of the stall).** The signature "body fully written, no response headers, 25–30 s" is backend/path, not local. The hedge escaping on a second connection (R2) proves it is **connection‑localized**, which narrows it to TCP HOL (loss on the hostile path) or a wedged h2 stream / slow frontend instance — **not** project/account throttling.
- **HOL / connection reuse (the amplifier).** On a shared h2 connection, a stalled bulk POST does not block other h2 streams at the application layer, but **TCP‑level** loss stalls every stream on that socket, and connection‑level flow‑control exhaustion from a big in‑flight body can starve small PAT/GETs. This is why moving text to a *different carrier on a different pool* helped — it changed the **connection**, not (mainly) the field.
- **Application scheduler queues (the domino — removable).** The slow main arm showed normal‑download **slot‑wait p95 8.44 s** vs 0.5–0.6 s elsewhere: one wedged Google request **pinned a bounded worker slot**, backing up unrelated traffic. That is a scheduler head‑of‑line domino independent of HTTP.
- **Request budgets.** The 64 KiB priority budget + 8‑object burst / 4‑per‑second process budget interact with stalls: when the budgeted slots are occupied by a stalled op, the budget can't refill, so the domino and the budget reinforce each other.
- **Drive visibility (~0.5–1 s floor).** Publish‑to‑observed ~447 ms median / 966 ms p95 (obs). This bounds interactive latency for *any* Drive design but is unrelated to the multi‑second stalls.
- **CPU/JSON expansion, cleanup contention, quota.** No evidence any of these caused the stalls: arms stalled with **zero** API errors, units stayed under guard, and cleanup is deferred off the hot path. Rule them out as primary; keep cleanup contention on watch at 10 clients (37–46 s sweeps, obs).

**Measurements to distinguish the surviving candidates:** (1) per‑request lifecycle timestamps — connect / TLS / write‑done / first‑byte / status — bucketed by carrier and pool (the trace exists; publish the distributions, not sums). (2) TCP retransmit / RTT counters per connection to separate path loss from backend wait. (3) slot‑wait vs request‑stall decomposition per class. (4) same object via two simultaneous connections to quantify how often the second wins and by how much (connection‑localization rate).

**Can decoupled pools / fair clocks remove the domino, not just patch a threshold?** **Partly — and that is the right fix for the part that is removable.** Class‑decoupled pools (interactive carrier on its own connection, bulk on another) remove TCP/connection‑flow coupling; a per‑stream virtual‑time (fair‑clock) scheduler stops any one stream monopolizing slots; and a **bounded slot‑hold** (a request with no response headers after T ms yields its worker to a replacement rather than pinning it for 30 s) directly dissolves the observed domino. Together these remove the *scheduler‑level* domino structurally, not by tuning a magic number. They **cannot** remove the *Google backend stall itself* — only hedging/connection diversity survives that, and only idempotent generated‑ID reuse keeps "survive" from meaning "double‑write." So: domino removable; root stall survivable, never removable from the client side. Honest decomposition, not a threshold patch.

---

## Deliverable 4 — One coherent protocol from first principles

**Name it a two‑carrier, capability‑negotiated, poll‑asymmetric transport.** It exists because the two traffic classes have opposite optima and the two directions have opposite reachability.

**Carriers.**
- *Interactive carrier:* a per‑direction, per‑(client‑ID, run‑ID) **fixed‑ID description ring**. Single writer; each PATCH publishes the cumulative set of **unacked** sealed frames (≤ ~6 KiB plaintext after base64+AEAD). Readers tolerate skipped Drive versions because the snapshot is cumulative. No per‑message object → **no discovery, no cleanup** for this class.
- *Bulk carrier:* immutable **media objects**, as large as HOL tolerance allows, **generated‑ID** for idempotent retry, on a **connection pool isolated** from the interactive carrier, with **stall‑state hedging**.

**Discovery, by direction (R3).** Bootstrap and every bulk object are **list‑discovered** (the universal floor). Steady‑state interactive discovery is known‑ID GET. Upstream (client→exit) idle wake is **exit‑side push**; downstream (exit→client) is always client poll with adaptive cadence (fast during the client's own send‑epoch, exponential back‑off when idle).

**ACK taxonomy and the invariant each carries.**
- *Upload ACK* = write returned 200 ⇒ bytes are durable in Drive. It does **not** authorize freeing the sender's pre‑upload buffer, because an immutable object may be cleaned before the peer reads it. Ring: "this version is published."
- *Accepted ACK* = receiver fetched and AEAD‑verified the frame, advanced a **cumulative** accepted‑cursor on the reverse ring ⇒ sender may drop that frame from the ring and allow object cleanup. It does **not** mean the bytes reached the destination socket (the slot‑ACK caveat the records correctly raise — an out‑of‑order or queued frame can be "accepted" yet undelivered).
- *Remote‑write ACK* = exit wrote the bytes to the real destination TCP socket ⇒ sender may release flow‑control window. Across a crash this ACK may be lost ⇒ at‑least‑once is possible ⇒ **never auto‑replay an OPEN that carried side‑effecting initial bytes.** Resolution: surface the stream as failed and let the application open a new connection (TCP semantics). Exactly‑once at an arbitrary destination is **not** promised, and the protocol must be able to report "unknown" for the window between socket‑write and durable record.

**Bounded state, no unsafe replay, sparse idle.** Ring holds only unacked frames (hard ~6 KiB bound → overflow applies backpressure by demoting that stream's surplus to a media object, never by dropping order). Dedup digest cache bounded (e.g., 30 min / 65 536 names, as prototyped). Outstanding bulk IDs tracked in a bounded ring so a crashed sender re‑lists its own prefix and deletes orphans on restart; nothing outlives the janitor age. Idle: empty ring + exponential poll back‑off + exit push‑wake ⇒ near‑zero steady units when quiet.

**Cleanup on lost ACK/crash.** Ring objects are deleted only at teardown (overwrite during operation). Media objects: deferred delete, then age janitor, then startup orphan‑sweep by own prefix. A lost accepted‑ACK only costs one redundant retransmit into the ring (idempotent) or one extra media object (janitored) — never corruption, never unbounded growth.

**Backward compatibility and capability negotiation.** The **list‑discovered OPEN is the compatibility floor** every peer understands; it carries a capability bitmap (ring‑capable, ring IDs, push‑capable, H3‑capable). Two peers that both advertise ring + matching IDs migrate that stream's interactive frames to the ring; old peers never learn the ring IDs and transparently stay on immutable list‑discovered objects. No flag day.

**Operations — unavoidable vs eliminated.**
- *Unavoidable:* 1 OPEN + 1 reply to bootstrap any stream (poll‑only client floor); 1 media GET (200 u) per bulk fetch above the inline cap (irreducible — Drive is not a streaming API); the ~0.5–1 s write‑visibility delay downstream (a Google backend property, measured, not a theorem).
- *Eliminated for interactive vs baseline:* the 100‑unit list (→5‑unit get), the 200‑unit media GET (→inline), the 50‑unit delete (→overwrite). Per steady interactive message ≈ one 50‑unit PATCH + cheap polling, versus ~400 units and three on‑path ops today.

---

## Deliverable 5 — Frontier and decision

The optima genuinely diverge; name them rather than pretend one design wins everything:

| Objective | Winner | Why / cost |
|---|---|---|
| Fastest chat | Known‑ID ring | 2 on‑path ops, ~0.5–1 s Drive floor, no cleanup; capped at ~6 KiB/snapshot; needs bootstrap + single‑writer |
| Fastest bulk | Large immutable media, isolated pool, stall‑hedge (H3 exit‑side optional) | media GET (200 u) irreducible; stall is the ceiling, survived not removed |
| Best mixed | Hybrid ring+media, class‑split pools, fair clock | the synthesis; promote only after Experiment A shows thin integration keeps the substrate win |
| Lowest quota | Ring (5‑u polls) + inline‑description for small + exit‑push upstream | kills list, media GET, and delete for small traffic |
| Easiest UX | mux v4 baseline | no bootstrap, multi‑version safe, no new reachability — keep as default and floor |

**User options vs automatic controls.** The **route** (pinned `www.google.com` / default front / Direct) stays a **manual** user choice — it is a security/reachability decision, never auto‑switched (the records already enforce that refresh/retry/redirect never silently cross routes; keep it). Everything else should be **automatic with hard rollback gates**: carrier selection (quiet small stream → ring; sustained → media, reusing the existing 64 KiB‑demotion signal), poll cadence, hedge triggering, pool isolation, capability negotiation. The *only* extra user‑facing knob worth exposing is a single mobile‑oriented "latency vs battery/quota" bound on idle poll aggressiveness — because on a phone, polling cadence is a real power/data cost, not just a quota number.

**Evidence that lets the team stop (the promotion gate).** A same‑day, order‑balanced ABBA in which the thin‑integrated hybrid (Experiment A) + bulk hardening (Experiment B) **simultaneously**: beats mux v4 on interactive p95, **matches or beats** bulk goodput (no repeat of the five‑client 25.0→20.4 Mb/s regression), stays ≤1.15× units, at 16 **and** 100 MiB, 5 **and** 10 clients, both orders, with zero hash failures, zero stale/duplicate objects, and a clean restart/reconnect — **repeated on ≥2 separate days** to clear Google‑latency variance. Meeting that = stop tuning, ship opt‑in, then flip the default. Anything less keeps mux v4 the default, exactly as the records concluded.

**What finite tests can never prove.** That no lower‑latency/higher‑rate design exists (no global‑optimum claim is provable). That the service supports 200 000 sustained users (short shared‑account runs cannot establish multi‑day quota, backend, or population‑tail behavior; per‑user projects redistribute allocation but do not make any mailbox unbounded). That rare 25–60 s backend stalls won't occur (they are tail events — you can bound their frequency in a sample, never their absence). That Google's visibility latency, quota model, description cap, or route behavior will hold (all are external and can change). The ~0.5–1 s downstream visibility floor is a *measured property*, not a guarantee.

---

## Prior positives the attachments under‑credited (and correct rejections)

**Under‑credited / worth re‑opening:**
- *Inline‑description download* (32 MiB: units 19 905→12 355, bulk 36.4→39.8 Mb/s, no bulk loss) — a clean positive that was overshadowed by the heavier slot‑delivery work; it is the lowest‑risk deployment and was never driven to a full gate (candidate #2).
- *Metadata‑only create* (56% faster than multipart, 0 media bytes confirmed, removes the 200‑u media GET) — correctly characterized as a primitive, but its safe productization (immutable + inline list, not the mutable ring) wasn't pursued.
- *Substrate two‑slot mixed* (text −71/−73%, bulk **+13/+16%**, units lower) — the strongest signal in the corpus; the subsequent integrated regression led the records toward the wrong conclusion about the **carrier** when the data indicts the **coordination wrapper** (R1). Experiment A is the fair re‑test.
- *Second‑connection rescue* (12/12 wins, 3.7–4.1 s vs 20 s+ stalls) — the mechanism is sound; only the **fixed‑delay trigger** was weak. A stall‑**state** trigger was never tried (Experiment B).
- *Exit‑side push* — dismissed as symmetric; half of it (upstream, exit callback) is free (R3, Experiment C).

**Correctly rejected (do not revisit as defaults):** indiscriminate fixed‑delay hedging (amplified units); full H3 on the **client** mux (UDP reachability + observed stalls); `files.update` media ring for bulk (write tails); the slot/processing ACK described as durable delivery (it is not — out‑of‑order/queued frames can be "accepted" yet undelivered); widening the 30 s startup catch‑up window (replay risk).

## Facts I cannot infer — verify against official docs (named, since I cannot browse)
- **`description` hard byte cap** (tested ~8 KiB; Google publishes no number) → Drive `files` reference. Decisive for ring capacity.
- Whether **metadata‑only create vs media create** cost the same unit (records assume 50) → Drive API **limits/quota** guide.
- **`properties`/`appProperties`** exact per‑entry and total caps (inf ~124 B/entry) → Drive **properties** guide.
- **Filename** length cap (unk/large) and `name contains` prefix semantics → Drive **search‑terms** guide.
- **Resumable upload partial readability** (inf: not readable until finalized — kills #19) → **manage‑uploads** guide.
- **Generated‑ID idempotency** guarantee across indeterminate failure (load‑bearing for Experiment B) → **manage‑uploads** guide.
- **Batch** endpoint current support, no‑media and no‑quota‑discount claims → Drive batch reference (Google retired several global batch endpoints).
- **`files.watch`/`changes.watch`** notice‑latency distribution, channel address/renewal, and whether `files.watch` needs no billing while **Workspace Events** does → Drive **push** reference + **Workspace Events / create‑subscription** guides.
- **HTTP/3 for Google APIs** on the split SNI/authority pinned front → Cloud APIs HTTP guidance.

Net recommendation: keep mux v4 as the default and compatibility floor; pursue, in order, **(#2) inline‑description for small payloads** (lowest risk, already positive), **Experiment A** (thin ring, to settle R1), and **Experiment B** (stall‑state hedge + bulk isolation, to clear the 100 MiB gate), with **Experiment C** sizing the real per‑direction quota ceiling. The hybrid (#1) is the destination, but only after A proves the integration can be as thin as the probe.
