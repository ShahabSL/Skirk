# Adaptive mux discovery (opt-in)

`drive.adaptive_mux_discovery=true` keeps a single legacy prefix query when the
mailbox is sparse. If that query reports a next page or `incompleteSearch`, the
receiver also queries p0 by class for that poll. This finds a late text object
even when thousands of newer p1 file objects fill the broad result. It does
not change the mux wire format, encryption, route, peer negotiation, or
cleanup. Old peers' untagged object names remain visible. This flag takes
precedence over `drive.combined_mux_discovery` if both are set.

Each adaptive broad query is limited to four pages of 100 objects. The p0
fallback is also limited to four pages of 100. A dense poll therefore makes
at most eight discovery files.list requests without a rejected-token retry,
and a sparse poll makes one. The default
split strategy remains unchanged. Normal p1 discovery in the default split
strategy can return up to four pages of 1,000 objects, so the adaptive mode
may progress through a dense file backlog more slowly. The broad page token
continues across polls; the freshness cursor advances only after a complete
query, and a rejected page token restarts at the same cursor. Incomplete
results without a next token are retried from the same cursor. Failed queue
insertion retains the existing cursor rewind.

The deterministic test creates 4,001 newer p1 objects, inserts an older p0
after the first truncated poll, and verifies the next poll finds p0 through
the class fallback while broad pagination continues and eventually completes.
Other tests cover a one-list sparse poll with an old-peer object and an
incomplete search without a page token. These tests prove the discovery and
cursor behavior, not Google-side latency, bulk goodput, or long-run quota.
Keep this mode opt-in until bounded live A/B runs measure all three.
