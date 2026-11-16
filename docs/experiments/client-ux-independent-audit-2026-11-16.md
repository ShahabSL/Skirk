# Client UX independent audit — 16 November 2026

This review starts from `55a58cc` and covers the current iOS, Android, and desktop client code. The app is an operational tool: importing a client profile, choosing the Google route, connecting, and recovering from Google authorization errors must be immediately understandable.

## Findings and changes

| Finding | Impact | Change |
| --- | --- | --- |
| On iOS, the first required action (importing `client.skirk`) appeared after Google authorization guidance and route settings. | A new user could see a disabled Connect action while import sat below the first screen. | The Profile section now follows connection status; the no-profile header says “Set up Skirk” and hides Connect until import. The Google authorization guide and route remain available after import. |
| On an iOS Simulator, the unavailable VPN service could produce a red connection error before any profile was imported or Connect was attempted. | The initial screen looked broken while import was still possible. | Initial manager-load errors are surfaced only when a profile exists; Connect still reports service errors when attempted. |
| On Android, two local proxy port fields occupied the first-run import form even when their defaults were appropriate. | The file picker and profile name competed with advanced settings. | Local port fields are behind an explicit “Change local ports” control; existing values and validation are unchanged. The no-profile status no longer implies import is below it. |
| On desktop, no-profile state presented a disabled Connect primary action while the import form was farther down the page. | The first action was unclear. | The primary action now says “Import profile” and scrolls/focuses the file import control. |
| The OAuth guide gave Cloud Audience steps without explicitly assigning production publishing to the owner. | A phone or desktop user might expect the client to change Google Cloud publishing status. | Each client now says only the project owner can publish the OAuth app. The existing Audience link and renewal command remain accessible. |

## Existing behavior confirmed

- Android and desktop expose distinct Google-front (`www.google.com` SNI) and Direct Google choices. iOS additionally permits the imported profile route. New clients default to Google front.
- The clients do not claim a guaranteed refresh-token expiry date from the saved profile. They offer an exit round-trip test and targeted renewal guidance when Google rejects a grant.
- Publishing removes the External Testing seven-day refresh-token condition for new grants; it is not a promise that Google will never revoke access. The client cannot publish a user's Google Cloud project by itself.

## Validation

- iOS Simulator app and packet extension build: passed. A fresh iPhone 17 Pro Simulator UI test passed 2/2: file import is visible without scrolling, the owner-only publishing guidance opens, a no-profile launch has no premature VPN error, and route choice survives relaunch. Final first-run capture: `.skirk-runs/ui-audit-ios-first-run-final2.png`. This test does not validate packet forwarding on a physical iPhone.
- Android API 36 Pixel emulator: debug APK and unit tests passed; a clean first-run screen was captured at 320 dp width in `.skirk-runs/ui-audit-android-320dp.png`. This visual check did not reconnect a live exit.
- Desktop TypeScript/Vite build passed; the UI detector reported no findings for `App.tsx`. The desktop import action was not exercised in a packaged Windows/macOS app in this pass.

## Remaining product UX work

The owner still creates/publishes a personal Google OAuth project in Google Cloud and renews the exit grant. A client can guide, copy commands, and import a renewed profile, but cannot truthfully infer Cloud publishing status from the profile or publish the owner's project. A future assisted owner flow would need an authenticated Cloud project ownership path, explicit scope/consent, and a recovery path when Google requires verification. Native on-device packet forwarding and packaged desktop interactions need their platform release checks before distribution.
