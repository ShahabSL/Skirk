# Contributing

Skirk is intended to stay small, reviewable, and explicit about its security boundaries.

Contributions must preserve the legal and acceptable-use boundary in [DISCLAIMER.md](DISCLAIMER.md): lawful, authorized, owned-account and owned-network use only.

## Repository Hygiene

Keep pull requests focused and keep generated artifacts out of the tracked tree.
Before sending a change, run:

```bash
git status --short
git ls-files \
  .skirk-runs private skirk-kit skirk-config bin dist cloud_resources probe_results sources zips \
  application_default_credentials.json skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key'
```

The second command should print nothing. Local benchmark history belongs under
`.skirk-runs/`; credentials and operator-local files belong under `private/`.

## Local Checks

Run the normal preflight before opening a pull request:

```bash
make preflight
```

For desktop and Android checks too:

```bash
SKIRK_FULL_PREFLIGHT=1 make preflight
```

## Commit Style

Use Conventional Commits:

- `feat: add new behavior`
- `fix: correct a bug`
- `docs: update documentation`
- `test: add or adjust tests`
- `chore: maintain build/release tooling`

## Secrets

Never commit generated Skirk configs or Google credentials. These files are ignored by default:

- `skirk-kit/`
- `skirk-config/`
- `skirk.json`
- `*.skirk`
- `.skirk-runs/`
- `private/`
- `bin/`
- `dist/`
- `probe_results/`
- `cloud_resources/`

Generated `client.json` and `exit.json` files contain a Google refresh token and the Skirk tunnel secret. Treat them like passwords.

## Design Rules

- Default to the Go CLI for core transport behavior.
- Keep Linux headless operation first-class.
- Keep Windows and Android clients as wrappers around the same config model.
- Avoid local TLS MITM in the default path.
- Do not add unauthenticated public relay behavior.
- Keep Drive appData cleanup and OAuth revocation paths working when config format changes.
- Do not promote transport changes on single-stream speed alone. Use same-day
  muxv4 controls and mixed browsing/bulk gates.

## Testing External Services

Unit tests should not require Google credentials or network access. Live Google tests belong in manual verification docs or explicitly marked integration runs.
