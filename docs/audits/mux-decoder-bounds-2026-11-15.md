# Mux decoder allocation and integer bounds

A malformed batch could declare 65,535 frames in a tiny body and allocate the
frame array before the first truncated-header check. On a 32-bit target, a
declared payload size of `0x7fffffff` could also overflow `offset + size` and
reach an invalid slice instead of returning an error.

The decoder now proves that the remaining body can contain every fixed frame
header before allocating. Both header and payload bounds use subtraction from
the known remaining length, avoiding signed offset addition. Valid wire
representations round-trip unchanged, including empty batches and existing
counts above the sender's 512-frame scheduling bound.

The focused regression covers impossible counts, declared lengths around the
32-bit signed boundary, valid counts and canonical wire round-trips. An old
decoder source overlay failed the allocation regression as expected: two
allocations instead of the one error allocation. The overlay did not replace
tracked source. The hardened decoder and fuzz seeds passed under race in
1.460 seconds. A four-second native fuzz run completed 69,176 executions with
no failure. Linux/386 test-binary cross-compilation passed; this Mac did not
execute that Linux binary.

The existing Linux preflight target and the Linux x64 Smoke job now execute
`TestDecodeMuxBatchBounds` under `GOARCH=386 CGO_ENABLED=0`, so the signed-boundary
case is exercised on an actual 32-bit Go runtime in CI. Its result must be
recorded separately from local cross-compilation. These checks use public test
bytes and make no Google requests.

Private logs and the compiled test binary are under
`.skirk-runs/decoder-bounds-20261115/` in the isolated decoder worktree. Full
integrated preflight and final-source cross-platform checks remain required
before release; this research change does not publish a release.
