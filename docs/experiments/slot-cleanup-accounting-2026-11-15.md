# Scoped slot cleanup absence accounting

The first sustained async run completed its 100 MiB file, then stopped when the
strict harness observed four metadata `notFound` errors. Client and exit logs
showed their 64th slot publications and writer stops before that counter change.
The source provides a precise matching explanation: closing the client-owned
link deletes two slot IDs and verifies each deletion with two expected 404 GETs.
Individual 404 HTTP timing was disabled in that run, so the live attribution
remains an inference. The previous incomplete result is retained unchanged.

The correction marks only these post-delete checks with a private context bound
to the exact ID and name. `GetMetadataSlot` validates that pair before translating
the marker into the named `slot_cleanup_probe` operation. Its expected HTTP 404
still returns the existing not-found error to the cleanup caller; only its
classification as an unexpected API error changes. Four verification requests
still cost four calls and twenty estimated units. Wire requests, fallback,
slot lifetimes, retry policy, quotas and defaults remain unchanged.

Normal slot polling, mismatched ID/name markers and media GETs retain normal
error accounting. Probe 401, 403, 429 and request deadlines remain errors. A
recovered 500 followed by the expected 404 still records the failed attempt.
Ordinary `DeleteID` idempotent 404 accounting is deliberately not broadened by
this correction.

Validation: real `DriveStore` fake-HTTP tests exercise the actual retirement
callsite's DELETE/GET/GET sequence for both IDs, the four-call/twenty-unit probe
subtotal, exact context binding, media isolation, statuses, deadlines and
recovered failures. Full `go test ./...`, `go vet ./...` and the targeted slot
race suite pass. No live test or retrospective success is inferred from those
checks; a fresh separately labeled cohort must use the new binary hash.
