#!/usr/bin/env sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

make_asset() {
  version="$1"
  dir="$2"
  mkdir -p "$dir/build" "$dir/assets"
  cat >"$dir/build/skirk" <<EOF
#!/usr/bin/env sh
if [ "\${1:-}" = "version" ]; then
  echo "skirk $version"
  exit 0
fi
echo "fake skirk $version"
EOF
  chmod 0755 "$dir/build/skirk"
  tar -czf "$dir/assets/skirk-linux-amd64.tar.gz" -C "$dir/build" skirk
}

if sh install.sh --version main >/tmp/skirk-installer-main.out 2>&1; then
  echo "error: installer accepted non-release version main" >&2
  exit 1
fi

good="$tmp/good"
make_asset v9.9.9 "$good"
SKIRK_ASSET_BASE="file://$good/assets" \
  SKIRK_INSTALL_DIR="$tmp/install-good" \
  sh install.sh --dev-install --version v9.9.9 >/tmp/skirk-installer-good.out 2>&1

if [ "$("$tmp/install-good/skirk" version)" != "skirk v9.9.9" ]; then
  echo "error: installer smoke installed wrong fake version" >&2
  exit 1
fi

bad="$tmp/bad"
make_asset v9.9.8 "$bad"
if SKIRK_ASSET_BASE="file://$bad/assets" \
  SKIRK_INSTALL_DIR="$tmp/install-bad" \
  sh install.sh --dev-install --version v9.9.9 >/tmp/skirk-installer-bad.out 2>&1; then
  echo "error: installer accepted release asset with mismatched version" >&2
  exit 1
fi
if [ -e "$tmp/install-bad/skirk" ]; then
  echo "error: installer left mismatched fake binary installed" >&2
  exit 1
fi

fakebin="$tmp/fakebin"
mkdir -p "$fakebin"
cat >"$fakebin/curl" <<EOF
#!/usr/bin/env sh
printf '%s\n' "\$*" >"$tmp/curl-args"
exit 22
EOF
chmod 0755 "$fakebin/curl"

if PATH="$fakebin:$PATH" \
  SKIRK_REPO=bad/repo \
  SKIRK_ASSET_BASE="file://$good/assets" \
  SKIRK_INSTALL_DIR="$tmp/install-hostile" \
  sh install.sh --version v9.9.9 >/tmp/skirk-installer-hostile.out 2>&1; then
  echo "error: installer accepted hostile release env without --dev-install" >&2
  exit 1
fi
if grep -Eq 'file://|bad/repo' "$tmp/curl-args"; then
  echo "error: installer used hostile SKIRK_REPO or SKIRK_ASSET_BASE in normal mode" >&2
  cat "$tmp/curl-args" >&2
  exit 1
fi
if ! grep -Fq 'github.com/ShahabSL/Skirk/releases/download/v9.9.9/skirk-linux-amd64.tar.gz' "$tmp/curl-args"; then
  echo "error: installer did not use canonical release URL in normal mode" >&2
  cat "$tmp/curl-args" >&2
  exit 1
fi
if [ -e "$tmp/install-hostile/skirk" ]; then
  echo "error: installer left binary installed after hostile-env release failure" >&2
  exit 1
fi

echo "installer smoke ok"
