# Examples

This directory contains sanitized examples only. Do not copy real generated
profiles, Google OAuth files, refresh tokens, or operator-local configs here.

`skirk.config.example.json` shows the public shape of a generated Skirk config,
but real configs contain secrets and belong under ignored local paths such as
`skirk-kit/`, `skirk-config/`, or `private/`.

To regenerate a sample config locally:

```bash
skirk sample-config --out /tmp/skirk.example.json
```

Before committing, confirm no generated configs or profiles are tracked:

```bash
git ls-files \
  .skirk-runs private skirk-kit skirk-config \
  application_default_credentials.json skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key'
```
