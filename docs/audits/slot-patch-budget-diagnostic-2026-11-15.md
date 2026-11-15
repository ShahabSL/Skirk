# Exact local slot-budget retirement diagnostic

This change affects one log classification only. It does not change wire
bytes, PATCH/GET caps, timers, API scheduling, handoff, publication ACKs,
fallback, delivery, cleanup, or defaults.

The existing 64-PATCH refusal returned a newly allocated error with text
`metadata slot shadow PATCH budget exhausted`. Its sanitized log reason was
the generic `transport_error`, also used by authentication/integrity and other
unknown errors. The benchmark observer must not infer healthy retirement from
that generic text or from a previously observed revision64.

That exact local refusal now returns a private error sentinel with the same
`Error()` text and the same false `errors.Is(err, errMetadataSlotClosed)`
result. Only that exact sentinel logs `reason=patch_budget_exhausted`. Same-text
new errors, generic closed errors, actual tampered AEAD failures, wrapped/joined
errors, and unknown errors retain their existing sanitized classification.
There is no new link field, runtime decision, or cause-propagation rule.

An initially considered reader closed follow-on was disproved by source
inspection: `metadataSlotPeer.receive` authenticates, checks revision/ACK and
accepts inbound state; those paths do not return `errMetadataSlotClosed`.
The writer exits after its budget refusal. No speculative retired-state flag
is retained, and no other error is reclassified because a link once retired.

The regression tests exercise the actual local cap without issuing another
PATCH, typed-error/text/closed semantics, genuine AEAD tampering and same-text
spoofs, writer join/owner wakeup with the reader still active, and byte-identical
immutable fallback through actual mux frame delivery. A larger preliminary
draft passed an earlier targeted race run; that evidence is not reused for the
final minimal source. The final snapshot, including an explicit original-error-
text assertion, passed `go test ./...`, `go vet ./...`, and the targeted race
suite in 6.625 seconds. The race selection includes the new budget diagnostics
and actual fallback tests plus existing writer/async and slot identity tests.
Independent read review found no blocker in the minimal production delta.

Runtime provenance: production Go sources (excluding test files), `go.mod`, and
`go.sum` have no differences between frozen source `63532c8` and this worktree's
base `287354a`. The only subsequent production Go difference is the sentinel
and log label in `metadata_slot_shadow.go`. The toolchain is Go 1.26.4 on
darwin/arm64, matching c47; its dependency versions/checksums and `-trimpath`
build flags will also be compared against the new diagnostic executable.

The diagnostic CLI will be built once from a clean reviewed source revision,
with a new binary hash recorded separately from frozen candidate c47 and main
340. The observer's source-bound read map/retirement whitelist must then add
that exact reviewed hash in a separate harness commit. Historical generic
slot-stop runs remain unknown/stopped; this logging improvement cannot recast
them as healthy.
