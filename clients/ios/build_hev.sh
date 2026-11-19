#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source_dir="$repo_root/third_party/hev-socks5-tunnel"
work_dir="$repo_root/.skirk-runs/ios-hev-src"
output_dir="$repo_root/clients/ios/Frameworks"
mkdir -p "$work_dir" "$output_dir"

# The Android NDK also builds this vendored C tree. Keep Apple objects in an
# ignored copy so platform-specific .d files and libraries never cross-build.
rsync -a --delete \
  --exclude '/obj/' --exclude '/bin/' --exclude '/libs/' \
  --exclude '/apple_xcframework/' --exclude '/HevSocks5Tunnel.xcframework/' \
  "$source_dir/" "$work_dir/"

# The vendored tree can contain ignored nested third-part/*.a outputs from
# another SDK. The device slice builds first, so clear those before make can
# treat them as up to date and merge incompatible objects into its archive.
make -C "$work_dir" clean

build_slice() {
  local sdk="$1" architecture="$2" min_version="$3" destination="$4"
  (
    cd "$work_dir"
    make PP="xcrun --sdk $sdk --toolchain $sdk clang" \
      CC="xcrun --sdk $sdk --toolchain $sdk clang" \
      CFLAGS="-arch $architecture -m${sdk}-version-min=$min_version" \
      LFLAGS="-arch $architecture -m${sdk}-version-min=$min_version -Wl,-Bsymbolic-functions" \
      static
    mkdir -p "$(dirname "$destination")"
    libtool -static -o "$destination" \
      bin/libhev-socks5-tunnel.a \
      third-part/lwip/bin/liblwip.a \
      third-part/yaml/bin/libyaml.a \
      third-part/hev-task-system/bin/libhev-task-system.a
    make clean
  )
}

slice_dir="$repo_root/.skirk-runs/ios-hev-slices"
mkdir -p "$slice_dir"
build_slice iphoneos arm64 17.0 "$slice_dir/iphoneos-arm64.a"
build_slice iphonesimulator arm64 17.0 "$slice_dir/iphonesimulator-arm64.a"
build_slice iphonesimulator x86_64 17.0 "$slice_dir/iphonesimulator-x86_64.a"
lipo -create \
  "$slice_dir/iphonesimulator-arm64.a" \
  "$slice_dir/iphonesimulator-x86_64.a" \
  -output "$slice_dir/iphonesimulator-universal.a"

headers="$slice_dir/headers"
mkdir -p "$headers"
cp "$work_dir/src/hev-main.h" "$headers/"
cp "$work_dir/module.modulemap" "$headers/"
rm -rf "$output_dir/HevSocks5Tunnel.xcframework"
xcodebuild -create-xcframework \
  -library "$slice_dir/iphoneos-arm64.a" -headers "$headers" \
  -library "$slice_dir/iphonesimulator-universal.a" -headers "$headers" \
  -output "$output_dir/HevSocks5Tunnel.xcframework"
