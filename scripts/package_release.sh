#!/usr/bin/env sh
set -eu

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
commit="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
date="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
dist="${DIST_DIR:-dist}"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"
oauth_client_id="${SKIRK_OAUTH_CLIENT_ID:-}"
oauth_client_secret="${SKIRK_OAUTH_CLIENT_SECRET:-}"
if [ "${SKIRK_REQUIRE_BUILTIN_OAUTH:-0}" = "1" ]; then
  if [ -z "$oauth_client_id" ] || [ -z "$oauth_client_secret" ]; then
    echo "error: release builds require SKIRK_OAUTH_CLIENT_ID and SKIRK_OAUTH_CLIENT_SECRET for the built-in device-code OAuth client" >&2
    exit 1
  fi
fi
if [ -n "$oauth_client_id" ] || [ -n "$oauth_client_secret" ]; then
  if [ -z "$oauth_client_id" ]; then
    echo "error: SKIRK_OAUTH_CLIENT_ID must be set when SKIRK_OAUTH_CLIENT_SECRET is set" >&2
    exit 1
  fi
  ldflags="$ldflags -X main.defaultOAuthClientID=$oauth_client_id -X main.defaultOAuthClientSecret=$oauth_client_secret"
fi

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: missing required command: $1" >&2
    exit 1
  }
}

build_one() {
  os="$1"
  arch="$2"
  name="skirk-$os-$arch"
  out="$dist/$name/skirk"
  if [ "$os" = "windows" ]; then
    out="$dist/$name/skirk.exe"
  fi
  mkdir -p "$dist/$name"
  echo "Building $name"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/skirk
  cp README.md README.fa.md LICENSE DISCLAIMER.md SECURITY.md CHANGELOG.md "$dist/$name/"
  mkdir -p "$dist/$name/docs/images" "$dist/$name/third_party"
  cp docs/install.md docs/install.fa.md docs/setup.md docs/personal-google-setup.md docs/personal-google-setup.fa.md docs/skirk_modes.md docs/go_skirk.md docs/architecture.md docs/udp-preview-platform-parity.md "$dist/$name/docs/"
  cp -R docs/images/. "$dist/$name/docs/images/"
  cp clients/licenses/engine.txt "$dist/$name/third_party/Go-licenses.txt"
  cp third_party/NOTICE.md "$dist/$name/third_party/"
  if [ "$os" = "windows" ]; then
    (cd "$dist/$name" && python3 - "$name" <<'PY'
import pathlib
import sys
import zipfile

with zipfile.ZipFile(f"../{sys.argv[1]}.zip", "w", zipfile.ZIP_DEFLATED) as archive:
    for path in sorted(pathlib.Path(".").rglob("*")):
        if path.is_file():
            archive.write(path, path.as_posix())
PY
    )
  else
    (cd "$dist/$name" && COPYFILE_DISABLE=1 tar -czf "../$name.tar.gz" .)
  fi
}

main() {
  need go
  need tar
  need python3
  need sha256sum
  rm -rf "$dist"
  mkdir -p "$dist"
  build_one linux amd64
  build_one linux arm64
  build_one darwin amd64
  build_one darwin arm64
  build_one windows amd64
  build_one windows arm64
  (cd "$dist" && sha256sum skirk-*.tar.gz skirk-*.zip > SHA256SUMS)
  echo "Release artifacts written to $dist"
}

main "$@"
