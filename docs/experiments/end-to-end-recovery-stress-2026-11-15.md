# Separately labelled end-to-end recovery stress observation

This is an opt-in measurement policy, not a product transport or a clean-API
benchmark. Earlier strict, normal-upload-only, and read-plus-normal-upload arms
keep their original classifications and frozen executable pins. No earlier
failure becomes a success because this policy exists. No live result has yet
been measured with this policy at this source checkpoint.

The engineering question is whether the existing tunnel completes the exact
application workload after a finite number of observable RPC timeouts. A mutable
slot PATCH can time out before its existing exact readback or immutable fallback
finishes. Stopping immediately at that RPC error cannot answer the higher-level
question. Continuing without bounded outcome evidence would also be misleading.

The new `--bounded-end-to-end-recovery-stress` flag is mutually exclusive with the
historical policies and requires observation plus persistent framed text. Its
shared ceilings remain at most eight permitted timeouts per arm, four per rolling
minute, and sixteen per cohort. The rejecting ninth/fifth/seventeenth event still
remains in the observed counts. Existing quota-unit,
workload, progress, and cleanup-tail limits are unchanged. Authentication, quota,
backoff, terminal, integrity, unknown-error, and unclassified-stop evidence still
stops the arm. The harness observes recovery; it sends no retry of its own.

Normal uploads retain the existing `idempotent=true` requirement and pre-generated
Drive ID. Actual priority objects do **not** reserve a Drive ID in either frozen
source: their logger correctly says `idempotent=false`. The new policy records
these as a separate, source-bound logical-deduplication class. They reuse the same
prepared mux name and AEAD bytes, but their physical create is non-idempotent and
can leave duplicate Drive files. This is never described as API idempotency.
Every physical alias must be exact-prefix cleaned by the outer cleanup wrapper.

Metadata-only JSON creation is separately observed as operation `create`, with
the exact known `POST /drive/v3/files?fields=id,name,size` shape; slot setup uses a
different projection. Bare `create` deadlines remain fatal because the operation
label alone cannot distinguish those paths. Media creates use the multipart
`upload` operation. The report retains all class and operation counters; every
create/upload failure must match an eligible mux timeout, and all classes share
the same finite caps. Historical policies still reject priority upload timeouts.

## Mutable PATCH evidence

The diagnostic runtime preserves routing, wire contents, request scheduling,
timeouts, caps, exact-readback decisions, retirement, and fallback. Observation
adds target and sealed-snapshot SHA-256 digests, revision, a fixed readback budget,
and a controlled outcome. Raw target IDs, names, encrypted descriptions, tokens,
and payloads are not logged by these markers.

Only a typed, single-cause network/deadline timeout with a still-live parent
context begins a potentially permitted PATCH recovery. Joined causes,
authentication/API errors, cancellation, and error-text lookalikes do not. Every
original Drive error and operation counter remains visible. Ordinary API failure
lines do not contain the target/snapshot identity: their correlation to markers
is by role and operation count, not individual wire request identity. The markers
themselves bind the exact logical target, generation-bound encrypted snapshot,
revision, and subsequent outcome.

An exact readback must contain the identical sealed description and the expected
returned ID and name before the diagnostic can claim `exact_readback`. The
pre-existing runtime success decision is left unchanged even if a fake Store
violates the returned-identity contract; diagnostics then reject that evidence.

An empty or authenticated older readback is **unconfirmed publication**, never
proof that the PATCH did not commit. The existing writer is permanently retired
before `immutable_fallback` is emitted; the immutable path retains the original
prepared mux name and sealed bytes. A late original PATCH completion is possible.
Malformed, foreign-generation/direction/namespace, same-revision conflicting, or
newer-conflicting data does not receive that label. An actual authenticated
processing ACK that races with readback can be recorded as additional evidence;
it cannot excuse malformed readback data.
For an older snapshot, the read-only diagnostic also applies the existing
shadow-record admissibility rules in the sender's outbound direction: inner mux
AEAD, session/direction/sequence, namespace/lane key, single stream and DATA/FIN
frame kind. It does not claim validation of every optional name hint such as the
plain-size or frame-range projection.

Only the exact private fallback sentinel gets the dedicated shadow-stop label.
Wrapped/joined/same-text/closed/unknown errors stay generic and stop the observer.
The original unresolved error text and closed-error matching semantics remain
unchanged. The observer bounds pending outcomes by the source's 6/12-second
readback timer plus two seconds of observation allowance. It retains all hashes,
revisions, outcomes, counters, and failed correlations in the private report.

Finite reader-GET retirement is logged only at the actual 320-GET refusal branch,
without another GET. Owned two-minute lifetime expiry is identified before the
wrapper's own cancellation and only while the original parent remains live.
These markers never waive a concurrent generic error. The reader cap excludes
ambiguous-PATCH readbacks and cleanup GETs, whose ordinary quota accounting stays
intact. A fallback-publication marker proves publication, not remote processing.

## Application proof and teardown

