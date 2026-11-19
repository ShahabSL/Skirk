# Skirk Clients

Every client consumes the same generated profile:

```text
skirk:...
```

Clients do not need Google login or `gcloud`. Treat the profile like a password.
The same profile can be copied to multiple devices. Windows and Android create a
stable local identity for each imported profile; the CLI can generate one
automatically or accept `--client-id my-device`. Every client start also gets a
fresh run identity, so simultaneous devices using the same copied profile do not
consume each other's responses.

## Linux CLI

Install:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

Run:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

Stable identity for repeated use on the same Linux machine:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

If you received the profile as text, paste it into the Android or desktop app.
For CLI use, save the text in a private `client.skirk` file and run the file
command above. On Linux and macOS, `chmod 600 ./client.skirk` limits file
access to your account.

Test:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

For apps, configure SOCKS5 `127.0.0.1:18080`. Prefer `socks5h` behavior when
the app exposes that choice.

Optional HTTP/HTTPS proxy:

```bash
skirk serve-client \
  --config client.skirk \
  --listen 127.0.0.1:18080 \
  --http-proxy-listen 127.0.0.1:18081
```

Trusted-LAN sharing is the same command with both listeners bound to all
interfaces:

```bash
skirk serve-client \
  --config client.skirk \
  --listen 0.0.0.0:18080 \
  --http-proxy-listen 0.0.0.0:18081
```

Only do this on a LAN you control, and rely on host firewall rules if the
machine is reachable from untrusted networks.

## Desktop GUI

The preferred desktop UX is the portable app from release assets:

- Windows: `Skirk_windows_x64_portable.zip`. Extract it and open the root
  `Skirk.exe`.
- Linux: `Skirk_linux_x64_portable.zip`. Extract it and run the root `./Skirk`.
  Run with root or `CAP_NET_ADMIN` privileges when using VPN mode.
- macOS Apple Silicon: `Skirk_macos_arm64_signed_notarized.app.zip` when
  available. Extract it and open `Skirk.app`.
- macOS Intel: `Skirk_macos_x64_signed_notarized.app.zip` when available.
  Extract it and open `Skirk.app`.

If the Apple signing secrets are absent, the release instead contains
`Skirk_macos_arm64_unsigned.app.zip` and `Skirk_macos_x64_unsigned.app.zip`.
macOS Gatekeeper may block normal launch of those unsigned builds.

Use the Apple Silicon asset on M-series Macs. Running the x64 app through
Rosetta can trigger macOS warnings about future Intel app support.

The `skirk-windows-amd64.zip` asset is the CLI-only build for PowerShell users.
The `skirk-linux-*.tar.gz` assets are CLI-only builds for shell/headless users.

The desktop app:

- imports one-line `skirk:` profiles or `client.json`;
- stores profiles in portable data;
- assigns each imported profile a local client identity;
- starts and stops the Go Skirk SOCKS and HTTP sidecar;
- can bind both proxy listeners to `0.0.0.0` for LAN sharing;
- exposes performance presets for Drive polling and worker concurrency;
- lets the user choose Google front (`www.google.com` TLS SNI) or direct Google
  per profile while disconnected, and uses that saved route at launch;
- shows an estimated local Drive API units/min rate for the current desktop
  process;
- shows connection status and logs.

Windows supports Proxy, System Proxy, and VPN modes. Linux desktop supports
Proxy and VPN modes. macOS desktop supports a local proxy or the system proxy. Linux
VPN mode uses the bundled sing-box TUN sidecar and requires root or
`CAP_NET_ADMIN` privileges. Proxy mode exposes SOCKS5
`127.0.0.1:18080` and HTTP `127.0.0.1:18081` by default. LAN sharing is
explicit and should only be enabled on trusted networks.

Command-line client:

```powershell
.\skirk-windows-amd64.exe serve-client --config .\client.skirk --listen 127.0.0.1:18080
```

Optional local browser dashboard:

```powershell
.\skirk-windows-amd64.exe client-ui --config .\client.skirk --socks 127.0.0.1:18080 --ui 127.0.0.1:18280
```

Development run:

```bash
make build-windows
clients/desktop/scripts/stage_sidecars.sh
cd clients/desktop
npm install
npm run tauri dev
```

Linux portable package:

```bash
clients/desktop/scripts/stage_sidecars.sh
cd clients/desktop
npm install
npm run tauri build -- --no-bundle
cd ../..
python clients/desktop/scripts/package_linux_portable.py
```

## Android

The Android app packages the Go Skirk engine and starts it as a foreground
service. Each imported Android profile gets a UUID-backed local client identity.
VPN mode routes all apps by default. While disconnected, **Choose apps** can
limit the VPN to selected launchable apps. Unselected apps use their normal
network, or may be blocked when Android's **Block connections without VPN** is
enabled. Skirk itself remains outside the VPN to prevent recursion.

Manual build:

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

Install:

```bash
adb install -r app/build/outputs/apk/debug/app-universal-debug.apk
```

Use:

1. Open Skirk.
2. Import or paste the one-line `skirk:` profile.
3. Select `VPN` for all-app routing, optionally choose specific apps, or select
   `Proxy` for SOCKS and HTTP proxy mode.
4. Tap `Connect`.
5. Approve Android's VPN permission prompt the first time.

Proxy/LAN sharing is explicit. In `Proxy` mode, the Android app starts SOCKS5
and HTTP proxy listeners. Enable LAN sharing only when another trusted device
should use the phone as a proxy.

Android exposes the same performance presets as desktop:

- `Recommended`: balanced default for normal browsing and mixed traffic.
- `Lower usage`: slower polling and fewer workers to reduce Drive API burn.
- `Responsive`: burst polling after traffic, with a visible quota warning.
- `Bulk transfer`: more workers for large downloads.

The Android Drive API estimate is local to the active sidecar process. It is not
project-wide remaining quota and does not include other clients or exits using
the same OAuth project.

Telegram note: when Skirk VPN mode is connected, Telegram's built-in proxy
setting should be off. If Telegram's internal proxy remains enabled, Telegram
keeps testing that internal proxy entry instead of relying on Android VPN
routing.

## iOS prototype

The iOS source is in [`clients/ios/`](../clients/ios/README.md). It uses a
Packet Tunnel extension, an in-process Go Skirk client, and an iOS Keychain
profile. The simulator build and app launch are verified. A signed iPhone with
the Network Extension entitlement is required to test packet routing, recovery,
and real network behavior. There is no iOS release asset yet.

## Android emulator smoke

```bash
adb install -r clients/android/app/build/outputs/apk/debug/app-universal-debug.apk
adb shell am start -n app.skirk.client/.MainActivity
```

Import a disposable profile in the UI, select an app, save, then force-stop and
relaunch Skirk to check persistence. Real VPN packet routing requires a working
Skirk exit and a signed-in test account; the UI smoke alone does not verify it.
