# Release Checklist

## Before Publishing

Run:

```bash
make preflight
```

For client UI checks too:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```

Confirm no local runtime artifacts are tracked:

```bash
git status --short
git ls-files probe_results cloud_resources sources zips skirk-kit skirk-config
```

The second command should print nothing.

## Build Release Archives

```bash
VERSION=v0.1.3 make package-release
```

This writes:

- `dist/skirk-linux-amd64.tar.gz`
- `dist/skirk-linux-arm64.tar.gz`
- `dist/skirk-windows-amd64.zip`
- `dist/SHA256SUMS`

Client release assets are built by GitHub Actions:

- `skirk-android-arm64-preview.apk`
- `Skirk_windows_x64_portable.zip`
- `skirk-desktop-windows-x64-installer.exe`

## Publish A GitHub Release

The included GitHub Actions release workflow publishes these artifacts when a `v*` tag is pushed:

```bash
git tag v0.1.3
git push origin v0.1.3
```

The Android APK is a debug-signed preview build so it can be sideloaded without
release-signing secrets. A production Android release should add a signing
keystore through GitHub Actions secrets and publish a signed release APK or AAB.

After the release exists, Linux users can install with:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

## Operational Cleanup

Skirk-created Google resources can be deleted with:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

This deletes the Sheet and Drive folder by default, then revokes the refresh token when `--revoke-oauth` is provided.
