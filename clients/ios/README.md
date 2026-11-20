# Skirk for iOS

The iOS app imports a `client.skirk` profile into the device Keychain and
starts a packet tunnel. The tunnel extension runs the Go Skirk client and the
vendored HEV SOCKS-to-TUN bridge inside the extension process. The imported
profile is passed in tunnel start options and saved in a shared Keychain group
for OS-initiated reconnects; it is not written into VPN preferences. The
extension routes IPv4 and IPv6 through the local SOCKS listener.
For legacy profiles' system resolver queries, its mapped DNS returns synthetic A and AAAA
addresses and sends the original hostname to the exit through SOCKS. Android's
IPv4-only mapped DNS configuration is separate.
Profiles with `udp_preview_remote_dns` instead use the authenticated exit DNS
path and disable mapped DNS. Public UDP and IPv6 UDP remain gated by the
profile's existing preview flags and matching exit capabilities. The preview
advertises MTU 1280; legacy profiles keep MTU 1500. See
[frontend parity and validation limits](../../docs/udp-preview-platform-parity.md).
The app reports **VPN active** only when iOS reports the local packet tunnel
connected. It does not treat that state as proof that the remote exit is
reachable. While connected, **Test exit** sends one authenticated challenge
through the existing Drive mailbox and reports one round-trip sample. It does
not run in the background or test arbitrary websites. An older or unreachable
exit cannot return the proof; the app then reports that reachability is
unverified rather than diagnosing an expired OAuth grant.
While connected and foregrounded, the app asks the extension for local Drive
counter snapshots every ten seconds. It computes a recent units/minute and
errors/minute pace and shows any active cooldown. This status makes no Google
request and includes only this iPhone's client activity, not the exit or other
apps using the same Google project. It becomes unavailable on disconnect and
starts a fresh measurement after a long pause.

## Build

Install Xcode and XcodeGen. From the repository root, run:

```sh
clients/ios/build.sh
```

The script builds the Go and HEV XCFrameworks, generates
`clients/ios/Skirk.xcodeproj`, and builds an iOS Simulator app. For a physical
iPhone, open the project in Xcode, choose an Apple developer team that has
Network Extension packet-tunnel entitlement, and sign both app and extension.
The Simulator build uses ad-hoc signing so the host app can store an imported
profile in its app-private Keychain. This supports import and persistence
testing without putting OAuth secrets in app defaults. Packet tunnel networking
still requires a signed physical-device test.
On the iOS 26.5 Simulator tested with Xcode 27.2,
`NETunnelProviderManager.loadAllFromPreferences` returned
`NEConfigurationErrorDomain` code 11 (`IPC failed`): the Simulator could not
connect to `nehelper`, before it tried to launch the tunnel extension. Import,
Keychain persistence, and UI checks work there; packet forwarding cannot be
validated in that runtime.
The same signed test must verify Keychain group sharing, OS restart recovery,
and whether the provider's own Google egress avoids VPN routing recursion.

Generate a profile with `skirk setup init`. In the iOS app, tap **Scan QR code**
and point the camera at the code from the exit. You can also paste the profile
text or tap **Choose client.skirk file**. To transfer a saved profile, tap
**Share profile** to show its QR code, copy it, or share the text. Treat the
profile QR code like a password. Review the Google access route and tap
**Connect**. The first connection explains what Google Drive and the exit
operator can see. Review the explanation, tap **Agree and connect**, then allow
iOS to add the VPN configuration. **About Skirk** includes the privacy policy
and open-source notices and works offline. File reads are limited to 256 KiB and valid UTF-8; the selected
file is not retained after the profile is saved in Keychain. Cloud-provider
coordination and file reads run off the UI actor, with one active read and at
most one newer pending file. A canceled or replaced selection cannot save a
late result. Canceling a provider read is cooperative: security-scoped access
is released when its actual I/O finishes, while the UI immediately allows
another selection or pasted profile. A new import defaults
to Google front, which sends TLS SNI to `www.google.com` even if an older
profile contains a Direct route. The explicit Direct Google choice connects to
Drive API hosts and may be unavailable in some regions. Existing installations
that previously selected the legacy imported-profile route retain its behavior;
the app labels the resolved imported route so a Direct choice is visible, then
offers Google front and Direct Google as the explicit new choices. An explicit route choice is preserved
when a renewed profile is imported. It is saved in the shared Keychain on a
signed iPhone and applies on the next connection. Simulator builds store only
this nonsecret preference in app defaults; the OAuth profile stays in the
app-private Keychain. The Skirk exit must be running.
When connected, **Test exit** reports the result of a round trip through Drive.
When Google rejects the saved grant, the connection status shows **Google
access expired** and offers **Import renewed profile**. The exit owner can
follow the [profile renewal instructions](../../docs/setup.md#renew-an-expiring-personal-oauth-grant) and send an
updated profile. Scan, paste, or import it from Files. Replacement
requires the same tunnel and transport version. Remove the saved profile
before importing a different setup.

The isolated macOS test package exercises the shared file-import helper with
Swift 6 concurrency checking, including blocked provider reads, newer-selection
ownership, picker cancellation, security-scope lifetime and bounded input:

```sh
swift test --package-path clients/ios
```

The pure Swift client-activity parser and rate model can also be checked
without a VPN entitlement:

```sh
swiftc clients/ios/App/ClientDriveActivity.swift clients/ios/Tests/ClientDriveActivityTests.swift -o /tmp/skirk-client-drive-activity-tests
/tmp/skirk-client-drive-activity-tests
```

These focused tests do not replace Simulator UI checks or signed-device tests
with real Files/iCloud providers.
