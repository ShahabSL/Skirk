# Fixed known-ID rendezvous: four interrupted live discriminators

Status: **no timed arm completed**. Four fresh, bounded attempts on the
secondary `shahabpre` test mailbox stopped at the first Drive v2 conditional
write capability check. The result cannot rank a hot fixed snapshot against
immutable create/list, nor against Xray-core XDRIVE's different prewarmed
known-ID ring. The product carrier and its Google-front default were not
changed.

The [isolated tool](../../tools/fixed_rendezvous_probe/README.md) uses the
same OAuth grant, visible folder, pinned `www.google.com` route, and quota
project on both sides. It durably journals exact generated IDs before create.
For each attempt it precreated an owned file, read its v2 resource ETag, and
sent a deliberately impossible quoted `If-Match` in a metadata PATCH. A
capable fence must reject that mutation with HTTP 412 and leave the original
description intact. Earlier owned-file probes did observe 412, but the v2
[file resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/files)
and [PATCH method](https://developers.google.com/workspace/drive/api/reference/rest/v2/files/patch)
do not promise that behavior permanently.

| Fresh attempt | PATCH projection/headers | Bogus `If-Match` response | Timed arms | Exact owned cleanup |
| --- | --- | ---: | ---: | --- |
| 01 | Full identity projection, JSON with charset, no-cache | HTTP 500 | 0 | ID 404 twice; prefix empty now and after 31 s |
| 02 | Same as 01 | HTTP 500 | 0 | ID 404 twice; prefix empty now and after 31 s |
| 03 | Minimal PATCH projection; identity still checked by GET | HTTP 500 | 0 | ID 404 twice; prefix empty now and after 31 s |
| 04 | Minimal projection and headers aligned to prior v2 probe | HTTP 500 | 0 | ID 404 twice; prefix empty now and after 31 s |

Each attempt dispatched three `generateIds`, one insert, two successful
metadata GETs, one PATCH, one delete, two prefix lists, and eight additional
GETs including cleanup verification. It reserved **415 nominal / 3,320
eightfold upper quota units**; the four attempts total **1,660 nominal /
13,280 eightfold upper**. Every attempt deleted its one created file and
verified exact 404 twice and an empty unique prefix immediately and after
31 seconds. Recovery ledgers were removed only after those checks. Private
raw reports are ignored under `.skirk-runs/fixed-rendezvous-live-20261116-0{1,2,3,4}.json`.

The repeated 500 is a negative **capability observation on this route and
fixture**, not proof that v2 CAS never works. The tool deliberately refuses to
interpret a server error as a stale-tag rejection, so no 1/2/4/8-writer
latency sample exists. The prior local fake still demonstrates a contention
crossover at four/eight writers, not Google performance. If this design is
revisited, first run the separate exact owned-file v2 ETag probe on a fresh
generation and inspect sanitized response categories; only a clean 412/200/412
fence sequence can authorize the timed comparison. Current research priority
should favor carriers with a documented or freshly verified write contract.

[Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits)
lists five units for metadata GET, 50 for metadata update, and 100 for list.
The eightfold figures are conservative guard reservations, not Google's
measured charge or a speed result.
