#!/usr/bin/env sh
set -eu

tracked_runtime="$(git ls-files -- \
  .skirk-runs private skirk-kit skirk-config bin dist \
  cloud_resources probe_results sources zips skirk_research_bundle \
  application_default_credentials.json google-services.json oauth-client.json \
  skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key' \
  '*.jks' '*.keystore' '*.p12' '*.pfx' 2>/dev/null || true)"
if [ -n "$tracked_runtime" ]; then
  echo "error: runtime/research artifacts are tracked:" >&2
  echo "$tracked_runtime" >&2
  exit 1
fi

email_tmp="$(mktemp)"
secret_tmp="$(mktemp)"
trap 'rm -f "$email_tmp" "$secret_tmp"' EXIT INT TERM

if git grep -IlEi '[[:alnum:]_.%+-]+@(gmail|googlemail|icloud|me|hotmail|outlook|yahoo)[.](com|net)' -- . ':!third_party/**' ':!clients/licenses/**' ':!scripts/preflight.sh' >"$email_tmp" 2>/dev/null; then
  echo "error: tracked files contain personal/work email residue:" >&2
  cat "$email_tmp" >&2
  exit 1
fi

secret_pattern='ya29\.[A-Za-z0-9._-]{20,}|AIza[0-9A-Za-z_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY|"(refresh_token|client_secret|private_key|client_email)"[[:space:]]*:[[:space:]]*"[^"]{20,}"'
if git grep -IEn "$secret_pattern" -- . ':!scripts/preflight.sh' >"$secret_tmp" 2>/dev/null; then
  echo "error: tracked files look like they contain generated credentials:" >&2
  cat "$secret_tmp" >&2
  exit 1
fi

git diff --check
go test ./...
go vet ./...

if [ "${SKIRK_FULL_PREFLIGHT:-0}" = "1" ]; then
  (cd clients/desktop && npm ci && npm run build)
  if [ -n "${ANDROID_HOME:-}" ] || [ -n "${ANDROID_SDK_ROOT:-}" ]; then
    (cd clients/android && ./gradlew :app:assembleDebug --console=plain)
  else
    echo "Skipping Android build because ANDROID_HOME/ANDROID_SDK_ROOT is not set."
  fi
fi

echo "preflight ok"
