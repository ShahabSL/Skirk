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

This writes:

- `dist/skirk-linux-amd64.tar.gz`
- `dist/skirk-linux-arm64.tar.gz`
- `dist/skirk-windows-amd64.zip`
- `dist/SHA256SUMS`

Client release assets are built by GitHub Actions:

- Windows portable desktop zip
- Windows desktop installer

The Android workflow validates that a debug APK still builds, but debug-signed
APKs are not uploaded as public release assets.

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

## Android Signing

Debug APKs are for local sideload testing only. A production Android release
must use a release keystore through GitHub Actions secrets and publish a signed
APK or AAB.

## Operational Validation

Before tagging, validate the public setup flow from a clean Linux machine:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_VERSION=vX.Y.Z sh
export PATH="$HOME/.local/bin:$PATH"
skirk version
skirk setup init --out skirk-kit
skirk serve-exit --config skirk-kit/exit.json
```

In another terminal with the generated client profile:

```bash
skirk bench-live --config skirk-kit/client.skirk --samples 3
```

## Cleanup Validation

Manual cleanup dry-run:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
```

OAuth revocation:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

`revoke` invalidates the embedded OAuth token. It does not delete a visible
Drive folder because the production mailbox uses Drive `appDataFolder`.
