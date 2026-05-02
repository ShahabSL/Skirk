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

Android is different from desktop because Android apps cannot become whole-device VPNs by opening a SOCKS listener alone. The standard native path is an Android `VpnService` that owns a TUN interface and forwards traffic into the Skirk tunnel engine.

The repository includes an Android project scaffold under `clients/android` with:

- config import UI;
- shadcn/ChatGPT-inspired neutral styling using native Compose Material 3;
- `VpnService` permission wiring;
- a placeholder service boundary for the Go tunnel engine bridge.

The Android network engine is intentionally not marked production-complete until the Go core is bound with `gomobile` or reimplemented behind the same config contract. Desktop Linux/Windows clients are the production clients in this repository today.
