# Development

## Repository Hygiene

The repository should stay reviewable from `git status --short`. Runtime,
benchmark, credential, and release artifacts are ignored and should not be
committed.

Important ignored locations:

- `.skirk-runs/`: local benchmark and protocol lab records;
- `private/`: local credentials, configs, and operator notes;
- `skirk-kit/`, `skirk-config/`, `*.skirk`: generated profiles and kits;
- `bin/`, `dist/`: local builds and release archives;
- `.skirk-runs/probe_results/`, `.skirk-runs/cloud_resources/`: external-service probes;
- `cloud_resources/`, `probe_results/`: legacy probe output directories kept ignored
  in case old scripts or notes are rerun;
- client build outputs such as `node_modules/`, Gradle build directories, and
  Tauri targets.

Before opening a pull request:

```bash
git status --short
git ls-files \
  .skirk-runs private skirk-kit skirk-config bin dist cloud_resources probe_results sources zips \
  application_default_credentials.json skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key'
```

The second command should print nothing.

## Normal Checks

```bash
make preflight
```

Include desktop and Android checks:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```

Useful focused checks:

```bash
go test ./...
go vet ./...
go test -race ./internal/skirk
```

## GitHub Smoke Coverage

The public repository has a heavier `Smoke` workflow for platform coverage that
is too broad for every local edit. It runs on GitHub's standard hosted runners
for:

- Linux x64 and arm64 CLI tests;
- Windows x64 and arm64 CLI tests;
- macOS Intel and Apple Silicon CLI tests;
- Linux, Windows, and macOS desktop build/package smoke;
- Android debug APK build plus static ABI/package validation.

This uses public-repository hosted runners only. It does not prove physical
Android VPN behavior, Windows administrator VPN behavior, hostile-network
latency, or live Google Drive quota behavior. Those still need real devices,
real Windows hosts, and live Drive credentials under `.skirk-runs/`.

## Live Transport Testing

Live Drive tests require real credentials and should remain manual. Keep outputs
under `.skirk-runs/` and do not paste generated configs into public logs.

Minimum live smoke test:

```bash
skirk serve-exit --config skirk-kit/exit.json
skirk bench-live --config skirk-kit/client.skirk --samples 5
```

For transport changes, use same-day paired controls. Run muxv4 and the candidate
against the same exit, route, target URLs, binary build, and cleanup state.

Do not promote a candidate on single-stream speed alone. The gate is mixed
browsing and bulk behavior.
