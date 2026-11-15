# Skirk validation record — November 14, 2026

This records an earlier validation pass on the uncommitted research branch.
The subsequent [Astra audit](astra-audit-2026.md) records the latest builds,
fault tests, stricter integrity checks, and retained live transport failures.
The local
artifacts under `.skirk-runs/` are ignored because some experiment directories
contain credentials or private logs. New bounded live Drive transport tests
ran with a dedicated personal test account and project. The older figures
remain identified as archived May 2026 experiments; the polling figures
remain identified as an in-memory simulation.

## Current code and platform checks

| Area | Result | Evidence and limit |
| --- | --- | --- |
| Go core, CLI, OAuth renewal | Final `SKIRK_FULL_PREFLIGHT=1 make preflight` passed, including Go tests, vet, installer smoke, Linux/386 cross-compilation, desktop build, and Android build on this Mac. `go test -race ./internal/skirk ./cmd/skirk` passed. Tests confirm running exit and client processes surface terminal OAuth failures as status 78 across list, upload, and download; transient list errors still retry. After the test OAuth app moved to In production, a live `setup renew-oauth` run succeeded and updated the isolated exit/client kit without changing its mailbox. The restarted exit and updated client delivered a local HTTP 200 response through the live Drive tunnel in 2.723 s. | `.skirk-runs/full-preflight-ultimate.log`, `.skirk-runs/go-race-post-drain.log`, and the ignored `.skirk-runs/personal-test/kit/.oauth-renew-backup-*` directory; focused renewal tests cover custom exported-profile settings. The smoke was one 19-byte request, not a new throughput sample. |
| iOS app and packet extension | `clients/ios/build.sh` built the latest Go and HEV frameworks and the ad-hoc signed iOS Simulator app/extension. The app installed and launched on an iPhone 17 Pro simulator. A test profile imported to the private Keychain, survived relaunch, and was removed. The route-choice UI test passed: a new import defaults to Google front, while an explicit Direct choice survives re-import. Recovery UI now shows an honest Unknown grant status, copies a nonsecret exit renewal command, and guides replacement of the updated `client.skirk`; disposable-profile UI tests passed. | `.skirk-runs/ios-build-ultimate.log`, `.skirk-runs/ios-final-simulator.png`. Simulator Connect could not reach `nehelper` and failed at `NETunnelProviderManager.loadAllFromPreferences` with `NEConfigurationErrorDomain Code=11`, before starting the extension or making Drive calls. The same service was absent on iOS 26.5 and 27.0 Simulators. Packet routing, shared Keychain access, and background reconnect need a signed physical iPhone test. |
| HEV mapped DNS | `scripts/test_hev_mapped_dns.sh` passed with AddressSanitizer and UndefinedBehaviorSanitizer. | `.skirk-runs/hev-mapped-dns-final.log`: A/AAAA mapping, mixed questions, EDNS count, SOCKS conversion, IPv4/IPv6 literals, name bounds, and 50,000 malformed packets. Android ARM64/ARMv7 native libraries and iOS framework also compiled. No device-level IPv6 DNS result. |
| Android | Debug APK assembled and installed on a Pixel 7 API 36 emulator. The per-app VPN picker blocked an empty selection, retained Chrome after restart, and showed a removed app as unavailable. A temporary private test profile connected the Android VPN to the live Mac exit: a disposable Android probe app fetched `https://example.com` over the active VPN with HTTP 200 and 577 response bytes. The profile and probe app were removed afterward. Later synthetic tests confirmed mismatched renewal identities are refused without overwriting the saved profile, the selected route and VPN app count appear above Connect, and the Profiles screen can replace a saved config from a pasted or picked `client.skirk`. An invalid picked file left the Google-front profile unchanged. The idle quota card labels the zero-stream client estimate and excludes exit/shared-project usage. The final APK includes explicit expired-grant handling for VPN and proxy sidecar exit 78. | `.skirk-runs/android-build-ultimate.log`, `.skirk-runs/android-renewal-action.png`, `.skirk-runs/android-idle-estimate.png`, `.skirk-runs/personal-test/android-vpn-probe.png`, and `clients/android/app/build/outputs/apk/debug/app-universal-debug.apk`. The installed NDK was 27.1, selected locally with `SKIRK_ANDROID_NDK_VERSION`; the repository default remains 27.0. Exit-78 UI behavior was build-verified but not rerun on the emulator; this was one emulator connection, not a longevity or reconnect test. |
| Desktop web UI and Rust backend | A clean `npm ci` reports zero vulnerabilities after compatible lockfile updates. `npm run build` and 21 Rust library tests passed. The latest macOS ARM64 Tauri app built and launched with an isolated synthetic profile: a legacy Direct import selected Google front by default, explicit Direct worked, and in-place replacement preserved route/ports/performance. An offline `invalid_grant` followed by a generic final line still showed recovery guidance; replacing the profile cleared the old error. The backend now maps terminal sidecar exit 78 to that same recovery action. The selected route has a readable `www.google.com` SNI label and the idle estimate excludes exit/shared-project usage. | `clients/desktop/src-tauri/target/release/bundle/macos/Skirk.app`. Bundled GUI and Go sidecar are arm64. The local bundle is not distribution-signed; `codesign --verify --deep` and `spctl --assess` fail, so Developer ID signing/notarization remain required before public macOS distribution. The exit-78 mapping passed a Rust test, not an additional native UI smoke. |
| Windows | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build` produced the latest x86-64 CLI. `cargo xwin build --release --target x86_64-pc-windows-msvc --xwin-sdk-version 10.0.22621 --bin skirk-desktop` linked the latest x86-64 GUI; Windows Rust test binaries also linked. A static CRT build removed the Visual C++ runtime DLL dependency. The 51 MB portable ZIP passes CRC and contains x86-64 GUI, Go sidecar, and tunnel PE binaries. | `dist/windows-portable/Skirk_windows_x64_portable.zip`; `actionlint` passed for the release workflow. No Windows VM or Wine runtime is available here, so GUI launch, proxy, and VPN behavior on Windows are untested. |
| OpenWrt | `sh -n contrib/openwrt/skirk` passed for the procd service template. | [OpenWrt guide](openwrt.md). No OpenWrt router runtime was available. |
| Mixed-workload tool | Python compilation and an offline local HTTP/SOCKS relay smoke passed, then the tool drove bounded live Skirk/Drive tests. | `tools/skirk_live_mix_bench.py`; the smoke verified one 1 MiB bulk response and five small responses with no URL/body leakage in JSON. Live results are below. |

The iOS build exercises packaging and compilation; Android also completed one
Drive-backed VPN request on the emulator. The release workflow's optional macOS
Developer ID signing and notarization path passed static checks and a local
ad-hoc signing dry run; the real Apple credential path has not run.
The current code has not passed a new browser/video overlap run,
cleanup/quota-pressure soak, or VPN stop/reconnect test.

After the live runs, `cleanup --older-than 1m` found 323 stale mux files
(414,257,901 bytes) in the isolated test mailbox. The explicit Skirk cleanup
deleted all 323 with zero failures; a dry run against the original kit then
found zero remaining runtime files. Records are in ignored
`.skirk-runs/personal-test/{quota-cleanup-result,kit-cleanup-dry}.json`.
These short-lived benchmark processes did not demonstrate steady-state
automatic cleanup under a long-running exit.

## Performance evidence by source

| Source | Result | Interpretation |
| --- | --- | --- |
| Current in-memory mux simulation | With 20 ms upload, 12 ms list, and 8 ms download delays, bounded burst polling cut post-workload list calls from 138–140 to 43–44 in three short-transfer runs, about 69% fewer; small-response time rose from 157–160 to 193–195 ms. In the 24-small-plus-32 MiB simulation, bounded burst p95 was 200–220 ms and bulk rate was 1,054–1,060 simulated Mb/s. | [Polling benchmark](polling-benchmark.md) and `tools/skirk_memory_bench.go`. These are local scheduling values, **not** live Drive speeds or quota usage. |
| New single-client live Drive tests, November 2026 | Across sequential 32 MiB runs, `google_front` without burst delivered 35.793 and 36.434 Mb/s with overlapping small-request p95 first byte of 3.025 and 2.682 s; `google_front_pinned` delivered 33.421 and 48.573 Mb/s with p95 of 3.881 and 3.444 s. HTTP/1.1 pinned delivered 14.514 Mb/s and 3.988 s p95 in one run. Direct Google delivered 38.146 Mb/s and 2.266 s p95 in one run. | `.skirk-runs/personal-test/{front-a,front-b,h2-a,h2-b,h1-a,direct-a}.json`. All scheduled HTTP requests succeeded and both Skirk ends reported zero Drive API errors. This Mac can reach direct Google; the direct result does not establish reachability elsewhere. |
| New longer live Drive run, November 2026 | One 128 MiB `google_front` transfer completed in 14.330 s (74.928 Mb/s). All 24 small requests succeeded; the 12 starting during bulk had 3.064 s p95 first byte. | `.skirk-runs/personal-test/front-128.json`. One run on a local exit, not a maximum sustained rate or a general latency guarantee. |
| Live burst polling A/B, November 2026 | With burst enabled on both ends, two 32 MiB `google_front` runs delivered 22.939 and 26.981 Mb/s with overlapping small-request p95 of 3.452 and 3.596 s. | `.skirk-runs/personal-test/{frontburst-a,frontburst-b}.json`. Burst was worse than no burst in these conditions and increased the local Drive request estimate, so it remains off by default. |
| New concurrent-client live Drive runs, November 2026 | Three, five, and ten simultaneous `google_front` clients completed 3/3, 5/5, and 10/10 random 32 MiB transfers. Aggregate goodput was 28.655, 60.690, and 78.244 Mb/s; all 12, 20, and 40 small requests succeeded, with p95 first byte of 3.519, 3.281, and 4.941 s. | `.skirk-runs/personal-test/capacity-{three-rolling100,five-final,ten-final}/summary.json`. Client plus exit estimated units were 67,210, 89,110, and 209,075; zero Drive API errors or backoff. Ten is the largest completed bounded test, not a sustained user-capacity limit. |
| New quota-project integration and idle wake, November 2026 | With `drive.quota_project` set to a second enabled project on both ends, a live mailbox upload/read/delete validation passed. A small HTTP request through the Drive tunnel returned 200 with 2.302 s first byte; after over 60 s client idle, another returned 200 with 2.147 s first byte. With both endpoints explicitly on the pinned `www.google.com` front and the same quota project, one more returned 200 with 2.250 s first byte. Under the previous idle-backoff code, a full quiet client 60-second window used one Drive list, 100 estimated units; a separate quiet exit window used 63 lists, 6,300 estimated units. | `.skirk-runs/personal-test/{client,exit}-quota-project-metrics.json` and `{client,exit}-front-quota-final-metrics.json`. The control with the second project's Drive API disabled returned `SERVICE_DISABLED`; after enablement, the core tunnel worked. These runs prove compatibility and request wake, not population latency or 200,000-user scale. |
| Header-selected project request-count attribution, November 15, 2026 | One isolated OAuth grant sent 100 read-only `files.list` calls with `X-Goog-User-Project` set to the second enabled project; all returned HTTP 200. The Console's one-hour Drive HTTP 200 chart showed rates of 0.058, 0.878, 0.675, and 0.056 requests/s in four consecutive minute buckets on that project, about 100.02 chart requests after rounding, with no corresponding OAuth-client project points. | `.skirk-runs/quota-attribution-header-burst-20261115.json` and the sanitized [chart readings](research-2026.md). The planned Windows Drive test was held during the probe. This directly supports bounded request-count attribution, not consumed quota units, sustained rate limits, or 200,000-user capacity. |
| Zero-stream client receive pause, November 2026 | After the later client-loop change, a live pinned-front client made zero Drive calls over its initial 18-second idle window, then returned HTTP 200. In a longer run, its client list total stopped rising by 20 seconds after the first HTTP request and stayed at 12 lists through the 70-second sample; another request returned HTTP 200. All calls had zero Drive API errors. A subsequent local test passed for one delayed post-close list and no continued idle polling. | `.skirk-runs/personal-test/{client,exit}-idle-zero-{final,tail}-metrics.json` and `TestClientFinalDrainListsOnceThenWaits`. A completed curl can leave a stream open briefly, so lists continue until the stream closes. The delayed post-close list was added after these live traces. First-byte times in the long run were 5.436 and 6.044 s; one old-binary comparison was 4.272 s. This does not establish a latency improvement or long-run population capacity. |
| Archived single-client Drive run, May 2026 | 100 MiB in 24.868 s, 33.73 Mb/s, six small probes with 9.658 s p95 first byte. The client reported 27,600 estimated units over 52.105 s. | `.skirk-runs/protocol-lab/final-20260514/stable-live-control-20260515T024838Z/bench-live-100mb.json`. Small sample and client-only quota accounting. |
| Archived paired five-client Drive run, May 2026 | Five simultaneous 64 MiB downloads succeeded in up to 77.945 s, 34.44 Mb/s aggregate; all 40 small requests succeeded with 9.44 s p95 total time. Aligned client and exit windows reported 147,550 estimated units in about a minute and zero API errors. | `.skirk-runs/protocol-lab/multiclient-20260514T165626Z/`. This demonstrates five clients for one roughly 78-second workload. It is not a sustained user-capacity figure. |

The dedicated Google Cloud test project showed 1,000,000 Drive units per minute
per project and 325,000 per user. [Research and capacity notes](research-2026.md)
explain why a linear quota projection cannot be advertised as supported users.
The live route comparison was bounded and sequential; broader promotion still
needs randomized repeats and constrained-network reachability.

The isolated Cloud project under the dedicated personal test account has the
Drive API enabled, an External OAuth app **In production**, a Desktop OAuth
client, and only the non-sensitive `drive.file` scope. The test account's
Drive grant was renewed after publication; this removes its Testing-specific
seven-day expiry, subject to Google's other refresh-token revocation rules.
The publisher's `getephemra.com` Domain Property was verified in Search
Console through a Cloudflare TXT record. Verified branding is published and
shown to users, with the homepage, privacy, and terms live at
`https://skirk.getephemra.com/`. A separate private draft site PR at
`shahablavasani.com/skirk` has not been merged to that site's main branch.
Neither site is part of the Drive data path. The second quota project
experiment is recorded in [research](research-2026.md); it used a bounded
two-call read-only comparison before and after enabling that project's Drive
API, followed by a 100-call chart attribution probe. These tests do not
establish sustained capacity.

## Reproduce the local gates

```bash
SKIRK_FULL_PREFLIGHT=1 SKIRK_ANDROID_NDK_VERSION=27.1.12297006 make preflight
go test -race ./internal/skirk ./cmd/skirk
scripts/test_hev_mapped_dns.sh
clients/ios/build.sh
(cd clients/desktop && npm run build)
(cd clients/android && SKIRK_ANDROID_NDK_VERSION=27.1.12297006 ./gradlew :app:assembleDebug --console=plain)
```

`tools/skirk_live_mix_bench.py --help` describes a bounded live mixed workload
for a running SOCKS listener and an HTTP origin on the exit. It caps elapsed
time and response sizes. Capture both client and exit Drive counters, errors,
cleanup debt, and Cloud Console usage in any future comparison; report p50,
p95, and p99 small-request latency during bulk traffic as well as bulk Mb/s.
