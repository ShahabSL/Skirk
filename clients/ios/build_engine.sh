#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tool_bin="$repo_root/.skirk-runs/ios-tools/bin"
mobile_version="v0.0.0-20261011034528-197770444867"
mkdir -p "$tool_bin" "$repo_root/clients/ios/Frameworks"

for tool in gomobile gobind; do
  if [[ ! -x "$tool_bin/$tool" ]] ||
    ! go version -m "$tool_bin/$tool" | awk -v version="$mobile_version" '
      $1 == "mod" && $2 == "golang.org/x/mobile" && $3 == version { found = 1 }
      END { exit !found }
    '; then
    GOBIN="$tool_bin" go install "golang.org/x/mobile/cmd/$tool@$mobile_version"
  fi
done

export PATH="$tool_bin:$PATH"
# gomobile writes the main module's checkout path into its generated go.mod.
# Build through a temporary alias so Go's module metadata keeps host paths private.
module_root=$(mktemp -d /tmp/skirk-ios-module.XXXXXXXX)
trap 'rm -f "$module_root/source"; rmdir "$module_root"' EXIT
ln -s "$repo_root" "$module_root/source"
cd "$module_root/source"
# The pinned tools are ready. gomobile init would reinstall gobind@latest.
gomobile bind \
  -trimpath \
  -target=ios,iossimulator \
  -o clients/ios/Frameworks/Engine.xcframework \
  ./clients/ios/engine
