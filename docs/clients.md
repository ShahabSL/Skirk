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
export PATH="$HOME/.local/bin:$PATH"
```

Run:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

Stable identity for repeated use on the same Linux machine:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

Or paste the one-line profile:

```bash
read -r SKIRK_CLIENT_CONFIG
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

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

## Windows Desktop

The preferred Windows UX is the portable desktop app from release assets:
`Skirk_windows_x64_portable.zip`. Extract it and open the root `Skirk.exe`.
The `skirk-windows-amd64.zip` asset is the CLI-only build for PowerShell users.

The desktop app:

- imports one-line `skirk:` profiles or `client.json`;
- stores profiles in portable data;
- assigns each imported profile a local client identity;
- starts and stops the Go Skirk SOCKS sidecar;
- can bind the SOCKS listener to `0.0.0.0` for LAN sharing;
- shows connection status and logs.

Windows is currently proxy-first. It does not install a system VPN/TUN driver.
Configure the browser or application proxy settings to SOCKS5
`127.0.0.1:18080`.

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

## Android

The Android app packages the Go Skirk engine and starts it as a foreground
service. Each imported Android profile gets a UUID-backed local client identity.
The default UX is whole-device VPN mode.

Manual build:

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

Install:

```bash
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Use:

1. Open Skirk.
2. Import or paste the one-line `skirk:` profile.
3. Select `VPN` for all-app routing, or `Proxy` for SOCKS-only mode.
4. Tap `Connect`.
5. Approve Android's VPN permission prompt the first time.

Proxy/LAN sharing is explicit. In `Proxy` mode, enable LAN sharing only when
another device should use the phone as a SOCKS5 proxy.

Telegram note: when Skirk VPN mode is connected, Telegram's built-in proxy
setting should be off. If Telegram's internal proxy remains enabled, Telegram
keeps testing that internal proxy entry instead of relying on Android VPN
routing.

## Debug E2E On Android

```bash
adb install -r clients/android/app/build/outputs/apk/debug/app-debug.apk
adb shell am start -n app.skirk.client/.MainActivity

CONFIG="$(cat skirk-kit/client.skirk)"
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
```

For SOCKS/LAN sharing tests, import with `--es mode proxy --ez shareLan true`
and connect another device to `PHONE_LAN_IP:18080`.
