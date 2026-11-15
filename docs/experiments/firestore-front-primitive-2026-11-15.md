# Owned Firestore documents through the Google front

All six bounded encrypted-document trials passed through Skirk's actual
`GoogleHTTPClient`, using `google_front_pinned`, pin `216.239.38.120` and TLS
front `www.google.com`. The logical API authority was `firestore.googleapis.com`.
All thirty measured create/read/delete/absence requests returned HTTP/2.
There was no client fallback to a direct service route.

| Plaintext | Create, ms | Known-ID read, ms | Delete, ms |
| --- | ---: | ---: | ---: |
| 2 KiB, fresh client | 608.630 | 152.657 | 2,213.977 |
| 2 KiB, reused client | 180.727 | 149.882 | 150.592 |
| 2 KiB, reused client | 166.828 | 178.839 | 141.076 |
| 16 KiB | 228.077 | 141.012 | 134.579 |
| 16 KiB | 266.199 | 134.466 | 157.940 |
| 256 KiB | 491.702 | 248.552 | 208.670 |

Every document held freshly generated AES-GCM ciphertext. Exact document
identity, ciphertext, decrypted bytes and plaintext SHA-256 were checked.
Creation used an exists-false precondition and a fresh owned namespace. Every
exact document was deleted and returned 404 immediately and after 31 seconds.
The two warm 2 KiB sequential create-plus-known-read observations were
330.609 and 345.667 ms. These are tiny primitive samples, not application RTT,
notifications, mixed messenger performance or a steady file throughput rate.
An initial 2.2-second delete also remains visible rather than being hidden.

Source was frozen at `9fe438f`, before the later relay-only integration. The
exact source/test-binary SHA-256 and all per-call observations are in the
[sanitized evidence](firestore-front-primitive-2026-11-15.json). Each visible
dispatch used `RequestSingleAttempt`; the guard conservatively reserved eight
possible HTTP/2 wire attempts per dispatch. Thirty visible calls therefore
reserve an upper bound of 240, not proof of 240 physical requests. The fixed
ceilings were 40 visible calls, 16 MiB encoded request-byte upper estimate and
1 MiB decompressed response per call. A separate four-call direct API control
verified the owned schema/authentication before the fronted probe; it was not
a transport fallback or a performance comparator.

The isolated project had billing disabled. Its fresh Standard Native database
reported `freeTier=true`, in `us-central1`, and the initial database list was
empty. Current published free limits include 50,000 document reads and 20,000
writes/deletes daily, with 10 GiB monthly outbound transfer. This makes an
optional small control carrier plausible; it does not establish an unlimited
bulk replacement or central 200,000-user capacity.
[Official free quotas](https://firebase.google.com/docs/firestore/quotas).

The research owner OAuth token was read from a private file and never included
in a product profile or tracked artifact. That credential is not equivalent to
Skirk's `drive.file` grant. Any product design would need its own least-privilege
Firebase authentication/rules and provisioning flow.
[REST authentication](https://firebase.google.com/docs/firestore/use-rest-api).

Whole-database deletion was requested after the object checks. The first
administrative-operation wait did not report completion within 60 seconds;
this is retained separately from the successful data plane. Subsequent exact
database GETs returned Google's `NOT_FOUND` immediately and after another
31 seconds. The previously disabled Firestore API was restored and independently
verified `DISABLED`, then the borrowed token file was removed. No billing,
Firebase Management setup, IAM grants, public rules or pre-existing data changed.

A pushed Listen/WebChannel probe is the next independent feasibility test.
Firestore's push path is not established by these CRUD measurements, and a
prior Pub/Sub gRPC-front 404 does not prove the different Firestore WebChannel
path fails. Neither this result nor the separate unloaded Apps Script echo
should be ranked directly against Drive's loaded full-TCP p95.
