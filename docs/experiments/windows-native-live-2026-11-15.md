# Native Windows proxy traffic, November 15, 2026

An isolated manual [GitHub Actions Windows 2022 run](https://github.com/ShahabSL/Skirk/actions/runs/43684300085)
built the Skirk Go CLI on Windows and started both the exit and client there.
It used only the disposable `shahabpre` test kit, a new random session, and
the pinned `www.google.com` front. The kit was transferred as a temporary
encrypted Actions secret, decoded only under the runner's temporary directory,
and the secret was deleted immediately after the prepare step. No OAuth value
or config was printed or committed.

A local HTTP origin on the same Windows runner served a fixed text body and
random 1 MiB download and accepted a 1 MiB upload. Windows `curl.exe` used
the native Skirk SOCKS listener. The GET body was compared byte for byte and
the POST response's length and SHA-256 were checked against the uploaded body.
The CLI's authenticated `probe-exit` also verified the running exit.

| Check | Observed time | Result |
| --- | ---: | --- |
| Authenticated exit proof | 5.509 s | Verified |
| Exact small text GET | 3.860 s | HTTP 200, exact body |
| Exact 1 MiB GET | 9.671 s | Exact bytes |
| Exact 1 MiB POST | 29.500 s | Length and SHA-256 acknowledgment |

The client recorded 83 Drive calls, 8,055 estimated units and zero errors
over 50 seconds. The exit recorded 100 calls, 10,255 units and zero errors
over 55 seconds. Neither recorded active backoff. Those durations are not
identical, and this one short run is not a Windows throughput or latency
ceiling.

The always-run cleanup stopped the native processes, deleted three residual
`muxv4/` objects and no `slotv1/` objects under only the generated session,
then found both prefixes empty immediately and after 30 seconds. The cleanup
step and the full manual workflow succeeded. The temporary Actions secret
was absent from the repository secret list after the run.

This verifies native Windows CLI, OAuth, Drive transport, SOCKS proxy, and
exact local-origin traffic. It does **not** exercise the Tauri GUI, system
proxy settings, Windows TUN/VPN sidecar, a remote origin, or a sustained mixed
workload. The [portable Windows package smoke](https://github.com/ShahabSL/Skirk/actions/runs/43682374499)
separately verifies that the GUI bundle launches and contains both sidecars;
that smoke does not send VPN traffic.

## Targeted Windows TUN traffic

A second isolated [Windows 2022 run](https://github.com/ShahabSL/Skirk/actions/runs/43686641849)
at `fdd9ed3` used the same native Skirk CLI, a SHA-pinned sing-box 1.13.12
sidecar, and a fresh disposable `shahabpre` session. The temporary encrypted
Actions secret was deleted immediately after Prepare and its exact name was
verified absent. The TUN installed only `1.1.1.1/32`; the runner's default
route was never changed. `curl.exe` explicitly disabled proxies, pinned
`one.one.one.one` to `1.1.1.1`, and fetched `/cdn-cgi/trace` through the TUN.

| Check | Observed time | Result |
| --- | ---: | --- |
| Authenticated exit proof | 3.643 s | Verified |
| Same endpoint through explicit Skirk SOCKS, before TUN | 9.422 s | HTTP 200, 206 bytes |
| Same endpoint through TUN, with no curl proxy | 12.000 s | HTTP 200, 211 bytes |

The response is dynamic, so its byte count differed between requests.
Comparing metrics immediately before and after the TUN request, client Drive
uploads rose by four and exit Drive downloads rose by five. At the final
snapshot the client had 73 calls and 7,300 estimated units; the exit had 88
calls and 9,000 estimated units. Both had zero Drive errors and no active
backoff. These short control and TUN timings do not establish a Windows
latency floor or maximum throughput.

The live probe asserted that the default and target routes returned to their
previous values and the TUN adapter disappeared. Its always-run cleanup
deleted three `muxv4/` objects and zero `slotv1/` objects under only that
session, then found both prefixes empty immediately and after 30 seconds.
The full workflow passed.

The exit and client shared one runner for this test. An initial targeted
[run](https://github.com/ShahabSL/Skirk/actions/runs/43685707675) timed out
when the exit's own connection to `1.1.1.1` could re-enter the same TUN. A
[credential-free process-rule control](https://github.com/ShahabSL/Skirk/actions/runs/43686260263)
showed that a `skirk-smoke.exe` process could reach the endpoint directly while
ordinary curl traffic was rejected under the same scoped route. The passing
live run used that [Windows process-name rule](https://sing-box.sagernet.org/configuration/route/rule/)
to exempt the co-located Skirk process from this test topology's recursion.
This exemption is specific to the co-located QA setup, not a product routing
recommendation.

This proves one native Windows TUN data path for a single pinned IP and HTTPS
endpoint. It does not prove whole-device VPN mode, default-route capture, the
Tauri GUI's connect flow, a separate physical exit host, or sustained mixed
traffic.

## GUI backend default-route TCP check

A further disposable [Windows 2022 run](https://github.com/ShahabSL/Skirk/actions/runs/43689331948)
at `f3aed4a` called the same Rust `DesktopRuntime::connect()` backend used by
the GUI's Connect command. It imported a fresh-session client profile,
selected VPN mode, started the native Go sidecar and product sing-box 1.13.12
configuration, and ran an authenticated exit check. The exit and client were
co-located on the hosted runner; the exit process used the `skirk.exe` name
already covered by the product's direct process rule. No test-only tunnel
routing exception was added.

Before Connect, the runner saved its default and selected routes and
registered an independent SYSTEM scheduled task to kill the disposable tunnel
and remove remaining adapter routes after three minutes. That task was a
fallback; the app's own Disconnect restored the routes and removed the adapter.
With the VPN active, Windows selected `Skirk Tunnel` for `1.1.1.1` and
`curl.exe`, with proxies explicitly disabled, fetched the pinned HTTPS
endpoint through the tunnel. The SHA-pinned sing-box download, native Rust
test build, route recovery, and exact Drive cleanup all passed.

| Check | Observed result |
| --- | ---: |
| Authenticated exit proof | 2.570 s, verified |
| Selected route to `1.1.1.1` | `Skirk Tunnel` (two matching route entries) |
| Unproxied HTTPS request | HTTP 200, 206 bytes, 11.408 s |
| Client Drive upload change during request | +11 calls |
| Native Disconnect | Previous default and selected routes restored; adapter removed |
| Exact session cleanup | 3 mux objects deleted, 0 slot; both prefixes empty immediately and after 30 s |

The temporary encrypted Actions secret was removed immediately after the
Prepare step and verified absent. The disposable workflow and harness were
removed from the isolated test branch after the run; they are not part of the
draft PR. An earlier
[run](https://github.com/ShahabSL/Skirk/actions/runs/43688912687) reached
the same VPN route and exit proof but stopped on a test assertion that expected
one selected route entry; Windows returned two identical `Skirk Tunnel`
entries. That run also restored routes and cleaned its exact session. The
assertion was corrected for the passing run.

This is one default-route **TCP** request through the real GUI backend, not a
visual GUI click, arbitrary destinations, sustained traffic, a separate exit
host, or complete Windows app compatibility. The current product tunnel
configuration rejects non-DNS UDP, so this result does not verify UDP apps.
