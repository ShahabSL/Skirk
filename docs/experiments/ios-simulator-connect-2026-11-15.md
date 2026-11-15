# iOS Simulator Connect diagnostic, November 15, 2026

An iPhone 18 Pro Max running the iOS 27.0 Simulator imported a synthetic,
credential-free profile and attempted Connect. The packet tunnel did not enter
`VPN active`. The Simulator's unified log recorded
`NEConfigurationErrorDomain Code=11 (IPC failed)` while loading Network
Extension configurations, before the extension could reach Drive. This
repeated the earlier iPhone 17 Pro Simulator failure on iOS 26.5; it is not a
physical-device packet-tunnel test.

The error helper itself was produced, but the app placed it after the Profile
form. The baseline UI test saw no helper after Connect until one upward swipe;
then it appeared. The UI now places connection errors next to Connect while
keeping profile and file-import errors beside the Profile form. On the
same simulated iPhone, the synthetic Connect test observed
`SIMULATOR_PACKET_TUNNEL_CONNECTED=false` and
`SIMULATOR_VPN_UNAVAILABLE=true` without scrolling. The same Connect check
passed on a fresh iPhone 17 Pro running the iOS 26.5 Simulator. It asserts
that Connect shows either an active tunnel or the visible Simulator
diagnostic. The test scrolls to its later Remove profile control on smaller
screens. The full iOS 27 Simulator UI suite passed two tests, skipped the
credential-dependent import test, and had zero failures. Generic iPhoneOS and
iOS Simulator builds passed after rebuilding the vendored HEV framework from
clean device and Simulator slices.

Private logs and result bundles are in this worktree's `.skirk-runs/` directory.
The Simulator checks establish profile and UI behavior; a signed physical
iPhone remains necessary to verify packet forwarding, Keychain group sharing,
provider egress routing, and live Drive traffic.
