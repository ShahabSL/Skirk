# Native first-run review — November 16, 2026

The client starts with the action it can complete: import the exit's
`client.skirk` profile. Android and iOS show a file chooser first and put
paste behind a secondary action. With no profile, route settings, diagnostics,
and a disabled Connect control do not compete with import. Owner-only OAuth
and renewal instructions stay available from a collapsed guide. After import,
the app displays the effective route; the user can choose the pinned
`www.google.com` front (default for new profiles) or Direct Google. Imported
legacy profiles retain their effective route rather than silently changing it.

Google access has an actionable unchecked/reconnect state instead of an
unexplained unknown expiry date. A published production OAuth app removes the
Testing-specific seven-day refresh-token lifetime, but does not make a token
permanent. The client guides a project owner through renewal; it cannot
publish a Cloud project using a normal `drive.file` token. A user authorizing
the publisher's app does not need a personal domain.

An Android exit probe now has a distinct **not verified** state after a failed
test. Previously the connection panel showed the failed test while the Google
authorization card still said access had not been checked. The changing probe
result is a polite TalkBack live region. A failed probe alone does not prove
that a Google grant expired.

## Verification

| Surface | Exact local evidence | Limit |
| --- | --- | --- |
| Android | Root-source `:app:testDebugUnitTest :app:assembleDebug` passed with NDK `27.1.12297006`; first-run screenshots visually checked at 320 and 411 dp, including enlarged font; a dedicated emulator was removed afterward. | Emulator layout and build do not prove every physical device or network. |
| iOS | Fresh dedicated iPhone Simulator first-run, import lifecycle and route-choice UI tests passed; root-source selected UI suite exited zero; app and Packet Tunnel extension Simulator build passed. | Simulator UI/build cannot execute the packet tunnel end to end on physical iPhone. |
| macOS/Windows desktop | Snapshot 5/5 and desktop build passed locally; GitHub Smoke matrix includes Windows/macOS/Linux checks at earlier source revisions. | A distributable notarized macOS release is still unverified; the current head awaits its own CI run. |

These are onboarding and status changes, not measured transport speedups. The
private screenshots and Simulator artifacts are under `.skirk-runs/` and are
not published with the repository because test profiles and logs share that
ignored tree.
