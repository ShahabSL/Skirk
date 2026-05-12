#!/usr/bin/env sh
set -eu

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
commit="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
date="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
dist="${DIST_DIR:-dist}"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"

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
  cp README.md README.fa.md LICENSE "$dist/$name/"
  if [ "$os" = "windows" ]; then
    (cd "$dist/$name" && python3 -c 'import pathlib, zipfile; z=zipfile.ZipFile("../skirk-windows-amd64.zip", "w", zipfile.ZIP_DEFLATED); [z.write(p, p.name) for p in pathlib.Path(".").iterdir()]; z.close()')
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
