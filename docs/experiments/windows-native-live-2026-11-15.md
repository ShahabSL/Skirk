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
