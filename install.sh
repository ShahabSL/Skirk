#!/bin/sh
set -eu

repo="${SKIRK_REPO:-ShahabSL/Skirk}"
version="${SKIRK_VERSION:-latest}"
install_dir="${SKIRK_INSTALL_DIR:-$HOME/.local/bin}"
asset_base="${SKIRK_ASSET_BASE:-}"
server_setup="${SKIRK_SERVER_SETUP:-}"
adc_path="${SKIRK_ADC:-}"
setup_out="${SKIRK_SETUP_OUT:-skirk-kit}"
install_systemd="${SKIRK_INSTALL_SYSTEMD:-}"
service_name="${SKIRK_SERVICE_NAME:-skirk-exit}"
service_user="${SKIRK_SERVICE_USER:-}"
exit_proxy="${SKIRK_EXIT_PROXY:-}"
install_wireproxy="${SKIRK_INSTALL_WIREPROXY:-}"
wireproxy_bind="${SKIRK_WIREPROXY_BIND:-127.0.0.1:40000}"
wireproxy_dir="${SKIRK_WIREPROXY_DIR:-/etc/wireproxy}"
wireproxy_bin="${SKIRK_WIREPROXY_BIN:-/usr/local/bin/wireproxy}"
wireproxy_version="${SKIRK_WIREPROXY_VERSION:-v1.1.2}"
wgcf_bin="${SKIRK_WGCF_BIN:-/usr/local/bin/wgcf}"
wgcf_version="${SKIRK_WGCF_VERSION:-v2.2.30}"

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

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    echo "error: this step needs root or sudo: $*" >&2
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
  install_tmp="$install_dir/.skirk.tmp.$$"
  cp "$tmp/skirk" "$install_tmp"
  chmod 0755 "$install_tmp"
  mv "$install_tmp" "$install_dir/skirk"
}

install_warp_wireproxy() {
  if [ "$install_wireproxy" != "1" ]; then
    return 0
  fi
  need tar

  case "$platform" in
    linux-amd64|linux-arm64) ;;
    *) echo "error: WARP wireproxy installer supports linux-amd64 and linux-arm64 only (got $platform)" >&2; exit 1 ;;
  esac

  if [ -z "$exit_proxy" ]; then
    exit_proxy="socks5h://$wireproxy_bind"
  fi

  tmp_warp="$tmp/warp"
  mkdir -p "$tmp_warp"
  wgcf_arch="${platform#linux-}"
  wgcf_no_v="${wgcf_version#v}"
  wgcf_url="https://github.com/ViRb3/wgcf/releases/download/$wgcf_version/wgcf_${wgcf_no_v}_linux_${wgcf_arch}"
  wireproxy_url="https://github.com/windtf/wireproxy/releases/download/$wireproxy_version/wireproxy_linux_${wgcf_arch}.tar.gz"

  echo "Installing WARP wireproxy..."
  download "$wgcf_url" "$tmp_warp/wgcf"
  chmod 0755 "$tmp_warp/wgcf"
  run_root install -m 0755 "$tmp_warp/wgcf" "$wgcf_bin"

  download "$wireproxy_url" "$tmp_warp/wireproxy.tar.gz"
  mkdir "$tmp_warp/wireproxy"
  tar -xzf "$tmp_warp/wireproxy.tar.gz" -C "$tmp_warp/wireproxy"
  wireproxy_src="$(find "$tmp_warp/wireproxy" -type f -name wireproxy -perm -111 | head -n 1)"
  if [ -z "$wireproxy_src" ]; then
    echo "error: wireproxy release archive did not contain executable wireproxy" >&2
    exit 1
  fi
  run_root install -m 0755 "$wireproxy_src" "$wireproxy_bin"

  run_root mkdir -p "$wireproxy_dir"
  run_root chmod 700 "$wireproxy_dir"
  if [ ! -f "$wireproxy_dir/wgcf-account.toml" ] || [ "${SKIRK_RESET_WARP:-}" = "1" ]; then
    (
      cd "$tmp_warp"
      if [ "${SKIRK_ACCEPT_WARP_TOS:-}" = "1" ]; then
        printf "yes\n" | "$wgcf_bin" register
      else
        "$wgcf_bin" register
      fi
      "$wgcf_bin" generate --profile "$tmp_warp/wgcf-profile.conf"
    )
    run_root install -m 0600 "$tmp_warp/wgcf-account.toml" "$wireproxy_dir/wgcf-account.toml"
    run_root install -m 0600 "$tmp_warp/wgcf-profile.conf" "$wireproxy_dir/wgcf-profile.conf"
  elif [ ! -f "$wireproxy_dir/wgcf-profile.conf" ]; then
    cp "$wireproxy_dir/wgcf-account.toml" "$tmp_warp/wgcf-account.toml"
    (
      cd "$tmp_warp"
      "$wgcf_bin" generate --profile "$tmp_warp/wgcf-profile.conf"
    )
    run_root install -m 0600 "$tmp_warp/wgcf-profile.conf" "$wireproxy_dir/wgcf-profile.conf"
  fi

  wireproxy_conf="$tmp_warp/wireproxy.conf"
  cat >"$wireproxy_conf" <<EOF
