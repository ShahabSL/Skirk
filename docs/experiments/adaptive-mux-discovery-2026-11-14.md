# Adaptive mux discovery (opt-in)

`drive.adaptive_mux_discovery=true` keeps a single legacy prefix query when the
mailbox is sparse. If that query reports a next page or `incompleteSearch`, the
receiver also queries p0 by class for that poll. This finds a late text object
even when thousands of newer p1 file objects fill the broad result. It does
not change the mux wire format, encryption, route, peer negotiation, or
cleanup. Old peers' untagged object names remain visible. This flag takes
precedence over `drive.combined_mux_discovery` if both are set.

DriveStore's adaptive scan requests one broad page at the configured normal
list page size (1,000 by default). If that page is partial, it queries p0
before continuing the broad scan for up to three more pages. The p0 fallback
is limited to four pages of 100. A dense poll therefore makes at most eight
discovery files.list requests without a rejected-token retry, and a sparse
poll makes one. The default split strategy remains unchanged. Normal p1
discovery in the default split strategy can return up to four pages of 1,000
objects; adaptive discovery has the same broad object count ceiling. Under
a dense backlog, p0 waits for one broad page, rather than all four, before
its separate query. Live latency and quota still need measurement. The broad page token
continues across polls; the freshness cursor advances only after a complete
query, and a rejected page token restarts at the same cursor. Incomplete
results are retried from the same cursor even if Google returns a page token.
If a continuation finishes empty, the first page still advances the cursor.
Failed queue insertion retains the existing cursor rewind and prevents the
same poll from continuing from a stale page token.

If a p0 object fails to enqueue, adaptive discovery remembers its exact name
and Drive ID even after the broad list completes. The next sparse poll still
queries p0. After a complete p0 query, at most four exact-ID metadata checks
per poll can recover an object omitted by search, or retire a retry after
two 404s separated by a 30-second cooldown, confirmed trash, rename, or a
move out of the mailbox. Oldest unprobed IDs are checked first. A surviving
ID is checked at most once every 30 seconds. Omitted mailbox membership
fields leave the retry pending; no search miss alone discards a real object.

The deterministic test creates 8,001 newer p1 objects, inserts an older p0
after the first truncated poll, and verifies the next poll queries p0 between
broad page one and broad page two, then continues and eventually completes.
Other tests cover a one-list sparse poll with an old-peer object, incomplete
searches with and without page tokens, an empty final page, a rejected page
token, queue backpressure, a failed p0 enqueue followed by sparse recovery,
confirmed deletion, mailbox membership, and the exact-ID check cap/fairness. These
tests prove the discovery and
cursor behavior, not Google-side latency, bulk goodput, or long-run quota.
Keep this mode opt-in until bounded live A/B runs measure all three.
