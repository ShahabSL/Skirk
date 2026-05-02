#!/bin/sh
set -eu

repo="${SKIRK_REPO:-ShahabSL/Skirk}"
version="${SKIRK_VERSION:-latest}"
install_dir="${SKIRK_INSTALL_DIR:-$HOME/.local/bin}"
asset_base="${SKIRK_ASSET_BASE:-}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: missing required command: $1" >&2
    exit 1
  }
}

detect_platform() {
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$os" in
    linux) os="linux" ;;
    *) echo "error: this installer currently supports Linux exit/client machines only (got $os)" >&2; exit 1 ;;
  esac
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "error: unsupported CPU architecture: $arch" >&2; exit 1 ;;
  esac
  echo "$os-$arch"
}

download() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    echo "error: install curl or wget" >&2
    exit 1
  fi
}

install_from_release() {
  platform="$1"
  tmp="$2"
  asset="skirk-$platform.tar.gz"
  if [ -n "$asset_base" ]; then
    url="$asset_base/$asset"
  elif [ "$version" = "latest" ]; then
    url="https://github.com/$repo/releases/latest/download/$asset"
  else
    url="https://github.com/$repo/releases/download/$version/$asset"
  fi

  echo "Downloading $url"
  if ! download "$url" "$tmp/$asset"; then
    return 1
  fi
  tar -xzf "$tmp/$asset" -C "$tmp"
  if [ ! -x "$tmp/skirk" ]; then
    echo "error: release archive did not contain executable skirk" >&2
    return 1
  fi
  mkdir -p "$install_dir"
  cp "$tmp/skirk" "$install_dir/skirk"
  chmod 0755 "$install_dir/skirk"
}

install_from_source() {
  tmp="$1"
  need go
  ref="$version"
  if [ "$ref" = "latest" ]; then
    ref="main"
  fi
  url="https://github.com/$repo/archive/refs/heads/$ref.tar.gz"
  case "$ref" in
    v*) url="https://github.com/$repo/archive/refs/tags/$ref.tar.gz" ;;
  esac

  echo "Release asset unavailable; building from source at $url"
  download "$url" "$tmp/source.tar.gz"
  mkdir "$tmp/source"
  tar -xzf "$tmp/source.tar.gz" -C "$tmp/source" --strip-components 1
  mkdir -p "$install_dir"
  (
    cd "$tmp/source"
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$install_dir/skirk" ./cmd/skirk
  )
  chmod 0755 "$install_dir/skirk"
}

main() {
  need uname
  need tar
  platform="$(detect_platform)"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  if ! install_from_release "$platform" "$tmp"; then
    install_from_source "$tmp"
  fi

  echo
  "$install_dir/skirk" version
  echo "Installed: $install_dir/skirk"
  case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) echo "Add this to your shell profile if skirk is not found: export PATH=\"$install_dir:\$PATH\"" ;;
  esac
  echo
  echo "Next: run 'skirk' for the operator menu, or 'skirk setup init --out skirk-kit'."
}

main "$@"
