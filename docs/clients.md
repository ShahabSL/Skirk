# Skirk Clients

## Linux

Linux uses the Go binary directly:

```bash
./bin/skirk client --config client.skirk --listen 127.0.0.1:18080
```

This is the correct path for headless Linux servers, terminal-only desktops, and SSH sessions.

For a desktop Linux machine with a browser, an optional local dashboard is available:

```bash
./bin/skirk client-ui --config client.skirk --socks 127.0.0.1:18080 --ui 127.0.0.1:18280
```

Open `http://127.0.0.1:18280`.

## Windows

Preferred Windows UX is the portable desktop app under `clients/desktop`. It imports the one-line `client.skirk` text config, stores profiles in portable data, and starts/stops the Go Skirk SOCKS sidecar.

Development run:

```bash
make build-windows
clients/desktop/scripts/stage_sidecars.sh
cd clients/desktop
npm install
npm run tauri dev
```

Portable release layout:

```text
Skirk.exe
skirk-portable
portable-data/
sidecars/windows/skirk.exe
```

The command-line Windows client is still available:

```bash
make build-windows
```

Run it from PowerShell:

```powershell
.\skirk-windows-amd64.exe client-ui --config .\client.skirk --socks 127.0.0.1:18080 --ui 127.0.0.1:18280
```

Configure browser or application proxy settings to SOCKS5 `127.0.0.1:18080`.

The dashboard is optional on Windows too. The non-GUI command also works:

```powershell
.\skirk-windows-amd64.exe client --config .\client.skirk --listen 127.0.0.1:18080
```

## Android

Android currently ships as a native proxy client. It packages the Go `skirk`
engine inside the APK, imports the same one-line `skirk:` config, and runs a
foreground SOCKS5 service.

Build:

```bash
cd clients/android
./gradlew :app:assembleDebug --console=plain
```

Install `app/build/outputs/apk/debug/app-debug.apk`, paste the generated
one-line config, import it, then tap Connect. The default listener is
`127.0.0.1:18080`; enable LAN sharing to bind `0.0.0.0:18080` and let another
device use the phone as a SOCKS5 proxy.

Whole-device VPN mode is not released yet. A correct VPN mode needs Android
`VpnService` plus a real TUN-to-SOCKS forwarding engine such as a tun2socks
component. The previous fake `VpnService` scaffold was removed because it
created a VPN interface but did not forward packets.
