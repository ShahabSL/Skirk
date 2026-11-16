# Known-comment WAKE rendezvous: local fault model and live gate

Status: **test-only correctness model, no Google call, no transport or default
change**. Source: [`tools/comment_wake_model/model_test.go`](../../tools/comment_wake_model/model_test.go).
This evaluates a low-idle-cost bootstrap for a copied client profile and
future immutable, metadata-only known-ID channels. Bulk remains muxv5 in the
hypothesis. The current live evidence is the earlier
[known-comment primitive](drive-known-comment-live-2026-11-16.md), not this
model's synthetic timing.

## API facts and the critical refinement

[Drive `comments.get`](https://developers.google.com/workspace/drive/api/reference/rest/v3/comments/get)
requires an explicit `fields` projection. The
[Comment resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/comments)
defines `modifiedTime` as changing when a comment **or any reply** is modified,
and defines `replies[]` as the **full chronological list**. Thus an exact
known-comment poll with `fields=id,modifiedTime` has a small response regardless
of accumulated replies; requesting `replies` on every idle poll does not.
On change, [`replies.list`](https://developers.google.com/workspace/drive/api/reference/rest/v3/replies/list)
can fetch live replies, up to 100 per page, excluding deleted replies by
default. It costs 100 units per page. Page tokens can become unreliable as
items change; a reader must restart from page zero and dedupe by authenticated
attempt ID. A bounded inability to reach a stable scan is an explicit error.
The documented timestamp is **not** a promised unique change counter. If
two writes serialize to the same `modifiedTime`, a head-only watcher can miss
the only WAKE forever. The model includes this contract-permitted case.
Periodic reply-list reconciliation or another proven change signal is needed
for a bounded delivery guarantee; a live collision test alone cannot turn a
missing documentation guarantee into one.

The [comment/reply management guide](https://developers.google.com/workspace/drive/api/guides/manage-comments)
says only the creator of a comment or reply can edit or delete it. The
[Reply resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/replies)
retains deleted replies as no-content tombstones. If exit and client use the
**same Google identity**, as the existing same-grant live probe did, the exit
can plausibly delete the client's processed WAKE reply; that specific
permission and repeated cleanup still require live proof. If they use
different identities, the exit cannot prune the client's reply; the client
must do it. Distinct OAuth applications also have an unresolved `drive.file`
access question. No destination, OPEN bytes, OAuth token or user content may
appear in a Drive-visible comment: the fixture's WAKE/ACK envelope is AEAD
protected and 200–512 encoded bytes.

## Candidate protocol and local safety results

One stable comment ID lives in the copied kit. Each installation creates a
fresh runtime device identity and independent immutable channel IDs; pending
WAKE attempts are durable locally before publication. A WAKE includes device
ID, attempt ID, and channel IDs under AEAD. The exit polls only `id,modifiedTime`
while idle. On change it lists nondeleted replies without deleting during
pagination, verifies and deduplicates WAKEs, persists registrations before
posting authenticated ACK replies, then deletes processed WAKEs when creator
permission allows. The client retries the **same** attempt after an ambiguous
append or lost ACK. A stale ACK after exit restart cannot open an old exit
generation; the client re-WAKEs and the exit repairs the ACK from its durable
journal. ACK replies require bounded retention and deletion after client
receipt, with client retry if an ACK was pruned while it was offline. Active
known-ID channels need their own end-to-end proof; this model stops at WAKE.

The deterministic fake Drive tests cover:

- Two and eight copied profiles with independent runtime IDs, an encrypted
  registration envelope, and ciphertext tamper rejection.
- Append committed with response lost, append not committed, duplicate raw
  replies, ACK lost, exit crash before and after durable registration, ACK
  repair, and exit generation change. One attempt remains one logical
  registration in the fake journal.
- Creator-only deletion, tombstones, a fixed-size `modifiedTime` head despite
  500 deleted replies, and growth of the full `comments.get` response.
- A reply appended during pagination: restart and recover on a quiet pass;
  permanent churn: fail visibly after bounded retries. A coalesced projected
  modification time hides a real WAKE from head-only polling until a forced
  reply-list reconciliation.
- Comment rotation with an unconditioned anchor-file pointer: an older timed
  out update arriving after a later one rolls the pointer back to a deleted
  comment. Scoped orphan discovery and exact owned-comment cleanup preserve
  an unrelated sentinel in the fake.

These are state-machine outcomes under instant fake visibility, **not** Drive
latency, access, long-term retention, or product validation. They do not show
that a copied kit actually persists fresh device entropy or pending attempts
across every platform; those are implementation requirements. The model does
not rely on a server-side Drive conditional write.

## Why rotation is not a shortcut

The original full-`replies[]` plan must rotate or its read body grows without
bound. A file metadata pointer to the current comment looks attractive, but
without a server-enforced stale-write fence, a delayed older `files.update`
can roll that pointer back. Four fresh pinned-front v2 conditional-PATCH
[discriminators](fixed-rendezvous-live-2026-11-16.md) returned HTTP 500 before
timed arms; a reliable fence cannot be assumed. Immutable old-comment
successor links avoid rollback only by retaining an ever-growing cold-start
chain. A fixed anchor with unique monotonic property keys postpones reuse,
but [Drive custom properties](https://developers.google.com/workspace/drive/api/guides/properties)
are capped at 30 public and 30 private keys per file, and each key/value pair
is capped at 124 bytes. Reusing keys reintroduces delayed-write risk.

The stable `modifiedTime`/nondeleted-replies design needs no comment rotation
for **API response size** if cleanup succeeds. It does not prove that Drive
supports unlimited tombstones internally. If comment count or tombstone
retention eventually imposes a limit, a different bounded bootstrap or a
verified rotation protocol is required. The fake deliberately keeps
tombstones, and its full-comment response grows. The creator's deletion of a
comment is logical (`deleted:true`), so exact HTTP-404 behavior and its
effect on replies must be checked live before claiming complete cleanup.

## Quota envelope, not speed

The [current Drive limit table](https://developers.google.com/workspace/drive/api/guides/limits)
charges `comments.get` 5, `replies.list` 100, and comment/reply/file edits
50 units. It specifies 1,000,000 units/minute/project and a
400,000,000-unit/day **no-charge billing threshold**, not a hard daily cap.
One stable comment polled once per second costs 300 units/minute or 432,000
units/day **per mailbox**, before WAKEs, active lanes, bulk, and cleanup. A
single shared project can therefore cover at most 3,333 such idle mailboxes
under the minute cap or 925 under the daily free threshold, with no reserve.
For 200,000 continuously polled mailboxes the figures are 60 million
units/minute and 86.4 billion units/day. User-owned projects distribute the
project bucket; they do not make a single shared project free at that scale.

A minimal cold WAKE with the stable scan is at least **415 nominal units**:
client reply create 50; one idle and two scan-consistency comment GETs 15;
one exit reply-list page 100; ACK reply create 50; one client ACK-list page
100; WAKE and ACK deletes 100. This excludes retries, extra polls/pages,
backend throttling, immutable channel provisioning, and bulk. If the client
uses a full `comments.get` for ACK, one 100-unit list can become a 5-unit GET,
but its response grows with every historical tombstone. At one WAKE/minute,
the 415-unit lower bound adds 597,600 units/day to the 432,000-unit idle
baseline for that mailbox. A 30-second idle poll would reduce baseline units
by 30× but can add up to 30 seconds to a dormant text message's wake time.
The necessary reconciliation changes the idle envelope: one 100-unit
`replies.list` every 60 seconds adds 100 units/minute per mailbox and permits
up to 60 seconds of extra wake delay after a missed timestamp; doing it every
second adds 6,000 units/minute, for 6,300 total with the head poll. Neither
choice gives both a documented 1-second guarantee and the 300-unit/minute
low-idle cost.

The previous same-grant pinned-front live probe delivered 32 short replies,
but its 1/2/4/8-writer p95 rose **1.156 → 2.691 → 4.233 → 7.604 seconds**.
It used full-comment polling at 100 ms on a growing thread and overlapped a
local Go suite, so these are not fair performance rankings of the refined
stable-head design. It also hit an unclassified HTTP 403 on an 8 KiB reply;
200–512-byte WAKEs avoid that specific attempted size, but the 403 reason
could have been rate or permission related. No peak throughput, text RTT,
200,000-user capacity, or full SOCKS/TLS result follows.

## Bounded pinned-front live plan before any promotion

1. Use only a newly created owned test file/comment under `shahabpre`, the
   approved pinned `www.google.com` route, same OAuth grant, sealed synthetic
   WAKEs, a recovery ledger, and a shared active/cleanup quota guard. Never
   place secrets or destinations in comments. Capture sanitized 403/429
   reasons, status counts, physical dispatch counts, response bytes, and
   the identities/permissions of every reply. Keep muxv5 untouched.
2. On fresh equal-history comments, interleave 1/2/4/8 writers in an ABBA
   order with a quiet CPU. Measure append-to-head visibility, head-to-list
   discovery, ACK-to-client visibility, total dormant-to-ready p50/p95,
   bytes returned and quota units. Use 200, 256 and 512-byte sealed records;
   do not infer full-tunnel speed from this primitive.
3. Inject committed and uncommitted lost responses, exit crash before/after
   durable registration and ACK, client crash with a persisted pending WAKE,
   page mutation, same-timestamp or stale timestamp, stale generation,
   cross-OAuth-app `drive.file` denial, and
   retry after ACK pruning. Prove no duplicate active registration and no
   missing ACK under each bounded fault.
4. Stage repeated append/ACK/delete at 64 then 512 cycles only if each prior
   gate passes. Inspect `modifiedTime`, full-comment bytes, nondeleted
   `replies.list` pages, tombstone count, throttling/notifications, and
   response-size behavior. Cap all attempts, bytes, wall time, active units
   and cleanup reserve before publishing. An unclassified 403 or inability
   to finish a stable scan stops the run.
5. Delete only exact owned IDs, verify two 404 reads, an empty unique prefix
   immediately and after the usual delayed check, and retain the ledger on
   any incomplete cleanup. Only then run a frozen same-binary full SOCKS/TLS
   A/B with short-message p95, mixed bulk goodput, quota and recovery. A
   faster primitive alone is not a product win.

**Decision:** the single stable comment with projected head polling is a
plausible **research** bootstrap for mailboxes with few copies under one
Google identity; its
fake crash and deduplication paths pass. The coalesced-timestamp case is a
correctness counterexample to head-only polling. The unbounded full-replies
variant and unfenced rotation pointer are also negative. No WAKE variant is
ready for the product or for a claim of lower latency, higher speed, or
200,000-user free shared scale.
