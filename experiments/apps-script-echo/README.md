# Temporary opaque Apps Script echo

This measures only an owned Apps Script endpoint and its Content Service
redirects through the selected `www.google.com` front. It is not a TCP tunnel
or a bulk-speed benchmark. It uses no Drive, Gmail, URL Fetch or paid service.

`Code.gs` is inactive until a private copy replaces the secret and zero expiry.
Use a unique temporary script in the authorized secondary account, a random
secret of at least 32 characters, and an expiry a few hours in the future.
Deploy as an owner-executed web app with anonymous invocation; the secret and
strict body bounds protect this limited echo. Archive its deployment and
delete the temporary script after testing. Never install a certificate.

The env-gated `TestLiveAppsScriptOpaqueEcho` test in
`internal/skirk/apps_script_echo_probe_test.go` reads an ignored
`.skirk-runs/` directory containing `settings.json` with `deployment_url`,
`auth`, and the existing `route` object. Do not place OAuth credentials in
that file. Explicit opt-in requires `SKIRK_LIVE_APPS_SCRIPT_ECHO=1` and
`SKIRK_APPS_SCRIPT_ECHO_PRIVATE_DIR` naming that directory. Ordinary tests
remain offline.

The probe permits only the deployed `script.google.com` URL and Google
Content Service redirect authority, preserving pinned fronting for each hop.
It sends at most twelve random AEAD envelopes: six 64-byte, two 16 KiB, two
256 KiB and two 1 MiB plaintexts. Each exchange has a 20-second deadline and a
1.5 MB decompressed response bound. The local encryption key never leaves the
probe; the script sees only opaque ciphertext and returns it unchanged.
Any HTTP, redirect or integrity failure stops the sequence and retains a
sanitized measurement record. Successful echo does not demonstrate a working
external-fetch/TCP-node path, protocol reliability or fleet capacity.
