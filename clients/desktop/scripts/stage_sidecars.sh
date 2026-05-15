#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
desktop_root="$repo_root/clients/desktop"
version="${VERSION:-$(git -C "$repo_root" describe --tags --always --dirty 2>/dev/null || echo dev)}"
commit="${COMMIT:-$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
date="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"

mkdir -p "$desktop_root/src-tauri/resources/sidecars/linux"
mkdir -p "$desktop_root/src-tauri/resources/sidecars/windows"

GOOS=linux GOARCH=amd64 go build -C "$repo_root" -trimpath -ldflags "$ldflags" -o "$desktop_root/src-tauri/resources/sidecars/linux/skirk" ./cmd/skirk
GOOS=windows GOARCH=amd64 go build -C "$repo_root" -trimpath -ldflags "$ldflags" -o "$desktop_root/src-tauri/resources/sidecars/windows/skirk-sidecar.exe" ./cmd/skirk

chmod +x "$desktop_root/src-tauri/resources/sidecars/linux/skirk"
