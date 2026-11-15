# Skirk for iOS

The iOS app imports a `client.skirk` profile into the device Keychain and
starts a packet tunnel. The tunnel extension runs the Go Skirk client and the
vendored HEV SOCKS-to-TUN bridge inside the extension process. The imported
profile is passed in tunnel start options and saved in a shared Keychain group
for OS-initiated reconnects; it is not written into VPN preferences. The
extension routes IPv4 and IPv6 through the local SOCKS listener.
For system resolver queries, its mapped DNS returns synthetic A and AAAA
addresses and sends the original hostname to the exit through SOCKS. Android's
IPv4-only mapped DNS configuration is separate.
The app reports **VPN active** only when iOS reports the local packet tunnel
connected. It does not treat that state as proof that the remote exit is
reachable.

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

To use it, generate a profile with `skirk setup init`, tap **Choose client.skirk
file** in the iOS app (or paste the profile text), review the Google access route,
and tap Connect. File reads are limited to 256 KiB and valid UTF-8; the selected
file is not retained after the profile is saved in Keychain. A new import defaults
to Google front, which sends TLS SNI to `www.google.com` even if an older
profile contains a Direct route. "Use profile route" deliberately follows the
imported config. The explicit Direct Google choice connects to Drive API hosts
and may be unavailable in some regions. An explicit route choice is preserved
when a renewed profile is imported. It is saved in the shared Keychain on a
signed iPhone and applies on the next connection. Simulator builds store only
this nonsecret preference in app defaults; the OAuth profile stays in the
app-private Keychain. The Skirk exit must be running.
A profile whose OAuth
grant expires needs a renewed profile imported again; publishing the Google
OAuth app avoids the seven-day Testing grant limit but cannot guarantee an
indefinite grant.
The Profile section reports the Google grant as unverified because this screen
cannot independently verify it. If the tunnel reports `invalid_grant`, the app
shows renewal steps and profile replacement in a separate recovery section.
Its Copy exit renewal command button copies
only the nonsecret CLI command; Replace profile accepts the renewed
`client.skirk` from Files or pasted text into Keychain. Restart the exit after
renewing its kit. Neither
the profile nor its refresh token is placed on the clipboard by the copy button.
