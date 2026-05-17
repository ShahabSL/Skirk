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
  mkdir -p "$dist/$name/docs" "$dist/$name/third_party"
  cp docs/setup.md docs/skirk_modes.md docs/go_skirk.md docs/architecture.md "$dist/$name/docs/"
  cp third_party/NOTICE.md "$dist/$name/third_party/"
  if [ "$os" = "windows" ]; then
    (cd "$dist/$name" && python3 -c 'import pathlib, zipfile; z=zipfile.ZipFile("../skirk-windows-amd64.zip", "w", zipfile.ZIP_DEFLATED); [z.write(p, p.as_posix()) for p in pathlib.Path(".").rglob("*") if p.is_file()]; z.close()')
  else
    (cd "$dist/$name" && tar -czf "../$name.tar.gz" .)
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
  build_one windows amd64
  (cd "$dist" && sha256sum skirk-*.tar.gz skirk-*.zip > SHA256SUMS)
  echo "Release artifacts written to $dist"
}

main "$@"
