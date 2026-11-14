# Astra audit — November 14, 2026

This pass independently revisits the previous [experiment record](audit-experiments-2026.md),
the live harness, mux lifecycle, credential recovery, and the rejected Worker
architecture. Work remains on `codex/skirk-audit-experiments-20261114`; main was
not committed or pushed. Google front remains the default. Inline discovery
remains opt-in.

## Correctness before tuning

The new fault tests reproduce issues that successful short throughput tests
would miss:

- An exit could publish a stream before writing Open's initial bytes, allowing
  later DATA to overtake them. A separate lookup/register race could strand
  DATA/FIN after the opening queue had already been flushed. An explicit ready
  state and locked handoff preserve order and ensure pending frames are drained.
- Cancellation could close the local stream without signaling the exit. One
  bounded RST now closes an active canceled stream, including cancellation
  during Open acknowledgement. An already closed stream does not send another.
- Repeated priority discovery pages could indefinitely suppress normal-class
  discovery. Normal traffic now receives its existing throttled polling turn
  even when priority pagination continues.
- Transient Drive delete failures lost their queued task. Deletes now get a
  bounded retry schedule. Persistent failures still exhaust the budget and are
  logged for the stale-file janitor; this is not unlimited storage protection.
- Identical retried priority uploads can have different Drive IDs. Cleanup now
  tracks those IDs without replaying the frames. Duplicate deletion must wait
  until the selected payload has been successfully authenticated and processed.
- A late 401 for an old access token could invalidate a newer token. An older
  background refresh could also overwrite a newer foreground refresh. Token
  invalidation now checks the rejected token and refresh results carry a
  generation so superseded work cannot replace current credentials.
- The HTTP proxy could hold a completed response open waiting for a remote FIN.
  Its per-request context now ends with the response. Proxy credentials and
  Connection-nominated headers are removed before forwarding to the origin.

The tests exercise blocked initial writes, out-of-order frames, stream
cancellation, repeated discovery, transient deletion failure, delayed token
refresh, and an origin that sends a complete response without closing.
A primary-file 404 preserves a known duplicate but does not yet recover stream
delivery from that alternate; the fault test establishes retention, not recovery.

## Client lifecycle and persistence

iOS validates bounded JSON/gzip profiles before Keychain storage and verifies
tunnel/session/mailbox/OAuth identity before renewal. An ordinary import cannot
overwrite an existing saved profile. Desktop serializes connection startup
against profile mutations and atomically replaces private profile/settings files;
status polling no longer writes settings. Android bounds profile decoding and
rejects missing identity data. Recovery copy explains that Production removes
the Testing-specific seven-day expiry, not all possible revocations.

## Measurement changes

The previous live mixed harness checked curl exit/status and download counts,
but discarded response bodies. The updated harness optionally checks exact
length and SHA-256 using private temporary files that are removed on success,
failure, or timeout. Corruption fails the run even after HTTP 200. Scheduling
latency now starts at the intended dispatch time, including scheduler and
executor delay. The fixed cohort of all scheduled small requests is the primary
latency measure; overlap-only figures are diagnostic.

The A/B runner now refuses missing/stale metrics, checks both processes, includes
the final quota sample, limits total and rolling local estimated units, records
failed arms, and cleans up benchmark children and temporary credential copies.
These are application estimates, not independently verified Console accounting.

## Live failure reproduction and upload experiments

The new 64 MiB mixed run used 24 scheduled small requests at 250 ms intervals,
eight small-request workers, a 15-second cleanup tail, exact SHA-256 checks,
and Google front on both peers. The first baseline delivered all bytes at
67.454 Mbps with 2.958-second scheduled small-request p95 and 39,105 estimated
units. The next inline arm stalled before delivering any bulk bytes; all 24
small requests completed, but their scheduled p95 rose to 6.484 seconds. The
runner stopped and retained the failed arm instead of completing the planned
eight-arm comparison. This is not evidence of an inline win or regression:
the same stall pattern also occurred in an earlier baseline.

The exit had four normal uploads still pending when the 15-second stall guard
canceled the run. Separate bounded probes tested four concurrent 512 KiB or
1,048,631-byte uploads across eight fresh HTTP pools. All used pre-generated IDs,
the existing test account/folder, byte verification, and exact-ID cleanup.

| Probe | Verified files | Slowest upload | Estimated units | Observation |
|---|---:|---:|---:|---|
| Initial timing | 28/32 | Four timed out at 20 s | 8,840 | All four bodies were written locally by 2.94 s; no response headers arrived |
| Metadata check after 3 s | 32/32 | 19.580 s | 9,680 | Eight slow 512 KiB uploads still returned metadata 404 at 3 s |
| Same-ID second connection after 3 s | 32/32 | 4.000 s | 9,840 | Secondary leg won four times |
| Repeated second-connection probe | 32/32 | 4.061 s | 10,040 | Secondary leg won eight times, at both payload sizes |

