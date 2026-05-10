# Skirk Android Client

This directory contains the native Android client. It packages the Go `skirk`
engine inside the APK and runs it as a foreground service. The default mode is
Android VPN mode, with SOCKS proxy mode kept as an explicit fallback.

Current scope:

- import one-line `skirk:` configs or `client.json`;
- save, select, and delete profiles;
- start/stop a whole-device VPN through Android `VpnService`;
- route Android app TCP traffic through Skirk without per-app proxy settings;
- keep proxy mode available for apps or LAN devices that explicitly use SOCKS5;
- optionally bind SOCKS to `0.0.0.0` so the phone can share the proxy on LAN;
- debug-only ADB receiver for E2E tests.

VPN mode uses a real TUN-to-SOCKS bridge and excludes the Skirk app itself from
the VPN route so the tunnel cannot loop into its own interface.

## Build

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

The Gradle build compiles two native pieces:

- `./cmd/skirk` as an Android arm64 PIE executable packaged as
  `lib/arm64-v8a/libskirk.so`;
- the vendored TUN-to-SOCKS bridge packaged as
  `lib/arm64-v8a/libhev-socks5-tunnel.so`.

The app launches the Skirk sidecar with:

```text
skirk client --config <profile-config> --listen <host:port>
```

On Android the service forces Google API transport to `google_front_pinned`.
That avoids the standalone Go resolver path on Android, which can otherwise try
to use `[::1]:53` and fail before the SOCKS listener starts.

## Manual Use

Install the APK, paste the generated one-line `skirk:` profile, leave **Use VPN
mode** enabled, import it, then tap Connect. Android will show the standard VPN
consent dialog the first time. After approval, ordinary apps should use the VPN
without their own proxy settings.

For Telegram, turn Telegram's built-in proxy setting off when using Skirk VPN
mode. If Telegram's own proxy is enabled, Telegram may keep testing its internal
proxy entry instead of relying on Android's VPN routing.

## Debug E2E

```bash
adb install -r app/build/outputs/apk/debug/app-debug.apk
adb shell am start -n app.skirk.client/.MainActivity

CONFIG="$(cat /tmp/skirk-client-config.txt)"
adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.IMPORT \
  --es name Android-E2E \
  --es config "$CONFIG" \
  --ei port 18080 \
  --ez shareLan false \
  --es mode vpn
adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.START

adb shell am start -a android.intent.action.VIEW -d http://example.com/

adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.STOP
adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.DELETE_ALL
```

For SOCKS/LAN sharing tests, import with `--es mode proxy --ez shareLan true`
and connect another device to `PHONE_LAN_IP:18080`.
