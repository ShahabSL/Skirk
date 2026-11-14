#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tool_bin="$repo_root/.skirk-runs/ios-tools/bin"
mobile_version="v0.0.0-20261011034528-197770444867"
mkdir -p "$tool_bin" "$repo_root/clients/ios/Frameworks"

if [[ ! -x "$tool_bin/gomobile" || ! -x "$tool_bin/gobind" ]]; then
  GOBIN="$tool_bin" go install "golang.org/x/mobile/cmd/gomobile@$mobile_version"
  GOBIN="$tool_bin" go install "golang.org/x/mobile/cmd/gobind@$mobile_version"
fi

export PATH="$tool_bin:$PATH"
cd "$repo_root"
gomobile init
gomobile bind \
  -target=ios,iossimulator \
  -o clients/ios/Frameworks/Engine.xcframework \
  ./clients/ios/engine
