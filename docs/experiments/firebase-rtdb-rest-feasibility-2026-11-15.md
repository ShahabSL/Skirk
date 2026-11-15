# Realtime Database Spark REST feasibility

Status: primary-document research only. No database, registration, grant, rule,
credential, or live request was created for this candidate. In particular,
acceptance of a `www.google.com` pinned front by a Realtime Database authority
has not been tested. This is a new candidate, not a measured transport result.

## What the current primary sources establish

| Question | Documented result |
| --- | --- |
| Free capacity | Spark lists 100 simultaneous connections, 1 GB stored and 10 GB/month downloaded. It does not list Firestore's 20,000-writes/day free allowance. Spark is not a centralized 200,000-concurrent-user free service. [Pricing](https://firebase.google.com/pricing), [limits](https://firebase.google.com/docs/database/usage/limits). |
| Quota accounting | Downloads include session-layer connection/encryption overhead; counting encrypted message bodies alone understates usage. [Billing](https://firebase.google.com/docs/database/usage/billing). |
| Conditional writes | Request an ETag using `X-Firebase-ETag: true`; conditional PUT and DELETE reject mismatches with 412. Conditional PATCH is unsupported. This is an explicit REST contract, unlike the currently inconclusive Drive v3 metadata-header documentation. [REST reference](https://firebase.google.com/docs/reference/rest/database). |
| Notifications | REST supports SSE with `Accept: text/event-stream`, including 307 redirects. Named events include state replacements/patches, keep-alive, cancellation and revoked authentication. Notifications still need a cumulative reliable protocol above them. [REST reference](https://firebase.google.com/docs/reference/rest/database). |
| Authentication | REST documents OAuth2 bearer headers and the `firebase.database` plus `userinfo.email` scopes, or Firebase Authentication ID tokens. Existing Drive-only or cloud-platform grants are not assumed sufficient. An owner/service-account credential cannot become a client profile. [REST authentication](https://firebase.google.com/docs/database/rest/auth). |
| Hostnames | The endpoint depends on database location, using `firebaseio.com` or a regional `firebasedatabase.app` name. This does not establish domain-front compatibility. [Locations](https://firebase.google.com/docs/database/locations). |

Context7's official Firebase corpus was consulted for the conditional-write/SSE
contract. The SSE page's generic authentication examples use the `auth` query
parameter, while the authentication guide documents OAuth bearer headers for
REST. Header-authenticated SSE must therefore be verified rather than inferred
from a successful ordinary GET. No token-bearing URL is needed in a report.

## Small next experiment, only after review

Use an explicitly owned Spark database with billing unattached and public access
denied. Adding Firebase/Realtime Database registration, enabling an API, creating
the database, or obtaining additional scopes is a separate concrete setup step;
none is authorized or performed by this document.

The first fixture should own one fresh nonce path and no other data, cap ordinary
REST dispatches at 20, and allow one at-most-30-second, one-MiB SSE stream. Keep
the selected Google IP and TLS SNI `www.google.com` for every hop, while setting
only the exact owned Realtime Database authority. Record redirects before
following: accept only a reviewed Google backend and the same owned database
namespace/path, never forward credentials to an arbitrary Location or silently
switch to a direct route.

First demonstrate authenticated GET/conditional PUT while unauthenticated access
is denied. Then open header-authenticated SSE and publish two random AEAD state
updates; verify the exact ordered state, then exercise valid/stale ETags and
readback preservation. Test cancellation/EOF and exact null readback cleanup,
with independent bounded cleanup reservations and all raw error counters kept.
This is route/primitive feasibility, not TCP throughput or a latency comparison.

The existing buffered `GoogleHTTPClient.RequestSingleAttempt` cannot deliver
events until a streaming response ends. A test-only bounded HTTP/2 response-body
reader would be required, preserving the same selected front, peer-auth checks,
redirect fence, cancellation ownership, byte cap, and conservative replay
reservations. There is no justification for adding a native/direct SDK fallback.

If fronted SSE works, a later carrier could expose an opaque server write-fence
token rather than a Firestore-specific timestamp. The cumulative queue,
processing receipts, credits, exact-ciphertext retry, physical fallback identity,
and replay ledger remain necessary. No architecture or capacity win follows
before an actual mixed SOCKS/file/text experiment and least-privilege onboarding.