The two retry probes verified 64/64 files. All twelve extra-leg wins completed
in 3.715–4.061 seconds, including cancellation and joining the original request.
A healthy 32-file probe uses 9,640 estimated units, so extra-leg overhead in
these runs was approximately 2.1% and 4.1%. These are sequential short probes,
not a randomized causal comparison or a guarantee of four-second completion.
The original requests were canceled, so their counterfactual completion times
are unknown. Smaller chunks did not reliably remove the stall, and the
metadata check did not provide an early completion acknowledgement.

[Google documents pre-generated IDs for safe retry after indeterminate failures](https://developers.google.com/workspace/drive/api/guides/manage-uploads).
The probe reuses the same ID, name, and bytes, and checks downloaded bytes after
success. A local write callback alone does not establish server acceptance.
Immediate delete success/404 also does not rule out a late server commit; the
test prefix needs a delayed final sweep. Source is `tools/drive_upload_probe/`;
raw timing results remain privately in `.skirk-runs/astra-upload-*.json`.

The delayed sweep found exactly four files (4,194,524 bytes) under the initial
timing probe's prefix and deleted them successfully. The other three probe
prefixes, including both second-connection runs, had no remaining files. This
is direct evidence that immediate cleanup after canceled creates was
insufficient in the first run. A separate sweep removed 21 stale test transport
files from the isolated mailbox before the next comparison.

`SKIRK_HTTP_TRACE=1` enables credential-free slow-request timing diagnostics in
the HTTP client (connection acquisition, write, first response byte, status and
cancellation). It does not log URLs, headers, bodies, tokens, or error contents.
It is disabled by default.

The resulting experimental setting is `drive.upload_hedge_delay_ms` (default
`0`, or an explicit 1000–10000 ms delay). It only applies to normal `muxv4`
uploads of at least 128 KiB with a pre-generated file ID. At most four extra
upload legs run concurrently per store. Each uses a separate HTTP pool with
the same route and credentials, shares quota/backoff accounting, and preserves
the existing bounded HTTP retries. First success cancels and joins the other
leg; a failure on one leg does not discard a pending success on the other.
An exhausted extra-leg slot skips the hedge. This is an experiment, not a
default or a quota increase. Google front and Direct remain user choices.

The subsequent full-tunnel ABBA experiment used the same 64 MiB / 24-small
workload and retained this less favorable result:

| Arm, in order | Bulk Mbps | Small successes | Scheduled small p95 | Estimated units including warmup/tail |
|---|---:|---:|---:|---:|
| Baseline | 21.631 | 24/24 | 6.450 s | 43,355 |
| Retry after 3 s | 37.531 | 24/24 | 8.912 s | 38,655 |
| Retry after 3 s | 19.681 | 24/24 | 19.991 s | 41,355 |
| Baseline, failed arm | 18.788 | 17/24 | Not comparable with seven stalled requests | Tail incomplete |

Every completed payload passed byte verification. The first retry arm never
triggered a retry; the second triggered 19, with 13 secondary wins and six
primary wins. In the last baseline, the bulk body completed, but seven small
requests hit the 15-second stall guard. This does **not** establish that hedging
improves full-tunnel latency or reliability. It rescues some individual uploads,
while control-path delays remain. The default stays off. Raw data and failures
are in `.skirk-runs/astra-hedge-abba-64m/`.

The post-run sweep removed another 60 transport files (98,577,913 bytes).
Those manual cleanup operations are outside the table's fixed 15-second tail;
the table must not be read as the full cost of eventual cleanup. Short-run
success and a small API error count do not establish that garbage collection
keeps up indefinitely.

A second disabled-by-default setting, `drive.separate_upload_pool`, isolates
large generated-ID normal uploads on a persistent HTTP pool with the same
route. Control/list/priority operations and other uploads keep their ordinary
pool. It adds no speculative copy by itself; accounting and backoff remain
shared. The retry timer also skips an extra leg while shared quota backoff is
active. Token acquisition can still consume part of that timer's delay.

The isolation-only comparison was planned as isolated/baseline/baseline/isolated,
but its first arm failed and stopped the runner: all 24 small bodies verified
with 2.627-second scheduled p95, while the bulk response delivered no bytes
before the stall guard. This suggests a useful control-path separation to
investigate, but does not establish an improvement against a completed paired
control. It also demonstrates that isolation alone does not resolve bulk
stalls. The failed run is retained in `.skirk-runs/astra-isolated-abba-64m/`.

Finally, two combined isolation+retry repeats were planned. The first verified
the 64 MiB bulk body at 13.891 Mbps, but only 22/24 small requests completed;
two hit the stall guard. No upload exceeded the retry trigger, so no hedge
actually ran in this arm. The second repeat was canceled by the failure gate.
Raw data is in `.skirk-runs/astra-isolated-hedge-repeat-64m/`.

**Decision:** neither new transport setting earns a default change. The probes
identified a useful way to rescue individual uploads, but the full workload
still exposes long control/download delays and failures. Keep the experiments
available explicitly, retain the default route and transport settings, and do
not advertise a new maximum speed, a latency guarantee, or production capacity
from these results. The best fully successful 64 MiB mixed arm in this pass
was the initial 67.454 Mbps baseline, not an experimental configuration.

## Reopened architecture option: opaque Worker TCP

The older notes inferred a blanket web-port restriction from one failed
`connect()` call. Current [Cloudflare socket documentation](https://developers.cloudflare.com/workers/runtime-apis/tcp-sockets/)
instead describes blocked Cloudflare IPs and other destination/lifecycle limits.
A new authenticated, fixed-destination deployment confirmed raw TCP port 443
to Google and GitHub. A local Node TLS client then completed certificate-verified
TLS 1.3 and HTTP 200 through the Worker's opaque WebSocket/TCP bridge. The
Cloudflare-hosted control returned the old restriction error.

The successful requests took 199.7 and 205.6 ms respectively, including the
direct Worker connection and TLS handshake. **These are not Drive measurements**
and do not establish free-plan capacity or regional reachability. The first
probe had a binary-frame decoding bug caused by the newer Blob default; an
explicit `binaryType = "arraybuffer"` fixed it, consistent with the current
[WebSocket documentation](https://developers.cloudflare.com/workers/runtime-apis/websockets/#binary-messages).
Both attempts remain in `.skirk-runs/astra-cf-probe/`. The temporary Worker and
its local secret were deleted. Source: `experiments/cloudflare-tcp-probe/`.

This corrects the architecture record. A Worker exit still needs Drive-side
discovery, stream lifetime and reconnect handling, destination coverage, and
quota/cost measurements. The [HTTP invocation lifetime](https://developers.cloudflare.com/workers/platform/limits/#duration)
differs from post-response work, and a direct WebSocket is not a valid substitute
for the user's Google-only path. An owned direct relay remains a separate
explicitly selected route, not an automatic bypass of Google front.

## Final validation and artifacts

After the final engine changes:

- `scripts/preflight.sh` passed: installer checks, all Go tests, Linux/386
  cross-compilation, vet, and whitespace checks.
- `go test -race ./...` passed. The experimental upload lifecycle tests also
  passed five repeated runs under the race detector during development.
- All seven Python benchmark tests passed. They include corrupted-body and
  missing/stale/malformed metrics rejection.
- Android Debug APKs rebuilt successfully with the final engine. The local,
  ignored SDK path was corrected for this Mac.
- iOS engine/HEV and Simulator app rebuilt successfully. The final Swift
  profile lifecycle UI test passed 1/1 on a fresh iPhone 17 Pro Simulator,
  including the import nonreplacement guard; its temporary Simulator was
  deleted. This does not test physical-device packet forwarding.
- macOS `Skirk.app` bundled successfully with the rebuilt Go sidecar.
- Windows x64 GUI cross-compilation passed earlier in this pass; its source
  did not change afterward. The final Windows Go sidecar was rebuilt and the
  portable ZIP repackaged; ZIP integrity passed. Windows execution was not
  tested.
- Desktop Rust tests passed 23/23 after the client changes. Frontend and Tauri
  builds passed. The macOS bundle is not Developer ID signed/notarized.

Final logs use `.skirk-runs/astra-final-*`. Artifact SHA-256 hashes and sizes are
in `.skirk-runs/astra-final-artifact-manifest.json`; the final engine source
snapshot is `.skirk-runs/astra-final-engine-source.tar.gz`. The app artifacts
are under `clients/android/app/build/outputs/apk/debug/`, the macOS Tauri bundle
directory, the Xcode Simulator products directory, and
`dist/windows-portable/Skirk_windows_x64_portable.zip`.

The last delayed cleanup checks found zero matching transport or probe files,
with no truncation or failures. All test servers stopped and all temporary
per-arm credential copies were removed. The existing private test kit remains
available for future work. A pattern scan of 714 tracked/nonignored new source
files found no credential matches. Repo-local GitHub authentication was checked
as `ShahabSL`; no commit or push was made in this pass.

## Capacity and release limits

No short run proves 200,000 simultaneously active users. Separate personal
OAuth projects distribute legitimate ownership and quota; they do not make one
mailbox or exit unbounded. The [current Drive quota model](https://developers.google.com/workspace/drive/api/guides/limits)
has per-project and per-user limits, with project-age differences. Actual Console
limits, grant rate, egress, idle discovery, storage cleanup, and local resources
must be assessed separately. The daily egress threshold should not be described
as an unlimited free service or confused with measured concurrent capacity.

Physical iOS packet forwarding, Windows runtime behavior, constrained-region
reachability, long-running cleanup pressure, and public distribution signing
remain outside the evidence from this Mac. The cleanup task slice can still
grow if sustained production exceeds deletion throughput. The CLI renewal
flow's `--json` output still includes interactive prompts before its final JSON.
