#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/skirk-hev-udp.XXXXXXXX")"
trap 'rm -rf "$work"' EXIT
tree="$work/source"
mkdir -p "$tree"
rsync -a --exclude build --exclude bin --exclude obj --exclude libs \
    --exclude apple_xcframework --exclude HevSocks5Tunnel.xcframework \
    "$root/third_party/hev-socks5-tunnel/" "$tree/"

compiler="${CC:-clang}"
if ! make -C "$tree" -j "${SKIRK_HEV_TEST_JOBS:-2}" PP="$compiler" CC="$compiler" static >"$work/build.log" 2>&1; then
    tail -60 "$work/build.log"
    exit 1
fi

# Build the dependency archive explicitly because Apple's ar can emit empty
# archives for the vendor Makefile's csq invocation. Keep dependencies in an
# archive so shared utility symbols are resolved only once.
objects=()
while IFS= read -r -d '' object; do
    objects+=("$object")
done < <(find "$tree/build" -type f -name '*.o' -print0)
find "$tree/third-part/lwip/build" "$tree/third-part/yaml/build" \
    "$tree/third-part/hev-task-system/build" -type f -name '*.o' \
    -exec "${AR:-ar}" -rcs "$work/dependencies.a" {} +
"$compiler" -Wall -Werror -I"$tree/include" "$tree/tests/udp_tunnel_runner.c" \
    "${objects[@]}" "$work/dependencies.a" -pthread -o "$work/hev-tunnel-test"
python3 "$tree/tests/test_udp_fragments.py" "$work/hev-tunnel-test"

# Recompile only the two UDP translation units for fault injection. The test
# includes the real session forwarder and replaces framed socket I/O.
send_objects=()
for object in "${objects[@]}"; do
    case "$object" in
        */hev-socks5-session-udp.o|*/core/src/hev-socks5-udp.o) ;;
        *) send_objects+=("$object") ;;
    esac
done
"$compiler" -Wall -Werror -I"$tree/src" -I"$tree/src/misc" \
    -I"$tree/src/core/include" -I"$tree/third-part/lwip/src/include" \
    -I"$tree/third-part/lwip/src/ports/include" \
    -I"$tree/third-part/hev-task-system/include" \
    "$tree/tests/test_udp_send_failure.c" "$tree/tests/udp_send_injected.c" \
    "${send_objects[@]}" "$work/dependencies.a" -pthread -o "$work/hev-send-test"
"$work/hev-send-test"
