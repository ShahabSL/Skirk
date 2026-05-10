# Skirk Android Client

This directory contains the native Android proxy client. It packages the Go `skirk`
engine inside the APK and runs it as a foreground service.

Current scope:

- import one-line `skirk:` configs or `client.json`;
- save, select, and delete profiles;
- start/stop a local SOCKS5 proxy;
- optionally bind SOCKS to `0.0.0.0` so the phone can share the proxy on LAN;
- debug-only ADB receiver for E2E tests.

The Android client is proxy-mode first. It does not claim whole-device VPN mode
until a real TUN-to-SOCKS forwarding layer is added.

## Build

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

The Gradle build compiles `./cmd/skirk` as an Android arm64 PIE executable and
packages it under `lib/arm64-v8a/libskirk.so`. The app launches that executable
with:

```text
skirk client --config <profile-config> --listen <host:port>
```

On Android the service forces Google API transport to `google_front_pinned`.
That avoids the standalone Go resolver path on Android, which can otherwise try
to use `[::1]:53` and fail before the SOCKS listener starts.

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
  --ez shareLan true
adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.START

curl --socks5-hostname PHONE_LAN_IP:18080 http://127.0.0.1:8000/1m.bin -o /tmp/1m.bin

adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.STOP
adb shell am broadcast -n app.skirk.client/.DebugControlReceiver \
  -a app.skirk.client.debug.DELETE_ALL
```
