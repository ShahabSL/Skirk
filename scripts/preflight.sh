#!/usr/bin/env sh
set -eu

tracked_runtime="$(git ls-files -- \
  .skirk-runs private skirk-kit skirk-config bin dist \
  cloud_resources probe_results sources zips skirk_research_bundle \
  application_default_credentials.json google-services.json \
  ':(glob)**/oauth-client.json' ':(glob)**/oauth-client-*.json' \
  ':(glob)**/client_secret_*.apps.googleusercontent.com.json' \
  ':(glob)**/client-qr.png' \
  skirk.json client.json exit.json \
  '*.skirk' '*.secret' '*.token' '*.pem' '*.key' \
  '*.jks' '*.keystore' '*.p12' '*.pfx' 2>/dev/null || true)"
if [ -n "$tracked_runtime" ]; then
  echo "error: runtime/research artifacts are tracked:" >&2
  echo "$tracked_runtime" >&2
  exit 1
fi

# Third-party copyright notices sometimes include an upstream author's address.
# Keep those notices intact. Report only paths so a failure cannot expose the
# matching address or credential in a public CI log.
personal_email_pattern='[[:alnum:]_.%+-]+@(gmail|googlemail|icloud|me|hotmail|outlook|yahoo)[.](com|net)'
if git grep -IlEi "$personal_email_pattern" -- . ':!third_party/**' ':!clients/licenses/**' ':!scripts/preflight.sh'; then
  echo "error: check the listed first-party files for a personal email address" >&2
  exit 1
fi

secret_pattern='ya29[.][A-Za-z0-9._-]{20,}|1//[A-Za-z0-9_-]{24,}|GOCSPX-[A-Za-z0-9_-]{18,}|AIza[0-9A-Za-z_-]{25,}|-----BEGIN [A-Z ]*PRIVATE KEY|"((refresh|access)_token|client_secret|private_key|client_email)"[[:space:]]*:[[:space:]]*"[^"]{20,}"'
if git grep -IlE "$secret_pattern" -- . ':!scripts/preflight.sh'; then
  echo "error: check the listed files for generated credentials" >&2
  exit 1
fi

git diff --check
sh -n install.sh
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck install.sh scripts/test_installer.sh
else
  echo "Skipping shellcheck because it is not installed."
fi
scripts/test_installer.sh
go test ./...
if [ "$(go env GOOS)" = "linux" ]; then
  GOARCH=386 CGO_ENABLED=0 go test ./internal/skirk -run 'Test(NewTunnelStartsOn32BitAtomicRuntime|TunnelSOCKSToExitWithMemoryStores|DecodeMuxBatchBounds)' -count=1
else
  # macOS cannot execute a Linux/386 test binary, but can still catch build errors.
  GOOS=linux GOARCH=386 CGO_ENABLED=0 go test -c -o /dev/null ./internal/skirk
fi
go vet ./...

if [ "${SKIRK_FULL_PREFLIGHT:-0}" = "1" ]; then
  (cd clients/desktop && npm ci && npm run test:snapshot && npm run build)
  if [ -n "${ANDROID_HOME:-}" ] || [ -n "${ANDROID_SDK_ROOT:-}" ]; then
    (cd clients/android && ./gradlew :app:testDebugUnitTest :app:assembleDebug --console=plain)
  else
    echo "Skipping Android build because ANDROID_HOME/ANDROID_SDK_ROOT is not set."
  fi
fi

echo "preflight ok"
