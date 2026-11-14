#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_dir="$repo_root/third_party/hev-socks5-tunnel"
work_dir="$repo_root/.skirk-runs/hev-mapped-dns-test"
copy_dir="$work_dir/source"
mkdir -p "$copy_dir"

# Build in an ignored copy so Android and Apple object files cannot mix.
rsync -a --delete \
  --exclude 'build/' --exclude 'bin/' --exclude 'obj/' --exclude 'libs/' \
  "$source_dir/" "$copy_dir/"

for dependency in hev-task-system yaml; do
  log="$work_dir/$dependency-build.log"
  if ! make -C "$copy_dir/third-part/$dependency" static >"$log" 2>&1; then
    tail -40 "$log"
    exit 1
  fi
done

# The vendored Makefiles' `ar csq` emits empty archives with Apple ar. Repack
# the built objects into test-only archives; the release builder uses libtool.
find "$copy_dir/third-part/hev-task-system/build" -name '*.o' \
  -exec ar -rcs "$work_dir/libtask.a" {} +
find "$copy_dir/third-part/yaml/build" -name '*.o' \
  -exec ar -rcs "$work_dir/libyaml.a" {} +

"${CC:-clang}" -O1 -g -Wall -Werror -fsanitize=address,undefined \
  -fno-omit-frame-pointer \
  -I"$copy_dir" \
  -I"$copy_dir/src" \
  -I"$copy_dir/src/misc" \
  -I"$copy_dir/src/core/include" \
  -I"$copy_dir/third-part/yaml/include" \
  -I"$copy_dir/third-part/lwip/src/include" \
  -I"$copy_dir/third-part/lwip/src/ports/include" \
  -I"$copy_dir/third-part/hev-task-system/include" \
  "$copy_dir/tests/test_mapdns_ipv6.c" \
  "$copy_dir/src/hev-mapped-dns.c" \
  "$copy_dir/src/hev-config.c" \
  "$copy_dir/src/misc/hev-utils.c" \
  "$copy_dir/src/core/src/hev-rbtree.c" \
  "$copy_dir/src/core/src/hev-socks5-misc.c" \
  "$copy_dir/src/core/src/hev-socks5-logger.c" \
  "$copy_dir/src/misc/hev-list.c" \
  "$copy_dir/src/misc/hev-logger.c" \
  "$work_dir/libtask.a" "$work_dir/libyaml.a" \
  -o "$work_dir/test_mapdns_ipv6"

"$work_dir/test_mapdns_ipv6"
