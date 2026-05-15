# Security Policy

## Sensitive Files

Generated Skirk configs are credentials. `client.skirk`, `client.json`, and
`exit.json` can contain:

- a Google OAuth refresh token;
- a Google OAuth client ID and client secret;
- the Skirk tunnel encryption secret;
- the Drive appData mailbox settings.

Do not commit generated configs, paste them into logs, or share them outside the intended client/exit devices.

Raw benchmark artifacts under `.skirk-runs/` can also contain target URLs,
timing history, account or project hints, local paths, and logs. Keep them
private or scrub them before sharing.

## Revocation

If a config leaks:

1. Stop the exit.
2. Revoke the Google OAuth access:

   ```bash
   skirk revoke --config skirk-kit/exit.json --revoke-oauth
   ```

   If the config is unavailable, revoke the app access from the Google account security page.

3. Generate a new kit.

OAuth revocation invalidates refresh tokens so leaked configs cannot mint new Google access tokens.

## Trust Boundary

The Google account stores encrypted Skirk chunks and control metadata. The exit machine dials target hosts and can see target addresses. Non-TLS application payloads are visible to the exit, as with any proxy or VPN exit. HTTPS payloads remain protected by the destination site's TLS.

## Responsible Use

Skirk is intended for owned accounts, owned exits, and authorized network testing. Do not run it as an unauthenticated public relay.

See [DISCLAIMER.md](DISCLAIMER.md) for the full legal and acceptable-use notice.
