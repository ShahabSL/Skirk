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

The final diagnostic CLI is frozen from clean source
`6ecd41e92e44df1256c3bb8fb14a81ff6d8e0919`, SHA-256
`bba4bc72e1964ab521d7e65f7739ae413fb601600231979cd7978d0ed026ae6f`.
`go version -m` confirms that source revision and `vcs.modified=false`.
Its Go version, dependencies/checksums, target, CGO setting, trimpath/compiler/
build-mode flags all match frozen c47, apart from the expected VCS revision/time.
The production Go diff SHA-256 is
`ada839b424b4c0e684aca19ae4fedb9ab6a7dd1eb48296541ac37fc9d4c38f6a`;
only `metadata_slot_shadow.go` differs.

Build provenance required a correction. The installed Go 1.26.4
`cmd/go/internal/vcs/vcs.go` recognizes `.git` directories, not linked-worktree
`.git` files. Two initial worktree builds (auto and explicit buildvcs) omitted
VCS metadata; both have SHA-256
`dabfc65ff9a9186dfe735f2ea773c9308bfe07248f8c47d7762c6be220ad0ad2` and
are retained but excluded. The final build used a private local shared clone
with a real `.git` directory, detached at the same reviewed clean commit:

```sh
go build -trimpath -buildvcs=true -o ../skirk-slot-budget-diagnostic-final ./cmd/skirk
```

Private executable, initial artifacts, test logs, exact build metadata and
`provenance.json` are retained in the diagnostic worktree under
`.skirk-runs/slot-budget-diagnostic-20261115/`. No binary is tracked or pushed.
The observer adds bba4's exact hash/source in a separate harness commit; the
CLI is not rebuilt for that registry/docs update. Historical generic slot-stop
runs remain unknown/stopped and are not recast as healthy.
