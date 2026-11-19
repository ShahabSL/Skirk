# Skirk information site

This standalone Cloudflare Worker serves the Skirk overview, Google Drive
privacy disclosure, and terms at `https://skirk.getephemra.com`. It handles no
Skirk transport traffic, OAuth callbacks, or profile data. The content is
adapted from the draft pages in
[ShahabSL/shahablavasani.com PR #1](https://github.com/ShahabSL/shahablavasani.com/pull/1).

The Worker has no bindings or secrets. `wrangler.jsonc` attaches only the exact
`skirk.getephemra.com` Custom Domain; Cloudflare creates its DNS record and
certificate. It does not configure any other hostname.

From this directory, with Wrangler authenticated to the account in the config:

```sh
node --check src/index.js
wrangler deploy --dry-run
wrangler deploy
```

After deployment, verify `/`, `/privacy`, and `/terms` return HTTP 200 and
`/skirk` redirects to `/`. Keep the privacy disclosure aligned with changes to
Drive scopes, mailbox metadata, credential storage, and exit behavior.
