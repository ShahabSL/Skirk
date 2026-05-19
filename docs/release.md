# Release Checklist

## Before Publishing

Run:

```bash
make preflight
```

Include desktop and Android checks:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```

Confirm no local runtime artifacts are tracked:

```bash
git status --short
git ls-files \
  .skirk-runs private skirk-kit skirk-config bin dist cloud_resources probe_results sources zips \
  application_default_credentials.json skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key'
```

The second command should print nothing.

## Version

Choose the release version explicitly:

```bash
VERSION=vX.Y.Z make package-release
```

Official release builds must include the Skirk OAuth client so setup does not
ask users for `oauth-client.json`. The GitHub release workflow fails unless
these repository secrets are configured before tagging:

- `SKIRK_OAUTH_CLIENT_ID`
- `SKIRK_OAUTH_CLIENT_SECRET`
- `SKIRK_ANDROID_KEYSTORE_BASE64`
- `SKIRK_ANDROID_KEYSTORE_PASSWORD`
- `SKIRK_ANDROID_KEY_ALIAS`
- `SKIRK_ANDROID_KEY_PASSWORD`

Local release smoke builds can use the same variables:

```bash
SKIRK_OAUTH_CLIENT_ID='...' \
SKIRK_OAUTH_CLIENT_SECRET='...' \
VERSION=vX.Y.Z make package-release
```

The built-in easy setup path uses Google's device-code OAuth flow. Google's
token polling requires both the client ID and client secret for that flow.

This writes:

- `dist/skirk-linux-amd64.tar.gz`
- `dist/skirk-linux-arm64.tar.gz`
- `dist/skirk-windows-amd64.zip` (Windows CLI)
- `dist/SHA256SUMS`

Client release assets are built by GitHub Actions:

- Windows portable desktop zip (`Skirk_windows_x64_portable.zip`) for normal GUI use.
- Windows CLI zip (`skirk-windows-amd64.zip`) for manual PowerShell use. This
  asset is not the desktop app.
- Android arm64 APK (`skirk-android-arm64.apk`) for sideload testing. The
  GitHub release tag carries the version; the APK filename intentionally stays
  stable for simple user instructions.

The workflow publishes SHA-256 checksums and GitHub artifact attestations for
the APK and archives. Verify a downloaded asset with:

```bash
gh attestation verify ./skirk-android-arm64.apk -R ShahabSL/Skirk
sha256sum -c SHA256SUMS
```

## Publish

The release workflow publishes artifacts when a `v*` tag is pushed:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

After the release exists, Linux users can install with:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

Or pin the version:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_VERSION=vX.Y.Z sh
```

Equivalent pinned install using an argument:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh -s -- --version vX.Y.Z
```

## Android Signing

The Android release workflow builds `assembleRelease`, verifies the APK with
`apksigner verify --print-certs`, and uploads the signed APK. Rotate the
keystore only deliberately; changing Android signing keys means existing
sideload users cannot update in place without uninstalling first.

Current Android release signing certificate SHA-256:

```text
45c73cd055ad189ff421e4bd84facbc2512ab26e505aed4b0d867ee6e9c347cf
```

## Operational Validation

Before tagging, validate the public setup flow from a clean Linux machine:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_VERSION=vX.Y.Z sh
export PATH="$HOME/.local/bin:$PATH"
skirk version
skirk setup init --out skirk-kit --reset-google-login
skirk serve-exit --config skirk-kit/exit.json
```

Also validate that pinned release updates cannot be redirected by inherited
installer environment:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | \
  SKIRK_REPO=bad/repo SKIRK_ASSET_BASE=file:///tmp/bad SKIRK_VERSION=vX.Y.Z sh
skirk version
```

In another terminal with the generated client profile:

```bash
skirk bench-live --config skirk-kit/client.skirk --samples 3
```

## Cleanup Validation

Manual cleanup dry-run:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
skirk cleanup --config skirk-kit/exit.json --all --older-than 1ns --delete --max-pages 20000
```

OAuth revocation:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

`revoke` invalidates the embedded OAuth token. Delete the generated
`skirk-mailbox-...` Drive folder manually if you want to remove mailbox
leftovers immediately.