Every successful final arm must retain the existing durable prelude, all unique
64-byte echo receipts, exact file size/hash and direction, plus origin-side-effect
proof. The origin baseline excludes exactly the successful shared warm-up and is
taken before the measured workload. Every later HTTP route/error is counted; the
selected bulk direction must start and finish exactly once, the echo connection
must be accepted once, and exactly one prelude plus the configured frames must
arrive. Duplicate requests/connections/frames, wrong direction, failed completion,
unexpected HTTP, overflow, or incomplete final counts reject the arm. Controlled
failure reasons and the triggering counts remain in the report.

This evidence covers the configured process epoch and observed origin effects.
It does not count failed TCP dial attempts, prove exactly-once behavior across a
process restart, or measure hidden HTTP/2 retries, TLS/header traffic, or physical
wire requests. All visible RPC errors remain errors even when the complete
application workload recovers.

Each A/B arm persists its primary workload outcome before teardown. Native owned
process identities and original unreaped Popen handles authorize individual
signals; names, PID guesses, process groups, and sudo do not. Every original
writer must be reaped before external Drive cleanup is allowed. Unresolved
children, identity/signal/manifest errors, and teardown failures prevent another
arm. Private profiles are removed and the independent teardown result is kept
alongside the original outcome.
The origin counters are revalidated after all registered writers, benchmark, and
curl children are reaped, with both pre-stop and post-stop snapshots durably
recorded. A duplicate first observed during teardown still rejects success. This
is an observed post-stop handler boundary, not a proof about future unobserved
effects or process restarts.

## Validation checkpoint

Source tests exercise committed response loss, empty/older/timeout readback,
late original completion after retirement, exact-target binding, wrong
generation/direction, mixed timeout/integrity causes, real mux acceptance before
processing ACK, parent versus owned lifetime, no extra GET/PATCH at a cap, and
byte-identical immutable fallback. Policy tests cover outcome identity/window,
shared error caps, per-operation counters, mandatory application proofs, and
side-effect failures. Exact final source, executable hash, commands, and test
results follow.

Final runtime source is `8c9829dc9ccbe98d62c4357d60ab762e02ac6373` on the
initial diagnostic change `3f595bf`. The initial source passed `go test ./...`
(internal/skirk 203.293 s), `go vet ./...`, and the focused slot
recovery/retirement/async adversarial race suite (4.436 s). The fixture was corrected
to perform the production receiver loop's actual `markProcessed` step after
successful mux processing, before constructing its authenticated ACK; the earlier
omitted step produced an ordinary exact-readback success and was retained as a
test-construction finding, not a runtime failure.

Independent review then identified the older-snapshot inner-authentication gap;
the final source adds read-only validation and six adversarial cases for tampered
inner ciphertext, wrong lane/direction/namespace, unsupported kind and stream
identity, plus an authentic prior DATA snapshot. Its focused race suite passed
4.590 s and vet passed; full final-source Go validation follows below.

The frozen final diagnostic CLI SHA-256 is
`2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b`.
It was built in a separate clean local clone with a real `.git` directory using
`go build -trimpath -buildvcs=true`, Go 1.26.4, darwin/arm64, CGO enabled.
`go version -m` reports the exact source above and `vcs.modified=false`.
Dependencies, compiler, platform, and flags match the preceding diagnostic CLI;
the only non-test Go changes since `6ecd41e` are the two documented diagnostic
files. The new Python registry authorizes this hash separately; it does not edit
historical policy registries or rebuild the runtime recursively.
The earlier clean `3f595bf` executable
`91ddba00f16eac93ce2a9adba8c386b01c0ff3187f31789df9c9263f1865a5c1`
is retained as build provenance and explicitly excluded from the live registry.

Final-source `go test ./...` passed (internal/skirk 199.021 s), the focused race
suite passed 4.590 s, and `go vet ./...` passed. Final frozen-registry and combined
Python policy/origin/finalizer/native-journal/benchmark validation passed 158 cases
in 3.819 s. The Python command, run from `tools`, was:

```sh
python3 -m unittest test_skirk_end_to_end_recovery test_skirk_stress_origin_proof test_skirk_ab_finalize test_skirk_concurrent_process_journal test_skirk_owned_processes test_skirk_read_upload_recovery test_skirk_bounded_recovery test_skirk_live_ab_scale test_skirk_persistent_text_bench test_skirk_benchmarks
```

The focused Go command was:

```sh
go test -race ./internal/skirk -run 'TestSlot(PatchRecovery|FiniteRetirement|PatchBudget)|TestSlotAsync(AmbiguousPATCH|UnresolvedPATCH|LateProcessingACK)' -count=1
```

The production-Go diff from preceding diagnostic source `6ecd41e` touches only
`metadata_slot_async.go` (3 insertions) and `metadata_slot_shadow.go` (142 insertions,
5 deletions), SHA-256
`33818a154d456bf1c2ee8dd7db873d542445147535f5134dacd2e57ac8147b66`.
The corresponding diff from frozen counterfix `63532c8` has SHA-256
`516ebfe301dd51f59b65ebae65af83358343db73cacce381cbd65fe1b7090d1f`.
No live Google request or payload benchmark was run by this implementation task.
