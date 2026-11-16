# Fronted carrier gap review: idle cost versus interactive delay

This began as an independent source and documentation review of the now 228-entry
[experiment catalog](catalog/experiments.json) and [twenty-design matrix](../drive-architecture-candidates-2026-11-15.md).
The original review added no live samples; subsequent linked primitives are
reported separately and incomparable primitive and full-tunnel workloads are
not ranked as if they were the same test. The client must retain
the user-selected pinned `www.google.com` route by default, explicit Direct
choice, and free-service requirements.

## One material unclosed gap

The ordinary receive loop pauses a client with no open stream, but a quiet
exit still lists for new sessions roughly once per second
(`internal/skirk/mux.go`, `runReceiveLoop` and `pollDelay`). The published
[Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits)
charges `files.list` **100 units**, metadata `files.get` **5**, metadata
`files.update` **50**, and `changes.watch` or `files.watch` **100**. For one
continuously running idle exit at exactly one list per second, this is
`100 × 60 = 6,000 units/min` and `100 × 86,400 = 8.64 million units/day`,
before uploads, downloads, extra pages, retries, other apps, or cleanup.
At Google's current 1 million-unit/min project limit and 400 million-unit/day
free billing threshold, the corresponding **arithmetic-only** ceilings are
166 such idle exits at once by minute and 46 continuously by day per shared
project. An exit can serve multiple clients, so these are not user counts.
The actual cadence and account/project layout must be measured over a full
day before any capacity claim.

Known-ID metadata polling would cost 5 units per read: **432,000 units/day**
for one read per second, a 20× reduction from one list per second. The live
[ID-train primitive](id-train-live-2026-11-15.md) measured 465 versus 1,105
core units over eight records, while its descriptive scheduled p95 was
0.829 versus 0.775 seconds. That result excludes authenticated ID exchange
and is not evidence that a product known-ID queue has lower messenger RTT.
The Drive v2 conditional queue already researches the known-ID path; its
distributed setup, epoch rotation, crash recovery and fair quota admission
are still unfinished. Another ID encoding does not remove those obligations.

Watch notifications are a possible **exit-only idle wakeup** when that exit
can host a public HTTPS callback. A per-file watch costs 100 units per
registration and expires within one day; a change-feed watch costs 100 and
expires within one week, plus `changes.list` (100) to identify changed files.
Google says notification deliveries themselves are not Drive quota charges.
[Official notifications guide](https://developers.google.com/workspace/drive/api/guides/push),
[official quota table](https://developers.google.com/workspace/drive/api/guides/limits).
The existing [pinned-front live probe](drive-push-notification-live-2026-11-15.md)
measured median post-PATCH `files.watch` notification **2.53 s** and
`changes.watch` **5.45 s** (one of six change notifications missed a 10 s
window), against immediate known-ID GET medians **0.188/0.159 s** in those
arms. Thus watch may save idle units but worsens a cold interactive wakeup
on present evidence. It cannot deliver an unknown object to a phone without
a reachable callback; Google-front client reachability and exit webhook
reachability are different properties. Missed or duplicate notifications,
channel renewal and periodic reconciliation remain mandatory.

The subsequent [pinned-front change-watermark probe](drive-change-watermark-live-2026-11-16.md)
tested 5-unit v2 `about.get(largestChangeId)` and v3
`changes.getStartPageToken` against exact `files.list`. Both eventually
advanced on six same-account file mutations, but in five phases the exact
list was already current while a confirming read of **both** signals still
returned the old value. They first changed around 2.3–2.7 s in those phases,
versus list visibility around 0.5–0.7 s. They may reduce idle list calls only
as advisory signals with periodic reconciliation; neither preserves the
fastest cold OPEN as a sole gate. A fixed known-ID mailbox GET is a distinct
candidate because it directly reads mailbox state rather than an account
change signal.

## Why no new general carrier was promoted

The older fresh-comment/reply idea pays a 50-unit create and a 100-unit list
to discover a new comment. A **known** precreated comment can instead be
read for 5 units while replies append for 50; the [bounded pinned-front
primitive](drive-known-comment-live-2026-11-16.md) delivered 32 small records
but saw p95 start-to-visible rise from 1.16 s at one writer to 7.60 s at
eight, then stopped on an unclassified 8 KiB HTTP 403. Its run overlapped a
local Go suite and was not a matched full-tunnel ranking. Retained reply
growth, polling, cross-account access and cleanup still matter. The official
[quota table](https://developers.google.com/workspace/drive/api/guides/limits)
supports those method costs, not a latency advantage. Filename and
property payloads, immutable ID trains, changes feeds, Apps Script, Pub/Sub,
Firestore, Drive watch and v2 conditional metadata all have recorded
primitive or mixed evidence in the catalog. None establishes a universally
faster, free, front-compatible and long-lived replacement. Pub/Sub's first
10 GiB/month is free but usage beyond it is priced
([official pricing](https://cloud.google.com/pubsub/pricing)); Firestore's
free Standard database has 20,000 writes/day
([official quotas](https://firebase.google.com/docs/firestore/quotas)).

**Next discriminating experiment:** compare an idle exit's actual 24-hour
Drive units and cold OPEN p95/p99 under (a) ordinary list polling, (b) a
versioned known-ID queue with full restart/rotation, and (c) an exit-only
change watch with periodic list reconciliation. Keep the same account, front,
client schedule, exact cleanup proof and no paid dependency. If a candidate
saves idle units but makes cold OPEN materially slower, expose that as a
user-visible tradeoff rather than silently changing the default.
