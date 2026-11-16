# Official Google carrier quotas and Skirk's free default — November 16, 2026

This is an independent reading of Google's current published limits, not a
new live test or a measurement of 200,000 users. The comparison assumes
Skirk must retain the user's route choice, with the pinned `www.google.com`
front as default, and that the default carrier must work without paid usage.
The Google front in Skirk's experiments is **observed behavior**, not an
availability guarantee in Google's API documentation.

## Firestore priority traffic does not fit a shared free project

The completed [hybrid versus Drive campaigns](pipelined-live-comparisons-2026-11-16.md#firestore-priority-plus-drive-bulk-versus-drive-only-control)
carried 16 MiB down, 16 MiB up, and 41 short echoes per arm. Their two
successful Firestore arms recorded **168 and 179 REST document writes**. The
hybrid had lower scheduled text p95 in those pairs, but these were two short,
single-client experiments, not sustained capacity tests. Each also retained
one HTTP 400 near observer teardown in the guard record.

Firestore Standard's free tier is **one eligible database per project**:
20,000 document writes, 50,000 reads, and 20,000 deletes per day, 1 GiB
stored, and 10 GiB outbound per month. Its free usage resets at midnight
Pacific; more usage requires billing. Listener result additions/updates cost
document reads, and reconnects can replay query charges. [Quotas](https://docs.cloud.google.com/firestore/quotas),
[pricing and listener accounting](https://cloud.google.com/firestore/pricing).

| Scenario, writes only | Arithmetic | Result |
| --- | ---: | ---: |
| Repeats of the observed arm in one project/day | `floor(20,000 / 179)` to `floor(20,000 / 168)` | **111–119** |
| One observed arm/day for each of 200,000 users | `200,000 × (168–179)` | **33.6–35.8 million writes/day** |
| Ideal free projects for that scenario | `(33.6–35.8 million) / 20,000` | **1,680–1,790** |
| One write/user/day in a shared project | `200,000 / 20,000` | **10×** the free writes |

These are workload equivalents, not a concurrent-user ceiling. Real capacity
is lower once other writes, reads, cleanup, retries, storage and egress are
included. Per-user Cloud projects distribute the quota only if each user
actually creates, authorizes and maintains one, with the exit granted the
appropriate access. That is a distinct, much harder setup flow; it is not a
shared free service with unlimited usage.

Google publishes no universal Firestore database-wide write-per-second cap.
Its guidance is to begin a new collection at no more than 500 operations/s,
increase traffic by at most 50% every five minutes, distribute document IDs,
and avoid hot documents. A collection with a sequential **indexed** field
can hit 500 writes/s until that index is exempted or sharded. These rate
considerations are separate from the free daily write budget.
[Best practices](https://docs.cloud.google.com/firestore/native/docs/best-practices),
[sequential-index limit](https://docs.cloud.google.com/firestore/native/docs/solutions/shard-timestamp).

**Named-database documentation conflict:** Google's current quota page and
the [`Database.free_tier` API field](https://docs.cloud.google.com/firestore/docs/reference/rpc/google.firestore.admin.v1)
say the first database created in a project without a free-tier database is
eligible. Google's [Standard pricing page](https://cloud.google.com/firestore/pricing)
still says named (non-default) databases require billing and receive no free
quota. The Skirk experiment used an eligible temporary database with billing
disabled, but that observation does not resolve the published contradiction
for all projects. Do not design a public free onboarding flow around named
databases without checking the target project's database eligibility and
billing behavior; the `(default)` database is the conservative documented
choice.

## Other published free allowances

| Service | Current official allowance or constraint | Relevance to a shared interactive carrier |
| --- | --- | --- |
| [Drive API](https://developers.google.com/workspace/drive/api/guides/limits) | New-project limit: 1,000,000 units/min/project; 325,000 units/min/user/project; 400,000,000 units/day/project before charges; 1 TB/day/project egress. Metadata `get` 5, `list` 100, `update` 50 units. Existing projects may retain older limits temporarily. | Highest documented free daily API budget among the tested carriers; Drive bulk and `drive.file` onboarding already work in the product. The published allowance still requires admission control and does not prove 200,000-user capacity. |
| [Pub/Sub](https://cloud.google.com/pubsub/pricing) | First 10 GiB/month of basic message delivery throughput is free **per billing account**. Publish and subscription throughput both count. | Cannot be an unbounded shared data carrier; 10 GiB divided among 200,000 users is only about 52 KiB/user/month before delivery duplication and protocol overhead. |
| [Apps Script](https://developers.google.com/apps-script/guides/services/quotas) | Consumer accounts: 20,000 URL Fetch calls/day, 30 simultaneous executions/user, six-minute execution limit. | The measured relay is useful research, but a centrally owned free deployment cannot assume independent quota for each user. |
| [Sheets API](https://developers.google.com/workspace/sheets/api/limits) / [Docs API](https://developers.google.com/workspace/docs/api/limits) | Sheets: 300 reads and 300 writes/min/project. Docs: 3,000 reads and 600 writes/min/project. | Even one read for each of 200,000 simultaneously active users would take at least 667 minutes on Sheets or 67 minutes on Docs at those project limits, before traffic or writes. |
| [Realtime Database Spark](https://firebase.google.com/pricing) | 100 simultaneous connections, 1 GB stored, 10 GB/month downloaded. | Useful real-time semantics but not a shared free 200,000-connection default. Its separate regional host also has no proved `www.google.com`-front route here. |
| [Cloud Storage](https://cloud.google.com/storage/pricing) | 5,000 Class A and 50,000 Class B operations/month, 5 GB-month stored in eligible regions. | Far smaller free object-operation budget than Drive. |

### Gmail draft mailbox: a research candidate, not a replacement

The [Gmail API](https://developers.google.com/workspace/gmail/api/reference/quota)
has 1.2 million units/min/project, 6,000 units/min/user/project, and an
80-million-unit/day/project threshold before planned charges. Draft creation,
update, list and get cost 10, 15, 5 and 20 units respectively; a full/raw
get costs 60. A [draft has a stable ID](https://developers.google.com/workspace/gmail/api/guides/drafts)
when its message is replaced, which could support a versioned mailbox. But
[`drafts.list` returns IDs rather than body bytes](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/list),
so receiving ciphertext requires another request. Draft access requires a
[restricted Gmail scope](https://developers.google.com/workspace/gmail/api/auth/scopes)
such as `gmail.compose`, much broader than Skirk's `drive.file` access.
Gmail [push notification requires Pub/Sub](https://developers.google.com/workspace/gmail/api/guides/push)
and does not directly push to a user's phone. Its Google-front behavior,
mixed-tunnel latency, race semantics, per-mailbox concurrency and cleanup
are untested. The smaller daily API threshold and mailbox-access burden do
not justify switching the default on quota arithmetic alone.

**Decision:** Keep Drive as Skirk's free default and retain the user-selected
Direct route. Continue to compare any priority carrier on full mixed traffic,
quota accounting, restart/recovery, multi-client fairness and exact cleanup.
Firestore may remain an explicitly provisioned, quota-aware optional
research mode; Gmail drafts need a separate bounded prototype before making
performance or compatibility claims.