WGConfig = $wireproxy_dir/wgcf-profile.conf

[Socks5]
BindAddress = $wireproxy_bind
EOF
  run_root install -m 0600 "$wireproxy_conf" "$wireproxy_dir/wireproxy.conf"

  if command -v systemctl >/dev/null 2>&1; then
    wireproxy_unit="$tmp_warp/wireproxy.service"
    cat >"$wireproxy_unit" <<EOF
[Unit]
Description=Wireproxy WARP SOCKS proxy for Skirk exit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$wireproxy_bin -c $wireproxy_dir/wireproxy.conf
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadOnlyPaths=$wireproxy_dir

[Install]
WantedBy=multi-user.target
EOF
    run_root install -m 0644 "$wireproxy_unit" /etc/systemd/system/wireproxy.service
    run_root systemctl daemon-reload
    run_root systemctl enable --now wireproxy.service
  fi

  echo "WARP wireproxy listening on $wireproxy_bind"
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
  build_out="$tmp/skirk-built"
  (
    cd "$tmp/source"
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$build_out" ./cmd/skirk
  )
  install_tmp="$install_dir/.skirk.tmp.$$"
  cp "$build_out" "$install_tmp"
  chmod 0755 "$install_tmp"
  mv "$install_tmp" "$install_dir/skirk"
}

run_server_setup() {
  if [ "$server_setup" != "1" ]; then
    return 0
  fi

  set -- setup init --out "$setup_out"
  if [ -n "$adc_path" ]; then
    set -- "$@" --adc "$adc_path"
  fi
  if [ -n "${SKIRK_OAUTH_CLIENT_FILE:-}" ]; then
    set -- "$@" --oauth-client-file "$SKIRK_OAUTH_CLIENT_FILE"
  fi
  if [ "${SKIRK_RESET_GOOGLE_LOGIN:-}" = "1" ]; then
    set -- "$@" --reset-google-login
  fi
  if [ -n "${SKIRK_CLIENT_ROUTE:-}" ]; then
    set -- "$@" --client-route "$SKIRK_CLIENT_ROUTE"
  fi
  if [ -n "${SKIRK_EXIT_ROUTE:-}" ]; then
    set -- "$@" --exit-route "$SKIRK_EXIT_ROUTE"
  fi
  if [ -n "$exit_proxy" ]; then
    set -- "$@" --exit-proxy "$exit_proxy"
  fi

  echo
  echo "Running server setup..."
  "$install_dir/skirk" "$@"

  if [ "$install_systemd" = "1" ]; then
    install_exit_service
  fi
}

install_exit_service() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "Note: systemd is not available; start the exit manually with: $install_dir/skirk serve-exit --config $setup_out/exit.json" >&2
    return 0
  fi
  if [ -z "$service_user" ]; then
    service_user="$(id -un)"
  fi
  config_path="$setup_out/exit.json"
  case "$config_path" in
    /*) ;;
    *) config_path="$(pwd)/$config_path" ;;
  esac
  unit="/etc/systemd/system/$service_name.service"
  tmp_unit="$(mktemp)"
  cat >"$tmp_unit" <<EOF
[Unit]
Description=Skirk exit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$service_user
WorkingDirectory=$(dirname "$config_path")
ExecStart=$install_dir/skirk serve-exit --config $config_path
Restart=always
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
  if [ "$install_wireproxy" = "1" ]; then
    tmp_with_deps="$(mktemp)"
    awk '
      /^After=/ { print $0 " wireproxy.service"; next }
      /^Wants=/ { print $0 " wireproxy.service"; next }
      { print }
    ' "$tmp_unit" >"$tmp_with_deps"
    mv "$tmp_with_deps" "$tmp_unit"
  fi
  if [ "$(id -u)" -eq 0 ]; then
    cp "$tmp_unit" "$unit"
    systemctl daemon-reload
    systemctl enable --now "$service_name"
  elif command -v sudo >/dev/null 2>&1; then
    sudo cp "$tmp_unit" "$unit"
    sudo systemctl daemon-reload
    sudo systemctl enable --now "$service_name"
  else
    echo "Note: sudo is unavailable; systemd service file was left at $tmp_unit" >&2
    return 0
  fi
  rm -f "$tmp_unit"
  echo "Installed and started systemd service: $service_name"
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
  install_warp_wireproxy

  echo
  "$install_dir/skirk" version
  echo "Installed: $install_dir/skirk"
  case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) echo "Current shell may not find skirk yet. Run: export PATH=\"$install_dir:\$PATH\"" ;;
  esac
  echo
  echo "Next: run '$install_dir/skirk' for the operator menu, or '$install_dir/skirk setup init --out skirk-kit'."
  echo "Server setup will check/install Google Cloud CLI if Google login is needed."

  run_server_setup
}

main "$@"
