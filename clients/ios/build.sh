#!/usr/bin/env bash
set -euo pipefail

ios_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$ios_dir/build_engine.sh"
"$ios_dir/build_hev.sh"
cd "$ios_dir"
xcodegen generate
xcodebuild -project Skirk.xcodeproj -scheme Skirk \
  -configuration Debug -sdk iphonesimulator \
  -destination 'generic/platform=iOS Simulator' \
  CODE_SIGNING_ALLOWED=YES CODE_SIGN_IDENTITY=- build
