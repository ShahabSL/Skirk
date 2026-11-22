#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 APK [APK ...]" >&2
  exit 2
fi

expected=$(go env GOVERSION)
engine=$(mktemp)
trap 'rm -f "$engine"' EXIT

for apk in "$@"; do
  unzip -p "$apk" lib/armeabi-v7a/libskirk.so > "$engine"
  version=$(go version -m -json "$engine" | jq -er '
    (.Settings | from_entries) as $settings |
    select(.Path == "github.com/ShahabSL/Skirk/cmd/skirk" and
           $settings.GOOS == "android" and $settings.GOARCH == "arm" and
           ($settings.GOARM | split(",")[0]) == "7") | .GoVersion
  ') || { echo "$apk has no identifiable Android ARMv7 Skirk engine" >&2; exit 1; }

  # Go 1.26.0–1.26.2 probe futex_time64 on older 32-bit Android (golang/go#77880).
  case "$version" in
    go1.26.0|go1.26.1|go1.26.2|go1.26beta*|go1.26rc*)
      echo "$apk contains $version, which crashes on Android 8–10 ARMv7" >&2
      exit 1
      ;;
  esac
  if [ "$version" != "$expected" ]; then
    echo "$apk contains $version, expected the build toolchain $expected" >&2
    exit 1
  fi
  echo "$apk passed Android ARMv7 engine verification ($version)"
done
