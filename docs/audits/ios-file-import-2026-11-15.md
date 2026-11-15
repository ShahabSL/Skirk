# iOS profile import ownership and background reads

Cloud-provider coordination and bounded file reads now run on one explicit
detached worker rather than the SwiftUI actor. The worker captures only a URL
and a Sendable reader; it owns security-scoped access from acquisition until
the real read returns. Canceling a blocked provider operation does not revoke
access underneath it. The UI responds immediately, discards the canceled
result, and permits pasted input while the provider finishes.

One active read and one replaceable pending selection bound concurrency and
memory. Reads check cancellation between 16 KiB chunks and consume at most
256 KiB plus the overflow byte. Empty, invalid UTF-8, access-denied and
oversized profiles retain the existing user-facing errors.

The main-actor generation fence invalidates previous file results when the
user chooses another file, edits pasted text, cancels replacement/import or
leaves the view. A separate picker launch identity captures replacement mode.
Because the platform callback has no operation ID, its sole outstanding token
is retained even after cancellation until that callback returns; a late picker
cannot be mistaken for a later selection. Typed Import is disabled during a
file read, and successful saving still goes through the existing Keychain and
tunnel-identity validation.

Validation completed on the local Swift 6.4.2/Xcode toolchain:

- `swift test --package-path clients/ios`: six Swift Testing tests, ten cases,
  passed with Swift 6 language/concurrency checking. Tests block synthetic
  provider reads to verify the main actor remains responsive, one-reader
  bounds, latest-selection ownership after stale success/error, cancellation,
  picker-target identity and security-scope lifetime. Real temporary-file
  coordination exercises the input boundary and UTF-8 errors.
- The full `Skirk` iOS Simulator app and packet-tunnel target built with
  ad-hoc signing and reused already-built Go/HEV frameworks. The new helper has
  no concurrency warnings. Xcode emitted its unrelated no-AppIntents metadata
  notice.
- Independent source review found and resolved the picker launch/result
  ownership gap before these validations.

This verifies the helper and native compilation. It does not claim real iCloud
provider behavior, a signed iPhone packet tunnel or a fresh Simulator UI run.
Cancellation of uninterruptible provider I/O remains cooperative and bounded;
one pending file waits for that provider, while pasted import remains usable.
No Go transport, network route, OAuth grant or production default changed.
