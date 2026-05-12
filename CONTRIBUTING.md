# Contributing

Skirk is intended to stay small, reviewable, and explicit about its security boundaries.

Contributions must preserve the legal and acceptable-use boundary in [DISCLAIMER.md](DISCLAIMER.md): lawful, authorized, owned-account and owned-network use only.

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

## Testing External Services

Unit tests should not require Google credentials or network access. Live Google tests belong in manual verification docs or explicitly marked integration runs.
